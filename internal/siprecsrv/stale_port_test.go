package siprecsrv

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"

	"github.com/voicetel/siprec-receiver/internal/rtp"
)

// A port that is already receiving a stream before any answer names it
// (an SRC fork for a dialog the SRS ended or lost) must not be handed to
// a new recording: that call's audio would be locked onto and recorded.
func TestInviteSkipsPortReceivingAStaleStream(t *testing.T) {
	const busy, free = 45400, 45402

	srv := New("127.0.0.1", t.TempDir(), rtp.NewPortPool(busy, free))
	addr := startTestServer(t, srv)
	_, portStr, _ := net.SplitHostPort(addr)
	sipPort, _ := strconv.Atoi(portStr)

	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: busy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	stop := make(chan struct{})
	done := make(chan struct{})
	t.Cleanup(func() { close(stop); <-done })

	go func() {
		defer close(done)

		pkt := make([]byte, 172)
		pkt[0] = 0x80
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()

		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				_, _ = conn.Write(pkt)
			}
		}
	}()

	c := newHardeningClient(t, sipPort)
	body, ct := inviteBody("123e4567-e89b-12d3-a456-426614174000")

	res := c.send(sip.INVITE, 1, "dlg-stale-port", "tag-a", body, ct)
	if res.StatusCode != sip.StatusOK {
		t.Fatalf("INVITE = %d, want 200", res.StatusCode)
	}

	m := regexp.MustCompile(`(?m)^m=audio (\d+) `).FindStringSubmatch(string(res.Body()))
	if m == nil {
		t.Fatalf("no m=audio in answer:\n%s", res.Body())
	}

	if got := m[1]; got != fmt.Sprint(free) {
		t.Fatalf("answered RTP port %s, want %d (port %d is receiving another stream)", got, free, busy)
	}
}

// With no probe the check is skipped and the first port is used.
func TestOpenReceiverWithoutProbe(t *testing.T) {
	srv := New("127.0.0.1", t.TempDir(), rtp.NewPortPool(45410, 45410))
	srv.shared.portProbe = 0

	recv, err := srv.shared.openReceiver(45410, nil)
	if err != nil {
		t.Fatalf("openReceiver: %v", err)
	}

	recv.Stop()

	if _, err := srv.shared.openReceiver(-1, nil); err == nil {
		t.Error("an unbindable port must error")
	}
}
