package siprecsrv

import (
	"fmt"
	"strconv"
	"strings"
)

// MediaDescription holds the bits of one m=audio block we care about:
// the remote port FreeSWITCH advertises (so we know where to send
// our own RTP if we wanted to, though we don't), the codec payload
// type, and the codec name.
type MediaDescription struct {
	Port        int    // remote RTP port (FS-side)
	PayloadType int    // RTP payload type (0=PCMU, 8=PCMA)
	CodecName   string // "PCMU" or "PCMA" (uppercase, normalised)
	Label       string // a=label:<id> — SIPREC tags each stream with the leg
	Direction   string // a=sendonly|sendrecv|recvonly|inactive; "" if absent (SRC offers are sendonly)
}

// ParsedSDP is the slice of an INVITE's SDP we extract: the
// connection-line IP and one MediaDescription per m=audio block.
// SIPREC INVITEs carry two m=audio blocks (one per call leg); the
// SRS responds with its own dual-port SDP and records each.
type ParsedSDP struct {
	ConnectionIP string
	Media        []MediaDescription
}

// ParseSDP extracts the connection IP and m=audio blocks from a
// raw SDP body. Tolerant of optional fields and ordering — the
// FS-side SDP carries plenty of attributes we don't care about.
//
// Returns an error only when the SDP is structurally broken (no
// m=audio at all, malformed port number); missing optional
// attributes are silently ignored.
func ParseSDP(sdp string) (*ParsedSDP, error) {
	out := &ParsedSDP{}

	var current *MediaDescription

	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}

		if len(line) < 2 || line[1] != '=' {
			continue
		}

		key, val := line[0], line[2:]

		switch key {
		case 'c':
			// c=IN IP4 192.168.1.1
			parts := strings.Fields(val)
			if len(parts) >= 3 && current == nil {
				out.ConnectionIP = parts[2]
			}
		case 'm':
			// m=audio 40000 RTP/AVP 0 8 101
			parts := strings.Fields(val)
			if len(parts) < 4 || parts[0] != "audio" {
				current = nil

				continue
			}

			port, err := strconv.Atoi(parts[1])
			if err != nil {
				return nil, fmt.Errorf("srs sdp: bad m=audio port %q: %w", parts[1], err)
			}

			pt, err := strconv.Atoi(parts[3])
			if err != nil {
				return nil, fmt.Errorf("srs sdp: bad payload type %q: %w", parts[3], err)
			}

			out.Media = append(out.Media, MediaDescription{
				Port:        port,
				PayloadType: pt,
				CodecName:   defaultCodecName(pt),
			})
			current = &out.Media[len(out.Media)-1]
		case 'a':
			if current == nil {
				continue
			}

			switch {
			case strings.HasPrefix(val, "rtpmap:"):
				// rtpmap:<pt> <name>/<rate>
				// An m=audio block can list multiple PTs (e.g.
				// "RTP/AVP 0 8" → PCMU + PCMA). Each PT gets its
				// own rtpmap. The FIRST PT in the m= line is the
				// offerer's preference; only the matching rtpmap
				// updates CodecName. Without this guard, later
				// rtpmaps overwrite the chosen codec.
				rest := val[len("rtpmap:"):]
				parts := strings.Fields(rest)
				if len(parts) >= 2 {
					var pt int
					if _, err := fmt.Sscanf(parts[0], "%d", &pt); err == nil &&
						pt == current.PayloadType {
						name := strings.SplitN(parts[1], "/", 2)[0]
						current.CodecName = strings.ToUpper(name)
					}
				}
			case strings.HasPrefix(val, "label:"):
				current.Label = val[len("label:"):]
			case val == "sendonly" || val == "sendrecv" || val == "recvonly" || val == "inactive":
				current.Direction = val
			}
		}
	}

	if len(out.Media) == 0 {
		return nil, fmt.Errorf("srs sdp: no m=audio in SDP")
	}

	return out, nil
}

// AllInactive reports whether every offered audio stream is a=inactive —
// the mod_siprec pause re-INVITE shape (RFC 7866 §6.4). An SRS must keep
// the recording (and its files) open across a pause; no RTP arrives until
// the matching sendonly resume re-INVITE.
func (p *ParsedSDP) AllInactive() bool {
	if p == nil || len(p.Media) == 0 {
		return false
	}

	for _, m := range p.Media {
		if m.Direction != "inactive" {
			return false
		}
	}

	return true
}

// defaultCodecName maps a static payload type to its canonical
// name. Used as a fallback when no rtpmap is present.
func defaultCodecName(pt int) string {
	switch pt {
	case 0:
		return "PCMU"
	case 8:
		return "PCMA"
	default:
		return ""
	}
}

// BuildAnswerSDP composes the SDP we send back in the SIPREC 200
// OK. Mirrors the offer's m=audio count + codec choice; replaces
// the ports with our local-side ports.
//
// For each offered media block we pick the first codec we support
// (PCMU > PCMA preference). If neither is offered we'd return an
// error — but every SIPREC-emitting SBC offers at least one of
// these.
//
// localIP is the host SRS advertises (caller decides — typically
// the same address callBroadcast's public host runs on, or a
// loopback when FS lives on the same box).
//
// localPorts must have at least len(offer.Media) entries; the
// caller has already opened the UDP listeners and knows the port
// numbers.
func BuildAnswerSDP(localIP string, offer *ParsedSDP, localPorts []int) (string, error) {
	if len(localPorts) < len(offer.Media) {
		return "", fmt.Errorf("srs sdp: need %d local ports, got %d",
			len(offer.Media), len(localPorts))
	}

	var b strings.Builder

	// session-level
	b.WriteString("v=0\r\n")
	b.WriteString("o=- 0 0 IN IP4 ")
	b.WriteString(localIP)
	b.WriteString("\r\n")
	b.WriteString("s=siprec-receiver SRS\r\n")
	b.WriteString("c=IN IP4 ")
	b.WriteString(localIP)
	b.WriteString("\r\n")
	b.WriteString("t=0 0\r\n")

	// media-level: one block per offered stream
	for i, m := range offer.Media {
		pt := chooseAnswerPayload(m)
		if pt < 0 {
			return "", fmt.Errorf("srs sdp: stream %d offers unsupported codec %q",
				i, m.CodecName)
		}

		fmt.Fprintf(&b, "m=audio %d RTP/AVP %d\r\n", localPorts[i], pt)
		fmt.Fprintf(&b, "a=rtpmap:%d %s/8000\r\n", pt, codecNameForPayload(pt))
		// recvonly — we never send RTP back. RFC 7866-compliant
		// SIPREC SRS answer: the SRC streams to us, we never
		// stream back. mod_siprec sets bypass_media=true on the
		// recording leg so FS's media negotiator doesn't trip
		// on the direction mismatch with sofia's auto-generated
		// sendrecv offer.
		b.WriteString("a=recvonly\r\n")

		if m.Label != "" {
			fmt.Fprintf(&b, "a=label:%s\r\n", m.Label)
		}
	}

	return b.String(), nil
}

// chooseAnswerPayload picks the first PT we support out of the
// offered media. Returns -1 when neither PCMU nor PCMA is on
// offer.
func chooseAnswerPayload(m MediaDescription) int {
	switch strings.ToUpper(m.CodecName) {
	case "PCMU":
		return 0
	case "PCMA":
		return 8
	}

	// Fallback to the offered payload type only when it's a known
	// static code.
	switch m.PayloadType {
	case 0, 8:
		return m.PayloadType
	}

	return -1
}

func codecNameForPayload(pt int) string {
	switch pt {
	case 0:
		return "PCMU"
	case 8:
		return "PCMA"
	}

	return "PCMU"
}
