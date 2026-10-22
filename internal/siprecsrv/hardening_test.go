package siprecsrv

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"github.com/voicetel/siprec-receiver/internal/rtp"
)

// hardeningClient is a small sipgo UAC for the dialog tests below.
type hardeningClient struct {
	t      *testing.T
	cli    *sipgo.Client
	target sip.Uri
	port   int
	toTags map[string]string
}

func newHardeningClient(t *testing.T, port int) *hardeningClient {
	t.Helper()

	uac, err := sipgo.NewUA(sipgo.WithUserAgent("hardening-test"))
	if err != nil {
		t.Fatalf("NewUA: %v", err)
	}

	t.Cleanup(func() { _ = uac.Close() })

	cli, err := sipgo.NewClient(uac)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	t.Cleanup(func() { _ = cli.Close() })

	target := sip.Uri{User: "srs", Host: "127.0.0.1", Port: port, UriParams: sip.NewParams()}
	target.UriParams.Add("transport", "tcp")

	return &hardeningClient{t: t, cli: cli, target: target, port: port, toTags: make(map[string]string)}
}

func (c *hardeningClient) send(method sip.RequestMethod, cseq int, callID, fromTag string, body []byte, ct string) *sip.Response {
	c.t.Helper()

	req := sip.NewRequest(method, c.target)
	if body != nil {
		req.SetBody(body)
		req.AppendHeader(sip.NewHeader("Content-Type", ct))
	}
	req.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<sip:caller@example.com>;tag=%s", fromTag)))
	req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:srs@127.0.0.1:%d>", c.port)))
	if tag := c.toTags[callID]; tag != "" {
		to := req.To()
		to.Params.Add("tag", tag)
		req.RemoveHeader("To")
		req.AppendHeader(to)
	}
	req.AppendHeader(sip.NewHeader("Call-ID", callID))
	req.AppendHeader(sip.NewHeader("CSeq", fmt.Sprintf("%d %s", cseq, method)))

	tx, err := c.cli.TransactionRequest(context.Background(), req)
	if err != nil {
		c.t.Fatalf("%s TransactionRequest: %v", method, err)
	}
	defer tx.Terminate()

	select {
	case res := <-tx.Responses():
		if method == sip.INVITE && res.StatusCode == 200 {
			c.toTags[callID], _ = res.To().Params.Get("tag")
		}
		return res
	case <-time.After(3 * time.Second):
		c.t.Fatalf("timeout waiting for %s response", method)

		return nil
	}
}

func startTestServer(t *testing.T, srv *Server) (addr string) {
	t.Helper()

	port := freeTCPPort(t)
	addr = fmt.Sprintf("127.0.0.1:%d", port)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = srv.Serve(ctx, "tcp", addr) }()
	waitForTCPListening(t, addr)

	return addr
}

func inviteBody(fsUUID string) ([]byte, string) {
	b := buildModSiprecBody(fsUUID)

	return b.body, b.contentType
}

// TestHardening_ContactAndDialogChecks: the 200 OK must carry our
// Contact (RFC 3261 §12.1.1), and in-dialog requests from a different
// dialog (From-tag) must be refused with 481 without touching the
// recording.
func TestHardening_ContactAndDialogChecks(t *testing.T) {
	dir := t.TempDir()
	finalizer := &fakeFinalizer{}
	srv := New("127.0.0.1", dir, rtp.NewPortPool(44000, 44100))
	srv.Finalizer = finalizer

	addr := startTestServer(t, srv)
	_, portStr, _ := net.SplitHostPort(addr)
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)

	c := newHardeningClient(t, port)
	const callID = "dlg-hardening-1"
	body, ct := inviteBody("123e4567-e89b-12d3-a456-426614174000")

	res := c.send(sip.INVITE, 1, callID, "tag-a", body, ct)
	if res.StatusCode != sip.StatusOK {
		t.Fatalf("INVITE = %d, want 200", res.StatusCode)
	}

	contact := res.GetHeader("Contact")
	if contact == nil || !strings.Contains(contact.Value(), "sip:srs@") || !strings.HasSuffix(contact.Value(), ";+sip.srs") {
		t.Fatalf("200 OK Contact = %v, want our sip:srs@ URI tagged +sip.srs", contact)
	}

	// BYE from a different dialog (wrong From-tag): 481, no finalize.
	res = c.send(sip.BYE, 2, callID, "tag-WRONG", nil, "")
	if res.StatusCode != 481 {
		t.Fatalf("foreign BYE = %d, want 481", res.StatusCode)
	}

	time.Sleep(50 * time.Millisecond)

	if legs := finalizer.snapshot(); len(legs) != 0 {
		t.Fatalf("foreign BYE finalized %d legs", len(legs))
	}

	if got := len(srv.SessionsSnapshot()); got != 1 {
		t.Fatalf("sessions after foreign BYE = %d, want 1", got)
	}

	// The real dialog's BYE still works.
	res = c.send(sip.BYE, 3, callID, "tag-a", nil, "")
	if res.StatusCode != sip.StatusOK {
		t.Fatalf("owner BYE = %d, want 200", res.StatusCode)
	}

	time.Sleep(50 * time.Millisecond)

	if legs := finalizer.snapshot(); len(legs) != 1 {
		t.Fatalf("finalized legs = %d, want 1", len(legs))
	}
}

// TestHardening_Allowlist: a source outside SIPREC_ALLOW_CIDRS gets
// 403 and no session.
func TestHardening_Allowlist(t *testing.T) {
	_, cidr, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}

	srv := New("127.0.0.1", t.TempDir(), rtp.NewPortPool(44200, 44300))
	srv.SetLimits([]*net.IPNet{cidr}, 0, 0)

	addr := startTestServer(t, srv)
	_, portStr, _ := net.SplitHostPort(addr)
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)

	c := newHardeningClient(t, port)
	body, ct := inviteBody("123e4567-e89b-12d3-a456-426614174000")

	res := c.send(sip.INVITE, 1, "dlg-hardening-2", "tag-a", body, ct)
	if res.StatusCode != sip.StatusForbidden {
		t.Fatalf("INVITE from non-allowlisted source = %d, want 403", res.StatusCode)
	}

	if got := len(srv.SessionsSnapshot()); got != 0 {
		t.Fatalf("sessions = %d, want 0", got)
	}
}

// TestHardening_StreamCap: an INVITE offering more streams than the
// cap is refused before allocating anything.
func TestHardening_StreamCap(t *testing.T) {
	srv := New("127.0.0.1", t.TempDir(), rtp.NewPortPool(44400, 44500))
	addr := startTestServer(t, srv)
	_, portStr, _ := net.SplitHostPort(addr)
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)

	var sdp strings.Builder
	sdp.WriteString("v=0\r\no=- 0 0 IN IP4 10.0.0.1\r\ns=-\r\nc=IN IP4 10.0.0.1\r\nt=0 0\r\n")
	for i := 0; i < maxStreamsPerInvite+1; i++ {
		fmt.Fprintf(&sdp, "m=audio %d RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\na=label:%d\r\na=sendonly\r\n", 40000+i*2, i+1)
	}

	c := newHardeningClient(t, port)
	res := c.send(sip.INVITE, 1, "dlg-hardening-3", "tag-a", []byte(sdp.String()), "application/sdp")

	if res.StatusCode != sip.StatusNotAcceptableHere {
		t.Fatalf("over-cap INVITE = %d, want 488", res.StatusCode)
	}
}

// TestHardening_ShutdownFinalizes: stopping the server finalizes
// active sessions instead of stranding open WAVs.
func TestHardening_ShutdownFinalizes(t *testing.T) {
	finalizer := &fakeFinalizer{}
	srv := New("127.0.0.1", t.TempDir(), rtp.NewPortPool(44600, 44700))
	srv.Finalizer = finalizer

	addr := startTestServer(t, srv)
	_, portStr, _ := net.SplitHostPort(addr)
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)

	c := newHardeningClient(t, port)
	body, ct := inviteBody("123e4567-e89b-12d3-a456-426614174000")

	if res := c.send(sip.INVITE, 1, "dlg-hardening-4", "tag-a", body, ct); res.StatusCode != sip.StatusOK {
		t.Fatalf("INVITE = %d", res.StatusCode)
	}

	srv.Shutdown()

	if legs := finalizer.snapshot(); len(legs) != 1 {
		t.Fatalf("finalized after Shutdown = %d legs, want 1", len(legs))
	}
}

// TestHardening_ReaperFinalizesIdle: a session whose BYE never
// arrives is finalized after the idle timeout.
func TestHardening_ReaperFinalizesIdle(t *testing.T) {
	finalizer := &fakeFinalizer{}
	srv := New("127.0.0.1", t.TempDir(), rtp.NewPortPool(44800, 44900))
	srv.Finalizer = finalizer
	srv.SetLimits(nil, 0, 120*time.Millisecond)
	srv.shared.reaperTick = 50 * time.Millisecond

	addr := startTestServer(t, srv)
	_, portStr, _ := net.SplitHostPort(addr)
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)

	c := newHardeningClient(t, port)
	body, ct := inviteBody("123e4567-e89b-12d3-a456-426614174000")

	if res := c.send(sip.INVITE, 1, "dlg-hardening-5", "tag-a", body, ct); res.StatusCode != sip.StatusOK {
		t.Fatalf("INVITE = %d", res.StatusCode)
	}

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if len(finalizer.snapshot()) == 1 {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatal("idle session was not reaped within 4s")
}
