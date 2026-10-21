package siprecsrv

import (
	"strings"
	"testing"
)

// The codec choice must scan every offered payload type, not just
// the first: telephone-event or a dynamic PT may precede the G.711
// codec we can record.
func TestParseSDP_CodecChoiceScansAllPayloadTypes(t *testing.T) {
	cases := map[string]struct {
		sdp  string
		want string
	}{
		"telephone-event first": {
			sdp: "v=0\r\no=- 0 0 IN IP4 10.0.0.1\r\ns=-\r\nc=IN IP4 10.0.0.1\r\nt=0 0\r\n" +
				"m=audio 40000 RTP/AVP 101 0\r\n" +
				"a=rtpmap:101 telephone-event/8000\r\na=rtpmap:0 PCMU/8000\r\n",
			want: "PCMU",
		},
		"dynamic PCMA via rtpmap": {
			sdp: "v=0\r\no=- 0 0 IN IP4 10.0.0.1\r\ns=-\r\nc=IN IP4 10.0.0.1\r\nt=0 0\r\n" +
				"m=audio 40000 RTP/AVP 96\r\n" +
				"a=rtpmap:96 PCMA/8000\r\n",
			want: "PCMA",
		},
		"static PCMA second": {
			sdp: "v=0\r\no=- 0 0 IN IP4 10.0.0.1\r\ns=-\r\nc=IN IP4 10.0.0.1\r\nt=0 0\r\n" +
				"m=audio 40000 RTP/AVP 101 8\r\n" +
				"a=rtpmap:101 telephone-event/8000\r\n",
			want: "PCMA",
		},
		"unsupported only": {
			sdp: "v=0\r\no=- 0 0 IN IP4 10.0.0.1\r\ns=-\r\nc=IN IP4 10.0.0.1\r\nt=0 0\r\n" +
				"m=audio 40000 RTP/AVP 111\r\n" +
				"a=rtpmap:111 opus/48000\r\n",
			want: "",
		},
	}

	for name, tc := range cases {
		parsed, err := ParseSDP(tc.sdp)
		if err != nil {
			t.Fatalf("%s: ParseSDP: %v", name, err)
		}

		if parsed.Media[0].CodecName != tc.want {
			t.Errorf("%s: codec = %q, want %q", name, parsed.Media[0].CodecName, tc.want)
		}

		if tc.want == "" {
			continue
		}

		answer, err := BuildAnswerSDP("127.0.0.1", parsed, []int{50000})
		if err != nil {
			t.Errorf("%s: BuildAnswerSDP: %v", name, err)

			continue
		}

		// Answer always carries the static PT for the codec.
		wantPT := map[string]string{"PCMU": "RTP/AVP 0", "PCMA": "RTP/AVP 8"}[tc.want]
		if !strings.Contains(answer, "m=audio 50000 "+wantPT) {
			t.Errorf("%s: answer missing %q:\n%s", name, wantPT, answer)
		}
	}
}

// A label carrying CR/LF must not inject lines into our SDP answer.
func TestParseSDP_LabelSanitised(t *testing.T) {
	sdp := "v=0\r\no=- 0 0 IN IP4 10.0.0.1\r\ns=-\r\nc=IN IP4 10.0.0.1\r\nt=0 0\r\n" +
		"m=audio 40000 RTP/AVP 0\r\na=label:1\r\na=sendonly\r\n"

	parsed, err := ParseSDP(sdp)
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}

	if parsed.Media[0].Label != "1" {
		t.Errorf("label = %q, want sanitised 1", parsed.Media[0].Label)
	}
}

// Offered port-0 streams are answered port 0, with no recorder behind
// them.
func TestBuildAnswerSDP_PortZeroStream(t *testing.T) {
	sdp := "v=0\r\no=- 0 0 IN IP4 10.0.0.1\r\ns=-\r\nc=IN IP4 10.0.0.1\r\nt=0 0\r\n" +
		"m=audio 40000 RTP/AVP 0\r\na=label:1\r\na=sendonly\r\n" +
		"m=audio 0 RTP/AVP 0\r\na=label:2\r\na=sendonly\r\n"

	parsed, err := ParseSDP(sdp)
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}

	answer, err := BuildAnswerSDP("127.0.0.1", parsed, []int{50000, 0})
	if err != nil {
		t.Fatalf("BuildAnswerSDP: %v", err)
	}

	if !strings.Contains(answer, "m=audio 50000 RTP/AVP 0\r\n") ||
		!strings.Contains(answer, "m=audio 0 RTP/AVP 0\r\n") {
		t.Errorf("answer port handling wrong:\n%s", answer)
	}

	if strings.Count(answer, "a=recvonly") != 1 {
		t.Errorf("declined stream got direction attributes:\n%s", answer)
	}
}

// Re-INVITE answers bump the o= session version (RFC 3264 §8) and an
// inactive offer is answered inactive (RFC 3264 §6.1).
func TestBuildAnswerSDP_VersionAndInactive(t *testing.T) {
	sdp := "v=0\r\no=- 0 0 IN IP4 10.0.0.1\r\ns=-\r\nc=IN IP4 10.0.0.1\r\nt=0 0\r\n" +
		"m=audio 40000 RTP/AVP 0\r\na=label:1\r\na=inactive\r\n"

	parsed, err := ParseSDP(sdp)
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}

	answer, err := BuildAnswerSDPWithVersion("127.0.0.1", parsed, []int{50000}, 3)
	if err != nil {
		t.Fatalf("BuildAnswerSDPWithVersion: %v", err)
	}

	if !strings.Contains(answer, "o=- 0 3 IN IP4") {
		t.Errorf("o= version not bumped:\n%s", answer)
	}

	if !strings.Contains(answer, "a=inactive\r\n") || strings.Contains(answer, "a=recvonly") {
		t.Errorf("inactive offer not answered inactive:\n%s", answer)
	}
}
