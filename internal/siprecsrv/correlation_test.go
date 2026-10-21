package siprecsrv

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"testing"

	"github.com/emiago/sipgo/sip"
	"github.com/voicetel/siprec-receiver/internal/rtp"
)

func TestMetadataCallSid(t *testing.T) {
	raw, err := hex.DecodeString("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		rec  *Recording
		want string
	}{
		{"nil", nil, ""},
		{"empty", &Recording{}, ""},
		{"invalid", &Recording{Sessions: []Session{{ID: "invalid"}}}, ""},
		{"channel", &Recording{Sessions: []Session{{ID: base64.StdEncoding.EncodeToString(raw)}}}, "CA0123456789abcdef0123456789abcdef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rec.CallSid(); got != tc.want {
				t.Fatalf("CallSid = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInviteCorrelation(t *testing.T) {
	for _, tc := range []struct {
		name, vmCall, aliasCall, vmAccount, aliasAccount, call, account string
	}{
		{"derived", "", "", "", "ACaccount", "CA123e4567e89b12d3a456426614174000", "ACaccount"},
		{"aliases", "", "CAexplicit", "", "ACalias", "CAexplicit", "ACalias"},
		{"precedence", "CAvm", "CAalias", "ACvm", "ACalias", "CAvm", "ACvm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := New("127.0.0.1", t.TempDir(), rtp.NewPortPool(49000, 49010))
			f := &fakeFinalizer{}
			srv.SetFinalizer(f)
			addr := startTestServer(t, srv)
			_, ps, _ := net.SplitHostPort(addr)
			var port int
			_, _ = fmt.Sscanf(ps, "%d", &port)
			c := newHardeningClient(t, port)
			for key, value := range map[string]string{"vm-callsid": tc.vmCall, "callsid": tc.aliasCall, "vm-account": tc.vmAccount, "accountsid": tc.aliasAccount} {
				if value != "" {
					c.target.UriParams.Add(key, value)
				}
			}
			body, ct := inviteBody("123e4567-e89b-12d3-a456-426614174000")
			if res := c.send(sip.INVITE, 1, "correlation-"+tc.name, "tag", body, ct); res.StatusCode != 200 {
				t.Fatalf("INVITE: %s", res)
			}
			srv.Shutdown()
			legs := f.snapshot()
			if len(legs) != 1 || legs[0].CallSid != tc.call || legs[0].AccountSid != tc.account {
				t.Fatalf("finalized correlation: %+v", legs)
			}
		})
	}
}
