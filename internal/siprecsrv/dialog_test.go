package siprecsrv

import (
	"fmt"
	"net"
	"testing"

	"github.com/emiago/sipgo/sip"
	"github.com/voicetel/siprec-receiver/internal/rtp"
)

func TestDialogRequiresBothTagsAndMonotonicCSeq(t *testing.T) {
	srv := New("127.0.0.1", t.TempDir(), rtp.NewPortPool(49040, 49050))
	t.Cleanup(srv.Shutdown)
	addr := startTestServer(t, srv)
	_, ps, _ := net.SplitHostPort(addr)
	var port int
	_, _ = fmt.Sscanf(ps, "%d", &port)
	c := newHardeningClient(t, port)
	body, ct := inviteBody("123e4567-e89b-12d3-a456-426614174000")
	if res := c.send(sip.INVITE, 10, "dialog", "owner", body, ct); res.StatusCode != 200 {
		t.Fatal(res)
	}
	tag := c.toTags["dialog"]
	for _, tc := range []struct{ name, from, to string }{
		{"missing From-tag", "", tag},
		{"foreign From-tag", "foreign", tag},
		{"missing To-tag", "owner", ""},
		{"foreign To-tag", "owner", "foreign"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c.toTags["dialog"] = tc.to
			if res := c.send(sip.BYE, 11, "dialog", tc.from, nil, ""); res.StatusCode != 481 {
				t.Fatalf("foreign BYE: %d", res.StatusCode)
			}
			if len(srv.SessionsSnapshot()) != 1 {
				t.Fatal("foreign request ended recording")
			}
		})
	}
	c.toTags["dialog"] = tag
	if res := c.send(sip.INVITE, 10, "dialog", "owner", body, ct); res.StatusCode != 500 {
		t.Fatalf("stale re-INVITE: %d", res.StatusCode)
	}
	if res := c.send(sip.INVITE, 11, "dialog", "owner", body, ct); res.StatusCode != 200 {
		t.Fatal(res)
	}
	if c.toTags["dialog"] != tag {
		t.Fatal("re-INVITE changed dialog's To-tag")
	}
	if res := c.send(sip.BYE, 11, "dialog", "owner", nil, ""); res.StatusCode != 500 {
		t.Fatalf("stale BYE: %d", res.StatusCode)
	}
	if res := c.send(sip.BYE, 12, "dialog", "owner", nil, ""); res.StatusCode != 200 {
		t.Fatal(res)
	}
}
