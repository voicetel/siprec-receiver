package siprecsrv

import (
	"strings"
	"testing"

	"github.com/emiago/sipgo/sip"
)

func TestUnacceptableOffers(t *testing.T) {
	for _, tc := range []struct {
		name string
		sdp  []byte
		code int // 0 = acceptable
	}{
		{"plain RTP", covSDP("40000 RTP/AVP 0 8"), 0},
		{"RTP/AVPF, any case", covSDP("40000 rtp/avpf 8"), 0},
		{"SRTP", covSDP("40000 RTP/SAVP 0\r\na=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:WVNfX19zZW1jdGwgKCkgewkyMjA7fQp9CnVubGVz"), 302},
		{"DTLS-SRTP", covSDP("40000 UDP/TLS/RTP/SAVPF 0"), 302},
		{"one SRTP leg of two", covSDP("40000 RTP/AVP 0", "40002 RTP/SAVP 0"), 302},
		{"no recordable codec", covSDP("40000 RTP/AVP 18\r\na=rtpmap:18 G729/8000"), 305},
		{"declined SRTP stream", covSDP("40000 RTP/AVP 0", "0 RTP/SAVP 0"), 0},
	} {
		offer, err := ParseSDP(string(tc.sdp))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}

		w := offer.Unacceptable()
		switch {
		case tc.code == 0 && w != nil:
			t.Errorf("%s refused: %+v", tc.name, w)
		case tc.code != 0 && (w == nil || w.Code != tc.code):
			t.Errorf("%s = %+v, want Warning %d", tc.name, w, tc.code)
		}
	}
}

// The answer keeps each stream's offered transport (RFC 3264 §6),
// declined streams included.
func TestAnswerMirrorsTheOfferedTransport(t *testing.T) {
	offer, err := ParseSDP(string(covSDP("40000 RTP/AVPF 0\r\na=label:1", "0 RTP/SAVP 8\r\na=label:2")))
	if err != nil {
		t.Fatal(err)
	}

	answer, err := BuildAnswerSDP("127.0.0.1", offer, []int{50000, 0})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(answer, "m=audio 50000 RTP/AVPF 0\r\n") || !strings.Contains(answer, "m=audio 0 RTP/SAVP 8\r\n") {
		t.Errorf("answer:\n%s", answer)
	}

	unknown, err := ParseSDP(string(covSDP("0 RTP/SAVP 18")))
	if err != nil {
		t.Fatal(err)
	}

	if answer, _ := BuildAnswerSDP("127.0.0.1", unknown, []int{0}); !strings.Contains(answer, "m=audio 0 RTP/SAVP 18\r\n") {
		t.Errorf("declined unknown-codec answer:\n%s", answer)
	}
}

func TestMediaWarningHeader(t *testing.T) {
	w := MediaWarning{Code: 302, Text: `no "SRTP"`}
	if got := w.Header(); got != `302 siprec-receiver "no \"SRTP\""` {
		t.Errorf("Header() = %s", got)
	}
}

// An SRTP offer is refused with 488 and a Warning before any port or
// file is allocated; on a live dialog the refusal leaves the recording
// as it was (RFC 3261 §14.2), whether it came by re-INVITE or UPDATE.
func TestSRTPOfferIsRefused(t *testing.T) {
	srv, pool := newCovServer(t, 47300, 47304)
	srtp := covSDP("40000 RTP/SAVP 0\r\na=label:1\r\na=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:WVNfX19zZW1jdGwgKCkgewkyMjA7fQp9CnVubGVz")

	res := respond(t, srv.onInvite, covRequest(sip.INVITE, "dlg-srtp", covFrom, "", srtp, "application/sdp"))
	if res.StatusCode != sip.StatusNotAcceptableHere || !strings.HasPrefix(header(res, "Warning"), "302 siprec-receiver ") {
		t.Fatalf("SRTP INVITE = %d, Warning %q", res.StatusCode, header(res, "Warning"))
	}

	if pool.Available() != 3 || srv.covSession("dlg-srtp") != nil {
		t.Error("the refused INVITE allocated a port or a session")
	}

	inv := openDialog(t, srv, "dlg-srtp-live")
	tag := toTag(inv)

	res = respond(t, srv.onInvite, inDialogReq(sip.INVITE, "dlg-srtp-live", tag, 2, srtp, "application/sdp"))
	if res.StatusCode != sip.StatusNotAcceptableHere {
		t.Errorf("SRTP re-INVITE = %d", res.StatusCode)
	}

	res = respond(t, srv.onUpdate, inDialogReq(sip.UPDATE, "dlg-srtp-live", tag, 3, srtp, "application/sdp"))
	if res.StatusCode != sip.StatusNotAcceptableHere || !strings.Contains(header(res, "Warning"), "SRTP") {
		t.Errorf("SRTP UPDATE = %d, Warning %q", res.StatusCode, header(res, "Warning"))
	}

	sess := srv.covSession("dlg-srtp-live")
	if sess == nil || len(sess.legs) != 1 || sess.offer.Media[0].Proto != "RTP/AVP" || pool.Available() != 2 {
		t.Errorf("the refused re-offers changed the recording: %+v", sess)
	}
}
