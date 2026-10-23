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
	"io/fs"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"os"
	"path/filepath"
	"strconv"
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

// maxCallIDLength bounds the SIP Call-ID used as the session-map and
// database key. Real FreeSWITCH/SIPREC Call-IDs are far shorter; the cap
// only rejects abusive or malformed values.
const maxCallIDLength = 512

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

// defaultPortProbe is how long a freshly bound RTP port is watched for
// traffic before it is offered in an answer. Just over one 20 ms
// packetization interval: a stream still aimed at the port shows up in
// that time.
const defaultPortProbe = 25 * time.Millisecond

// errStalePort marks a port that was already receiving a stream before
// any answer named it.
var errStalePort = errors.New("port is receiving another stream")

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
	// NoAudio: the leg never received an RTP packet. Reported only
	// when no leg of the dialog did; otherwise such a leg is dropped.
	NoAudio bool
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
	portProbe  time.Duration // stale-traffic watch on each bound RTP port

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
	client  *sipgo.Client // sends the SRS's own BYEs (see sendBye)
	contact string        // this listener's Contact URI, set by Serve/ServeTLS
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
		portProbe:    defaultPortProbe,
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
	offer       *ParsedSDP  // all offered m-lines, including declined streams
	peer        *dialogPeer // how to BYE the SRC; nil when the INVITE gave no Contact
	lastAnswer  string      // the SDP last answered, re-offered to an SDP-less re-INVITE

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
	srv.OnUpdate(s.onUpdate)
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

	cli, err := sipgo.NewClient(ua)
	if err != nil {
		return fmt.Errorf("siprecsrv: new client: %w", err)
	}

	s.ua, s.srv, s.client = ua, srv, cli
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

	cli, err := sipgo.NewClient(ua)
	if err != nil {
		return fmt.Errorf("siprecsrv: new client: %w", err)
	}

	s.ua, s.srv, s.client = ua, srv, cli
	s.contact = contactFor(s.shared.localIP, "tls", addr)
	s.syncFinalizer()
	s.registerHandlers(srv)
	s.shared.startReaper(ctx)

	log.Printf("[siprecsrv] listening on tls/%s, advertising RTP at %s", addr, s.shared.localIP)

	return srv.ListenAndServeTLS(ctx, "tcp", addr, conf)
}

// contactFor builds this listener's Contact header value. RFC 3261
// §12.1.1 requires a Contact in 2xx responses to INVITE (or strict
// clients cannot address in-dialog requests to us), with a sips: URI
// when the dialog runs over TLS; RFC 7866 §6.2 requires the SRS to mark
// it with the +sip.srs feature tag.
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

	if transport == "tls" {
		return fmt.Sprintf("<sips:srs@%s:%s>;+sip.srs", host, port)
	}

	return fmt.Sprintf("<sip:srs@%s:%s;transport=%s>;+sip.srs", host, port, transport)
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
	byeAll(victims)
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
				log.Printf("[siprecsrv] reaping idle session call-id=%q paused=%v", sess.callID, sess.paused)
				sh.finalizeSession(context.Background(), sess)
			}
			byeAll(victims)
		}
	}
}

func (sh *sharedState) finalizeSession(ctx context.Context, sess *session) {
	// Stop every leg first: whether a silent leg is worth keeping
	// depends on whether any other leg heard audio.
	heard := false

	for _, l := range sess.legs {
		l.receiver.Stop()

		if err := l.writer.close(); err != nil {
			log.Printf("[siprecsrv] %q: wav close: %v", sess.callID, err)
		}

		sh.pool.Release(l.port)

		heard = heard || l.writer.samples > 0
	}

	for _, l := range sess.legs {
		noAudio := l.writer.samples == 0
		if noAudio && heard {
			// A declared stream the SRC never sent, such as the second
			// stream when mod_siprec falls back to one mixed stream for
			// a multichannel call: no row, and no file (orphan recovery
			// would import the empty WAV later).
			log.Printf("[siprecsrv] %q: leg %q received no audio; dropped", sess.callID, l.label)

			if err := os.Remove(l.wavPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
				log.Printf("[siprecsrv] %q: remove empty wav: %v", sess.callID, err)
			}

			continue
		}

		if sh.finalizer == nil {
			continue
		}

		dur := l.writer.durationSec()
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
			NoAudio:     noAudio,
		}); err != nil {
			log.Printf("[siprecsrv] %q: finalize leg %q: %v", sess.callID, l.label, err)
		}
	}
}

// byeTimeout bounds one SRS-initiated BYE transaction.
var byeTimeout = 2 * time.Second

// dialogPeer is what the SRS needs to end a recording dialog itself
// (RFC 3261 §12.2.1.1, §15.1.1): the SRC's remote target (its Contact)
// and route set, the two parties as the INVITE named them, our CSeq
// space, and the client of the listener the INVITE arrived on.
type dialogPeer struct {
	client    *sipgo.Client
	target    sip.Uri
	routes    []sip.Uri
	local     sip.Uri // the INVITE's To: this SRS
	remote    sip.Uri // the INVITE's From: the SRC
	localCSeq uint32
}

// peerFor captures the dialog peer from an INVITE. It returns nil when
// the INVITE carries no Contact (RFC 3261 requires one): a BYE aimed at
// the From URI instead could go anywhere.
func (s *Server) peerFor(req *sip.Request) *dialogPeer {
	contact, from, to := req.Contact(), req.From(), req.To()
	if s.client == nil || contact == nil || from == nil || to == nil {
		return nil
	}

	p := &dialogPeer{client: s.client, target: contact.Address, local: to.Address, remote: from.Address}

	for _, h := range req.GetHeaders("Record-Route") {
		if rr, ok := h.(*sip.RecordRouteHeader); ok {
			p.routes = append(p.routes, rr.Address)
		}
	}

	return p
}

// refreshed applies a re-INVITE's target refresh (RFC 3261 §12.2.2):
// the new Contact and transport win, our CSeq space carries over. A
// re-INVITE without a Contact keeps the dialog's peer as it was.
func (p *dialogPeer) refreshed(next *dialogPeer) *dialogPeer {
	if next == nil {
		return p
	}

	if p != nil {
		next.localCSeq = p.localCSeq
	}

	return next
}

// sendBye ends the dialog from the SRS side. The reaper and Shutdown
// call it after finalizing a recording the SRC never hung up, so the SRC
// stops forking audio at a port that is about to be reused (and, for
// FreeSWITCH, hangs up its recording leg). Best effort and bounded: the
// outcome is logged, never retried.
func (sess *session) sendBye(ctx context.Context) {
	p := sess.peer
	p.localCSeq++

	req := sip.NewRequest(sip.BYE, p.target)

	from := &sip.FromHeader{Address: p.local, Params: sip.NewParams()}
	from.Params.Add("tag", sess.toTag)

	to := &sip.ToHeader{Address: p.remote, Params: sip.NewParams()}
	if sess.fromTag != "" {
		to.Params.Add("tag", sess.fromTag)
	}

	callID := sip.CallIDHeader(sess.callID)

	req.AppendHeader(from)
	req.AppendHeader(to)
	req.AppendHeader(&callID)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: p.localCSeq, MethodName: sip.BYE})

	for _, r := range p.routes {
		req.AppendHeader(&sip.RouteHeader{Address: r})
	}

	ctx, cancel := context.WithTimeout(ctx, byeTimeout)
	defer cancel()

	tx, err := p.client.TransactionRequest(ctx, req)
	if err != nil {
		log.Printf("[siprecsrv] BYE call-id=%q to %s: %v", sess.callID, p.target.String(), err)

		return
	}
	defer tx.Terminate()

	select {
	case res := <-tx.Responses():
		log.Printf("[siprecsrv] BYE call-id=%q to %s: %d %s", sess.callID, p.target.String(), res.StatusCode, res.Reason)
	case <-ctx.Done():
		log.Printf("[siprecsrv] BYE call-id=%q to %s: no answer within %s", sess.callID, p.target.String(), byeTimeout)
	}
}

// byeAll sends the SRS's BYE for every finalized session that can be
// addressed, concurrently, and waits for them all.
func byeAll(sessions []*session) {
	var wg sync.WaitGroup

	for _, sess := range sessions {
		if sess.peer == nil {
			continue
		}

		wg.Add(1)

		go func() {
			defer wg.Done()
			sess.sendBye(context.Background())
		}()
	}

	wg.Wait()
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
	if sess.toTag != "" && toTagOf(req) != sess.toTag {
		return true
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

func toTagOf(req *sip.Request) string {
	if to := req.To(); to != nil {
		tag, _ := to.Params.Get("tag")

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

func (s *Server) respondWithTag(req *sip.Request, tx sip.ServerTransaction, code int, reason string,
	body []byte, contentType, tag string, extra ...sip.Header,
) {
	resp := sip.NewResponseFromRequest(req, code, reason, body)
	for _, h := range extra {
		resp.AppendHeader(h)
	}

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

// refuseMedia answers 488 with a Warning when the offer cannot be
// recorded, and reports whether it did.
func (s *Server) refuseMedia(req *sip.Request, tx sip.ServerTransaction, callID string, offer *ParsedSDP) bool {
	w := offer.Unacceptable()
	if w == nil {
		return false
	}

	log.Printf("[siprecsrv] %q: refusing the offer: %s", callID, w.Text)
	s.respondWithTag(req, tx, sip.StatusNotAcceptableHere, "Not Acceptable Here", nil, "", "",
		sip.NewHeader("Warning", w.Header()))

	return true
}

func (s *Server) onOptions(req *sip.Request, tx sip.ServerTransaction) {
	resp := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	resp.AppendHeader(sip.NewHeader("Allow", allowedMethods))
	resp.AppendHeader(sip.NewHeader("Supported", "timer"))
	_ = tx.Respond(resp)
}

func (s *Server) onAck(req *sip.Request, _ sip.ServerTransaction) {
	callID, ok := requestCallID(req)
	if !ok {
		return
	}

	s.shared.mu.Lock()
	if sess, ok := s.shared.sessions[callID]; ok && !sess.dialogMismatch(req) &&
		(req.CSeq() != nil && req.CSeq().SeqNo == sess.remoteCSeq || sess.remoteCSeq == 0) {
		sess.touch()
	}
	s.shared.mu.Unlock()

	log.Printf("[siprecsrv] ACK call-id=%q — recording active", callID)
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

	callID, valid := requestCallID(req)
	if !valid {
		log.Printf("[siprecsrv] BYE rejected: missing or invalid Call-ID (length %d)", callIDLength(req))
		s.respond(req, tx, sip.StatusBadRequest, "invalid Call-ID", nil, "")

		return
	}

	sh := s.shared
	sh.mu.Lock()
	sess, ok := sh.sessions[callID]

	if ok && sess.dialogMismatch(req) {
		sh.mu.Unlock()
		log.Printf("[siprecsrv] BYE call-id=%q rejected: dialog mismatch", callID)
		s.respond(req, tx, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil, "")

		return
	}
	if ok && sess.staleCSeq(req) {
		sh.mu.Unlock()
		s.respond(req, tx, sip.StatusInternalServerError, "CSeq out of order", nil, "")
		return
	}

	if !ok {
		sh.mu.Unlock()
		// RFC 3261 §15.1.2: a BYE that matches no dialog gets 481.
		log.Printf("[siprecsrv] BYE for unknown call-id=%q", callID)
		s.respond(req, tx, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil, "")

		return
	}

	pending := sess.pending
	if pending {
		sess.cancelled = true
	}
	delete(sh.sessions, callID)
	sh.mu.Unlock()

	// Answer first: finalizing writes WAV headers and database rows,
	// which must not delay the 200 and invite BYE retransmissions.
	s.respond(req, tx, sip.StatusOK, "OK", nil, "")

	if pending {
		// The INVITE is still being built; flag it and let the
		// builder roll back (it may already have finished — the
		// cancelled flag is checked under the same lock).
		return
	}

	dur := time.Since(sess.started)
	sh.finalizeSession(context.Background(), sess)

	log.Printf("[siprecsrv] BYE call-id=%q duration=%s legs=%d",
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

	callID, valid := requestCallID(req)
	if !valid {
		log.Printf("[siprecsrv] INVITE rejected: missing or invalid Call-ID (length %d)", callIDLength(req))
		s.respond(req, tx, sip.StatusBadRequest, "invalid Call-ID", nil, "")

		return
	}

	sh := s.shared

	if srcIP := srcIPOf(req); !sh.allowedSource(srcIP) {
		log.Printf("[siprecsrv] INVITE call-id=%q rejected: source %q not in allowlist", callID, srcIP)
		s.respond(req, tx, sip.StatusForbidden, "Forbidden", nil, "")

		return
	}

	timer, ok := s.checkSessionTimer(req, tx)
	if !ok {
		return
	}

	if len(req.Body()) == 0 && toTagOf(req) != "" {
		s.reofferAnswer(req, tx, callID, timer)

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
		log.Printf("[siprecsrv] %q: parse body: %v", callID, err)
		s.respond(req, tx, sip.StatusBadRequest, "bad SIPREC body", nil, "")

		return
	}

	parsed, err := ParseSDP(sdp)
	if err != nil {
		log.Printf("[siprecsrv] %q: parse sdp: %v", callID, err)
		s.respond(req, tx, sip.StatusBadRequest, "bad SDP", nil, "")

		return
	}

	if len(parsed.Media) > maxStreamsPerInvite {
		log.Printf("[siprecsrv] %q: %d offered streams exceeds cap %d", callID, len(parsed.Media), maxStreamsPerInvite)
		s.respond(req, tx, sip.StatusNotAcceptableHere, "too many streams", nil, "")

		return
	}

	// Refused before anything is allocated. On a re-INVITE the 488
	// leaves the session as it was (RFC 3261 §14.2).
	if s.refuseMedia(req, tx, callID, parsed) {
		return
	}

	rec, metaErr := ParseMetadata(metadata)
	if metaErr != nil {
		// Metadata parse failure is non-fatal: record by SDP label
		// position, lose the participant naming.
		log.Printf("[siprecsrv] %q: parse metadata: %v (continuing with positional labels)",
			callID, metaErr)

		rec = &Recording{}
	}
	if callSid == "" {
		callSid = rec.CallSid()
	}

	sh.mu.Lock()
	existing, isReinvite := sh.sessions[callID]

	// An INVITE with a To-tag is a re-INVITE (RFC 3261 §12.2.2); one for
	// a dialog this SRS does not have (ended, or lost in a restart) gets
	// 481 rather than a new, metadata-less recording of nothing.
	if !isReinvite && toTagOf(req) != "" {
		sh.mu.Unlock()
		log.Printf("[siprecsrv] re-INVITE for unknown call-id=%q", callID)
		s.respond(req, tx, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil, "")

		return
	}

	if isReinvite && existing.pending {
		sh.mu.Unlock()
		s.respond(req, tx, sip.StatusRequestPending, "Request Pending", nil, "")

		return
	}

	if isReinvite && existing.dialogMismatch(req) {
		sh.mu.Unlock()
		log.Printf("[siprecsrv] re-INVITE call-id=%q rejected: dialog mismatch", callID)
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
			log.Printf("[siprecsrv] %q: re-INVITE build answer: %v", callID, aerr)
			s.respond(req, tx, sip.StatusInternalServerError, "sdp build failed", nil, "")

			return
		}

		existing.paused = parsed.AllInactive()
		existing.offer = parsed
		existing.peer = existing.peer.refreshed(s.peerFor(req))
		if seq := req.CSeq(); seq != nil {
			existing.remoteCSeq = seq.SeqNo
		}

		// A re-INVITE whose metadata fails to parse must not replace
		// good metadata with an empty document.
		if metadata != "" && metaErr == nil {
			existing.metadata = rec
			existing.metadataXML = metadata
		}

		existing.lastAnswer = answer
		existing.touch()
		paused, tag := existing.paused, existing.toTag
		sh.mu.Unlock()

		log.Printf("[siprecsrv] re-INVITE call-id=%q paused=%v — recording continues on %d leg(s)",
			callID, paused, len(existing.legs))
		s.respondWithTag(req, tx, sip.StatusOK, "OK", []byte(answer), "application/sdp", tag, timer...)

		return
	}

	if !isReinvite && len(sh.sessions) >= sh.maxSessions {
		sh.mu.Unlock()
		log.Printf("[siprecsrv] INVITE call-id=%q rejected: session cap %d reached", callID, sh.maxSessions)
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
		peer:        s.peerFor(req),
	}
	if seq := req.CSeq(); seq != nil {
		sess.remoteCSeq = seq.SeqNo
	}
	if isReinvite {
		sess.toTag = existing.toTag
		sess.answerVer = existing.answerVer
		sess.peer = existing.peer.refreshed(sess.peer)
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
		log.Printf("[siprecsrv] re-INVITE call-id=%q — media shape changed, replacing prior session", callID)
		sh.finalizeSession(context.Background(), existing)
	} else {
		log.Printf("[siprecsrv] INVITE call-id=%q from=%q", callID, fromURI)
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
			return sh.openReceiver(p, lw.append)
		})
		if aerr != nil {
			reason := "no rtp ports available"
			if errors.Is(aerr, rtp.ErrNoBindablePort) {
				reason = "rtp open failed"
			}

			log.Printf("[siprecsrv] %q: acquire port %d: %v", callID, i, aerr)
			rollback()
			s.respond(req, tx, sip.StatusInternalServerError, reason, nil, "")

			return
		}

		wavPath, wr, werr := openNewWAV(sh.recordingDir)
		if werr != nil {
			log.Printf("[siprecsrv] %q: open wav %d: %v", callID, i, werr)
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

		log.Printf("[siprecsrv] %q: leg=%q rtp_port=%d codec=%s wav=%q",
			callID, legLabel, port, m.CodecName, wavPath)
	}

	sh.mu.Lock()
	sess.legs = legs
	answer, err := BuildAnswerSDPWithVersion(sh.localIP, parsed, answerPorts, sess.answerVer+1)
	if err != nil {
		sh.mu.Unlock()
		log.Printf("[siprecsrv] %q: build answer: %v", callID, err)
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
	sess.lastAnswer = answer
	sh.mu.Unlock()

	s.respondWithTag(req, tx, sip.StatusOK, "OK", []byte(answer), "application/sdp", sess.toTag, timer...)
}

// allowedMethods is what this SRS answers (OPTIONS Allow).
const allowedMethods = "INVITE, ACK, BYE, OPTIONS, UPDATE"

// minSessionExpires is the smallest session interval accepted (RFC
// 4028 §5: 90 s is the floor every implementation must accept).
const minSessionExpires = 90

// checkSessionTimer applies RFC 4028 to an INVITE or UPDATE that asks
// for session timers. It returns the headers a 2xx must carry (the
// interval echoed with this SRS's choice of the SRC as refresher, and
// Require: timer when the SRC supports it), or answers 422 itself and
// reports false when the interval is below minSessionExpires. The SRS
// never refreshes; any refresh from the SRC just counts as activity.
func (s *Server) checkSessionTimer(req *sip.Request, tx sip.ServerTransaction) ([]sip.Header, bool) {
	h := req.GetHeader("Session-Expires")
	if h == nil {
		return nil, true
	}

	value := strings.TrimSpace(h.Value())
	if i := strings.IndexByte(value, ';'); i >= 0 {
		value = value[:i]
	}

	secs, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || secs <= 0 {
		s.respond(req, tx, sip.StatusBadRequest, "bad Session-Expires", nil, "")

		return nil, false
	}

	if secs < minSessionExpires {
		s.respondWithTag(req, tx, 422, "Session Interval Too Small", nil, "", "",
			sip.NewHeader("Min-SE", strconv.Itoa(minSessionExpires)))

		return nil, false
	}

	hdrs := []sip.Header{sip.NewHeader("Session-Expires", strconv.Itoa(secs)+";refresher=uac")}
	if supportsTimer(req) {
		hdrs = append(hdrs, sip.NewHeader("Require", "timer"))
	}

	return hdrs, true
}

// supportsTimer reports whether the request lists the timer extension in
// Supported or Require.
func supportsTimer(req *sip.Request) bool {
	for _, name := range []string{"Supported", "Require"} {
		for _, h := range req.GetHeaders(name) {
			for _, tag := range strings.Split(h.Value(), ",") {
				if strings.EqualFold(strings.TrimSpace(tag), "timer") {
					return true
				}
			}
		}
	}

	return false
}

// inDialog looks up the dialog an in-dialog request names, answering it
// itself (481, or 500 for a CSeq at or below the last one) and
// returning nil when it does not belong. Called with sh.mu held; on nil
// the lock has been released.
func (s *Server) inDialog(req *sip.Request, tx sip.ServerTransaction, callID string) *session {
	sh := s.shared

	sess, ok := sh.sessions[callID]
	if !ok || sess.dialogMismatch(req) {
		sh.mu.Unlock()
		log.Printf("[siprecsrv] %s call-id=%q rejected: no such dialog", req.Method, callID)
		s.respond(req, tx, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil, "")

		return nil
	}

	if sess.staleCSeq(req) {
		sh.mu.Unlock()
		s.respond(req, tx, sip.StatusInternalServerError, "CSeq out of order", nil, "")

		return nil
	}

	if sess.pending {
		sh.mu.Unlock()
		s.respond(req, tx, sip.StatusRequestPending, "Request Pending", nil, "")

		return nil
	}

	if seq := req.CSeq(); seq != nil {
		sess.remoteCSeq = seq.SeqNo
	}

	sess.touch()

	return sess
}

// reofferAnswer answers a re-INVITE without SDP (RFC 3261 §14.2: the
// 2xx then carries the offer). A session refresh by re-INVITE often
// looks like this; the SRS re-offers its current SDP unchanged and
// ignores the answer in the ACK, so the recording carries on.
func (s *Server) reofferAnswer(req *sip.Request, tx sip.ServerTransaction, callID string, timer []sip.Header) {
	sh := s.shared

	sh.mu.Lock()

	sess := s.inDialog(req, tx, callID)
	if sess == nil {
		return
	}

	offer, tag := sess.lastAnswer, sess.toTag
	sh.mu.Unlock()

	s.respondWithTag(req, tx, sip.StatusOK, "OK", []byte(offer), "application/sdp", tag, timer...)
}

// onUpdate handles UPDATE (RFC 3311) within a recording dialog: a
// session-timer refresh with no body, an SDP offer that keeps the
// media shape (pause/resume), and RS-Metadata, which RFC 7866 §9.2 lets
// the SRC send in UPDATE. A media-shape change needs a re-INVITE.
func (s *Server) onUpdate(req *sip.Request, tx sip.ServerTransaction) {
	if !s.shared.beginWork() {
		s.respond(req, tx, sip.StatusServiceUnavailable, "shutting down", nil, "")
		return
	}
	defer s.shared.work.Done()

	callID, valid := requestCallID(req)
	if !valid {
		s.respond(req, tx, sip.StatusBadRequest, "invalid Call-ID", nil, "")

		return
	}

	timer, ok := s.checkSessionTimer(req, tx)
	if !ok {
		return
	}

	sdp, metadata, _, err := splitBody(req)
	if err != nil {
		log.Printf("[siprecsrv] UPDATE %q: parse body: %v", callID, err)
		s.respond(req, tx, sip.StatusBadRequest, "bad body", nil, "")

		return
	}

	var parsed *ParsedSDP
	if sdp != "" {
		if parsed, err = ParseSDP(sdp); err != nil {
			s.respond(req, tx, sip.StatusBadRequest, "bad SDP", nil, "")

			return
		}

		if s.refuseMedia(req, tx, callID, parsed) {
			return
		}
	}

	rec, metaErr := ParseMetadata(metadata)

	sh := s.shared
	sh.mu.Lock()

	sess := s.inDialog(req, tx, callID)
	if sess == nil {
		return
	}

	var answer string

	if parsed != nil {
		if !reusableMedia(sess, parsed) {
			sh.mu.Unlock()
			s.respond(req, tx, sip.StatusNotAcceptableHere, "media change needs a re-INVITE", nil, "")

			return
		}

		var aerr error

		sess.answerVer++
		if answer, aerr = BuildAnswerSDPWithVersion(sh.localIP, parsed, sess.answerPorts(), sess.answerVer); aerr != nil {
			sh.mu.Unlock()
			s.respond(req, tx, sip.StatusInternalServerError, "sdp build failed", nil, "")

			return
		}

		sess.paused, sess.offer, sess.lastAnswer = parsed.AllInactive(), parsed, answer
	}

	if metadata != "" && metaErr == nil {
		sess.metadata, sess.metadataXML = rec, metadata
	}

	tag := sess.toTag
	sh.mu.Unlock()

	if answer == "" {
		s.respondWithTag(req, tx, sip.StatusOK, "OK", nil, "", tag, timer...)

		return
	}

	s.respondWithTag(req, tx, sip.StatusOK, "OK", []byte(answer), "application/sdp", tag, timer...)
}

// openReceiver binds an RTP receiver on port and refuses the port if a
// stream is already arriving on it: an SRC fork for a dialog this
// process ended without the SRC noticing (or lost in a restart) would
// otherwise be locked onto, and that call's audio recorded into this
// one. AcquireBound demotes a refused port to the back of the pool.
func (sh *sharedState) openReceiver(port int, cb rtp.FrameCallback) (*rtp.Receiver, error) {
	recv, err := rtp.NewReceiverIP(sh.rtpBindIP, port, cb)
	if err != nil {
		return nil, err
	}

	if !recv.Quiet(sh.portProbe) {
		recv.Stop()

		return nil, fmt.Errorf("rtp port %d: %w", port, errStalePort)
	}

	return recv, nil
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
	sdp, metadata, multi, err := splitBody(req)
	if err == nil && multi && sdp == "" {
		return "", metadata, fmt.Errorf("multipart body has no application/sdp part")
	}

	return sdp, metadata, err
}

// splitBody returns a request's SDP and RS-Metadata, either of which may
// be absent, and whether the body was multipart: a multipart body by
// part type, a single body by its Content-Type (anything but
// RS-Metadata, including no Content-Type, counts as SDP).
func splitBody(req *sip.Request) (sdp, metadata string, multi bool, err error) {
	body := req.Body()

	ct := req.GetHeader("Content-Type")
	if ct == nil {
		return string(body), "", false, nil
	}

	mediaType, params, perr := mime.ParseMediaType(ct.Value())
	if perr != nil {
		// Tolerate header parse failure — assume plain SDP.
		return string(body), "", false, nil
	}

	if isMetadataType(mediaType) {
		return "", string(body), false, nil
	}

	if !strings.HasPrefix(mediaType, "multipart/") {
		return string(body), "", false, nil
	}

	boundary := params["boundary"]
	if boundary == "" {
		return "", "", true, fmt.Errorf("multipart body missing boundary")
	}

	mr := multipart.NewReader(strings.NewReader(string(body)), boundary)

	for {
		part, perr := mr.NextPart()
		if errors.Is(perr, io.EOF) {
			break
		}

		if perr != nil {
			return "", "", true, fmt.Errorf("multipart read: %w", perr)
		}

		buf, rerr := io.ReadAll(part)
		if rerr != nil {
			return "", "", true, fmt.Errorf("multipart part read: %w", rerr)
		}

		// Media types are case-insensitive (RFC 2045 §5.1). A broken
		// parameter still yields the type; a part with no parseable type
		// is neither SDP nor metadata.
		partType, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))

		switch {
		case partType == "application/sdp":
			sdp = string(buf)
		case isMetadataType(partType):
			metadata = string(buf)
		}
	}

	return sdp, metadata, true, nil
}

// isMetadataType reports whether a parsed (lowercased) media type is
// RS-Metadata: RFC 7866's application/rs-metadata+xml, or the bare form
// some SRCs send. application/rs-metadata-request is not metadata.
func isMetadataType(mediaType string) bool {
	return mediaType == "application/rs-metadata+xml" || mediaType == "application/rs-metadata"
}

// callIDOf extracts the trimmed SIP Call-ID, or "" when the header
// is absent. Callers that key sessions or database rows by it must
// use requestCallID so malformed requests cannot collide on a
// sentinel value.
func callIDOf(req *sip.Request) string {
	if h := req.CallID(); h != nil {
		return strings.TrimSpace(h.Value())
	}

	return ""
}

// requestCallID returns the request's Call-ID only when it is safe
// to use as a session/database key: present, at most
// maxCallIDLength bytes, and made solely of printable ASCII other
// than space. This accepts ordinary SIP Call-ID formats (including
// UUIDs and UUID@host forms) without imposing a particular shape.
func requestCallID(req *sip.Request) (string, bool) {
	id := callIDOf(req)

	return id, validCallID(id)
}

func validCallID(id string) bool {
	if id == "" || len(id) > maxCallIDLength {
		return false
	}

	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}

	return true
}

func callIDLength(req *sip.Request) int {
	return len(callIDOf(req))
}
