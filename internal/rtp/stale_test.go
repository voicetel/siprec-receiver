package rtp

import (
	"errors"
	"net"
	"testing"
	"time"
)

// Quiet tells a fresh port apart from one a stream is still aimed at.
func TestReceiverQuiet(t *testing.T) {
	t.Run("no wait", func(t *testing.T) {
		recv, port := covReceiver(t, nil)
		t.Cleanup(recv.Stop)

		_, _ = dial(t, port).Write(rtpPacket(1, 0, 1, 0, silence(160), 0))

		if !recv.Quiet(0) {
			t.Error("Quiet(0) must not look at the socket")
		}
	})

	t.Run("silent port", func(t *testing.T) {
		recv, _ := covReceiver(t, nil)
		t.Cleanup(recv.Stop)

		if !recv.Quiet(20 * time.Millisecond) {
			t.Error("a port nobody sends to reported traffic")
		}
	})

	t.Run("port with a stream", func(t *testing.T) {
		recv, port := covReceiver(t, nil)
		t.Cleanup(recv.Stop)

		if _, err := dial(t, port).Write(rtpPacket(1, 0, 1, 0, silence(160), 0)); err != nil {
			t.Fatal(err)
		}

		if recv.Quiet(time.Second) {
			t.Error("a port already receiving RTP reported quiet")
		}
	})

	t.Run("deadline error", func(t *testing.T) {
		recv, _ := covReceiver(t, nil)
		t.Cleanup(recv.Stop)

		prev := connSetReadDeadline
		connSetReadDeadline = func(*net.UDPConn, time.Time) error { return errors.New("boom") }
		t.Cleanup(func() { connSetReadDeadline = prev })

		if !recv.Quiet(time.Second) {
			t.Error("an unreadable port must not be refused as busy")
		}
	})
}

func TestPortPoolRotateStart(t *testing.T) {
	cases := []struct {
		n    int
		want []int
	}{
		{0, []int{48100, 48102, 48104}},
		{1, []int{48102, 48104, 48100}},
		{5, []int{48104, 48100, 48102}},  // wraps
		{-1, []int{48104, 48100, 48102}}, // negative wraps too
	}

	for _, tc := range cases {
		pool := NewPortPool(48100, 48104)
		pool.RotateStart(tc.n)

		for i, want := range tc.want {
			got, err := pool.Acquire()
			if err != nil || got != want {
				t.Fatalf("n=%d acquire %d = %d, %v; want %d", tc.n, i, got, err, want)
			}
		}
	}

	empty := NewPortPool(48110, 48100)
	empty.RotateStart(3)

	if empty.Available() != 0 {
		t.Error("rotating an empty pool created ports")
	}
}
