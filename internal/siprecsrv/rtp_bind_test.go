package siprecsrv

import (
	"fmt"
	"net"
	"testing"

	"github.com/emiago/sipgo/sip"
	"github.com/voicetel/siprec-receiver/internal/rtp"
)

func TestAdvertisedAddressNeedNotBeLocal(t *testing.T) {
	srv := New("203.0.113.7", t.TempDir(), rtp.NewPortPool(49140, 49150))
	srv.SetRTPBindIP("127.0.0.1")
	t.Cleanup(srv.Shutdown)
	listener := srv.Clone()
	addr := startTestServer(t, listener)
	_, ps, _ := net.SplitHostPort(addr)
	var port int
	_, _ = fmt.Sscanf(ps, "%d", &port)
	c := newHardeningClient(t, port)
	body, ct := inviteBody("123e4567-e89b-12d3-a456-426614174000")
	res := c.send(sip.INVITE, 1, "nat", "owner", body, ct)
	if res.StatusCode != 200 {
		t.Fatal(res)
	}
	answer, err := ParseSDP(string(res.Body()))
	if err != nil || answer.ConnectionIP != "203.0.113.7" || answer.Media[0].Port == 0 {
		t.Fatalf("NAT SDP: %+v, %v", answer, err)
	}
	srv.shared.mu.Lock()
	leg := srv.shared.sessions["nat"].legs[0]
	srv.shared.mu.Unlock()
	// The advertised endpoint is not assigned to this host; a successful
	// allocation proves the receiver bound the configured local address.
	if leg.receiver.Port() != answer.Media[0].Port {
		t.Fatal("answer and bound port disagree")
	}
}
