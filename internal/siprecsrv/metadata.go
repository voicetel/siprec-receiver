package siprecsrv

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"strings"
)

// SIPREC RS-Metadata schema per RFC 7865. The XML body in a SIPREC
// INVITE describes the recorded communication session: which
// participants are talking to whom, which media streams correspond
// to whose voice, and grouping info that lets the SRS attribute
// each RTP packet to a participant.
//
// FreeSWITCH's mod_siprec emits a subset of this schema that we
// care about:
//
//	<recording xmlns="urn:ietf:params:xml:ns:recording:1">
//	  <datamode>complete</datamode>
//	  <session session_id="ABCD…">
//	    <start-time>2026-05-05T12:34:56Z</start-time>
//	  </session>
//	  <participant participant_id="P1">
//	    <nameID aor="sip:+12165285911@…"/>
//	  </participant>
//	  <participant participant_id="P2">
//	    <nameID aor="sip:+12085179911@…"/>
//	  </participant>
//	  <stream stream_id="S1" session_id="ABCD…">
//	    <label>1</label>
//	  </stream>
//	  <stream stream_id="S2" session_id="ABCD…">
//	    <label>2</label>
//	  </stream>
//	  <participantsessionassoc participant_id="P1" session_id="ABCD…"/>
//	  <participantstreamassoc participant_id="P1">
//	    <send>S1</send>
//	    <recv>S2</recv>
//	  </participantstreamassoc>
//	  <participantstreamassoc participant_id="P2">
//	    <send>S2</send>
//	    <recv>S1</recv>
//	  </participantstreamassoc>
//	</recording>
//
// We don't enforce schema validity — anything we can't make sense
// of is silently dropped; the SRS still records each m=audio block
// to its own WAV, just with a less-friendly leg label.

// Recording is the top-level <recording> element from RFC 7865.
type Recording struct {
	XMLName      xml.Name      `xml:"recording"`
	DataMode     string        `xml:"datamode"`
	Groups       []Group       `xml:"group"`
	Sessions     []Session     `xml:"session"`
	Participants []Participant `xml:"participant"`
	Streams      []Stream      `xml:"stream"`
	PSessAssoc   []PSessAssoc  `xml:"participantsessionassoc"`
	PSAssoc      []PSAssoc     `xml:"participantstreamassoc"`
}

// Group is the RFC 7865 <group> element grouping sessions that belong
// to one communication session (mod_siprec emits exactly one).
type Group struct {
	ID               string `xml:"group_id,attr"`
	AssociateTime    string `xml:"associate-time"`
	DisassociateTime string `xml:"disassociate-time"`
}

// Session is the recorded communication session (the FS leg the
// SBC is forking from). One per SIPREC INVITE in the FS-emitted
// case.
type Session struct {
	ID           string `xml:"session_id,attr"`
	Reason       string `xml:"reason"`
	GroupRef     string `xml:"group-ref"`
	SipSessionID string `xml:"sipSessionID"`
	StartTime    string `xml:"start-time"`
	StopTime     string `xml:"stop-time"`
}

// PSessAssoc binds a participant to a session (RFC 7865
// <participantsessionassoc>).
type PSessAssoc struct {
	ParticipantID    string `xml:"participant_id,attr"`
	SessionID        string `xml:"session_id,attr"`
	AssociateTime    string `xml:"associate-time"`
	DisassociateTime string `xml:"disassociate-time"`
}

// Participant identifies one talker on the recorded call. SIPREC
// metadata typically has two: the caller (A-leg) and the callee
// (B-leg). The aor attribute is the canonical identifier we expose
// in the WAV file name.
type Participant struct {
	ID     string `xml:"participant_id,attr"`
	NameID struct {
		AOR string `xml:"aor,attr"`
	} `xml:"nameID"`
	Name string `xml:"name"`
}

// Stream is one m=audio block in the SDP, identified by stream_id
// and tagged with the same label= integer that SDP carries via
// a=label. The stream-to-participant link comes from PSAssoc.
type Stream struct {
	ID        string `xml:"stream_id,attr"`
	SessionID string `xml:"session_id,attr"`
	Label     string `xml:"label"`
}

// PSAssoc binds participants to streams ("P1 sends on S1, receives
// on S2"). For FS the typical pattern is each participant sends on
// exactly one stream — that stream IS that participant's voice.
type PSAssoc struct {
	ParticipantID string   `xml:"participant_id,attr"`
	Send          []string `xml:"send"`
	Recv          []string `xml:"recv"`
}

// ParseMetadata parses the rs-metadata+xml body of a SIPREC INVITE.
// Empty bodies / parse errors return a zero-value *Recording with
// nil error — callers should treat absence of metadata as "label
// streams by index" and proceed.
func ParseMetadata(body string) (*Recording, error) {
	if strings.TrimSpace(body) == "" {
		return &Recording{}, nil
	}

	var rec Recording

	dec := xml.NewDecoder(strings.NewReader(body))
	// FS namespaces the elements; encoding/xml resolves prefixed
	// names by default. The strict namespace check would reject
	// FS's emission style — accept any namespace on top-level
	// recording.
	dec.DefaultSpace = "urn:ietf:params:xml:ns:recording:1"

	if err := dec.Decode(&rec); err != nil {
		return nil, fmt.Errorf("srs metadata: parse: %w", err)
	}

	return &rec, nil
}

// FsUUID decodes the recording's session_id back to the FreeSWITCH call
// UUID. mod_siprec encodes the parent call's UUID as standard base64 of
// its 16 raw bytes (RFC 7865 §6.9), so the SRS can correlate a recording
// with the FreeSWITCH call without any extra header. Returns ("", false)
// when there is no session or the ID is not a base64 16-byte value (a
// non-mod_siprec SRC using arbitrary session IDs).
func (r *Recording) FsUUID() (string, bool) {
	if r == nil || len(r.Sessions) == 0 {
		return "", false
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(r.Sessions[0].ID))
	if err != nil || len(raw) != 16 {
		return "", false
	}

	var b strings.Builder

	for i, v := range raw {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			b.WriteByte('-')
		}

		b.WriteString(hex.EncodeToString([]byte{v}))
	}

	return b.String(), true
}

// ParticipantAORs returns every participant's nameID AOR, in document
// order — for mod_siprec exactly [caller, callee].
func (r *Recording) ParticipantAORs() []string {
	if r == nil {
		return nil
	}

	out := make([]string, 0, len(r.Participants))
	for _, p := range r.Participants {
		out = append(out, p.NameID.AOR)
	}

	return out
}

// LabelFor returns a meaningful, filesystem-safe label for the
// stream with the given a=label string. Lookup order:
//
//  1. Find the Stream whose Label matches the SDP label.
//  2. Find the participant that <send>s on that stream (the
//     participant's own voice).
//  3. Use the participant's AOR userpart (or the participant's
//     <name> as a fallback).
//
// Returns "" when metadata can't resolve the label — caller falls
// back to a positional "leg1" / "leg2".
func (r *Recording) LabelFor(sdpLabel string) string {
	if r == nil {
		return ""
	}

	streamID := ""

	for _, s := range r.Streams {
		if s.Label == sdpLabel {
			streamID = s.ID

			break
		}
	}

	if streamID == "" {
		return ""
	}

	for _, a := range r.PSAssoc {
		for _, s := range a.Send {
			if s != streamID {
				continue
			}

			for _, p := range r.Participants {
				if p.ID != a.ParticipantID {
					continue
				}

				if name := userpartOf(p.NameID.AOR); name != "" {
					return name
				}

				if p.Name != "" {
					return sanitiseLabel(p.Name)
				}
			}
		}
	}

	return ""
}

// userpartOf extracts the user part of a sip: URI for use as a
// filesystem-safe label. Returns "" for empty / non-URI inputs.
//
//	sip:+12165285911@host → 12165285911  (leading + stripped)
//	sip:alice@host        → alice
//	sip:host              → ""           (no userpart)
func userpartOf(aor string) string {
	aor = strings.TrimSpace(aor)
	if aor == "" {
		return ""
	}

	if i := strings.Index(aor, ":"); i >= 0 {
		aor = aor[i+1:]
	}

	at := strings.Index(aor, "@")
	if at <= 0 {
		return ""
	}

	user := aor[:at]
	user = strings.TrimPrefix(user, "+")

	return sanitiseLabel(user)
}

// sanitiseLabel reduces an arbitrary string to characters safe for
// a filename: ASCII alphanumerics, dot, underscore, hyphen.
// Everything else collapses to underscore. Truncates to 32 chars
// to keep WAV paths sane.
func sanitiseLabel(s string) string {
	var b strings.Builder

	b.Grow(len(s))

	for _, r := range s {
		switch {
		case r >= '0' && r <= '9',
			r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}

	out := b.String()
	if len(out) > 32 {
		out = out[:32]
	}

	return out
}
