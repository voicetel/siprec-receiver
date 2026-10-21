package siprecsrv

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/emiago/sipgo/sip"
	"github.com/voicetel/siprec-receiver/internal/rtp"
)

func TestReinvitePreservesDeclinedMLineMapping(t *testing.T) {
	srv := New("127.0.0.1", t.TempDir(), rtp.NewPortPool(49120, 49130))
	t.Cleanup(srv.Shutdown)
	f := &fakeFinalizer{}
	srv.SetFinalizer(f)
	addr := startTestServer(t, srv)
	_, ps, _ := net.SplitHostPort(addr)
	var port int
	_, _ = fmt.Sscanf(ps, "%d", &port)
	c := newHardeningClient(t, port)
	offer := func(remote int, direction, codec string) []byte {
		return []byte(fmt.Sprintf("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 0 RTP/AVP 0\r\na=label:1\r\nm=audio %d RTP/AVP %s\r\na=label:2\r\na=%s\r\n", remote, codec, direction))
	}
	send := func(seq, remote int, direction, codec string) *ParsedSDP {
		res := c.send(sip.INVITE, seq, "mapping", "owner", offer(remote, direction, codec), "application/sdp")
		if res.StatusCode != 200 {
			t.Fatal(res)
		}
		if !strings.Contains(string(res.Body()), fmt.Sprintf("o=- 0 %d ", seq)) {
			t.Fatalf("origin version did not advance: %s", res.Body())
		}
		parsed, err := ParseSDP(string(res.Body()))
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	first := send(1, 4002, "sendonly", "0")
	path := srv.SessionsSnapshot()[0].WAVFiles[0]
	for seq, direction := range []string{"sendonly", "inactive", "sendonly"} {
		answer := send(seq+2, 4002, direction, "0")
		if answer.Media[0].Port != 0 || answer.Media[1].Port != first.Media[1].Port ||
			srv.SessionsSnapshot()[0].WAVFiles[0] != path || len(f.snapshot()) != 0 {
			t.Fatal("unchanged media mapping split the recording or changed its ports")
		}
	}
	disabled := send(5, 0, "sendonly", "0")
	if disabled.Media[0].Port != 0 || disabled.Media[1].Port != 0 || len(f.snapshot()) != 1 {
		t.Fatal("disabled stream remained active or its recording was not finalized")
	}
	resumed := send(6, 4002, "sendonly", "8")
	if resumed.Media[0].Port != 0 || resumed.Media[1].Port == 0 || resumed.Media[1].CodecName != "PCMA" {
		t.Fatalf("reactivated media: %+v", resumed)
	}
	if srv.SessionsSnapshot()[0].WAVFiles[0] == path {
		t.Fatal("reactivation reused the finished WAV")
	}
}
