// Package siprecsrv is callBroadcast's production SIPREC Session Recording
// Server (SRS). It accepts SIPREC INVITEs (RFC 7865/7866 — a multipart body
// of SDP + RS-Metadata), opens one RTP receiver per offered audio leg,
// writes each leg to a WAV file via internal/recording, and hands the
// finished recording to a pluggable Finalizer — production wiring uploads
// it to S3 via the same internal/recording.Uploader every other recording
// path uses; nothing here is test-only.
//
// This exists to close a gap in callBroadcast's recording story: calls
// placed by OpenSIPS-registered SIP devices relay straight to the carrier,
// never transiting FreeSWITCH, so they get none of the recording mechanisms
// that depend on a FreeSWITCH hop. OpenSIPS's own siprec module forks such
// a call's media here directly (see docs/research and config/dev/opensips.cfg.m4
// in the callBroadcast-opensips repo).
package siprecsrv

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"mime"
	"mime/multipart"
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
// PCMU/PCMA codecs this SRS answers with — the only codecs it offers in
// BuildAnswerSDP, so every recorded leg is always this rate.
const sipRecordingSampleRate = 8000

// FinalizedLeg describes one completed, closed recording leg — everything
// a Finalizer needs to persist and upload it.
type FinalizedLeg struct {
	CallID      string // SIP Call-ID of the SIPREC (recording) dialog
	AccountSid  string // from the INVITE Request-URI's cb-account param; "" if absent
	CallSid     string // from the INVITE Request-URI's cb-callsid param; "" if absent
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

// Finalizer is notified once per leg when a SIPREC session ends (BYE or
// re-INVITE replacing a prior session). Production wiring creates the
// store.Recording row and enqueues the S3 upload; a nil Finalizer just
// leaves the WAV file on disk (session introspection via SessionsSnapshot
// still works either way).
type Finalizer interface {
	Finalize(ctx context.Context, leg FinalizedLeg) error
}

// Server is callBroadcast's SIPREC SRS. It accepts INVITE with a multipart
// body (SDP + RS-Metadata), opens RTP receivers for the offered streams,
// replies with a recvonly SDP, and writes each stream to a WAV file under
// RecordingDir before handing it to Finalizer.
//
// One Server instance per process. Concurrency: a single per-Call-ID map
// guards session state; sipgo handles the SIP transaction layer threading.
type Server struct {
	// LocalIP is what the SDP answer advertises in c=IN IP4 … The SRC
	// (OpenSIPS/rtpengine) sends RTP to this address. Loopback when
	// co-located on the same node (the production deployment), a real
	// address only if ever run remotely.
	LocalIP string

	// RecordingDir is the on-disk root for WAV files. Each session writes
	// <RecordingDir>/<callID>-<leg>.wav (the leg label comes from a=label:
	// in the offered SDP, resolved against RS-Metadata when present,
	// falling back to a 1-based index).
	RecordingDir string

	// PortPool allocates the UDP port each leg's RTP receiver listens on.
	PortPool *rtp.PortPool

	// Finalizer is called once per leg when its recording completes. May
	// be nil (see Finalizer doc).
	Finalizer Finalizer

	// UA + SIP server are wired in Serve.
	ua  *sipgo.UserAgent
	srv *sipgo.Server

	mu       sync.Mutex
	sessions map[string]*session
}

// session tracks one active SIPREC dialog.
type session struct {
	callID      string
	accountSid  string
	callSid     string
	fromURI     string
	legs        []*leg
	started     time.Time
	metadata    *Recording // parsed RS-Metadata, nil when absent
	metadataXML string
	paused      bool
}

// leg is one recorded RTP stream (one offered m=audio block).
type leg struct {
	label    string
	codec    string
	port     int
	receiver *rtp.Receiver
	writer   *legWriter
}

// legWriter owns one leg's WAV writer plus the first write error it
// produced. The RTP callback runs on the receiver goroutine and the
// writer is single-goroutine by contract, so appends stay on that
// goroutine; the error slot is atomic because finalizeSession reads
// it from another goroutine. Capturing the error matters: a full disk
// otherwise truncates a recording silently — every Append fails, the
// frames are dropped, and the finalized call looks healthy.
type legWriter struct {
	w    *recording.Writer
	call string       // callID + label, for the one-time log line
	err  atomic.Value // error, first one wins
	once sync.Once
}

func (lw *legWriter) append(pcm []int16) {
	if lw.w == nil {
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

// New builds a Server. Caller wires LocalIP, RecordingDir, PortPool (and
// optionally Finalizer) before calling Serve.
func New(localIP, recordingDir string, portPool *rtp.PortPool) *Server {
	return &Server{
		LocalIP:      localIP,
		RecordingDir: recordingDir,
		PortPool:     portPool,
		sessions:     make(map[string]*session),
	}
}

// SessionInfo describes one active SIPREC session for read-only
// introspection (test assertions, debug endpoints).
type SessionInfo struct {
	CallID     string
	AccountSid string
	Started    time.Time
	WAVFiles   []string
	Recording  *Recording // parsed RS-Metadata, nil when none
}

// SessionsSnapshot returns a copy of every active SIPREC session's
// metadata.
func (s *Server) SessionsSnapshot() []SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]SessionInfo, 0, len(s.sessions))

	for _, sess := range s.sessions {
		paths := make([]string, 0, len(sess.legs))
		for _, l := range sess.legs {
			paths = append(paths, filepath.Join(s.RecordingDir, l.label))
		}

		out = append(out, SessionInfo{
			CallID:     sess.callID,
			AccountSid: sess.accountSid,
			Started:    sess.started,
			WAVFiles:   paths,
			Recording:  sess.metadata,
		})
	}

	return out
}

// Serve binds a SIP listener on addr (e.g. "127.0.0.1:5070") and dispatches
// INVITE / ACK / BYE to handlers. Blocks until ctx is cancelled.
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

	s.ua = ua
	s.srv = srv

	// OnInvite handles both initial INVITE and re-INVITE; sipgo dispatches
	// by Call-ID/CSeq the same way for both, so a single handler with a
	// "have we seen this Call-ID?" check inside is the correct shape.
	srv.OnInvite(s.onInvite)
	srv.OnAck(s.onAck)
	srv.OnBye(s.onBye)

	log.Printf("[siprecsrv] listening on %s/%s, advertising RTP at %s",
		network, addr, s.LocalIP)

	return srv.ListenAndServe(ctx, network, addr)
}

// ServeTLS is Serve over a TLS listener (SIPS, RFC 7866 §11.3) for SRCs
// configured with transport=tls.
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

	srv.OnInvite(s.onInvite)
	srv.OnAck(s.onAck)
	srv.OnBye(s.onBye)

	log.Printf("[siprecsrv] listening on tls/%s, advertising RTP at %s", addr, s.LocalIP)

	return srv.ListenAndServeTLS(ctx, "tcp", addr, conf)
}

func (s *Server) onInvite(req *sip.Request, tx sip.ServerTransaction) {
	callID := callIDOf(req)

	// The account SID / call SID are embedded as URI parameters on the
	// Request-URI by whoever chose this SRS (e.g. a per-call ad-hoc
	// `siprec <handle> sip:srs@host:5070;cb-callsid=CA…` from mod_siprec,
	// or an OpenSIPS recording-config lookup's ;cb-account=…). This is
	// how a receiver serving many tenants knows which account a session
	// belongs to — and how an AMD notification keyed by CallSid can be
	// correlated with the recording — with no new SIP header needed.
	accountSid, _ := req.Recipient.UriParams.Get("cb-account")

	callSid, _ := req.Recipient.UriParams.Get("cb-callsid")
	if callSid == "" {
		callSid, _ = req.Recipient.UriParams.Get("cb-call-sid")
	}

	fromURI := ""
	if f := req.From(); f != nil {
		fromURI = f.Address.String()
	}

	sdp, metadata, err := extractSDPAndMetadata(req)
	if err != nil {
		log.Printf("[siprecsrv] %s: parse body: %v", callID, err)
		_ = tx.Respond(sip.NewResponseFromRequest(req,
			sip.StatusBadRequest, "bad SIPREC body", nil))

		return
	}

	parsed, err := ParseSDP(sdp)
	if err != nil {
		log.Printf("[siprecsrv] %s: parse sdp: %v", callID, err)
		_ = tx.Respond(sip.NewResponseFromRequest(req,
			sip.StatusBadRequest, "bad SDP", nil))

		return
	}

	rec, err := ParseMetadata(metadata)
	if err != nil {
		// Metadata parse failure is non-fatal: we record by SDP label
		// position, lose the participant naming.
		log.Printf("[siprecsrv] %s: parse metadata: %v (continuing with positional labels)",
			callID, err)

		rec = &Recording{}
	}

	// Re-INVITE on a live dialog. mod_siprec pause/resume (RFC 7866
	// §6.4) re-INVITEs with SDP only: a=inactive pauses, a=sendonly
	// resumes. Neither may split the recording — keep the same ports,
	// receivers and WAV writers, and just re-answer. Only a re-INVITE
	// that changes the offered media shape falls through to the
	// teardown-and-rebuild path below.
	s.mu.Lock()
	prior, isReinvite := s.sessions[callID]
	s.mu.Unlock()

	if isReinvite && (parsed.AllInactive() || len(parsed.Media) == len(prior.legs)) {
		ports := make([]int, 0, len(prior.legs))
		for _, l := range prior.legs {
			ports = append(ports, l.port)
		}

		answer, aerr := BuildAnswerSDP(s.LocalIP, parsed, ports)
		if aerr != nil {
			log.Printf("[siprecsrv] %s: re-INVITE build answer: %v", callID, aerr)
			_ = tx.Respond(sip.NewResponseFromRequest(req,
				sip.StatusInternalServerError, "sdp build failed", nil))

			return
		}

		s.mu.Lock()
		prior.paused = parsed.AllInactive()
		if metadata != "" {
			prior.metadata = rec
			prior.metadataXML = metadata
		}
		s.mu.Unlock()

		log.Printf("[siprecsrv] re-INVITE call-id=%s paused=%v — recording continues on %d leg(s)",
			callID, prior.paused, len(prior.legs))

		body, contentType := buildSIPRECResponseBody(answer, rec, "active")
		resp := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", body)
		resp.AppendHeader(sip.NewHeader("Content-Type", contentType))
		_ = tx.Respond(resp)

		return
	}

	if isReinvite {
		log.Printf("[siprecsrv] re-INVITE call-id=%s — media shape changed, replacing prior session", callID)

		s.mu.Lock()
		delete(s.sessions, callID)
		s.mu.Unlock()

		s.finalizeSession(context.Background(), prior)
	} else {
		log.Printf("[siprecsrv] INVITE call-id=%s from=%s", callID, fromURI)
	}

	legs := make([]*leg, 0, len(parsed.Media))
	ports := make([]int, 0, len(parsed.Media))

	rollback := func() {
		for _, l := range legs {
			l.receiver.Stop()
			_ = l.writer.close()
			s.PortPool.Release(l.port)
		}
	}

	for i, m := range parsed.Media {
		legLabel := resolveLegLabel(rec, m, i)

		// Acquire the leg's port and bind its receiver in one step, so a
		// port another process already holds is skipped (#94). The receiver
		// starts only after the WAV writer below is open; the callback
		// captures write failures on the legWriter.
		lw := &legWriter{call: callID + " leg " + legLabel}

		port, recv, err := rtp.AcquireBound(s.PortPool, func(p int) (*rtp.Receiver, error) {
			return rtp.NewReceiver(p, lw.append)
		})
		if err != nil {
			reason := "no rtp ports available"
			if errors.Is(err, rtp.ErrNoBindablePort) {
				reason = "rtp open failed"
			}

			log.Printf("[siprecsrv] %s: acquire port %d: %v", callID, i, err)
			rollback()
			_ = tx.Respond(sip.NewResponseFromRequest(req,
				sip.StatusInternalServerError, reason, nil))

			return
		}

		wavPath := filepath.Join(s.RecordingDir,
			fmt.Sprintf("%s-%s.wav", sanitiseCallID(callID), legLabel))

		lw.w, err = recording.Open(wavPath, sipRecordingSampleRate, 1)
		if err != nil {
			log.Printf("[siprecsrv] %s: open wav %d: %v", callID, i, err)
			recv.Stop()
			s.PortPool.Release(port)
			rollback()
			_ = tx.Respond(sip.NewResponseFromRequest(req,
				sip.StatusInternalServerError, "wav open failed", nil))

			return
		}

		recv.Start()

		l := &leg{label: legLabel, codec: m.CodecName, port: port, receiver: recv, writer: lw}
		legs = append(legs, l)
		ports = append(ports, port)

		log.Printf("[siprecsrv] %s: leg=%s rtp_port=%d codec=%s wav=%s",
			callID, legLabel, port, m.CodecName, wavPath)
	}

	answer, err := BuildAnswerSDP(s.LocalIP, parsed, ports)
	if err != nil {
		log.Printf("[siprecsrv] %s: build answer: %v", callID, err)
		rollback()
		_ = tx.Respond(sip.NewResponseFromRequest(req,
			sip.StatusInternalServerError, "sdp build failed", nil))

		return
	}

	body, contentType := buildSIPRECResponseBody(answer, rec, "active")

	resp := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", body)
	resp.AppendHeader(sip.NewHeader("Content-Type", contentType))

	sess := &session{
		callID:      callID,
		accountSid:  accountSid,
		callSid:     callSid,
		fromURI:     fromURI,
		legs:        legs,
		started:     time.Now(),
		metadata:    rec,
		metadataXML: metadata,
	}
	s.mu.Lock()
	s.sessions[callID] = sess
	s.mu.Unlock()

	if err := tx.Respond(resp); err != nil {
		log.Printf("[siprecsrv] %s: respond 200: %v", callID, err)

		s.mu.Lock()
		if s.sessions[callID] == sess {
			delete(s.sessions, callID)
		}
		s.mu.Unlock()

		rollback()

		return
	}
}

// resolveLegLabel picks a filesystem-safe identifier for one recorded
// stream. Preference order:
//
//  1. The participant's userpart from RS-Metadata (e.g. "12165285911") —
//     most informative, lets an operator match a WAV to who was talking.
//  2. The SDP a=label tag (e.g. "1" / "2") — present even when metadata is
//     sparse.
//  3. A positional fallback ("leg1" / "leg2") — last resort when neither
//     is available.
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

// buildSIPRECResponseBody returns the SRS's 200 OK body. We always answer
// with plain application/sdp, NOT multipart/mixed — see the historical
// note in the callBroadcast-opensips repo's SIPREC research doc: some
// SIP stacks' answer-side parsers don't reliably extract application/sdp
// out of a multipart/mixed 200 OK, which trips media negotiation.
func buildSIPRECResponseBody(sdp string, _ *Recording, _ string) ([]byte, string) {
	return []byte(sdp), "application/sdp"
}

func (s *Server) onAck(req *sip.Request, _ sip.ServerTransaction) {
	log.Printf("[siprecsrv] ACK call-id=%s — recording active", callIDOf(req))
}

func (s *Server) onBye(req *sip.Request, tx sip.ServerTransaction) {
	callID := callIDOf(req)

	s.mu.Lock()
	sess, ok := s.sessions[callID]
	if ok {
		delete(s.sessions, callID)
	}
	s.mu.Unlock()

	if !ok {
		log.Printf("[siprecsrv] BYE for unknown call-id=%s", callID)
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))

		return
	}

	dur := time.Since(sess.started)

	s.finalizeSession(context.Background(), sess)

	log.Printf("[siprecsrv] BYE call-id=%s duration=%s legs=%d",
		callID, dur.Round(time.Millisecond), len(sess.legs))

	_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
}

// finalizeSession stops every leg's receiver, closes its WAV file, releases
// its port, and — if a Finalizer is wired — hands off the finished
// recording (create the store row + enqueue the S3 upload in production;
// see cmd/cb-siprec-srs). Errors are logged, not returned: a finalize
// failure must not prevent the port/socket from being released.
func (s *Server) finalizeSession(ctx context.Context, sess *session) {
	for _, l := range sess.legs {
		l.receiver.Stop()

		if err := l.writer.close(); err != nil {
			log.Printf("[siprecsrv] %s: wav close: %v", sess.callID, err)
		}

		dur := l.writer.durationSec()

		s.PortPool.Release(l.port)

		if s.Finalizer == nil {
			continue
		}

		wavPath := filepath.Join(s.RecordingDir,
			fmt.Sprintf("%s-%s.wav", sanitiseCallID(sess.callID), l.label))

		fsUUID, _ := sess.metadata.FsUUID()

		writeErr := ""
		if werr := l.writer.Err(); werr != nil {
			writeErr = werr.Error()
		}

		if err := s.Finalizer.Finalize(ctx, FinalizedLeg{
			CallID:      sess.callID,
			AccountSid:  sess.accountSid,
			CallSid:     sess.callSid,
			FsUUID:      fsUUID,
			FromURI:     sess.fromURI,
			Label:       l.label,
			Codec:       l.codec,
			WAVPath:     wavPath,
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

// extractSDPAndMetadata pulls the SDP body and (when present) the
// rs-metadata XML block out of a SIPREC INVITE. SIPREC INVITEs carry
// Content-Type: multipart/mixed; boundary=... with two parts:
//   - application/sdp (the offer)
//   - application/rs-metadata+xml (recording metadata)
//
// Plain INVITEs (a non-SIPREC test client) skip the multipart parse and
// treat the whole body as SDP.
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
		if perr != nil {
			break
		}

		partType := part.Header.Get("Content-Type")
		buf := make([]byte, 0, 8192)

		readBuf := make([]byte, 4096)
		for {
			n, rerr := part.Read(readBuf)
			if n > 0 {
				buf = append(buf, readBuf[:n]...)
			}

			if rerr != nil {
				break
			}
		}

		switch {
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

// sanitiseCallID replaces filesystem-hostile characters in the Call-ID so
// it can land on disk verbatim. SIP Call-IDs typically end with @host.tld
// which is fine on Linux but breaks on case-insensitive filesystems if
// nothing normalises.
func sanitiseCallID(callID string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "@", "_at_")

	return r.Replace(callID)
}
