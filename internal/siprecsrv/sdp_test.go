package siprecsrv

import (
	"strings"
	"testing"
)

const sampleSiprecSDP = `v=0
o=FreeSWITCH 1234 1234 IN IP4 192.0.2.10
s=Mod_siprec
c=IN IP4 192.0.2.10
t=0 0
m=audio 40000 RTP/AVP 0 8
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=label:1
a=sendonly
m=audio 40002 RTP/AVP 0 8
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=label:2
a=sendonly
`

func TestParseSDPDualStream(t *testing.T) {
	p, err := ParseSDP(sampleSiprecSDP)
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}

	if p.ConnectionIP != "192.0.2.10" {
		t.Errorf("ConnectionIP=%q want 192.0.2.10", p.ConnectionIP)
	}

	if len(p.Media) != 2 {
		t.Fatalf("Media count=%d want 2", len(p.Media))
	}

	for i, want := range []struct {
		port  int
		pt    int
		codec string
		label string
	}{
		{40000, 0, "PCMU", "1"},
		{40002, 0, "PCMU", "2"},
	} {
		got := p.Media[i]
		if got.Port != want.port {
			t.Errorf("Media[%d].Port=%d want %d", i, got.Port, want.port)
		}

		if got.PayloadType != want.pt {
			t.Errorf("Media[%d].PayloadType=%d want %d", i, got.PayloadType, want.pt)
		}

		if got.CodecName != want.codec {
			t.Errorf("Media[%d].CodecName=%q want %q", i, got.CodecName, want.codec)
		}

		if got.Label != want.label {
			t.Errorf("Media[%d].Label=%q want %q", i, got.Label, want.label)
		}
	}
}

func TestParseSDPRejectsNoAudio(t *testing.T) {
	video := `v=0
o=- 0 0 IN IP4 127.0.0.1
s=video-only
m=video 5000 RTP/AVP 99
`
	if _, err := ParseSDP(video); err == nil {
		t.Error("expected error on SDP with no m=audio")
	}
}

func TestBuildAnswerSDPMirrorsOffer(t *testing.T) {
	offer, err := ParseSDP(sampleSiprecSDP)
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}

	answer, err := BuildAnswerSDP("198.51.100.5", offer, []int{50000, 50002})
	if err != nil {
		t.Fatalf("BuildAnswerSDP: %v", err)
	}

	for _, want := range []string{
		"v=0",
		"c=IN IP4 198.51.100.5",
		"m=audio 50000 RTP/AVP 0",
		"m=audio 50002 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"a=recvonly",
		"a=label:1",
		"a=label:2",
	} {
		if !strings.Contains(answer, want) {
			t.Errorf("answer missing %q\n--- answer ---\n%s", want, answer)
		}
	}
}

func TestBuildAnswerSDPRejectsTooFewPorts(t *testing.T) {
	offer, _ := ParseSDP(sampleSiprecSDP)
	if _, err := BuildAnswerSDP("127.0.0.1", offer, []int{50000}); err == nil {
		t.Error("expected error when ports < media")
	}
}
