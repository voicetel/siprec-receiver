package siprecsrv

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"github.com/voicetel/siprec-receiver/internal/rtp"
)

// byeSRC is a minimal SRC: a UA that listens (so the SRS can address it
// by its Contact), sends INVITEs, and records the BYEs it receives.
type byeSRC struct {
	t      *testing.T
	cli    *sipgo.Client
	port   int
	srs    sip.Uri
	byes   chan *sip.Request
	answer bool
}

func newByeSRC(t *testing.T, srsPort int, answer bool) *byeSRC {
	t.Helper()

	ua, err := sipgo.NewUA(sipgo.WithUserAgent("bye-src"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ua.Close() })

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatal(err)
	}

	cli, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatal(err)
	}

	s := &byeSRC{t: t, cli: cli, port: freeTCPPort(t), byes: make(chan *sip.Request, 4), answer: answer}
	s.srs = sip.Uri{User: "srs", Host: "127.0.0.1", Port: srsPort, UriParams: sip.NewParams()}
	s.srs.UriParams.Add("transport", "tcp")

	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		s.byes <- req
		if s.answer {
			_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	go func() { _ = srv.ListenAndServe(ctx, "tcp", addr) }()
	waitForTCPListening(t, addr)

	return s
}

// invite opens a recording dialog whose Contact is this SRC and returns
// the SRS's To-tag.
func (s *byeSRC) invite(callID, fromTag string) string {
	s.t.Helper()

	body, ct := inviteBody("123e4567-e89b-12d3-a456-426614174000")

	req := sip.NewRequest(sip.INVITE, s.srs)
	req.SetBody(body)
	req.AppendHeader(sip.NewHeader("Content-Type", ct))
	req.AppendHeader(sip.NewHeader("From", "<sip:src@example.com>;tag="+fromTag))
	req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:srs@127.0.0.1:%d>", s.srs.Port)))
	req.AppendHeader(sip.NewHeader("Contact", fmt.Sprintf("<sip:src@127.0.0.1:%d;transport=tcp>;+sip.src", s.port)))
	req.AppendHeader(sip.NewHeader("Call-ID", callID))
	req.AppendHeader(sip.NewHeader("CSeq", "1 INVITE"))

	tx, err := s.cli.TransactionRequest(context.Background(), req)
	if err != nil {
		s.t.Fatalf("INVITE: %v", err)
	}
	defer tx.Terminate()

	select {
	case res := <-tx.Responses():
		if res.StatusCode != sip.StatusOK {
			s.t.Fatalf("INVITE = %d", res.StatusCode)
		}

		tag, _ := res.To().Params.Get("tag")

		return tag
	case <-time.After(3 * time.Second):
		s.t.Fatal("no INVITE response")

		return ""
	}
}

func (s *byeSRC) nextBye() *sip.Request {
	s.t.Helper()

	select {
	case req := <-s.byes:
		return req
	case <-time.After(4 * time.Second):
		s.t.Fatal("the SRS sent no BYE")

		return nil
	}
}

func srsPortOf(t *testing.T, addr string) int {
	t.Helper()

	_, p, _ := net.SplitHostPort(addr)
	n, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}

	return n
}

// checkBye asserts the SRS's BYE belongs to the dialog: its From-tag is
// the SRS's own To-tag, its To-tag the SRC's From-tag.
func checkBye(t *testing.T, bye *sip.Request, callID, fromTag, srsTag string) {
	t.Helper()

	if got := bye.CallID().Value(); got != callID {
		t.Errorf("BYE Call-ID = %q, want %q", got, callID)
	}

	if tag, _ := bye.From().Params.Get("tag"); tag != srsTag {
		t.Errorf("BYE From-tag = %q, want the SRS's To-tag %q", tag, srsTag)
	}

	if tag, _ := bye.To().Params.Get("tag"); tag != fromTag {
		t.Errorf("BYE To-tag = %q, want the SRC's From-tag %q", tag, fromTag)
	}

	if bye.CSeq().MethodName != sip.BYE {
		t.Errorf("BYE CSeq method = %s", bye.CSeq().MethodName)
	}
}

// A recording the SRC never hung up is reaped; the SRS then BYEs the SRC
// so it stops forking audio at a port about to be reused.
func TestReaperByesTheSRC(t *testing.T) {
	finalizer := &fakeFinalizer{}
	srv := New("127.0.0.1", t.TempDir(), rtp.NewPortPool(45500, 45520))
	srv.Finalizer = finalizer
	srv.SetLimits(nil, 0, 120*time.Millisecond)
	srv.shared.reaperTick = 50 * time.Millisecond

	src := newByeSRC(t, srsPortOf(t, startTestServer(t, srv)), true)
	tag := src.invite("dlg-reap-bye", "src-tag")

	checkBye(t, src.nextBye(), "dlg-reap-bye", "src-tag", tag)

	if got := len(finalizer.snapshot()); got != 1 {
		t.Errorf("finalized legs = %d, want 1", got)
	}
}

// Shutdown finalizes and BYEs every live recording.
func TestShutdownByesTheSRC(t *testing.T) {
	srv := New("127.0.0.1", t.TempDir(), rtp.NewPortPool(45530, 45550))
	srv.Finalizer = &fakeFinalizer{}

	src := newByeSRC(t, srsPortOf(t, startTestServer(t, srv)), true)
	tag := src.invite("dlg-shutdown-bye", "src-tag")

	srv.Shutdown()

	checkBye(t, src.nextBye(), "dlg-shutdown-bye", "src-tag", tag)
}

// An SRC that never answers the BYE costs at most byeTimeout.
func TestShutdownByeTimesOut(t *testing.T) {
	prev := byeTimeout
	byeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { byeTimeout = prev })

	srv := New("127.0.0.1", t.TempDir(), rtp.NewPortPool(45560, 45580))

	src := newByeSRC(t, srsPortOf(t, startTestServer(t, srv)), false)
	_ = src.invite("dlg-silent-bye", "src-tag")

	start := time.Now()
	srv.Shutdown()

	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Shutdown took %s with an unanswered BYE", took)
	}

	_ = src.nextBye()
}

// A BYE that cannot be sent at all is logged, not retried.
func TestSendByeUnreachableTarget(t *testing.T) {
	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ua.Close() })

	cli, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatal(err)
	}

	target := sip.Uri{User: "src", Host: "127.0.0.1", Port: freeTCPPort(t), UriParams: sip.NewParams()}
	target.UriParams.Add("transport", "tcp")

	sess := &session{callID: "dlg-unreachable", toTag: "srs", fromTag: "src",
		peer: &dialogPeer{client: cli, target: target, routes: []sip.Uri{target}}}

	done := make(chan struct{})
	go func() { sess.sendBye(context.Background()); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sendBye to a closed port did not return")
	}
}

// refreshed: a re-INVITE's Contact wins, our CSeq space carries over,
// and a re-INVITE with no Contact keeps the peer.
func TestDialogPeerRefreshed(t *testing.T) {
	old := &dialogPeer{target: sip.Uri{Host: "a"}, localCSeq: 7}
	next := &dialogPeer{target: sip.Uri{Host: "b"}}

	if got := old.refreshed(nil); got != old {
		t.Error("a re-INVITE without Contact must keep the peer")
	}

	got := old.refreshed(next)
	if got.target.Host != "b" || got.localCSeq != 7 {
		t.Errorf("refreshed = %+v, want target b and CSeq 7", got)
	}

	var none *dialogPeer
	if got := none.refreshed(next); got != next {
		t.Error("a dialog without a peer adopts the re-INVITE's")
	}

	if (&Server{}).peerFor(sip.NewRequest(sip.INVITE, sip.Uri{Host: "x"})) != nil {
		t.Error("no client, no peer")
	}
}

// peerFor keeps the INVITE's Record-Route set, in order, as the BYE's
// route set.
func TestPeerForKeepsRecordRoute(t *testing.T) {
	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ua.Close() })

	cli, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatal(err)
	}

	raw := "INVITE sip:srs@127.0.0.1:5070 SIP/2.0\r\n" +
		"Via: SIP/2.0/UDP 127.0.0.1:5060;branch=z9hG4bK1\r\n" +
		"Record-Route: <sip:p1.example.com;lr>\r\n" +
		"Record-Route: <sip:p2.example.com;lr>\r\n" +
		"From: <sip:src@example.com>;tag=a\r\n" +
		"To: <sip:srs@127.0.0.1>\r\n" +
		"Contact: <sip:src@127.0.0.1:5060>\r\n" +
		"Call-ID: rr-1\r\n" +
		"CSeq: 1 INVITE\r\n" +
		"Content-Length: 0\r\n\r\n"

	msg, err := sip.ParseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}

	p := (&Server{client: cli}).peerFor(msg.(*sip.Request))
	if p == nil {
		t.Fatal("no peer for an INVITE with a Contact")
	}

	if len(p.routes) != 2 || p.routes[0].Host != "p1.example.com" || p.routes[1].Host != "p2.example.com" {
		t.Errorf("routes = %v, want p1 then p2", p.routes)
	}
}
