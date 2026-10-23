package siprecsrv

import (
	"fmt"
	"strconv"
	"strings"
)

// MediaDescription holds the bits of one m=audio block we care
// about: the offered port and payload types, the codec we chose,
// the direction, and the SIPREC label.
type MediaDescription struct {
	Port         int    // remote RTP port (FS-side); 0 = declined stream
	Proto        string // transport protocol on the m= line ("RTP/AVP"; "" = RTP/AVP)
	PayloadType  int    // first RTP payload type on the m= line
	PayloadTypes []int  // every payload type on the m= line, in offer order
	CodecName    string // chosen supported codec: "PCMU" or "PCMA" ("" if none offered)
	Label        string // a=label:<id>, sanitised — SIPREC tags each stream with the leg
	Direction    string // a=sendonly|sendrecv|recvonly|inactive; "" if absent (SRC offers are sendonly)
	ConnectionIP string // media-level c= address when present
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

	curRtp := map[int]string{}

	finalizeMedia := func() {
		if current == nil {
			return
		}

		current.CodecName = chooseOfferedCodec(current.PayloadTypes, curRtp)
	}

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
			// c=IN IP4 192.168.1.1 (session-level before any m=,
			// media-level after)
			parts := strings.Fields(val)
			if len(parts) >= 3 {
				if current == nil {
					out.ConnectionIP = parts[2]
				} else {
					current.ConnectionIP = parts[2]
				}
			}
		case 'm':
			// m=audio 40000 RTP/AVP 0 8 101
			finalizeMedia()
			curRtp = map[int]string{}

			parts := strings.Fields(val)
			if len(parts) < 4 || parts[0] != "audio" {
				current = nil

				continue
			}

			port, err := strconv.Atoi(parts[1])
			if err != nil {
				return nil, fmt.Errorf("srs sdp: bad m=audio port %q: %w", parts[1], err)
			}

			pts := make([]int, 0, len(parts)-3)
			for _, p := range parts[3:] {
				pt, err := strconv.Atoi(p)
				if err != nil {
					return nil, fmt.Errorf("srs sdp: bad payload type %q: %w", p, err)
				}

				pts = append(pts, pt)
			}

			out.Media = append(out.Media, MediaDescription{
				Port:         port,
				Proto:        parts[2],
				PayloadType:  pts[0],
				PayloadTypes: pts,
			})
			current = &out.Media[len(out.Media)-1]
		case 'a':
			if current == nil {
				continue
			}

			switch {
			case strings.HasPrefix(val, "rtpmap:"):
				// rtpmap:<pt> <name>/<rate> — collected for the
				// codec choice at the end of the media block.
				rest := val[len("rtpmap:"):]
				parts := strings.Fields(rest)
				if len(parts) >= 2 {
					var pt int
					if _, err := fmt.Sscanf(parts[0], "%d", &pt); err == nil {
						curRtp[pt] = strings.ToUpper(strings.SplitN(parts[1], "/", 2)[0])
					}
				}
			case strings.HasPrefix(val, "label:"):
				// Sanitised at parse: the label is written back
				// into our SDP answer, so CR/LF or other controls
				// in it would inject lines into the answer.
				current.Label = sanitiseLabel(val[len("label:"):])
			case val == "sendonly" || val == "sendrecv" || val == "recvonly" || val == "inactive":
				current.Direction = val
			}
		}
	}

	finalizeMedia()

	if len(out.Media) == 0 {
		return nil, fmt.Errorf("srs sdp: no m=audio in SDP")
	}

	return out, nil
}

// chooseOfferedCodec picks the codec we will answer for one offered
// media block: the first offered payload type we support, in offer
// order, where support means a static PCMU/PCMA payload type or an
// rtpmap naming PCMU/PCMA (dynamic payload types included). The
// answer itself always uses the static payload type for the codec,
// which the sender adopts from our answer.
func chooseOfferedCodec(pts []int, rtpmap map[int]string) string {
	for _, pt := range pts {
		if name := rtpmap[pt]; name == "PCMU" || name == "PCMA" {
			return name
		}

		if name := defaultCodecName(pt); name != "" {
			return name
		}
	}

	return ""
}

// proto is the stream's transport protocol, answered as offered
// (RFC 3264 §6).
func (m MediaDescription) proto() string {
	if m.Proto == "" {
		return "RTP/AVP"
	}

	return m.Proto
}

// plainRTP reports whether the stream is unencrypted RTP. The SRS holds
// no SRTP keys: recording RTP/SAVP (or DTLS-SRTP) would write
// ciphertext as audio.
func (m MediaDescription) plainRTP() bool {
	return strings.EqualFold(m.proto(), "RTP/AVP") || strings.EqualFold(m.proto(), "RTP/AVPF")
}

// MediaWarning is a SIP Warning (RFC 3261 §20.43) explaining why an
// offer is refused with 488.
type MediaWarning struct {
	Code int // 302 incompatible transport, 305 incompatible media format
	Text string
}

// Header renders w as a Warning header value.
func (w MediaWarning) Header() string {
	return fmt.Sprintf("%d siprec-receiver %q", w.Code, w.Text)
}

// Unacceptable explains why the offer cannot be recorded, or is nil.
// Every stream that is not declined (port 0) must be plain RTP carrying
// PCMU or PCMA. One unrecordable stream refuses the whole offer: a
// two-leg recording that silently loses a leg is worse than an SRC
// told why.
func (p *ParsedSDP) Unacceptable() *MediaWarning {
	for i, m := range p.Media {
		switch {
		case m.Port == 0:
		case !m.plainRTP():
			return &MediaWarning{Code: 302, Text: fmt.Sprintf(
				"Incompatible transport protocol on stream %d: this SRS records plain RTP/AVP only, not SRTP", i)}
		case chooseAnswerPayload(m) < 0:
			return &MediaWarning{Code: 305, Text: fmt.Sprintf(
				"Incompatible media format on stream %d: this SRS records PCMU or PCMA only", i)}
		}
	}

	return nil
}

// AllInactive reports whether every offered audio stream is a=inactive —
// the mod_siprec pause re-INVITE shape (RFC 7866 §7.1.1.1). An SRS must keep
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
// OK. Mirrors the offer's m=audio count; replaces the ports with
// our local-side ports. For each offered media block we answer the
// first offered codec we support (PCMU/PCMA, static payload type);
// if neither is offered we'd return an error — but every
// SIPREC-emitting SRC offers at least one of these.
//
// localIP is the host the SRS advertises (caller decides —
// typically a loopback when the SRC lives on the same box, else an
// address the SRC can route RTP to).
//
// localPorts must have at least len(offer.Media) entries; the
// caller has already opened the UDP listeners and knows the port
// numbers.
func BuildAnswerSDP(localIP string, offer *ParsedSDP, localPorts []int) (string, error) {
	return BuildAnswerSDPWithVersion(localIP, offer, localPorts, 0)
}

// BuildAnswerSDPWithVersion is BuildAnswerSDP with an SDP session
// version for the o= line, which RFC 3264 §8 requires to increase
// with every new answer in the same session (re-INVITE answers).
// A localPorts entry of 0 answers that stream with port 0 (declined,
// e.g. an offered port-0 stream) and no attributes.
func BuildAnswerSDPWithVersion(localIP string, offer *ParsedSDP, localPorts []int, version int) (string, error) {
	if len(localPorts) < len(offer.Media) {
		return "", fmt.Errorf("srs sdp: need %d local ports, got %d",
			len(offer.Media), len(localPorts))
	}

	var b strings.Builder

	// session-level
	b.WriteString("v=0\r\n")
	fmt.Fprintf(&b, "o=- 0 %d IN IP4 %s\r\n", version, localIP)
	b.WriteString("s=siprec-receiver SRS\r\n")
	b.WriteString("c=IN IP4 ")
	b.WriteString(localIP)
	b.WriteString("\r\n")
	b.WriteString("t=0 0\r\n")
	// Session-level recvonly as well as per stream: FreeSWITCH clears a
	// leg's RTP timeout only for a session-level a=recvonly, and the SRS
	// never sends RTP back, so with only the media-level attribute the
	// SRC's profile rtp-timeout-sec (300 s in the vanilla profiles)
	// hangs the recording leg up mid-call. A media-level a=inactive
	// still overrides it for a paused stream (RFC 4566 §6).
	b.WriteString("a=recvonly\r\n")

	// media-level: one block per offered stream
	for i, m := range offer.Media {
		port := localPorts[i]
		if m.Port == 0 {
			port = 0
		}
		pt := chooseAnswerPayload(m)
		if pt < 0 {
			if port == 0 && len(m.PayloadTypes) > 0 {
				// Declined stream with no codec we support: echo
				// the first offered PT on the port-0 line.
				fmt.Fprintf(&b, "m=audio 0 %s %d\r\n", m.proto(), m.PayloadTypes[0])

				continue
			}

			return "", fmt.Errorf("srs sdp: stream %d offers unsupported codec %q",
				i, m.CodecName)
		}

		if port == 0 {
			fmt.Fprintf(&b, "m=audio 0 %s %d\r\n", m.proto(), pt)

			continue
		}

		fmt.Fprintf(&b, "m=audio %d %s %d\r\n", port, m.proto(), pt)
		fmt.Fprintf(&b, "a=rtpmap:%d %s/8000\r\n", pt, codecNameForPayload(pt))
		// recvonly — we never send RTP back. RFC 7866-compliant
		// SIPREC SRS answer: the SRC streams to us, we never
		// stream back. An inactive offer is answered inactive
		// (RFC 3264 §6.1); mod_siprec sets bypass_media=true on the
		// recording leg so FS's media negotiator doesn't trip on
		// the direction semantics with sofia's generated offers.
		if m.Direction == "inactive" {
			b.WriteString("a=inactive\r\n")
		} else {
			b.WriteString("a=recvonly\r\n")
		}

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
