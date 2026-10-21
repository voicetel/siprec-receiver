package rtp

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// rtpPacket builds a minimal RTP packet. pad > 0 adds that many
// padding bytes (last byte = count), as RFC 3550 §5.1 defines.
func rtpPacket(seq uint16, ts uint32, ssrc uint32, pt byte, payload []byte, pad int) []byte {
	pkt := make([]byte, 12+len(payload)+pad)
	pkt[0] = 0x80
	if pad > 0 {
		pkt[0] |= 0x20 // padding bit
	}
	pkt[1] = pt
	binary.BigEndian.PutUint16(pkt[2:4], seq)
	binary.BigEndian.PutUint32(pkt[4:8], ts)
	binary.BigEndian.PutUint32(pkt[8:12], ssrc)
	copy(pkt[12:], payload)

	if pad > 0 {
		pkt[len(pkt)-1] = byte(pad)
	}

	return pkt
}

type frameCollector struct{ ch chan []int16 }

func (f *frameCollector) cb(pcm []int16) {
	cp := make([]int16, len(pcm))
	copy(cp, pcm)

	select {
	case f.ch <- cp:
	default:
	}
}

func (f *frameCollector) next(t *testing.T, what string) []int16 {
	t.Helper()

	select {
	case pcm := <-f.ch:
		return pcm
	case <-time.After(2 * time.Second):
		t.Fatalf("no frame (%s) within 2s", what)

		return nil
	}
}

func startReceiver(t *testing.T, ip string) (*Receiver, int, *frameCollector) {
	t.Helper()

	pool := NewPortPool(45200, 45300)

	port, err := pool.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	fc := &frameCollector{ch: make(chan []int16, 16)}

	recv, err := NewReceiverIP(ip, port, fc.cb)
	if err != nil {
		t.Fatalf("NewReceiverIP: %v", err)
	}

	recv.Start()
	t.Cleanup(recv.Stop)

	return recv, port, fc
}

func dial(t *testing.T, port int) *net.UDPConn {
	t.Helper()

	conn, err := net.DialUDP("udp", nil,
		&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		t.Fatalf("DialUDP: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

func silence(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 0x55
	}

	return b
}

// Padding bytes must not be decoded as audio: a 160-byte payload
// with 40 bytes of padding yields 120 samples.
func TestReceiver_StripsPadding(t *testing.T) {
	_, port, fc := startReceiver(t, "127.0.0.1")
	conn := dial(t, port)

	pkt := rtpPacket(1, 0, 0x1111, 0, silence(120), 40)
	if _, err := conn.Write(pkt); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got := fc.next(t, "padded frame"); len(got) != 120 {
		t.Errorf("samples = %d, want 120 (padding stripped)", len(got))
	}
}

// The first sender owns the leg: packets from another source or SSRC
// are dropped.
func TestReceiver_LocksToFirstSender(t *testing.T) {
	_, port, fc := startReceiver(t, "127.0.0.1")
	owner := dial(t, port)
	intruder := dial(t, port)

	if _, err := owner.Write(rtpPacket(1, 0, 0xaaaa, 0, silence(160), 0)); err != nil {
		t.Fatal(err)
	}
	_ = fc.next(t, "owner frame")

	// Wrong SSRC, then wrong source socket: both must vanish.
	_, _ = owner.Write(rtpPacket(2, 160, 0xbbbb, 0, silence(160), 0))
	_, _ = intruder.Write(rtpPacket(2, 160, 0xaaaa, 0, silence(160), 0))

	if _, err := owner.Write(rtpPacket(2, 160, 0xaaaa, 0, silence(160), 0)); err != nil {
		t.Fatal(err)
	}

	got := fc.next(t, "second owner frame")
	if len(got) != 160 {
		t.Errorf("samples = %d, want 160", len(got))
	}

	select {
	case extra := <-fc.ch:
		t.Fatalf("unexpected extra frame of %d samples (foreign packet leaked)", len(extra))
	case <-time.After(150 * time.Millisecond):
	}
}

// A timestamp jump (lost packets / silence suppression) becomes
// silence in the output, keeping duration honest: 160 samples, then
// a packet 480 timestamps later, yields a 320-sample silence fill
// before it.
func TestReceiver_FillsTimestampGapsWithSilence(t *testing.T) {
	_, port, fc := startReceiver(t, "127.0.0.1")
	conn := dial(t, port)

	_, _ = conn.Write(rtpPacket(1, 0, 0x2222, 0, silence(160), 0))
	_ = fc.next(t, "first frame")

	_, _ = conn.Write(rtpPacket(4, 480, 0x2222, 0, silence(160), 0))

	fill := fc.next(t, "gap fill")
	if len(fill) != 320 {
		t.Fatalf("gap fill = %d samples, want 320", len(fill))
	}

	for _, s := range fill {
		if s != 0 {
			t.Fatalf("gap fill is not silence (sample %d)", s)
		}
	}

	_ = fc.next(t, "post-gap frame")
}
