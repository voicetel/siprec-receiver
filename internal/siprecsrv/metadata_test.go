package siprecsrv

import (
	"strings"
	"testing"
)

const fsMetadataSample = `<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <datamode>complete</datamode>
  <session session_id="1234abcd">
    <start-time>2026-05-05T12:00:00Z</start-time>
  </session>
  <participant participant_id="P1">
    <nameID aor="sip:+12165285911@example.com"/>
  </participant>
  <participant participant_id="P2">
    <nameID aor="sip:+12085179911@example.com"/>
  </participant>
  <stream stream_id="S1" session_id="1234abcd">
    <label>1</label>
  </stream>
  <stream stream_id="S2" session_id="1234abcd">
    <label>2</label>
  </stream>
  <participantstreamassoc participant_id="P1">
    <send>S1</send>
    <recv>S2</recv>
  </participantstreamassoc>
  <participantstreamassoc participant_id="P2">
    <send>S2</send>
    <recv>S1</recv>
  </participantstreamassoc>
</recording>`

func TestParseMetadata_FSSample(t *testing.T) {
	rec, err := ParseMetadata(fsMetadataSample)
	if err != nil {
		t.Fatalf("ParseMetadata: %v", err)
	}

	if got, want := len(rec.Sessions), 1; got != want {
		t.Errorf("sessions: got %d, want %d", got, want)
	}

	if got, want := rec.Sessions[0].ID, "1234abcd"; got != want {
		t.Errorf("session id: got %q, want %q", got, want)
	}

	if got, want := len(rec.Participants), 2; got != want {
		t.Errorf("participants: got %d, want %d", got, want)
	}

	if got, want := rec.Participants[0].NameID.AOR,
		"sip:+12165285911@example.com"; got != want {
		t.Errorf("p1 aor: got %q, want %q", got, want)
	}

	if got, want := len(rec.Streams), 2; got != want {
		t.Errorf("streams: got %d, want %d", got, want)
	}

	if got, want := len(rec.PSAssoc), 2; got != want {
		t.Errorf("psassoc: got %d, want %d", got, want)
	}
}

func TestLabelFor_MapsToParticipantUserpart(t *testing.T) {
	rec, err := ParseMetadata(fsMetadataSample)
	if err != nil {
		t.Fatalf("ParseMetadata: %v", err)
	}

	cases := map[string]string{
		"1": "12165285911", // P1 sends on S1; S1 has label "1"
		"2": "12085179911", // P2 sends on S2; S2 has label "2"
	}

	for label, want := range cases {
		got := rec.LabelFor(label)
		if got != want {
			t.Errorf("LabelFor(%q) = %q, want %q", label, got, want)
		}
	}
}

func TestLabelFor_UnknownLabel(t *testing.T) {
	rec, _ := ParseMetadata(fsMetadataSample)
	if got := rec.LabelFor("99"); got != "" {
		t.Errorf("LabelFor(99) = %q, want empty", got)
	}
}

func TestParseMetadata_Empty(t *testing.T) {
	rec, err := ParseMetadata("")
	if err != nil {
		t.Fatalf("ParseMetadata empty: %v", err)
	}

	if rec == nil {
		t.Fatal("expected non-nil Recording for empty body")
	}

	if got := rec.LabelFor("1"); got != "" {
		t.Errorf("LabelFor on empty: got %q, want empty", got)
	}
}

func TestParseMetadata_Malformed(t *testing.T) {
	_, err := ParseMetadata("<recording><not-closed>")
	if err == nil {
		t.Fatal("expected error on malformed XML")
	}
}

func TestUserpartOf(t *testing.T) {
	cases := map[string]string{
		"sip:+12165285911@example.com":  "12165285911",
		"sip:alice@example.com":         "alice",
		"sips:bob@secure.example.com":   "bob",
		"sip:example.com":               "",
		"":                              "",
		"sip:user-with-dash@host":       "user-with-dash",
		"sip:user_with_underscore@host": "user_with_underscore",
		"sip:has spaces@host":           "has_spaces",
	}

	for in, want := range cases {
		if got := userpartOf(in); got != want {
			t.Errorf("userpartOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitiseLabel_Truncates(t *testing.T) {
	long := strings.Repeat("x", 100)
	got := sanitiseLabel(long)

	if len(got) != 32 {
		t.Errorf("len = %d, want 32", len(got))
	}
}

func TestBuildMetadataResponse_IncludesSessionAndState(t *testing.T) {
	rec, _ := ParseMetadata(fsMetadataSample)
	resp := BuildMetadataResponse(rec, "active")

	for _, want := range []string{
		`<recording xmlns="urn:ietf:params:xml:ns:recording:1">`,
		`<session session_id="1234abcd">`,
		`<recordingstate>active</recordingstate>`,
		`<start-time>2026-05-05T12:00:00Z</start-time>`,
	} {
		if !strings.Contains(resp, want) {
			t.Errorf("response missing %q\nresp:\n%s", want, resp)
		}
	}
}

func TestBuildMetadataResponse_EmptyMetadata_StillValid(t *testing.T) {
	resp := BuildMetadataResponse(&Recording{}, "active")

	if !strings.Contains(resp, `<recording xmlns=`) {
		t.Errorf("response missing root element:\n%s", resp)
	}
}

func BenchmarkParseMetadata(b *testing.B) {
	body := fsMetadataSample

	b.SetBytes(int64(len(body)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, _ = ParseMetadata(body)
	}
}
