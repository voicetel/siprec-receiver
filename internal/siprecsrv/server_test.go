package siprecsrv

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"github.com/voicetel/siprec-receiver/internal/rtp"
)

// fakeFinalizer records every Finalize call for test assertions.
type fakeFinalizer struct {
	mu   sync.Mutex
	legs []FinalizedLeg
	err  error
}

func (f *fakeFinalizer) Finalize(_ context.Context, leg FinalizedLeg) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.legs = append(f.legs, leg)

	return f.err
}

func (f *fakeFinalizer) snapshot() []FinalizedLeg {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]FinalizedLeg, len(f.legs))
	copy(out, f.legs)

	return out
}

// TestServer_SIPRECInvite_ProducesNamedWAVsAndFinalizes end-to-ends a
// SIPREC INVITE through the production server: multipart body with two
// audio streams + RS-Metadata mapping P1->S1 (label 1, +1212...) and
// P2->S2 (label 2, +1310...), an account SID on the Request-URI, then a
// BYE. Expects named WAV files and the Finalizer called once per leg with
// the right account SID.
func TestServer_SIPRECInvite_ProducesNamedWAVsAndFinalizes(t *testing.T) {
	dir := t.TempDir()
	// SIPREC bodies (multipart/mixed: SDP + RS-Metadata XML) routinely
	// exceed UDP MTU; real SBCs use TCP for the SIPREC dialog.
	port := freeTCPPort(t)
	listenAddr := fmt.Sprintf("127.0.0.1:%d", port)

	pool := rtp.NewPortPool(41000, 41100)
	finalizer := &fakeFinalizer{}
	srv := New("127.0.0.1", dir, pool)
	srv.Finalizer = finalizer

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = srv.Serve(ctx, "tcp", listenAddr) }()

	waitForTCPListening(t, listenAddr)

	uac, err := sipgo.NewUA(sipgo.WithUserAgent("test-uac"))
	if err != nil {
		t.Fatalf("NewUA: %v", err)
	}
	defer func() { _ = uac.Close() }()

	cli, err := sipgo.NewClient(uac)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = cli.Close() }()

	body := buildTestSIPRECBody(t)

	const acctSid = "ACtestaccount0000000000000000000"

	target := sip.Uri{
		User:      "srs",
		Host:      "127.0.0.1",
		Port:      port,
		UriParams: sip.NewParams(),
	}
	target.UriParams.Add("transport", "tcp")
	target.UriParams.Add("vm-account", acctSid)

	req := sip.NewRequest(sip.INVITE, target)
	req.SetBody(body.body)
	req.AppendHeader(sip.NewHeader("Content-Type", body.contentType))
	req.AppendHeader(sip.NewHeader("From", "<sip:caller@example.com>;tag=test-tag"))
	req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:srs@127.0.0.1:%d>", port)))
	req.AppendHeader(sip.NewHeader("Call-ID", "test-call-1"))
	req.AppendHeader(sip.NewHeader("CSeq", "1 INVITE"))

	tx, err := cli.TransactionRequest(ctx, req)
	if err != nil {
		t.Fatalf("TransactionRequest: %v", err)
	}
	defer tx.Terminate()

	select {
	case res := <-tx.Responses():
		if res.StatusCode != sip.StatusOK {
			t.Fatalf("status=%d reason=%s body=%s",
				res.StatusCode, res.Reason, string(res.Body()))
		}

		ct := res.GetHeader("Content-Type")
		if ct == nil || ct.Value() != "application/sdp" {
			t.Errorf("expected Content-Type application/sdp, got %v", ct)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for 200 OK")
	}

	// Give the server a beat to write WAV headers.
	time.Sleep(100 * time.Millisecond)

	// Filenames are server-generated now; the session snapshot is
	// the source of truth for which files belong to this call.
	snap := srv.SessionsSnapshot()
	if len(snap) != 1 || len(snap[0].WAVFiles) != 2 {
		t.Fatalf("snapshot = %+v, want 1 session with 2 WAVs", snap)
	}

	for _, path := range snap[0].WAVFiles {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("snapshot WAV %s: %v", path, err)
		}
	}

	if avail := pool.Available(); avail != 51-2 {
		t.Errorf("port pool available = %d, want %d (2 legs acquired)", avail, 51-2)
	}

	// Send BYE and confirm the Finalizer sees both legs with the right
	// account SID.
	byeReq := sip.NewRequest(sip.BYE, target)
	byeReq.AppendHeader(sip.NewHeader("Call-ID", "test-call-1"))
	byeReq.AppendHeader(sip.NewHeader("CSeq", "2 BYE"))
	byeReq.AppendHeader(sip.NewHeader("From", "<sip:caller@example.com>;tag=test-tag"))
	byeReq.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:srs@127.0.0.1:%d>", port)))

	byeTx, err := cli.TransactionRequest(ctx, byeReq)
	if err != nil {
		t.Fatalf("BYE TransactionRequest: %v", err)
	}
	defer byeTx.Terminate()

	select {
	case res := <-byeTx.Responses():
		if res.StatusCode != sip.StatusOK {
			t.Fatalf("BYE status=%d reason=%s", res.StatusCode, res.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for BYE 200 OK")
	}

	time.Sleep(50 * time.Millisecond)

	legs := finalizer.snapshot()
	if len(legs) != 2 {
		t.Fatalf("finalizer saw %d legs, want 2", len(legs))
	}

	for _, l := range legs {
		if l.AccountSid != acctSid {
			t.Errorf("leg %s: AccountSid = %q, want %q", l.Label, l.AccountSid, acctSid)
		}

		if l.CallID != "test-call-1" {
			t.Errorf("leg %s: CallID = %q, want test-call-1", l.Label, l.CallID)
		}
	}

	if avail := pool.Available(); avail != 51 {
		t.Errorf("port pool available after BYE = %d, want all 51 released", avail)
	}
}

// TestServer_PortPoolExhausted_Returns500 confirms a pool-exhaustion
// failure degrades to a clean 500, not a panic or hung transaction, and
// releases any ports it did manage to acquire before the failure.
func TestServer_PortPoolExhausted_Returns500(t *testing.T) {
	dir := t.TempDir()
	port := freeTCPPort(t)
	listenAddr := fmt.Sprintf("127.0.0.1:%d", port)

	// Pool has room for only 1 port; this SIPREC body offers 2 legs.
	pool := rtp.NewPortPool(42000, 42000)
	srv := New("127.0.0.1", dir, pool)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = srv.Serve(ctx, "tcp", listenAddr) }()
	waitForTCPListening(t, listenAddr)

	uac, _ := sipgo.NewUA(sipgo.WithUserAgent("test-uac-exhaust"))
	defer func() { _ = uac.Close() }()
	cli, _ := sipgo.NewClient(uac)
	defer func() { _ = cli.Close() }()

	body := buildTestSIPRECBody(t)

	target := sip.Uri{User: "srs", Host: "127.0.0.1", Port: port, UriParams: sip.NewParams()}
	target.UriParams.Add("transport", "tcp")

	req := sip.NewRequest(sip.INVITE, target)
	req.SetBody(body.body)
	req.AppendHeader(sip.NewHeader("Content-Type", body.contentType))
	req.AppendHeader(sip.NewHeader("From", "<sip:caller@example.com>;tag=exhaust-tag"))
	req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:srs@127.0.0.1:%d>", port)))
	req.AppendHeader(sip.NewHeader("Call-ID", "test-call-exhaust"))
	req.AppendHeader(sip.NewHeader("CSeq", "1 INVITE"))

	tx, err := cli.TransactionRequest(ctx, req)
	if err != nil {
		t.Fatalf("TransactionRequest: %v", err)
	}
	defer tx.Terminate()

	select {
	case res := <-tx.Responses():
		if res.StatusCode != sip.StatusInternalServerError {
			t.Fatalf("status=%d reason=%s, want 500", res.StatusCode, res.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for 500")
	}

	time.Sleep(50 * time.Millisecond)

	if avail := pool.Available(); avail != 1 {
		t.Errorf("port pool available = %d, want 1 (rollback must release the acquired port)", avail)
	}
}

// freeTCPPort grabs a free TCP port. Used for SIPREC transport since
// multipart bodies blow UDP's MTU budget.
func freeTCPPort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen ephemeral tcp: %v", err)
	}

	port := l.Addr().(*net.TCPAddr).Port

	_ = l.Close()

	return port
}

// waitForTCPListening polls Dial("tcp", addr) until it succeeds or the
// deadline fires.
func waitForTCPListening(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()

			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("srv never started TCP listening on %s", addr)
}

type sipBody struct {
	body        []byte
	contentType string
}

func buildTestSIPRECBody(t *testing.T) sipBody {
	t.Helper()

	const boundary = "test-boundary"

	sdp := `v=0
o=FreeSWITCH 1 1 IN IP4 10.0.0.1
s=-
c=IN IP4 10.0.0.1
t=0 0
m=audio 40000 RTP/AVP 0
a=rtpmap:0 PCMU/8000
a=label:1
a=sendonly
m=audio 40002 RTP/AVP 0
a=rtpmap:0 PCMU/8000
a=label:2
a=sendonly
`

	var b strings.Builder
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: application/sdp\r\n\r\n")
	b.WriteString(sdp)
	b.WriteString("\r\n--" + boundary + "\r\n")
	b.WriteString("Content-Type: application/rs-metadata+xml\r\n")
	b.WriteString("Content-Disposition: recording-session\r\n\r\n")
	b.WriteString(fsMetadataSample)
	b.WriteString("\r\n--" + boundary + "--\r\n")

	return sipBody{
		body:        []byte(b.String()),
		contentType: `multipart/mixed; boundary="` + boundary + `"`,
	}
}
