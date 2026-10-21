package siprecsrv

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/voicetel/siprec-receiver/internal/rtp"
)

type drainingFinalizer struct {
	entered chan struct{}
	release chan struct{}
}

func (f *drainingFinalizer) Finalize(context.Context, FinalizedLeg) error {
	close(f.entered)
	<-f.release
	return nil
}

func TestShutdownWaitsForRemovedSession(t *testing.T) {
	srv := New("127.0.0.1", t.TempDir(), rtp.NewPortPool(49020, 49030))
	f := &drainingFinalizer{make(chan struct{}), make(chan struct{})}
	srv.SetFinalizer(f)
	addr := startTestServer(t, srv)
	_, ps, _ := net.SplitHostPort(addr)
	var port int
	_, _ = fmt.Sscanf(ps, "%d", &port)
	c := newHardeningClient(t, port)
	body, ct := inviteBody("123e4567-e89b-12d3-a456-426614174000")
	if res := c.send(sip.INVITE, 1, "drain", "tag", body, ct); res.StatusCode != 200 {
		t.Fatal(res)
	}
	if res := c.send(sip.BYE, 2, "drain", "tag", nil, ""); res.StatusCode != 200 {
		t.Fatal(res)
	}
	<-f.entered
	drained := make(chan struct{})
	go func() { srv.Shutdown(); close(drained) }()
	select {
	case <-drained:
		t.Error("shutdown returned before finalization")
	case <-time.After(20 * time.Millisecond):
	}
	close(f.release)
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not drain")
	}
	srv.Shutdown() // idempotent
	if res := c.send(sip.INVITE, 3, "after-drain", "tag", body, ct); res.StatusCode != 503 {
		t.Fatalf("new work after shutdown: %d", res.StatusCode)
	}
	if res := c.send(sip.BYE, 4, "drain", "tag", nil, ""); res.StatusCode != 503 {
		t.Fatalf("BYE after shutdown: %d", res.StatusCode)
	}
}
