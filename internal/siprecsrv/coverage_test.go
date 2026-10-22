package siprecsrv

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"

	"github.com/voicetel/siprec-receiver/internal/recording"
	"github.com/voicetel/siprec-receiver/internal/rtp"
)

// recordingTx is a sip.ServerTransaction that records every response
// so handlers can be driven directly, without a network round trip.
type recordingTx struct {
	mu    sync.Mutex
	resps []*sip.Response
	done  chan struct{}
}

func newRecordingTx() *recordingTx {
	return &recordingTx{done: make(chan struct{})}
}

func (tx *recordingTx) Respond(res *sip.Response) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	tx.resps = append(tx.resps, res)

	return nil
}

func (tx *recordingTx) Acks() <-chan *sip.Request          { return nil }
func (tx *recordingTx) OnCancel(sip.FnTxCancel) bool       { return false }
func (tx *recordingTx) Terminate()                         {}
func (tx *recordingTx) OnTerminate(sip.FnTxTerminate) bool { return false }
func (tx *recordingTx) Done() <-chan struct{}              { return tx.done }
func (tx *recordingTx) Err() error                         { return nil }

func (tx *recordingTx) only(t *testing.T) *sip.Response {
	t.Helper()

	tx.mu.Lock()
	defer tx.mu.Unlock()

	if len(tx.resps) != 1 {
		t.Fatalf("responses = %d, want exactly 1", len(tx.resps))
	}

	return tx.resps[0]
}

// covRequest builds an in-memory SIP request. from is the raw From
// header value ("" omits it), src the transport source ("" leaves it
// unset).
func covRequest(method sip.RequestMethod, callID, from, src string, body []byte, ct string) *sip.Request {
	uri := sip.Uri{User: "srs", Host: "127.0.0.1", Port: 5070, UriParams: sip.NewParams()}
	req := sip.NewRequest(method, uri)

	if body != nil {
		req.SetBody(body)

		if ct != "" {
			req.AppendHeader(sip.NewHeader("Content-Type", ct))
		}
	}

	if from != "" {
		req.AppendHeader(sip.NewHeader("From", from))
	}

	req.AppendHeader(sip.NewHeader("To", "<sip:srs@127.0.0.1:5070>"))

	if callID != "" {
		req.AppendHeader(sip.NewHeader("Call-ID", callID))
	}

	req.AppendHeader(sip.NewHeader("CSeq", "1 "+string(method)))

	if src != "" {
		req.SetSource(src)
	}

	return req
}

const covFrom = "<sip:caller@example.com>;tag=cov-tag"

// covSDP builds an offer with one m=audio block per entry; each entry
// is the block's body after "m=audio ".
func covSDP(blocks ...string) []byte {
	var b strings.Builder

	b.WriteString("v=0\r\no=- 0 0 IN IP4 10.0.0.1\r\ns=-\r\nc=IN IP4 10.0.0.1\r\nt=0 0\r\n")

	for _, blk := range blocks {
		b.WriteString("m=audio " + blk + "\r\n")
	}

	return []byte(b.String())
}

func covMultipart(boundary string, parts ...[2]string) []byte {
	var b strings.Builder

	for _, p := range parts {
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString("Content-Type: " + p[0] + "\r\n\r\n")
		b.WriteString(p[1])
		b.WriteString("\r\n")
	}

	b.WriteString("--" + boundary + "--\r\n")

	return []byte(b.String())
}

func newCovServer(t *testing.T, lo, hi int) (*Server, *rtp.PortPool) {
	t.Helper()

	pool := rtp.NewPortPool(lo, hi)
	srv := New("127.0.0.1", t.TempDir(), pool)
	t.Cleanup(srv.Shutdown)

	return srv, pool
}

func (s *Server) covSession(callID string) *session {
	s.shared.mu.Lock()
	defer s.shared.mu.Unlock()

	return s.shared.sessions[callID]
}

// fifoWriter returns a WAV writer on a FIFO: writes succeed (a reader
// holds the pipe open) but Close fails because a pipe cannot seek.
func fifoWriter(t *testing.T) *recording.Writer {
	t.Helper()

	path := filepath.Join(t.TempDir(), "leg.wav")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	r, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatalf("open fifo reader: %v", err)
	}

	t.Cleanup(func() { _ = r.Close() })

	w, err := recording.Open(path, sipRecordingSampleRate, 1)
	if err != nil {
		t.Fatalf("recording.Open fifo: %v", err)
	}

	return w
}

func acquireLeg(t *testing.T, pool *rtp.PortPool, label string, w *recording.Writer) *leg {
	t.Helper()

	port, recv, err := rtp.AcquireBound(pool, func(p int) (*rtp.Receiver, error) {
		return rtp.NewReceiverIP("127.0.0.1", p, nil)
	})
	if err != nil {
		t.Fatalf("AcquireBound: %v", err)
	}

	return &leg{
		label: label, codec: "PCMU", port: port, wavPath: "fifo",
		receiver: recv, writer: &legWriter{w: w, call: "cov " + label, started: time.Now()},
	}
}

func TestCov_MetadataNilReceiverAndNameFallback(t *testing.T) {
	var nilRec *Recording

	if id, ok := nilRec.FsUUID(); ok || id != "" {
		t.Errorf("nil FsUUID = %q, %v", id, ok)
	}

	if aors := nilRec.ParticipantAORs(); aors != nil {
		t.Errorf("nil ParticipantAORs = %v", aors)
	}

	if l := nilRec.LabelFor("1"); l != "" {
		t.Errorf("nil LabelFor = %q", l)
	}

	rec := &Recording{
		Streams: []Stream{{ID: "S1", Label: "1"}, {ID: "S2", Label: "2"}, {ID: "S3", Label: "3"}},
		PSAssoc: []PSAssoc{
			{ParticipantID: "P1", Send: []string{"S1"}},
			{ParticipantID: "P2", Send: []string{"S2"}},
		},
	}
	rec.Participants = make([]Participant, 2)
	rec.Participants[0].ID = "P1"
	rec.Participants[0].NameID.AOR = "sip:host.example.com"
	rec.Participants[0].Name = "Alice Smith"
	rec.Participants[1].ID = "P2"

	if got := rec.LabelFor("1"); got != "Alice_Smith" {
		t.Errorf("LabelFor(1) = %q, want name fallback Alice_Smith", got)
	}

	if got := rec.LabelFor("2"); got != "" {
		t.Errorf("LabelFor(2) = %q, want \"\" for a participant with no AOR userpart or name", got)
	}

	if got := rec.LabelFor("3"); got != "" {
		t.Errorf("LabelFor(3) = %q, want \"\" for a stream nobody sends on", got)
	}
}

func TestCov_ParseSDPEdgeCases(t *testing.T) {
	sdp := "v=0\r\nx\r\na=sendonly\r\nc=IN IP4 10.0.0.1\r\n" +
		"m=audio 40000 RTP/AVP 0\r\nc=IN IP4 10.0.0.9\r\na=label:1\r\n"

	parsed, err := ParseSDP(sdp)
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}

	if parsed.ConnectionIP != "10.0.0.1" || parsed.Media[0].ConnectionIP != "10.0.0.9" {
		t.Errorf("connection IPs = %q / %q", parsed.ConnectionIP, parsed.Media[0].ConnectionIP)
	}

	if parsed.Media[0].Direction != "" {
		t.Errorf("session-level a=sendonly leaked into media: %q", parsed.Media[0].Direction)
	}

	for name, bad := range map[string]string{
		"bad port": "v=0\r\nm=audio abc RTP/AVP 0\r\n",
		"bad pt":   "v=0\r\nm=audio 40000 RTP/AVP zz\r\n",
	} {
		if _, err := ParseSDP(bad); err == nil {
			t.Errorf("%s: ParseSDP accepted %q", name, bad)
		}
	}

	var nilSDP *ParsedSDP
	if nilSDP.AllInactive() || (&ParsedSDP{}).AllInactive() {
		t.Error("AllInactive must be false for nil/empty SDP")
	}
}

func TestCov_BuildAnswerUnsupportedCodec(t *testing.T) {
	opus := MediaDescription{Port: 40000, PayloadType: 111, PayloadTypes: []int{111}}
	offer := &ParsedSDP{Media: []MediaDescription{opus}}

	answer, err := BuildAnswerSDP("127.0.0.1", offer, []int{0})
	if err != nil {
		t.Fatalf("declined opus stream: %v", err)
	}

	if !strings.Contains(answer, "m=audio 0 RTP/AVP 111\r\n") {
		t.Errorf("declined unsupported stream not echoed on port 0:\n%s", answer)
	}

	if _, err := BuildAnswerSDP("127.0.0.1", offer, []int{50000}); err == nil {
		t.Error("BuildAnswerSDP accepted an active stream with no supported codec")
	}

	cases := []struct {
		m    MediaDescription
		want int
	}{
		{MediaDescription{PayloadType: 0}, 0},
		{MediaDescription{PayloadType: 8}, 8},
		{MediaDescription{PayloadType: 111}, -1},
	}
	for _, tc := range cases {
		if got := chooseAnswerPayload(tc.m); got != tc.want {
			t.Errorf("chooseAnswerPayload(pt=%d) = %d, want %d", tc.m.PayloadType, got, tc.want)
		}
	}

	if codecNameForPayload(8) != "PCMA" || codecNameForPayload(99) != "PCMU" {
		t.Error("codecNameForPayload mapping wrong")
	}
}

func TestCov_SetLimitsSetFinalizerClone(t *testing.T) {
	srv, pool := newCovServer(t, 47000, 47002)

	srv.SetLimits(nil, 3, 2*time.Second)

	if srv.shared.maxSessions != 3 || srv.shared.idleTimeout != 2*time.Second {
		t.Errorf("limits = %d / %s", srv.shared.maxSessions, srv.shared.idleTimeout)
	}

	f := &fakeFinalizer{}
	srv.SetFinalizer(f)

	if srv.Finalizer != f || srv.shared.finalizer != f {
		t.Error("SetFinalizer did not wire both the field and the shared state")
	}

	c := srv.Clone()
	if c.shared != srv.shared || c.Finalizer != f || c.PortPool != pool ||
		c.LocalIP != "127.0.0.1" || c.RecordingDir != srv.RecordingDir {
		t.Errorf("Clone = %+v, want shared family state", c)
	}
}

func TestCov_LegWriterNilBudgetAndCloseError(t *testing.T) {
	calls := 0
	nilLW := &legWriter{onPCM: func() { calls++ }}
	nilLW.append([]int16{1})

	if calls != 1 || nilLW.durationSec() != 0 || nilLW.close() != nil || nilLW.Err() != nil {
		t.Errorf("nil-writer legWriter: calls=%d err=%v", calls, nilLW.Err())
	}

	w, err := recording.Open(filepath.Join(t.TempDir(), "b.wav"), sipRecordingSampleRate, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	lw := &legWriter{w: w, call: "cov budget", started: time.Now()}
	lw.append(make([]int16, rateAllowanceBase+1000))

	if err := lw.Err(); err == nil || !strings.Contains(err.Error(), "sample budget") {
		t.Fatalf("Err = %v, want sample budget error", err)
	}

	lw.append(make([]int16, 10))

	if !lw.capped || lw.durationSec() != 0 {
		t.Errorf("capped=%v duration=%d, want capped leg with no audio written", lw.capped, lw.durationSec())
	}

	_ = lw.close()

	flw := &legWriter{w: fifoWriter(t), call: "cov fifo"}
	if err := flw.close(); err == nil {
		t.Fatal("close on a FIFO-backed writer returned nil")
	}

	if flw.Err() == nil {
		t.Error("close error not captured by Err")
	}
}

func TestCov_FinalizeSessionErrorsAndNilFinalizer(t *testing.T) {
	pool := rtp.NewPortPool(47010, 47014)
	f := &fakeFinalizer{err: errors.New("db down")}
	sh := &sharedState{pool: pool, finalizer: f, sessions: map[string]*session{}}

	sess := &session{callID: "cov-fin", legs: []*leg{acquireLeg(t, pool, "a", fifoWriter(t))}}
	sh.finalizeSession(context.Background(), sess)

	legs := f.snapshot()
	if len(legs) != 1 || legs[0].WriteErr == "" || legs[0].Label != "a" {
		t.Fatalf("finalized legs = %+v, want one leg carrying the close error", legs)
	}

	sh.finalizer = nil
	sess = &session{callID: "cov-fin-nil", legs: []*leg{acquireLeg(t, pool, "b", nil)}}
	sh.finalizeSession(context.Background(), sess)

	if len(f.snapshot()) != 1 {
		t.Error("nil finalizer path still notified a finalizer")
	}

	if pool.Available() != 3 {
		t.Errorf("pool available = %d, want all 3 released", pool.Available())
	}
}

func TestCov_ContactFor(t *testing.T) {
	cases := []struct{ ip, tp, addr, want string }{
		// RFC 7866 §6.2: the SRS's Contact carries +sip.srs; RFC 3261
		// §12.1.1: a TLS dialog's Contact is a sips: URI.
		{"10.0.0.5", "udp", "no-port", "<sip:srs@10.0.0.5:5060;transport=udp>;+sip.srs"},
		{"", "tcp", "192.0.2.1:5070", "<sip:srs@192.0.2.1:5070;transport=tcp>;+sip.srs"},
		{"0.0.0.0", "tcp", "0.0.0.0:5070", "<sip:srs@127.0.0.1:5070;transport=tcp>;+sip.srs"},
		{"::", "tls", "[::]:5061", "<sips:srs@127.0.0.1:5061>;+sip.srs"},
	}
	for _, tc := range cases {
		if got := contactFor(tc.ip, tc.tp, tc.addr); got != tc.want {
			t.Errorf("contactFor(%q,%q,%q) = %q, want %q", tc.ip, tc.tp, tc.addr, got, tc.want)
		}
	}
}

func TestCov_ShutdownCancelsPending(t *testing.T) {
	srv, _ := newCovServer(t, 47020, 47022)
	sess := &session{callID: "cov-pending", pending: true}
	srv.shared.sessions[sess.callID] = sess

	srv.Shutdown()

	srv.shared.mu.Lock()
	defer srv.shared.mu.Unlock()

	if !sess.cancelled || len(srv.shared.sessions) != 0 {
		t.Errorf("cancelled=%v sessions=%d, want cancelled and removed", sess.cancelled, len(srv.shared.sessions))
	}
}

func TestCov_ReaperDropsStalePending(t *testing.T) {
	srv, _ := newCovServer(t, 47030, 47032)
	sh := srv.shared
	sh.reaperTick = 10 * time.Millisecond

	stale := &session{callID: "cov-stale", pending: true, started: time.Now().Add(-2 * pendingTimeout)}
	stale.touch()

	fresh := &session{callID: "cov-fresh", pending: true, started: time.Now()}
	fresh.touch()

	sh.sessions[stale.callID] = stale
	sh.sessions[fresh.callID] = fresh

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		sh.reaperLoop(ctx)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		sh.mu.Lock()
		_, present := sh.sessions[stale.callID]
		cancelled := stale.cancelled
		_, freshPresent := sh.sessions[fresh.callID]
		sh.mu.Unlock()

		if !present {
			if !cancelled || !freshPresent {
				t.Errorf("stale cancelled=%v fresh kept=%v", cancelled, freshPresent)
			}

			break
		}

		if time.Now().After(deadline) {
			t.Fatal("stale pending session not reaped within 3s")
		}

		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	<-done
}

func TestCov_AllowedSource(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("10.0.0.0/8")
	sh := &sharedState{allowCIDRs: []*net.IPNet{cidr}}

	if !sh.allowedSource("10.1.2.3") {
		t.Error("10.1.2.3 should be allowed")
	}

	if sh.allowedSource("192.168.1.1") || sh.allowedSource("not-an-ip") {
		t.Error("outside/unparseable sources must be refused")
	}
}

func TestCov_DialogHelpers(t *testing.T) {
	sess := &session{fromTag: "cov-tag", srcIP: "10.0.0.1"}

	if !sess.dialogMismatch(covRequest(sip.BYE, "c", covFrom, "10.0.0.2:5060", nil, "")) {
		t.Error("different source IP must mismatch")
	}

	portless := covRequest(sip.BYE, "c", covFrom, "10.0.0.1", nil, "")
	if srcIPOf(portless) != "10.0.0.1" || sess.dialogMismatch(portless) {
		t.Error("a port-less source must compare as a bare IP")
	}

	bare := covRequest(sip.BYE, "", "", "", nil, "")
	if fromTagOf(bare) != "" || callIDOf(bare) != "" {
		t.Errorf("fromTag=%q callID=%q for a header-less request", fromTagOf(bare), callIDOf(bare))
	}
}

func TestCov_OnOptionsAndAck(t *testing.T) {
	srv, _ := newCovServer(t, 47040, 47042)

	tx := newRecordingTx()
	srv.onOptions(covRequest(sip.OPTIONS, "cov-opt", covFrom, "", nil, ""), tx)

	res := tx.only(t)
	if res.StatusCode != sip.StatusOK {
		t.Fatalf("OPTIONS = %d", res.StatusCode)
	}

	if allow := res.GetHeader("Allow"); allow == nil || !strings.Contains(allow.Value(), "INVITE") {
		t.Errorf("Allow = %v", allow)
	}

	sess := &session{callID: "cov-ack"}
	srv.shared.sessions[sess.callID] = sess
	t.Cleanup(func() { delete(srv.shared.sessions, sess.callID) })

	srv.onAck(covRequest(sip.ACK, "cov-ack", covFrom, "", nil, ""), nil)

	if sess.lastActivity.Load() == 0 {
		t.Error("ACK did not touch the session")
	}
}

func TestCov_OnByeBranches(t *testing.T) {
	srv, _ := newCovServer(t, 47050, 47052)

	tx := newRecordingTx()
	srv.onBye(covRequest(sip.BYE, "cov-unknown", covFrom, "", nil, ""), tx)

	// RFC 3261 §15.1.2: a BYE that matches no dialog gets 481.
	if code := tx.only(t).StatusCode; code != sip.StatusCallTransactionDoesNotExists {
		t.Errorf("BYE unknown call = %d, want 481", code)
	}

	pending := &session{callID: "cov-bye-pending", pending: true}
	srv.shared.sessions[pending.callID] = pending

	tx = newRecordingTx()
	srv.onBye(covRequest(sip.BYE, pending.callID, covFrom, "", nil, ""), tx)

	if code := tx.only(t).StatusCode; code != sip.StatusOK {
		t.Errorf("BYE pending = %d, want 200", code)
	}

	if srv.covSession(pending.callID) != nil || !pending.cancelled {
		t.Error("BYE on a pending session must remove it and mark it cancelled")
	}

	foreign := &session{callID: "cov-bye-ip", fromTag: "cov-tag", srcIP: "10.0.0.1"}
	srv.shared.sessions[foreign.callID] = foreign
	t.Cleanup(func() { delete(srv.shared.sessions, foreign.callID) })

	tx = newRecordingTx()
	srv.onBye(covRequest(sip.BYE, foreign.callID, covFrom, "10.9.9.9:5060", nil, ""), tx)

	if code := tx.only(t).StatusCode; code != sip.StatusCallTransactionDoesNotExists {
		t.Errorf("BYE from another IP = %d, want 481", code)
	}

	// A nil transaction panics inside respond; the handler must recover.
	srv.onBye(covRequest(sip.BYE, "cov-panic", covFrom, "", nil, ""), nil)
}

func TestCov_OnInviteEarlyRejections(t *testing.T) {
	srv, _ := newCovServer(t, 47060, 47062)

	invite := func(callID string, body []byte, ct string) *sip.Response {
		t.Helper()

		tx := newRecordingTx()
		srv.onInvite(covRequest(sip.INVITE, callID, covFrom, "127.0.0.1:5099", body, ct), tx)

		return tx.only(t)
	}

	if res := invite("cov-nobnd", []byte("x"), "multipart/mixed"); res.StatusCode != sip.StatusBadRequest ||
		res.Reason != "bad SIPREC body" {
		t.Errorf("multipart w/o boundary = %d %s", res.StatusCode, res.Reason)
	}

	if res := invite("cov-nosdp", []byte("v=0\r\n"), "application/sdp"); res.StatusCode != sip.StatusBadRequest ||
		res.Reason != "bad SDP" {
		t.Errorf("SDP w/o audio = %d %s", res.StatusCode, res.Reason)
	}

	offer := covSDP("40000 RTP/AVP 0\r\na=label:1")

	pending := &session{callID: "cov-491", pending: true}
	srv.shared.sessions[pending.callID] = pending

	if res := invite(pending.callID, offer, "application/sdp"); res.StatusCode != sip.StatusRequestPending {
		t.Errorf("re-INVITE on pending = %d, want 491", res.StatusCode)
	}

	delete(srv.shared.sessions, pending.callID)

	other := &session{callID: "cov-481", fromTag: "someone-else"}
	srv.shared.sessions[other.callID] = other

	if res := invite(other.callID, offer, "application/sdp"); res.StatusCode != sip.StatusCallTransactionDoesNotExists {
		t.Errorf("re-INVITE from another dialog = %d, want 481", res.StatusCode)
	}

	srv.SetLimits(nil, 1, 0)

	if res := invite("cov-503", offer, "application/sdp"); res.StatusCode != sip.StatusServiceUnavailable {
		t.Errorf("INVITE over session cap = %d, want 503", res.StatusCode)
	}

	delete(srv.shared.sessions, other.callID)

	// A nil transaction panics inside respond; the handler must recover.
	_, cidr, _ := net.ParseCIDR("10.0.0.0/8")
	srv.SetLimits([]*net.IPNet{cidr}, 0, 0)
	srv.onInvite(covRequest(sip.INVITE, "cov-panic", covFrom, "", offer, "application/sdp"), nil)
}

func TestCov_ReinviteSameShape(t *testing.T) {
	srv, _ := newCovServer(t, 47070, 47072)

	const callID = "cov-reinvite"
	existing := &session{callID: callID, fromTag: "cov-tag"}
	srv.shared.sessions[callID] = existing
	t.Cleanup(func() { delete(srv.shared.sessions, callID) })

	inactive := covSDP("40000 RTP/AVP 0\r\na=label:1\r\na=inactive")

	tx := newRecordingTx()
	srv.onInvite(covRequest(sip.INVITE, callID, covFrom, "", inactive, "application/sdp"), tx)

	if res := tx.only(t); res.StatusCode != sip.StatusInternalServerError || res.Reason != "sdp build failed" {
		t.Errorf("pause re-INVITE with no legs = %d %s, want 500 sdp build failed", res.StatusCode, res.Reason)
	}

	existing.legs = []*leg{{label: "1", port: 50000}}
	meta := modSiprecMetadataXML("123e4567-e89b-12d3-a456-426614174000")
	body := covMultipart("cov-b", [2]string{"application/sdp", string(inactive)},
		[2]string{"application/rs-metadata+xml", meta})

	tx = newRecordingTx()
	srv.onInvite(covRequest(sip.INVITE, callID, covFrom, "", body, `multipart/mixed; boundary="cov-b"`), tx)

	res := tx.only(t)
	if res.StatusCode != sip.StatusOK || !strings.Contains(string(res.Body()), "m=audio 50000 RTP/AVP 0") ||
		!strings.Contains(string(res.Body()), "o=- 0 2 ") {
		t.Fatalf("pause re-INVITE = %d body:\n%s", res.StatusCode, res.Body())
	}

	srv.shared.mu.Lock()
	defer srv.shared.mu.Unlock()

	if !existing.paused || existing.metadataXML != meta || len(existing.metadata.Participants) != 2 {
		t.Errorf("paused=%v metadata updated=%v", existing.paused, existing.metadataXML == meta)
	}
}

func TestCov_InviteLabelsPortZeroAndBadMetadata(t *testing.T) {
	srv, pool := newCovServer(t, 47080, 47090)
	f := &fakeFinalizer{}
	srv.SetFinalizer(f)

	sdp := covSDP(
		"40000 RTP/AVP 0\r\na=label:1",
		"40002 RTP/AVP 0\r\na=label:1",
		"0 RTP/AVP 0\r\na=label:3",
		"40004 RTP/AVP 8",
	)
	body := covMultipart("cov-b", [2]string{"application/sdp", string(sdp)},
		[2]string{"application/rs-metadata+xml", "<recording><unclosed>"})

	tx := newRecordingTx()
	srv.onInvite(covRequest(sip.INVITE, "cov-labels", covFrom, "", body, `multipart/mixed; boundary="cov-b"`), tx)

	res := tx.only(t)
	if res.StatusCode != sip.StatusOK || !strings.Contains(string(res.Body()), "m=audio 0 RTP/AVP 0\r\n") {
		t.Fatalf("INVITE = %d body:\n%s", res.StatusCode, res.Body())
	}

	sess := srv.covSession("cov-labels")
	if sess == nil {
		t.Fatal("no session after 200")
	}

	var labels []string
	for _, l := range sess.legs {
		labels = append(labels, l.label)
	}

	if strings.Join(labels, ",") != "label1,label1-2,leg4" {
		t.Errorf("labels = %v, want [label1 label1-2 leg4]", labels)
	}

	if sess.metadata == nil || len(sess.metadata.Participants) != 0 {
		t.Errorf("bad metadata should yield an empty Recording, got %+v", sess.metadata)
	}

	if pool.Available() != 6-3 {
		t.Errorf("pool available = %d, want 3", pool.Available())
	}
}

func TestCov_InviteResourceFailures(t *testing.T) {
	offer := covSDP("40000 RTP/AVP 0\r\na=label:1")

	t.Run("no bindable port", func(t *testing.T) {
		const port = 47100

		squat, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		if err != nil {
			t.Logf("port %d already held elsewhere (still unbindable): %v", port, err)
		} else {
			t.Cleanup(func() { _ = squat.Close() })
		}

		srv, pool := newCovServer(t, port, port)

		tx := newRecordingTx()
		srv.onInvite(covRequest(sip.INVITE, "cov-nobind", covFrom, "", offer, "application/sdp"), tx)

		if res := tx.only(t); res.StatusCode != sip.StatusInternalServerError || res.Reason != "rtp open failed" {
			t.Errorf("INVITE = %d %s, want 500 rtp open failed", res.StatusCode, res.Reason)
		}

		if pool.Available() != 1 || srv.covSession("cov-nobind") != nil {
			t.Error("failed INVITE leaked a port or a session")
		}
	})

	t.Run("wav open failure", func(t *testing.T) {
		pool := rtp.NewPortPool(47110, 47112)
		srv := New("127.0.0.1", filepath.Join(t.TempDir(), "missing"), pool)
		t.Cleanup(srv.Shutdown)

		tx := newRecordingTx()
		srv.onInvite(covRequest(sip.INVITE, "cov-nowav", covFrom, "", offer, "application/sdp"), tx)

		if res := tx.only(t); res.StatusCode != sip.StatusInternalServerError || res.Reason != "wav open failed" {
			t.Errorf("INVITE = %d %s, want 500 wav open failed", res.StatusCode, res.Reason)
		}

		if pool.Available() != 2 || srv.covSession("cov-nowav") != nil {
			t.Error("failed INVITE leaked a port or a session")
		}
	})

	t.Run("unsupported codec", func(t *testing.T) {
		srv, pool := newCovServer(t, 47120, 47124)
		opus := covSDP("40000 RTP/AVP 111\r\na=rtpmap:111 opus/48000\r\na=label:1")

		tx := newRecordingTx()
		srv.onInvite(covRequest(sip.INVITE, "cov-opus", covFrom, "", opus, "application/sdp"), tx)

		if res := tx.only(t); res.StatusCode != sip.StatusInternalServerError || res.Reason != "sdp build failed" {
			t.Errorf("INVITE = %d %s, want 500 sdp build failed", res.StatusCode, res.Reason)
		}

		if pool.Available() != 3 || srv.covSession("cov-opus") != nil {
			t.Error("rollback leaked a port or a session")
		}
	})
}

// byeOnFirstFinalize issues a BYE from inside the first Finalize call,
// which lands while a media-shape re-INVITE is still building its
// replacement session.
type byeOnFirstFinalize struct {
	mu      sync.Mutex
	legs    []FinalizedLeg
	once    sync.Once
	trigger func()
}

func (f *byeOnFirstFinalize) Finalize(_ context.Context, leg FinalizedLeg) error {
	f.mu.Lock()
	f.legs = append(f.legs, leg)
	f.mu.Unlock()

	f.once.Do(f.trigger)

	return nil
}

func TestCov_ReinviteShapeChangeCancelledDuringBuild(t *testing.T) {
	srv, pool := newCovServer(t, 47130, 47140)

	const callID = "cov-shape"
	inDialog := func(method sip.RequestMethod, body []byte, ct string) *sip.Request {
		req := covRequest(method, callID, covFrom, "", body, ct)
		if sess := srv.covSession(callID); sess != nil {
			req.To().Params.Add("tag", sess.toTag)
			req.CSeq().SeqNo = sess.remoteCSeq + 1
		}
		return req
	}

	byeTx := newRecordingTx()
	f := &byeOnFirstFinalize{trigger: func() {
		srv.onBye(inDialog(sip.BYE, nil, ""), byeTx)
	}}
	srv.SetFinalizer(f)

	tx := newRecordingTx()
	srv.onInvite(covRequest(sip.INVITE, callID, covFrom, "", covSDP("40000 RTP/AVP 0\r\na=label:1"), "application/sdp"), tx)

	if code := tx.only(t).StatusCode; code != sip.StatusOK {
		t.Fatalf("INVITE = %d", code)
	}

	two := covSDP("40000 RTP/AVP 0\r\na=label:1", "40002 RTP/AVP 0\r\na=label:2")

	tx = newRecordingTx()
	srv.onInvite(inDialog(sip.INVITE, two, "application/sdp"), tx)

	res := tx.only(t)
	if res.StatusCode != sip.StatusOK || len(res.Body()) != 0 {
		t.Errorf("cancelled re-INVITE = %d body=%q, want bodiless 200", res.StatusCode, res.Body())
	}

	if code := byeTx.only(t).StatusCode; code != sip.StatusOK {
		t.Errorf("BYE during build = %d", code)
	}

	f.mu.Lock()
	n := len(f.legs)
	f.mu.Unlock()

	if n != 3 {
		t.Errorf("finalized legs = %d, want 3 (1 replaced + 2 cancelled)", n)
	}

	if srv.covSession(callID) != nil || pool.Available() != 6 {
		t.Errorf("session left=%v pool available=%d, want none and 6", srv.covSession(callID) != nil, pool.Available())
	}
}

func TestCov_ExtractSDPAndMetadata(t *testing.T) {
	sdp := string(covSDP("40000 RTP/AVP 0"))

	plain := covRequest(sip.INVITE, "c", covFrom, "", []byte(sdp), "")
	if got, meta, err := extractSDPAndMetadata(plain); err != nil || got != sdp || meta != "" {
		t.Errorf("no Content-Type: %q %q %v", got, meta, err)
	}

	badCT := covRequest(sip.INVITE, "c", covFrom, "", []byte(sdp), "multipart/mixed; =x")
	if got, _, err := extractSDPAndMetadata(badCT); err != nil || got != sdp {
		t.Errorf("unparseable Content-Type should fall back to plain SDP: %q %v", got, err)
	}

	const ct = `multipart/mixed; boundary="b"`

	errCases := map[string]string{
		"malformed part header": "--b\r\nNoColonHere\r\n\r\nx\r\n--b--\r\n",
		"truncated part":        "--b\r\nContent-Type: application/sdp\r\n\r\nv=0\r\n",
		"no sdp part":           "--b\r\nContent-Type: text/plain\r\n\r\nhi\r\n--b--\r\n",
	}
	for name, body := range errCases {
		req := covRequest(sip.INVITE, "c", covFrom, "", []byte(body), ct)
		if _, _, err := extractSDPAndMetadata(req); err == nil {
			t.Errorf("%s: extractSDPAndMetadata accepted %q", name, body)
		}
	}
}

func selfSignedTLS(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "srs.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(cert)

	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
	}, roots
}

func TestCov_ServeTLS(t *testing.T) {
	srv, _ := newCovServer(t, 47150, 47152)
	f := &fakeFinalizer{}
	srv.Finalizer = f

	conf, roots := selfSignedTLS(t)
	addr := fmt.Sprintf("127.0.0.1:%d", freeTCPPort(t))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)

	go func() { served <- srv.ServeTLS(ctx, addr, conf) }()

	clientConf := &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
	dialer := &net.Dialer{Timeout: 200 * time.Millisecond}

	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, clientConf)
		if err == nil {
			if !conn.ConnectionState().HandshakeComplete {
				t.Error("TLS handshake not complete")
			}

			_ = conn.Close()

			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("TLS listener never accepted a handshake on %s: %v", addr, err)
		}

		time.Sleep(20 * time.Millisecond)
	}

	cancel()

	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("ServeTLS did not return after context cancel")
	}

	if want := contactFor("127.0.0.1", "tls", addr); srv.contact != want {
		t.Errorf("contact = %q, want %q", srv.contact, want)
	}

	if srv.shared.finalizer != f {
		t.Error("ServeTLS did not adopt the Finalizer field")
	}
}
