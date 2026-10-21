package rtp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

const (
	rtpHeaderSize = 12
	maxRTPPacket  = 1500
	readTimeout   = 100 * time.Millisecond

	// PayloadPCMU is the RTP payload type for G.711 μ-law (PCMU).
	PayloadPCMU = 0
	// PayloadPCMA is the RTP payload type for G.711 A-law (PCMA).
	PayloadPCMA = 8
)

// FrameCallback is called for each decoded PCM frame.
type FrameCallback func(pcm []int16)

// Receive-loop conn-operation seams. These default to the real *net.UDPConn
// methods and exist only so the loop's socket-error branches (deadline-set
// failure, non-timeout read error) can be exercised deterministically in
// tests without a flaky close-during-read race. They are not on the decode
// hot path (DecodePCMUInto/DecodePCMAInto) — the receive loop runs at the
// RTP packet rate (~50/sec/call), so the indirect call is immeasurable.
var (
	connSetReadDeadline = func(c *net.UDPConn, t time.Time) error { return c.SetReadDeadline(t) }
	connReadFromUDP     = func(c *net.UDPConn, b []byte) (int, *net.UDPAddr, error) { return c.ReadFromUDP(b) }
)

// Receiver listens on a UDP port for RTP and delivers decoded PCM frames.
//
// The receiver locks onto the first (source address, SSRC) it sees and
// drops everything else: an SRS leg has exactly one legitimate sender
// (the SRC's fork socket), and accepting other senders would let anyone
// who can reach the port inject audio into a recording. Packets are
// ordered by RTP timestamp: sequence gaps become silence, late packets
// are dropped.
type Receiver struct {
	conn            *net.UDPConn
	callback        FrameCallback
	done            chan struct{}
	loopDone        chan struct{}
	port            int
	PacketsReceived int64
	PacketsLost     int64
	once            sync.Once
	dropLogOnce     sync.Once
	started         bool
	lastSeq         uint16
	seqInit         bool
	lastTS          uint32
	tsInit          bool
	locked          bool
	srcAddr         string
	ssrc            uint32
}

// NewReceiver creates a receiver bound to all IPv4 interfaces.
func NewReceiver(port int, callback FrameCallback) (*Receiver, error) {
	return NewReceiverIP("", port, callback)
}

// NewReceiverIP creates a receiver bound to bindIP ("" or an
// unparseable value means all interfaces). Binding to the address the
// SDP answer advertises keeps the RTP surface off other interfaces.
func NewReceiverIP(bindIP string, port int, callback FrameCallback) (*Receiver, error) {
	ip := net.IPv4zero
	if bindIP != "" {
		if parsed := net.ParseIP(bindIP); parsed != nil && parsed.To4() != nil {
			ip = parsed.To4()
		}
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port, IP: ip})
	if err != nil {
		return nil, fmt.Errorf("rtp listen on port %d: %w", port, err)
	}

	return &Receiver{
		port:     port,
		conn:     conn,
		callback: callback,
		done:     make(chan struct{}),
		loopDone: make(chan struct{}),
	}, nil
}

// Start begins receiving RTP packets. Non-blocking — runs in a goroutine.
// The spawned loop closes loopDone on return so Stop can join it.
func (r *Receiver) Start() {
	r.started = true

	go func() {
		defer close(r.loopDone)
		r.loop()
	}()
}

// Stop shuts down the receiver and waits for the receive loop to exit.
//
// Joining the loop goroutine (not just signalling it) is what bounds the
// loop's lifetime to the Stop call: it guarantees no receive goroutine is
// still reading the conn — or the connReadFromUDP/connSetReadDeadline seams —
// after Stop returns. That closes the data race where a leaked loop goroutine
// outlived its owner, and it tightens the production invariant that the
// session manager relies on (releaseMedia returns the RTP port to the pool
// immediately after Stop; the join ensures the socket is truly idle before
// the port can be reacquired).
//
// Stop must not be called from inside the FrameCallback (i.e. on the receive
// goroutine itself) or the join would deadlock; production callers invoke it
// from the session-teardown goroutine, never from onPCM.
func (r *Receiver) Stop() {
	r.once.Do(func() {
		close(r.done)

		if err := r.conn.Close(); err != nil {
			log.Printf("[rtp] close port %d: %v", r.port, err)
		}
	})

	// Join outside the once so every Stop caller (idempotent re-calls
	// included) observes loop exit, not just the first. Wait only if a loop
	// was ever started — otherwise loopDone is never closed and the receive
	// goroutine that reads the seams/conn never existed, so there is nothing
	// to join.
	if r.started {
		<-r.loopDone
	}
}

// Port returns the listening port.
func (r *Receiver) Port() int {
	return r.port
}

func (r *Receiver) loop() {
	// A panic in the loop must not take the whole process (and every
	// other active recording) down with it.
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[rtp] receiver port %d: panic recovered: %v", r.port, rec)
		}
	}()

	buf := make([]byte, maxRTPPacket)
	// Reusable PCM scratch — sized to hold the largest plausible RTP
	// payload (an MTU-bounded 1500-byte packet of single-byte G.711
	// samples). Reused across all packets to avoid per-packet allocation
	// in the hot path. The slice is passed by view into the callback;
	// the callback must not retain it past return.
	pcm := make([]int16, maxRTPPacket)

	for {
		select {
		case <-r.done:
			return
		default:
		}

		if err := connSetReadDeadline(r.conn, time.Now().Add(readTimeout)); err != nil {
			// Connection closed during shutdown — exit cleanly.
			return
		}

		n, addr, err := connReadFromUDP(r.conn, buf)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			select {
			case <-r.done:
				return
			default:
				log.Printf("[rtp] read error port %d: %v", r.port, err)

				continue
			}
		}

		if n < rtpHeaderSize {
			continue
		}

		r.PacketsReceived++

		// Parse RTP header
		header, payload := parseRTP(buf[:n])
		if header == nil {
			continue
		}

		// Strip RTP padding: the last byte counts the padding bytes,
		// which are not audio.
		if header.Padding && len(payload) > 0 {
			pad := int(payload[len(payload)-1])
			if pad > 0 && pad <= len(payload) {
				payload = payload[:len(payload)-pad]
			}

			if len(payload) == 0 {
				continue
			}
		}

		// Lock onto the first sender (address + SSRC); drop the rest.
		src := ""
		if addr != nil {
			src = addr.String()
		}

		if !r.locked {
			r.locked, r.srcAddr, r.ssrc = true, src, header.SSRC
		} else if src != r.srcAddr || header.SSRC != r.ssrc {
			r.dropLogOnce.Do(func() {
				log.Printf("[rtp] port %d: dropping RTP from unexpected source %s ssrc=%d (locked to %s ssrc=%d)",
					r.port, src, header.SSRC, r.srcAddr, r.ssrc)
			})

			continue
		}

		// Sequence ordering: a signed difference keeps wrap and
		// reordering sane. Late/duplicate packets are dropped, gaps
		// count as loss.
		if r.seqInit {
			diff := int16(header.SequenceNumber - (r.lastSeq + 1)) // #nosec G115 -- RFC 3550 sequence wrap arithmetic
			if diff < 0 {
				continue
			}

			if diff > 0 {
				r.PacketsLost += int64(diff)
			}
		}

		r.lastSeq = header.SequenceNumber
		r.seqInit = true

		// Timestamp ordering: fill gaps with silence so the
		// recording keeps wall-clock duration (and legs stay in
		// sync); drop packets whose media we have already passed.
		// lastTS is the next expected timestamp, so the signed
		// delta is exactly the number of missing samples.
		if r.tsInit && r.callback != nil {
			missing := int64(int32(header.Timestamp - r.lastTS)) // #nosec G115 -- RTP timestamp wrap arithmetic

			switch {
			case missing > 0:
				if missing <= 16000 {
					r.callback(make([]int16, missing))
				}
			case missing < 0:
				continue
			}
		}

		// Decode G.711 into the receiver-owned PCM scratch buffer.
		var nSamples int

		switch header.PayloadType {
		case PayloadPCMU:
			nSamples = decodePCMUInto(pcm, payload)
		case PayloadPCMA:
			nSamples = decodePCMAInto(pcm, payload)
		default:
			// Unknown codec — skip.
			continue
		}

		if r.callback != nil && nSamples > 0 {
			r.callback(pcm[:nSamples])
		}

		r.lastTS = header.Timestamp + uint32(len(payload)) // #nosec G115 -- payload is MTU-bounded
		r.tsInit = true
	}
}

// ----------------------------------------------------------------------------
// RTP header parsing
// ----------------------------------------------------------------------------

type rtpHeader struct {
	Version        uint8
	Padding        bool
	Extension      bool
	CSRCCount      uint8
	Marker         bool
	PayloadType    uint8
	SequenceNumber uint16
	Timestamp      uint32
	SSRC           uint32
}

func parseRTP(buf []byte) (*rtpHeader, []byte) {
	if len(buf) < rtpHeaderSize {
		return nil, nil
	}

	h := &rtpHeader{
		Version:        (buf[0] >> 6) & 0x3,
		Padding:        (buf[0]>>5)&1 == 1,
		Extension:      (buf[0]>>4)&1 == 1,
		CSRCCount:      buf[0] & 0xF,
		Marker:         (buf[1]>>7)&1 == 1,
		PayloadType:    buf[1] & 0x7F,
		SequenceNumber: binary.BigEndian.Uint16(buf[2:4]),
		Timestamp:      binary.BigEndian.Uint32(buf[4:8]),
		SSRC:           binary.BigEndian.Uint32(buf[8:12]),
	}

	if h.Version != 2 {
		return nil, nil
	}

	// Skip CSRC list
	offset := rtpHeaderSize + int(h.CSRCCount)*4
	if offset > len(buf) {
		return nil, nil
	}

	// Skip extension header if present
	if h.Extension && offset+4 <= len(buf) {
		extLen := int(binary.BigEndian.Uint16(buf[offset+2 : offset+4]))
		offset += 4 + extLen*4
	}

	if offset >= len(buf) {
		return nil, nil
	}

	return h, buf[offset:]
}

// ----------------------------------------------------------------------------
// G.711 μ-law decode (PCMU payload type 0)
// ITU-T G.711 μ-law to linear PCM
// ----------------------------------------------------------------------------

var pcmuTable [256]int16

// μ-law decoded magnitudes are bounded at 8159 (well inside int16). All math
// stays in int16 to avoid widening casts that would otherwise trip overflow
// checkers.
func init() {
	for i := range 256 {
		b := ^byte(i)
		sign := b & 0x80
		exponent := (b >> 4) & 0x07
		mantissa := int16(b & 0x0F)

		sample := (mantissa<<1 + 33) << (exponent + 2)
		if sign != 0 {
			sample = -sample
		}

		pcmuTable[i] = sample
	}
}

// decodePCMUInto decodes μ-law into a caller-provided buffer and returns
// the number of samples written. The buffer must hold at least len(data)
// elements. Used by the RTP receiver to avoid per-packet allocation.
func decodePCMUInto(out []int16, data []byte) int {
	n := len(data)
	if n > len(out) {
		n = len(out)
	}

	for i := range n {
		out[i] = pcmuTable[data[i]]
	}

	return n
}

// ----------------------------------------------------------------------------
// G.711 A-law decode (PCMA payload type 8)
// ITU-T G.711 A-law to linear PCM
// ----------------------------------------------------------------------------

var pcmaTable [256]int16

// A-law decoded magnitudes are bounded at 4032 (well inside int16). All math
// stays in int16 to avoid widening casts that would otherwise trip overflow
// checkers.
func init() {
	for i := range 256 {
		b := byte(i) ^ 0x55
		sign := b & 0x80
		exponent := (b >> 4) & 0x07
		mantissa := int16(b & 0x0F)

		var sample int16

		if exponent == 0 {
			sample = mantissa<<1 + 1
		} else {
			sample = (mantissa<<1 | 0x21) << (exponent - 1)
		}

		if sign != 0 {
			sample = -sample
		}

		pcmaTable[i] = sample
	}
}

// decodePCMAInto decodes A-law into a caller-provided buffer and returns
// the number of samples written.
func decodePCMAInto(out []int16, data []byte) int {
	n := len(data)
	if n > len(out) {
		n = len(out)
	}

	for i := range n {
		out[i] = pcmaTable[data[i]]
	}

	return n
}
