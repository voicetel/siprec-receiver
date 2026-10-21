// Package siprecsrv is a SIPREC Session Recording Server (SRS,
// RFC 7866/7865). It accepts SIPREC INVITEs (a multipart body of SDP
// + RS-Metadata), opens one RTP receiver per offered audio stream,
// writes each stream to a WAV file via internal/recording, and hands
// the finished recording to a pluggable Finalizer when the dialog
// ends. It is built for FreeSWITCH's mod_siprec and interoperates
// with any compliant Session Recording Client.
//
// Sessions are keyed by the SIPREC dialog's Call-ID and are shared
// across every listener (UDP/TCP/TLS) of a Server family: a dialog
// may start on UDP and its BYE arrive on TCP. In-dialog requests are
// checked against the dialog's From-tag and source IP. Recordings
// are written to server-generated (random) filenames opened with
// O_EXCL, so a reused or colliding Call-ID can never overwrite an
// existing recording.
package siprecsrv

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"github.com/voicetel/siprec-receiver/internal/recording"
	"github.com/voicetel/siprec-receiver/internal/rtp"
)

// sipRecordingSampleRate is the fixed 8kHz sample rate of the G.711
// PCMU/PCMA codecs this SRS answers with — the only codecs it offers
// in BuildAnswerSDP, so every recorded leg is always this rate.
const sipRecordingSampleRate = 8000

// maxStreamsPerInvite caps the m=audio lines one INVITE can make us
// allocate ports and files for. SIPREC needs at most 2 (separate
// streams); 8 is generous headroom, not a target.
const maxStreamsPerInvite = 8

const (
	defaultMaxSessions  = 256
	defaultIdleTimeout  = 5 * time.Minute
	pausedIdleTimeout   = 24 * time.Hour // paused media is intentionally silent
	pendingTimeout      = 60 * time.Second
	maxLegSamples       = 8000 * 4 * 3600 // 4 h per leg, hard cap
	rateAllowanceFactor = 1.5             // vs. the 8 kHz sample clock
	rateAllowanceBase   = 24000           // samples of burst headroom
)

const reaperInterval = 15 * time.Second

// FinalizedLeg describes one completed, closed recording leg — everything
// a Finalizer needs to persist it.
type FinalizedLeg struct {
	CallID      string // SIP Call-ID of the SIPREC (recording) dialog
	AccountSid  string // from the INVITE Request-URI's vm-account param; "" if absent
	CallSid     string // from the INVITE Request-URI's vm-callsid param; "" if absent
	FsUUID      string // parent FreeSWITCH call UUID decoded from the metadata session_id; "" if unavailable
	FromURI     string // SIP From header of the recording INVITE
	Label       string // participant/leg label (see resolveLegLabel)
	Codec       string // negotiated codec name (PCMU/PCMA)
	WAVPath     string
	DurationSec int
	StartedAt   time.Time
	Metadata    *Recording // parsed RS-Metadata (same pointer for every leg of a call)
	MetadataXML string     // raw RS-Metadata body as received; "" when absent
	WriteErr    string     // first WAV write/close error, "" when the leg recorded cleanly
}

// Finalizer is notified once per leg when a SIPREC session ends (BYE,
// reaper, shutdown, or a re-INVITE replacing the media shape). A nil
// Finalizer just leaves the WAV files on disk.
type Finalizer interface {
	Finalize(ctx context.Context, leg FinalizedLeg) error
}

// sharedState is everything the listeners of one Server family share:
// the session map (a dialog may cross transports), the port pool,
// and the recording sink.
type sharedState struct {
	localIP      string
	rtpBindIP    string
	recordingDir string
	pool         *rtp.PortPool
	finalizer    Finalizer

	allowCIDRs  []*net.IPNet // empty = allow every source (firewall instead)
	maxSessions int
	idleTimeout time.Duration

	reaperTick time.Duration // how often the reaper scans; tests shrink it

	mu         sync.Mutex
	sessions   map[string]*session
	reaperOnce sync.Once
	stopping   bool
	stop       chan struct{}
	work       sync.WaitGroup // handlers and reaper, admitted under mu
	shutdownMu sync.Mutex     // concurrent Shutdown callers join the same drain
}

// Server is one SIP listener of an SRS family. Create the first with
// New, then Clone it per additional transport so all listeners share
// one session map.
type Server struct {
	// LocalIP, RecordingDir, PortPool, Finalizer mirror the shared
	// state for inspection; set them via New/SetLimits before serving.
	LocalIP      string
	RecordingDir string
	PortPool     *rtp.PortPool
	Finalizer    Finalizer

	shared  *sharedState
	ua      *sipgo.UserAgent
	srv     *sipgo.Server
	contact string // this listener's Contact URI, set by Serve/ServeTLS
}

// New builds the first Server of a family. Additional transports use
// Clone. Defaults: 256 concurrent sessions, 5-minute idle reaping,
// no source allowlist.
func New(localIP, recordingDir string, portPool *rtp.PortPool) *Server {
	shared := &sharedState{
		localIP:      localIP,
		rtpBindIP:    localIP,
		recordingDir: recordingDir,
		pool:         portPool,
		maxSessions:  defaultMaxSessions,
		idleTimeout:  defaultIdleTimeout,
		reaperTick:   reaperInterval,
		sessions:     make(map[string]*session),
		stop:         make(chan struct{}),
	}

	return &Server{
		LocalIP:      localIP,
		RecordingDir: recordingDir,
		PortPool:     portPool,
		shared:       shared,
	}
}

// SetLimits configures the shared family limits. Call on the New
// server before Clone/Serve. allowCIDRs empty means "accept every
// source" (rely on the firewall); maxSessions/idleTimeout <= 0 keep
// the defaults.
func (s *Server) SetLimits(allowCIDRs []*net.IPNet, maxSessions int, idleTimeout time.Duration) {
	s.shared.allowCIDRs = allowCIDRs

	if maxSessions > 0 {
		s.shared.maxSessions = maxSessions
	}

	if idleTimeout > 0 {
		s.shared.idleTimeout = idleTimeout
	}
}

// SetFinalizer wires the family's Finalizer (New leaves it nil).
func (s *Server) SetFinalizer(f Finalizer) {
	s.shared.finalizer = f
	s.Finalizer = f
}

// SetRTPBindIP separates the local UDP bind address from the address
// advertised in SDP, e.g. behind NAT. Call before Clone or Serve.
// Empty means all IPv4 interfaces. New defaults to the advertised IP.
func (s *Server) SetRTPBindIP(ip string) {
	s.shared.rtpBindIP = ip
}

// Clone returns another listener sharing this server's sessions,
// pool, and limits.
func (s *Server) Clone() *Server {
	return &Server{
		LocalIP:      s.LocalIP,
		RecordingDir: s.RecordingDir,
		PortPool:     s.PortPool,
		Finalizer:    s.Finalizer,
		shared:       s.shared,
	}
}

// session tracks one active SIPREC dialog.
type session struct {
	callID      string
	accountSid  string
	callSid     string
	fromURI     string
	fromTag     string
	toTag       string
	remoteCSeq  uint32
	srcIP       string
	legs        []*leg
	started     time.Time
	metadata    *Recording // parsed RS-Metadata, nil when absent
	metadataXML string
	paused      bool
	pending     bool // INVITE received, legs still being built
	cancelled   bool // BYE/shutdown arrived while pending
	answerVer   int
	offer       *ParsedSDP // all offered m-lines, including declined streams

	lastActivity atomic.Int64 // unix nano: last SIP message or RTP frame
}

func (sess *session) touch() {
	sess.lastActivity.Store(time.Now().UnixNano())
}

// leg is one recorded RTP stream (one offered m=audio block).
type leg struct {
	label    string
	codec    string
	port     int
	wavPath  string
	receiver *rtp.Receiver
	writer   *legWriter
}

// legWriter owns one leg's WAV writer plus the first write error it
// produced. The RTP callback runs on the receiver goroutine and the
// writer is single-goroutine by contract, so appends stay on that
// goroutine; the error slot is atomic because finalizeSession reads
// it from another goroutine. Capturing the error matters: a full disk
// otherwise truncates a recording silently. The writer also enforces
// a sane sample budget (rate vs. wall clock, and a 4 h cap) so a
// packet flood cannot fill the disk through one leg.
type legWriter struct {
	w       *recording.Writer
	call    string // callID + label, for the one-time log line
	onPCM   func()
	started time.Time
	samples int64
	capped  bool
	err     atomic.Value // error, first one wins
	once    sync.Once
}

func (lw *legWriter) append(pcm []int16) {
	if lw.onPCM != nil {
		lw.onPCM()
	}

	if lw.w == nil || lw.capped {
		return
	}

	lw.samples += int64(len(pcm))

	elapsed := time.Since(lw.started).Seconds()
	if lw.samples > maxLegSamples ||
		float64(lw.samples) > elapsed*sipRecordingSampleRate*rateAllowanceFactor+rateAllowanceBase {
		lw.capped = true
		lw.noteErr(fmt.Errorf("recording sample budget exceeded (samples=%d elapsed=%.1fs); leg truncated",
			lw.samples, elapsed))

		return
	}

	if _, err := lw.w.Append(pcm); err != nil {
		lw.noteErr(err)
	}
}

func (lw *legWriter) noteErr(err error) {
	lw.once.Do(func() {
		lw.err.Store(err)
		log.Printf("[siprecsrv] %s: WAV WRITE FAILED (recording degraded): %v", lw.call, err)
	})
}

// Err returns the first write/close error, or nil.
func (lw *legWriter) Err() error {
	if v := lw.err.Load(); v != nil {
		return v.(error)
	}

	return nil
}

func (lw *legWriter) durationSec() int {
	if lw.w == nil {
		return 0
	}

	return lw.w.DurationSec()
}

func (lw *legWriter) close() error {
	if lw.w == nil {
		return nil
	}

	if err := lw.w.Close(); err != nil {
		lw.noteErr(err)

		return err
	}

	return nil
}

// SessionInfo describes one active SIPREC session for read-only
// introspection (test assertions, debug endpoints).
type SessionInfo struct {
	CallID     string
	AccountSid string
	Started    time.Time
	Paused     bool
	WAVFiles   []string
	Recording  *Recording // parsed RS-Metadata, nil when none
}

// SessionsSnapshot returns a copy of every active SIPREC session's
// metadata.
func (s *Server) SessionsSnapshot() []SessionInfo {
	sh := s.shared

	sh.mu.Lock()
	defer sh.mu.Unlock()

	out := make([]SessionInfo, 0, len(sh.sessions))

	for _, sess := range sh.sessions {
		paths := make([]string, 0, len(sess.legs))
		for _, l := range sess.legs {
			paths = append(paths, l.wavPath)
		}

		out = append(out, SessionInfo{
			CallID:     sess.callID,
			AccountSid: sess.accountSid,
			Started:    sess.started,
			Paused:     sess.paused,
			WAVFiles:   paths,
			Recording:  sess.metadata,
		})
	}

	return out
}

// syncFinalizer adopts a Finalizer assigned to the exported field
// (the pre-Clone wiring style) into the shared state. Called when a
// listener starts; SetFinalizer is the explicit alternative.
func (s *Server) syncFinalizer() {
	if s.Finalizer != nil && s.shared.finalizer == nil {
		s.shared.finalizer = s.Finalizer
	}
}

// registerHandlers wires the SIP method handlers shared by Serve and
// ServeTLS.
func (s *Server) registerHandlers(srv *sipgo.Server) {
	srv.OnInvite(s.onInvite)
	srv.OnAck(s.onAck)
	srv.OnBye(s.onBye)
	srv.OnOptions(s.onOptions)
}

// Serve binds a SIP listener on addr (e.g. "127.0.0.1:5070") and
// dispatches INVITE / ACK / BYE / OPTIONS. Blocks until ctx is
// cancelled.
func (s *Server) Serve(ctx context.Context, network, addr string) error {
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("siprec-receiver"))
	if err != nil {
		return fmt.Errorf("siprecsrv: new ua: %w", err)
	}

	defer func() { _ = ua.Close() }()

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		return fmt.Errorf("siprecsrv: new server: %w", err)
	}

	s.ua, s.srv = ua, srv
	s.contact = contactFor(s.shared.localIP, network, addr)
	s.syncFinalizer()
	s.registerHandlers(srv)
	s.shared.startReaper(ctx)

	log.Printf("[siprecsrv] listening on %s/%s, advertising RTP at %s",
		network, addr, s.shared.localIP)

	return srv.ListenAndServe(ctx, network, addr)
}

// ServeTLS is Serve over a TLS listener (SIPS, RFC 7866 §11.3) for
// SRCs configured with transport=tls.
func (s *Server) ServeTLS(ctx context.Context, addr string, conf *tls.Config) error {
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("siprec-receiver"))
	if err != nil {
		return fmt.Errorf("siprecsrv: new ua: %w", err)
	}

	defer func() { _ = ua.Close() }()

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		return fmt.Errorf("siprecsrv: new server: %w", err)
	}

	s.ua, s.srv = ua, srv
	s.contact = contactFor(s.shared.localIP, "tls", addr)
	s.syncFinalizer()
	s.registerHandlers(srv)
	s.shared.startReaper(ctx)

	log.Printf("[siprecsrv] listening on tls/%s, advertising RTP at %s", addr, s.shared.localIP)

	return srv.ListenAndServeTLS(ctx, "tcp", addr, conf)
}

// contactFor builds this listener's Contact URI (RFC 3261 §12.1.1
// requires one in 2xx responses to INVITE, or strict clients cannot
// address in-dialog requests to us).
func contactFor(localIP, transport, addr string) string {
	host := localIP

	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		port = "5060"
	}

	if host == "" || host == "0.0.0.0" || host == "::" {
		if h, _, herr := net.SplitHostPort(addr); herr == nil && h != "" && h != "0.0.0.0" && h != "::" {
			host = h
		} else {
			host = "127.0.0.1"
		}
	}

	return fmt.Sprintf("<sip:srs@%s:%s;transport=%s>", host, port, transport)
}

// Shutdown finalizes every active session (as if each dialog had
// sent BYE) so a stopping process never strands open WAVs without
// database rows. Pending sessions are marked cancelled; their
// builders roll themselves back. Safe to call more than once and
// from any clone of the family.
func (s *Server) Shutdown() {
	sh := s.shared
	sh.shutdownMu.Lock()
	defer sh.shutdownMu.Unlock()

	sh.mu.Lock()
	if !sh.stopping {
		sh.stopping = true
		close(sh.stop)
	}

	victims := make([]*session, 0, len(sh.sessions))
	for id, sess := range sh.sessions {
		if sess.pending {
			sess.cancelled = true
		} else {
			victims = append(victims, sess)
		}

		delete(sh.sessions, id)
	}
	sh.mu.Unlock()

	for _, sess := range victims {
		sh.finalizeSession(context.Background(), sess)
	}
	sh.work.Wait()

	if len(victims) > 0 {
		log.Printf("[siprecsrv] shutdown: finalized %d active session(s)", len(victims))
	}
}

// Admission and shutdown use the same lock: no Add can race a Wait
// after shutdown starts, even when no sessions remain in the map.
func (sh *sharedState) beginWork() bool {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.stopping {
		return false
	}
	sh.work.Add(1)
	return true
}

// startReaper launches the family idle-session reaper once.
func (sh *sharedState) startReaper(ctx context.Context) {
	sh.reaperOnce.Do(func() {
		if sh.beginWork() {
			go func() {
				defer sh.work.Done()
				sh.reaperLoop(ctx)
			}()
		}
	})
}

// reaperLoop finalizes sessions that have seen no SIP message and no
// RTP for the idle timeout (the BYE was lost, the SRC crashed, the
// network dropped) and drops pending reservations that never
// completed, so neither ports nor files leak forever.
func (sh *sharedState) reaperLoop(ctx context.Context) {
	ticker := time.NewTicker(sh.reaperTick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-sh.stop:
			return
		case now := <-ticker.C:
			var victims []*session

			sh.mu.Lock()
			for id, sess := range sh.sessions {
				idle := now.Sub(time.Unix(0, sess.lastActivity.Load()))
				timeout := sh.idleTimeout
				if sess.paused && timeout < pausedIdleTimeout {
					timeout = pausedIdleTimeout
				}
				switch {
				case sess.pending && now.Sub(sess.started) > pendingTimeout:
					sess.cancelled = true
					delete(sh.sessions, id)
				case !sess.pending && idle > timeout:
					delete(sh.sessions, id)
					victims = append(victims, sess)
				}
			}
			sh.mu.Unlock()

			for _, sess := range victims {
				log.Printf("[siprecsrv] reaping idle session call-id=%s paused=%v", sess.callID, sess.paused)
				sh.finalizeSession(context.Background(), sess)
			}
		}
	}
}

func (sh *sharedState) finalizeSession(ctx context.Context, sess *session) {
	for _, l := range sess.legs {
		l.receiver.Stop()

		if err := l.writer.close(); err != nil {
			log.Printf("[siprecsrv] %s: wav close: %v", sess.callID, err)
		}

		dur := l.writer.durationSec()

		sh.pool.Release(l.port)

		if sh.finalizer == nil {
			continue
		}

		fsUUID, _ := sess.metadata.FsUUID()

		writeErr := ""
		if werr := l.writer.Err(); werr != nil {
			writeErr = werr.Error()
		}

		if err := sh.finalizer.Finalize(ctx, FinalizedLeg{
			CallID:      sess.callID,
			AccountSid:  sess.accountSid,
			CallSid:     sess.callSid,
			FsUUID:      fsUUID,
			FromURI:     sess.fromURI,
			Label:       l.label,
			Codec:       l.codec,
			WAVPath:     l.wavPath,
			DurationSec: dur,
			StartedAt:   sess.started,
			Metadata:    sess.metadata,
			MetadataXML: sess.metadataXML,
			WriteErr:    writeErr,
		}); err != nil {
			log.Printf("[siprecsrv] %s: finalize leg %s: %v", sess.callID, l.label, err)
		}
	}
}

// allowedSource enforces the family's source CIDR allowlist. An empty
// list allows everything (the listener is expected to be firewalled).
func (sh *sharedState) allowedSource(ip string) bool {
	if len(sh.allowCIDRs) == 0 {
		return true
	}

	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}

	for _, cidr := range sh.allowCIDRs {
		if cidr.Contains(parsed) {
			return true
		}
	}

	return false
}

// dialogMismatch reports an in-dialog request that does not belong to
// the session (wrong From-tag or a different source IP than the
// INVITE). Either field may be empty on the session when the INVITE
// didn't carry it; empty means "cannot check, allow".
func (sess *session) dialogMismatch(req *sip.Request) bool {
	if sess.fromTag != "" {
		if tag := fromTagOf(req); tag != sess.fromTag {
			return true
		}
	}
	if sess.toTag != "" {
		to := req.To()
		if to == nil {
			return true
		}
		tag, _ := to.Params.Get("tag")
		if tag != sess.toTag {
			return true
		}
	}

	if sess.srcIP != "" {
		if ip := srcIPOf(req); ip != "" && ip != sess.srcIP {
			return true
		}
	}

	return false
}

func fromTagOf(req *sip.Request) string {
	if f := req.From(); f != nil {
		tag, _ := f.Params.Get("tag")

		return tag
	}

	return ""
}

func srcIPOf(req *sip.Request) string {
	src := req.Source()
	if host, _, err := net.SplitHostPort(src); err == nil {
		return host
	}

	return src
}

func (s *Server) respond(req *sip.Request, tx sip.ServerTransaction, code int, reason string, body []byte, contentType string) {
	s.respondWithTag(req, tx, code, reason, body, contentType, "")
}

func (s *Server) respondWithTag(req *sip.Request, tx sip.ServerTransaction, code int, reason string, body []byte, contentType, tag string) {
	resp := sip.NewResponseFromRequest(req, code, reason, body)
	if tag != "" {
		resp.To().Params.Add("tag", tag)
	}
	if contentType != "" {
		resp.AppendHeader(sip.NewHeader("Content-Type", contentType))
	}

	if code >= 200 && code < 300 && s.contact != "" {
		resp.AppendHeader(sip.NewHeader("Contact", s.contact))
	}

	_ = tx.Respond(resp)
}

func (s *Server) onOptions(req *sip.Request, tx sip.ServerTransaction) {
	resp := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	resp.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, BYE, OPTIONS"))
	_ = tx.Respond(resp)
}

func (s *Server) onAck(req *sip.Request, _ sip.ServerTransaction) {
	callID := callIDOf(req)

	s.shared.mu.Lock()
	if sess, ok := s.shared.sessions[callID]; ok && !sess.dialogMismatch(req) &&
		(req.CSeq() != nil && req.CSeq().SeqNo == sess.remoteCSeq || sess.remoteCSeq == 0) {
		sess.touch()
	}
	s.shared.mu.Unlock()

	log.Printf("[siprecsrv] ACK call-id=%s — recording active", callID)
}

func (s *Server) onBye(req *sip.Request, tx sip.ServerTransaction) {
	if !s.shared.beginWork() {
		s.respond(req, tx, sip.StatusServiceUnavailable, "shutting down", nil, "")
		return
	}
	defer s.shared.work.Done()
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[siprecsrv] BYE handler panic: %v", rec)
		}
	}()

	callID := callIDOf(req)

	sh := s.shared
	sh.mu.Lock()
	sess, ok := sh.sessions[callID]

	if ok && sess.dialogMismatch(req) {
		sh.mu.Unlock()
		log.Printf("[siprecsrv] BYE call-id=%s rejected: dialog mismatch", callID)
		s.respond(req, tx, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil, "")

		return
	}
	if ok && sess.staleCSeq(req) {
		sh.mu.Unlock()
		s.respond(req, tx, sip.StatusInternalServerError, "CSeq out of order", nil, "")
		return
	}

	pending := ok && sess.pending
	if pending {
		sess.cancelled = true
	}
	if ok {
		delete(sh.sessions, callID)
	}
	sh.mu.Unlock()

	// Answer first: finalizing writes WAV headers and database rows,
	// which must not delay the 200 and invite BYE retransmissions.
	s.respond(req, tx, sip.StatusOK, "OK", nil, "")

	if !ok {
		log.Printf("[siprecsrv] BYE for unknown call-id=%s", callID)

		return
	}

	if pending {
		// The INVITE is still being built; flag it and let the
		// builder roll back (it may already have finished — the
		// cancelled flag is checked under the same lock).
		return
	}

	dur := time.Since(sess.started)
	sh.finalizeSession(context.Background(), sess)

	log.Printf("[siprecsrv] BYE call-id=%s duration=%s legs=%d",
		callID, dur.Round(time.Millisecond), len(sess.legs))
}

func (s *Server) onInvite(req *sip.Request, tx sip.ServerTransaction) {
	if !s.shared.beginWork() {
		s.respond(req, tx, sip.StatusServiceUnavailable, "shutting down", nil, "")
		return
	}
	defer s.shared.work.Done()
	// A handler panic must not kill every other recording.
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[siprecsrv] INVITE handler panic: %v", rec)
		}
	}()

	callID := callIDOf(req)
	sh := s.shared

	if srcIP := srcIPOf(req); !sh.allowedSource(srcIP) {
		log.Printf("[siprecsrv] INVITE call-id=%s rejected: source %s not in allowlist", callID, srcIP)
		s.respond(req, tx, sip.StatusForbidden, "Forbidden", nil, "")

		return
	}

	// Correlation keys may ride the Request-URI as parameters (a
	// per-call ad-hoc `siprec <handle> sip:srs@host:5070;vm-callsid=CA…`,
	// or an OpenSIPS recording-config lookup's ;vm-account=…).
	accountSid, _ := req.Recipient.UriParams.Get("vm-account")
	if accountSid == "" {
		accountSid, _ = req.Recipient.UriParams.Get("accountsid")
	}

	callSid, _ := req.Recipient.UriParams.Get("vm-callsid")
	if callSid == "" {
		callSid, _ = req.Recipient.UriParams.Get("callsid")
	}

	fromURI := ""
	if f := req.From(); f != nil {
		fromURI = f.Address.String()
	}

	sdp, metadata, err := extractSDPAndMetadata(req)
	if err != nil {
		log.Printf("[siprecsrv] %s: parse body: %v", callID, err)
		s.respond(req, tx, sip.StatusBadRequest, "bad SIPREC body", nil, "")

		return
	}

	parsed, err := ParseSDP(sdp)
	if err != nil {
		log.Printf("[siprecsrv] %s: parse sdp: %v", callID, err)
		s.respond(req, tx, sip.StatusBadRequest, "bad SDP", nil, "")

		return
	}

	if len(parsed.Media) > maxStreamsPerInvite {
		log.Printf("[siprecsrv] %s: %d offered streams exceeds cap %d", callID, len(parsed.Media), maxStreamsPerInvite)
		s.respond(req, tx, sip.StatusNotAcceptableHere, "too many streams", nil, "")

		return
	}

	rec, metaErr := ParseMetadata(metadata)
	if metaErr != nil {
		// Metadata parse failure is non-fatal: record by SDP label
		// position, lose the participant naming.
		log.Printf("[siprecsrv] %s: parse metadata: %v (continuing with positional labels)",
			callID, metaErr)

		rec = &Recording{}
	}
	if callSid == "" {
		callSid = rec.CallSid()
	}

	sh.mu.Lock()
	existing, isReinvite := sh.sessions[callID]

	if isReinvite && existing.pending {
		sh.mu.Unlock()
		s.respond(req, tx, sip.StatusRequestPending, "Request Pending", nil, "")

		return
	}

	if isReinvite && existing.dialogMismatch(req) {
		sh.mu.Unlock()
		log.Printf("[siprecsrv] re-INVITE call-id=%s rejected: dialog mismatch", callID)
		s.respond(req, tx, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil, "")

		return
	}
	if isReinvite && existing.staleCSeq(req) {
		sh.mu.Unlock()
		s.respond(req, tx, sip.StatusInternalServerError, "CSeq out of order", nil, "")
		return
	}

	// Re-INVITE on a live dialog. mod_siprec pause/resume (RFC 7866
	// §6.4) re-INVITEs with SDP only: a=inactive pauses, a=sendonly
	// resumes. Neither may split the recording — keep the same ports,
	// receivers and WAV writers, and just re-answer. Only a re-INVITE
	// that changes the offered media shape falls through to the
	// teardown-and-rebuild path below.
	if isReinvite && reusableMedia(existing, parsed) {
		ports := existing.answerPorts()

		existing.answerVer++

		answer, aerr := BuildAnswerSDPWithVersion(sh.localIP, parsed, ports, existing.answerVer)
		if aerr != nil {
			sh.mu.Unlock()
			log.Printf("[siprecsrv] %s: re-INVITE build answer: %v", callID, aerr)
			s.respond(req, tx, sip.StatusInternalServerError, "sdp build failed", nil, "")

			return
		}

		existing.paused = parsed.AllInactive()
		existing.offer = parsed
		if seq := req.CSeq(); seq != nil {
			existing.remoteCSeq = seq.SeqNo
		}

		// A re-INVITE whose metadata fails to parse must not replace
		// good metadata with an empty document.
		if metadata != "" && metaErr == nil {
			existing.metadata = rec
			existing.metadataXML = metadata
		}

		existing.touch()
		paused, tag := existing.paused, existing.toTag
		sh.mu.Unlock()

		log.Printf("[siprecsrv] re-INVITE call-id=%s paused=%v — recording continues on %d leg(s)",
			callID, paused, len(existing.legs))
		s.respondWithTag(req, tx, sip.StatusOK, "OK", []byte(answer), "application/sdp", tag)

		return
	}

	if !isReinvite && len(sh.sessions) >= sh.maxSessions {
		sh.mu.Unlock()
		log.Printf("[siprecsrv] INVITE call-id=%s rejected: session cap %d reached", callID, sh.maxSessions)
		s.respond(req, tx, sip.StatusServiceUnavailable, "session limit", nil, "")

		return
	}

	// Reserve the Call-ID before allocating anything, so a concurrent
	// duplicate INVITE gets 491 and a racing BYE is seen (it marks
	// the reservation cancelled) instead of leaking the session.
	sess := &session{
		callID:      callID,
		accountSid:  accountSid,
		callSid:     callSid,
		fromURI:     fromURI,
		fromTag:     fromTagOf(req),
		toTag:       sip.GenerateTagN(16),
		srcIP:       srcIPOf(req),
		started:     time.Now(),
		metadata:    rec,
		metadataXML: metadata,
		pending:     true,
		offer:       parsed,
		paused:      parsed.AllInactive(),
	}
	if seq := req.CSeq(); seq != nil {
		sess.remoteCSeq = seq.SeqNo
	}
	if isReinvite {
		sess.toTag = existing.toTag
		sess.answerVer = existing.answerVer
		if sess.accountSid == "" {
			sess.accountSid = existing.accountSid
		}
		if sess.callSid == "" {
			sess.callSid = existing.callSid
		}
		if metadata == "" || metaErr != nil {
			sess.metadata, sess.metadataXML = existing.metadata, existing.metadataXML
		}
	}
	sess.touch()
	sh.sessions[callID] = sess
	sh.mu.Unlock()

	if isReinvite {
		log.Printf("[siprecsrv] re-INVITE call-id=%s — media shape changed, replacing prior session", callID)
		sh.finalizeSession(context.Background(), existing)
	} else {
		log.Printf("[siprecsrv] INVITE call-id=%s from=%s", callID, fromURI)
	}

	legs := make([]*leg, 0, len(parsed.Media))
	answerPorts := make([]int, 0, len(parsed.Media))

	rollback := func() {
		for _, l := range legs {
			l.receiver.Stop()
			_ = l.writer.close()
			sh.pool.Release(l.port)
		}

		sh.mu.Lock()
		if sh.sessions[callID] == sess {
			delete(sh.sessions, callID)
		}
		sh.mu.Unlock()
	}

	seenLabels := map[string]int{}

	for i, m := range parsed.Media {
		if m.Port == 0 {
			// A port-0 stream is declined by definition: no leg, and
			// the answer carries port 0 for it.
			answerPorts = append(answerPorts, 0)

			continue
		}

		legLabel := resolveLegLabel(rec, m, i)
		seenLabels[legLabel]++
		if n := seenLabels[legLabel]; n > 1 {
			legLabel = fmt.Sprintf("%s-%d", legLabel, n)
		}

		lw := &legWriter{
			call:    callID + " leg " + legLabel,
			onPCM:   sess.touch,
			started: time.Now(),
		}

		port, recv, aerr := rtp.AcquireBound(sh.pool, func(p int) (*rtp.Receiver, error) {
			return rtp.NewReceiverIP(sh.rtpBindIP, p, lw.append)
		})
		if aerr != nil {
			reason := "no rtp ports available"
			if errors.Is(aerr, rtp.ErrNoBindablePort) {
				reason = "rtp open failed"
			}

			log.Printf("[siprecsrv] %s: acquire port %d: %v", callID, i, aerr)
			rollback()
			s.respond(req, tx, sip.StatusInternalServerError, reason, nil, "")

			return
		}

		wavPath, wr, werr := openNewWAV(sh.recordingDir)
		if werr != nil {
			log.Printf("[siprecsrv] %s: open wav %d: %v", callID, i, werr)
			recv.Stop()
			sh.pool.Release(port)
			rollback()
			s.respond(req, tx, sip.StatusInternalServerError, "wav open failed", nil, "")

			return
		}

		lw.w = wr
		recv.Start()

		legs = append(legs, &leg{
			label: legLabel, codec: m.CodecName, port: port,
			wavPath: wavPath, receiver: recv, writer: lw,
		})
		answerPorts = append(answerPorts, port)

		log.Printf("[siprecsrv] %s: leg=%s rtp_port=%d codec=%s wav=%s",
			callID, legLabel, port, m.CodecName, wavPath)
	}

	sh.mu.Lock()
	sess.legs = legs
	answer, err := BuildAnswerSDPWithVersion(sh.localIP, parsed, answerPorts, sess.answerVer+1)
	if err != nil {
		sh.mu.Unlock()
		log.Printf("[siprecsrv] %s: build answer: %v", callID, err)
		rollback()
		s.respond(req, tx, sip.StatusInternalServerError, "sdp build failed", nil, "")
		return
	}
	if sess.cancelled {
		// A BYE (or shutdown) landed while we were building. The
		// recording exists; finalize it like any ended dialog.
		if sh.sessions[callID] == sess {
			delete(sh.sessions, callID)
		}
		sh.mu.Unlock()
		sh.finalizeSession(context.Background(), sess)
		s.respond(req, tx, sip.StatusOK, "OK", nil, "")

		return
	}

	sess.pending = false
	sess.answerVer++
	sh.mu.Unlock()

	s.respondWithTag(req, tx, sip.StatusOK, "OK", []byte(answer), "application/sdp", sess.toTag)
}

func reusableMedia(sess *session, offer *ParsedSDP) bool {
	if sess.offer == nil {
		// Sessions built by embedders predating full offer tracking.
		return offer.AllInactive() || len(offer.Media) == len(sess.legs)
	}
	if len(sess.offer.Media) != len(offer.Media) || sess.offer.ConnectionIP != offer.ConnectionIP {
		return false
	}
	for i, m := range offer.Media {
		prior := sess.offer.Media[i]
		if m.Port != prior.Port || m.CodecName != prior.CodecName ||
			m.Label != prior.Label || m.ConnectionIP != prior.ConnectionIP {
			return false
		}
	}
	return true
}

func (sess *session) answerPorts() []int {
	if sess.offer == nil {
		ports := make([]int, len(sess.legs))
		for i, l := range sess.legs {
			ports[i] = l.port
		}
		return ports
	}
	ports := make([]int, len(sess.offer.Media))
	legIndex := 0
	for i, m := range sess.offer.Media {
		if m.Port != 0 {
			ports[i] = sess.legs[legIndex].port
			legIndex++
		}
	}
	return ports
}

func (sess *session) staleCSeq(req *sip.Request) bool {
	return sess.remoteCSeq != 0 && (req.CSeq() == nil || req.CSeq().SeqNo <= sess.remoteCSeq)
}

// openNewWAV creates a fresh recording file under a server-generated
// random name with O_EXCL: a reused, colliding, or hostile Call-ID
// can never truncate an existing recording. Labels and Call-IDs live
// in the database, not in filenames.
func openNewWAV(dir string) (string, *recording.Writer, error) {
	for range 4 {
		var b [12]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", nil, fmt.Errorf("wav name entropy: %w", err)
		}

		path := filepath.Join(dir, hex.EncodeToString(b[:])+".wav")

		wr, err := recording.OpenExclusive(path, sipRecordingSampleRate, 1)
		if err == nil {
			return path, wr, nil
		}

		if !errors.Is(err, os.ErrExist) {
			return "", nil, err
		}
	}

	return "", nil, fmt.Errorf("wav name allocation failed after retries")
}

// resolveLegLabel picks a display identifier for one recorded stream.
// Preference order:
//
//  1. The participant's userpart from RS-Metadata (e.g. "12165285911") —
//     most informative, lets an operator match a leg to who was talking.
//  2. The SDP a=label tag (e.g. "1" / "2") — present even when metadata is
//     sparse.
//  3. A positional fallback ("leg1" / "leg2") — last resort when neither
//     is available.
//
// Labels are display metadata only (filenames are random), so reuse of
// a label across calls is harmless; within one session onInvite
// de-duplicates them.
func resolveLegLabel(rec *Recording, m MediaDescription, idx int) string {
	if rec != nil && m.Label != "" {
		if name := rec.LabelFor(m.Label); name != "" {
			return name
		}
	}

	if m.Label != "" {
		return "label" + sanitiseLabel(m.Label)
	}

	return fmt.Sprintf("leg%d", idx+1)
}

// extractSDPAndMetadata pulls the SDP body and (when present) the
// rs-metadata XML block out of a SIPREC INVITE. SIPREC INVITEs carry
// Content-Type: multipart/mixed; boundary=... with two parts:
//   - application/sdp (the offer)
//   - application/rs-metadata+xml (recording metadata)
//
// Plain INVITEs (a non-SIPREC test client) skip the multipart parse and
// treat the whole body as SDP. A malformed multipart body is an error,
// never a silently truncated parse.
func extractSDPAndMetadata(req *sip.Request) (sdp string, metadata string, err error) {
	body := req.Body()

	ct := req.GetHeader("Content-Type")
	if ct == nil {
		return string(body), "", nil
	}

	mediaType, params, perr := mime.ParseMediaType(ct.Value())
	if perr != nil {
		// Tolerate header parse failure — assume plain SDP.
		return string(body), "", nil
	}

	if !strings.HasPrefix(mediaType, "multipart/") {
		return string(body), "", nil
	}

	boundary := params["boundary"]
	if boundary == "" {
		return "", "", fmt.Errorf("multipart body missing boundary")
	}

	mr := multipart.NewReader(strings.NewReader(string(body)), boundary)

	for {
		part, perr := mr.NextPart()
		if errors.Is(perr, io.EOF) {
			break
		}

		if perr != nil {
			return "", "", fmt.Errorf("multipart read: %w", perr)
		}

		buf, rerr := io.ReadAll(part)
		if rerr != nil {
			return "", "", fmt.Errorf("multipart part read: %w", rerr)
		}

		switch partType := part.Header.Get("Content-Type"); {
		case strings.HasPrefix(partType, "application/sdp"):
			sdp = string(buf)
		case strings.HasPrefix(partType, "application/rs-metadata"):
			metadata = string(buf)
		}
	}

	if sdp == "" {
		return "", metadata, fmt.Errorf("multipart body has no application/sdp part")
	}

	return sdp, metadata, nil
}

func callIDOf(req *sip.Request) string {
	if h := req.CallID(); h != nil {
		return h.Value()
	}

	return "unknown"
}
