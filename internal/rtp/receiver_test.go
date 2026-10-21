package rtp

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// TestReceiverDecodesPCMU sends one crafted PCMU RTP packet over
// loopback UDP and expects the decoded PCM frame on the callback.
func TestReceiverDecodesPCMU(t *testing.T) {
	pool := NewPortPool(45000, 45100)

	port, err := pool.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	defer pool.Release(port)

	frames := make(chan []int16, 1)

	recv, err := NewReceiver(port, func(pcm []int16) {
		cp := make([]int16, len(pcm))
		copy(cp, pcm)

		select {
		case frames <- cp:
		default:
		}
	})
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	recv.Start()
	defer recv.Stop()

	// Build an RTP packet: V=2, PT=0 (PCMU), seq=1, 160 payload bytes
	// of 0x55 (µ-law for a mid-scale sample).
	pkt := make([]byte, 12+160)
	pkt[0] = 0x80
	pkt[1] = 0x00
	binary.BigEndian.PutUint16(pkt[2:4], 1)
	binary.BigEndian.PutUint32(pkt[8:12], 0x1234)

	for i := 12; i < len(pkt); i++ {
		pkt[i] = 0x55
	}

	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		t.Fatalf("DialUDP: %v", err)
	}

	defer func() { _ = conn.Close() }()

	if _, err := conn.Write(pkt); err != nil {
		t.Fatalf("Write: %v", err)
	}

	select {
	case pcm := <-frames:
		if len(pcm) != 160 {
			t.Fatalf("frame samples = %d, want 160", len(pcm))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no decoded frame within 2s")
	}
}

func TestPortPoolExhaustion(t *testing.T) {
	pool := NewPortPool(46000, 46000)

	if _, err := pool.Acquire(); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	if _, err := pool.Acquire(); err == nil {
		t.Fatal("expected exhaustion error")
	}

	pool.Release(46000)

	if _, err := pool.Acquire(); err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
}
