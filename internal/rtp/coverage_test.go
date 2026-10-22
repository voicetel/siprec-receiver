package rtp

import (
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// covReceiver binds a receiver to an ephemeral loopback port and returns
// it with the port the kernel assigned.
func covReceiver(t *testing.T, cb FrameCallback) (*Receiver, int) {
	t.Helper()

	recv, err := NewReceiverIP("127.0.0.1", 0, cb)
	if err != nil {
		t.Fatalf("NewReceiverIP: %v", err)
	}

	return recv, recv.conn.LocalAddr().(*net.UDPAddr).Port
}

func TestNewReceiverBindFallbacks(t *testing.T) {
	for _, ip := range []string{"", "::1", "not-an-ip"} {
		recv, err := NewReceiverIP(ip, 0, nil)
		if err != nil {
			t.Fatalf("NewReceiverIP(%q): %v", ip, err)
		}

		got := recv.conn.LocalAddr().(*net.UDPAddr).IP
		if !got.Equal(net.IPv4zero) {
			t.Errorf("NewReceiverIP(%q) bound %v, want 0.0.0.0", ip, got)
		}

		recv.Stop()
	}

	recv, err := NewReceiver(0, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	recv.Stop()
	recv.Stop() // idempotent, never started
}

func TestNewReceiverPortInUse(t *testing.T) {
	first, port := covReceiver(t, nil)
	t.Cleanup(first.Stop)

	_, err := NewReceiverIP("127.0.0.1", port, nil)
	if err == nil || !strings.Contains(err.Error(), "rtp listen on port") {
		t.Fatalf("err = %v, want listen error", err)
	}
}

func TestReceiverPort(t *testing.T) {
	pool := NewPortPool(47000, 47100)

	port, recv, err := AcquireBound(pool, func(p int) (*Receiver, error) {
		return NewReceiverIP("127.0.0.1", p, nil)
	})
	if err != nil {
		t.Fatalf("AcquireBound: %v", err)
	}

	defer pool.Release(port)
	defer recv.Stop()

	if recv.Port() != port {
		t.Errorf("Port() = %d, want %d", recv.Port(), port)
	}
}

func TestStopLogsCloseError(t *testing.T) {
	recv, _ := covReceiver(t, nil)

	if err := recv.conn.Close(); err != nil {
		t.Fatal(err)
	}

	recv.Stop() // second close fails; Stop must still complete

	select {
	case <-recv.done:
	default:
		t.Fatal("done not closed by Stop")
	}
}

func TestLoopExitsWhenDeadlineFails(t *testing.T) {
	recv, _ := covReceiver(t, nil)

	if err := recv.conn.Close(); err != nil {
		t.Fatal(err)
	}

	finished := make(chan struct{})

	go func() {
		recv.loop()
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not exit on closed conn")
	}
}

func TestLoopExitsWhenDoneClosedBeforeIteration(t *testing.T) {
	recv, _ := covReceiver(t, nil)
	t.Cleanup(func() { _ = recv.conn.Close() })

	close(recv.done)
	recv.loop() // returns immediately

	if recv.PacketsReceived != 0 {
		t.Errorf("PacketsReceived = %d, want 0", recv.PacketsReceived)
	}
}

func TestLoopRecoversCallbackPanic(t *testing.T) {
	called := make(chan struct{})

	recv, port := covReceiver(t, func([]int16) {
		close(called)
		panic("callback exploded")
	})

	recv.Start()

	conn := dial(t, port)
	if _, err := conn.Write(rtpPacket(1, 0, 0x5150, PayloadPCMU, silence(160), 0)); err != nil {
		t.Fatal(err)
	}

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("callback not invoked")
	}

	select {
	case <-recv.loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not exit after recovered panic")
	}

	recv.Stop()
}

// TestLoopPacketHandling drives the loop through every per-packet branch
// using a single sender socket (loopback preserves order), checking the
// emitted frames and counters after Stop joins the loop.
func TestLoopPacketHandling(t *testing.T) {
	fc := &frameCollector{ch: make(chan []int16, 32)}
	recv, port := covReceiver(t, fc.cb)
	recv.Start()

	conn := dial(t, port)
	send := func(pkt []byte) {
		t.Helper()

		if _, err := conn.Write(pkt); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	const ssrc = 0xC0FFEE

	// Dropped before counting: shorter than an RTP header.
	send([]byte{0x80, 0, 0, 1, 0})

	// Counted, dropped by parseRTP: version 1.
	v1 := rtpPacket(1, 0, ssrc, PayloadPCMU, silence(10), 0)
	v1[0] = 0x40
	send(v1)

	// Counted, dropped: payload is entirely padding.
	send(rtpPacket(1, 0, ssrc, PayloadPCMU, nil, 4))

	// Padding bit set but count exceeds the payload: payload kept as-is.
	// Locks the receiver, emits 10 samples, next expected ts = 10.
	badPad := rtpPacket(1, 0, ssrc, PayloadPCMU, silence(10), 0)
	badPad[0] |= 0x20
	badPad[len(badPad)-1] = 0xFF
	send(badPad)

	// Duplicate sequence number: dropped.
	send(rtpPacket(1, 0, ssrc, PayloadPCMU, silence(10), 0))

	// Unknown payload type: advances seq, emits nothing.
	send(rtpPacket(2, 10, ssrc, 96, silence(10), 0))

	// PCMA after a one-packet sequence gap: no ts gap, 160 samples.
	alaw := make([]byte, 160)
	for i := range alaw {
		alaw[i] = 0xD5
	}

	send(rtpPacket(4, 10, ssrc, PayloadPCMA, alaw, 0))

	// Timestamp behind what was already played: dropped.
	send(rtpPacket(5, 5, ssrc, PayloadPCMU, silence(160), 0))

	// Timestamp jump beyond the 16000-sample fill cap: no silence fill.
	send(rtpPacket(6, 170+20000, ssrc, PayloadPCMU, silence(80), 0))

	first := fc.next(t, "bad-padding frame")
	if len(first) != 10 {
		t.Fatalf("bad-padding frame = %d samples, want 10", len(first))
	}

	if first[9] != pcmuTable[0xFF] {
		t.Errorf("last sample = %d, want decoded 0xFF (%d)", first[9], pcmuTable[0xFF])
	}

	pcma := fc.next(t, "PCMA frame")
	if len(pcma) != 160 {
		t.Fatalf("PCMA frame = %d samples, want 160", len(pcma))
	}

	for _, s := range pcma {
		if s != pcmaTable[0xD5] {
			t.Fatalf("PCMA sample = %d, want %d", s, pcmaTable[0xD5])
		}
	}

	last := fc.next(t, "post-jump frame")
	if len(last) != 80 {
		t.Fatalf("post-jump frame = %d samples, want 80 (no silence fill)", len(last))
	}

	recv.Stop()

	select {
	case extra := <-fc.ch:
		t.Errorf("unexpected extra frame of %d samples", len(extra))
	default:
	}

	if recv.PacketsReceived != 8 {
		t.Errorf("PacketsReceived = %d, want 8", recv.PacketsReceived)
	}

	if recv.PacketsLost != 1 {
		t.Errorf("PacketsLost = %d, want 1", recv.PacketsLost)
	}
}

// TestLoopReadErrors runs the loop synchronously over a real socket while
// wrapping the existing read hook, so a non-timeout read error can be
// injected and the loop terminated deterministically. A nil callback
// exercises the no-consumer path.
func TestLoopReadErrors(t *testing.T) {
	recv, port := covReceiver(t, nil)
	t.Cleanup(func() { _ = recv.conn.Close() })

	conn := dial(t, port)

	origRead := connReadFromUDP
	t.Cleanup(func() { connReadFromUDP = origRead })

	const packets = 3

	var calls, delivered int

	connReadFromUDP = func(c *net.UDPConn, b []byte) (int, *net.UDPAddr, error) {
		calls++

		switch {
		case calls == 1:
			// Nothing sent yet: real read deadline expires.
			return origRead(c, b)
		case calls == 2:
			for i := range packets {
				ts := uint32(i * 161)
				if _, err := conn.Write(rtpPacket(uint16(i+1), ts, 0x77, PayloadPCMU, silence(160), 0)); err != nil {
					t.Errorf("Write: %v", err)
				}
			}

			return 0, nil, errors.New("synthetic socket failure")
		case delivered < packets:
			delivered++

			return origRead(c, b)
		default:
			close(recv.done)

			return 0, nil, errors.New("read after shutdown")
		}
	}

	recv.loop()

	if recv.PacketsReceived != packets {
		t.Errorf("PacketsReceived = %d, want %d", recv.PacketsReceived, packets)
	}

	if !recv.tsInit || recv.lastTS != 2*160+2+160 {
		t.Errorf("lastTS = %d (init %v), want %d", recv.lastTS, recv.tsInit, 2*160+2+160)
	}
}

func TestParseRTPEdgeCases(t *testing.T) {
	base := rtpPacket(7, 1000, 0xABCD, PayloadPCMA, silence(4), 0)

	if h, p := parseRTP(base[:11]); h != nil || p != nil {
		t.Error("short buffer parsed")
	}

	if h, p := parseRTP(base[:12]); h != nil || p != nil {
		t.Error("header-only buffer parsed")
	}

	csrc := append([]byte(nil), base...)
	csrc[0] |= 0x0F // 15 CSRCs = 60 bytes, far past the buffer

	if h, _ := parseRTP(csrc); h != nil {
		t.Error("CSRC overflow parsed")
	}

	// One CSRC + extension header with one 32-bit word, then 2 payload bytes.
	ext := make([]byte, 12+4+4+4+2)
	copy(ext, base[:12])
	ext[0] = 0x80 | 0x10 | 0x01 // V=2, X=1, CC=1
	ext[1] = 0x80 | PayloadPCMA
	binary.BigEndian.PutUint16(ext[12+4+2:], 1)
	ext[len(ext)-2], ext[len(ext)-1] = 0xAA, 0xBB

	h, p := parseRTP(ext)
	if h == nil {
		t.Fatal("extension packet rejected")
	}

	if !h.Extension || h.CSRCCount != 1 || !h.Marker || h.PayloadType != PayloadPCMA ||
		h.SequenceNumber != 7 || h.Timestamp != 1000 || h.SSRC != 0xABCD || h.Version != 2 {
		t.Errorf("header = %+v", *h)
	}

	if len(p) != 2 || p[0] != 0xAA || p[1] != 0xBB {
		t.Errorf("payload = % x, want aa bb", p)
	}

	// Extension length that swallows the whole packet.
	binary.BigEndian.PutUint16(ext[12+4+2:], 50)

	if h, _ := parseRTP(ext); h != nil {
		t.Error("oversized extension parsed")
	}

	// Extension bit set without room for the extension header is ignored.
	noRoom := append([]byte(nil), base[:12]...)
	noRoom[0] |= 0x10
	noRoom = append(noRoom, 0x01, 0x02)

	h, p = parseRTP(noRoom)
	if h == nil || len(p) != 2 {
		t.Errorf("truncated-extension packet = %v, % x", h, p)
	}
}

func TestDecodeIntoClampsToOutput(t *testing.T) {
	data := []byte{0x00, 0x7F, 0x80, 0xFF, 0x55}
	out := make([]int16, 3)

	if n := decodePCMUInto(out, data); n != 3 {
		t.Fatalf("decodePCMUInto n = %d, want 3", n)
	}

	for i := range 3 {
		if out[i] != pcmuTable[data[i]] {
			t.Errorf("PCMU out[%d] = %d, want %d", i, out[i], pcmuTable[data[i]])
		}
	}

	if n := decodePCMAInto(out, data); n != 3 {
		t.Fatalf("decodePCMAInto n = %d, want 3", n)
	}

	for i := range 3 {
		if out[i] != pcmaTable[data[i]] {
			t.Errorf("PCMA out[%d] = %d, want %d", i, out[i], pcmaTable[data[i]])
		}
	}

	big := make([]int16, 8)
	if n := decodePCMAInto(big, data); n != len(data) {
		t.Errorf("decodePCMAInto n = %d, want %d", n, len(data))
	}

	if pcmuTable[0x7F] != -pcmuTable[0xFF] || pcmaTable[0x55] != -pcmaTable[0xD5] {
		t.Error("G.711 tables are not sign-symmetric")
	}
}

func TestPortPoolDemoteAndAvailable(t *testing.T) {
	pool := NewPortPool(48000, 48004) // 48000, 48002, 48004

	if got := pool.Available(); got != 3 {
		t.Fatalf("Available = %d, want 3", got)
	}

	p1, _ := pool.Acquire()
	if p1 != 48000 {
		t.Fatalf("first Acquire = %d, want 48000", p1)
	}

	pool.Demote(p1)
	pool.Demote(p1)    // no longer in use: ignored
	pool.Demote(12345) // never in pool: ignored
	pool.Release(p1)   // not in use: ignored

	if got := pool.Available(); got != 3 {
		t.Fatalf("Available after demote = %d, want 3", got)
	}

	order := make([]int, 0, 3)

	for range 3 {
		p, err := pool.Acquire()
		if err != nil {
			t.Fatal(err)
		}

		order = append(order, p)
	}

	want := []int{48002, 48004, 48000}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("acquire order = %v, want %v", order, want)
		}
	}

	if got := pool.Available(); got != 0 {
		t.Errorf("Available = %d, want 0", got)
	}
}

func TestAcquireBoundRetriesThenSucceeds(t *testing.T) {
	pool := NewPortPool(49000, 49010) // 6 ports

	var tried []int

	port, v, err := AcquireBound(pool, func(p int) (string, error) {
		tried = append(tried, p)
		if p < 49004 {
			return "", errors.New("squatted")
		}

		return "bound", nil
	})
	if err != nil || port != 49004 || v != "bound" {
		t.Fatalf("AcquireBound = %d, %q, %v", port, v, err)
	}

	if len(tried) != 3 {
		t.Errorf("tried %v, want 3 attempts", tried)
	}

	// The failed ports were demoted to the back, behind the untried ones.
	next, _ := pool.Acquire()
	if next != 49006 {
		t.Errorf("next Acquire = %d, want 49006", next)
	}
}

func TestAcquireBoundExhaustsAttempts(t *testing.T) {
	pool := NewPortPool(50000, 50000+2*(MaxBindAttempts+2))
	bindErr := errors.New("address in use")

	calls := 0

	port, v, err := AcquireBound(pool, func(int) (*int, error) {
		calls++

		return nil, bindErr
	})
	if !errors.Is(err, ErrNoBindablePort) || !errors.Is(err, bindErr) {
		t.Fatalf("err = %v, want ErrNoBindablePort wrapping bind error", err)
	}

	if port != 0 || v != nil || calls != MaxBindAttempts {
		t.Errorf("port=%d v=%v calls=%d, want 0 nil %d", port, v, calls, MaxBindAttempts)
	}

	if got := pool.Available(); got != MaxBindAttempts+3 {
		t.Errorf("Available = %d, want all %d ports back", got, MaxBindAttempts+3)
	}
}

func TestAcquireBoundEmptyPool(t *testing.T) {
	pool := NewPortPool(51000, 51000)
	if _, err := pool.Acquire(); err != nil {
		t.Fatal(err)
	}

	called := false

	_, _, err := AcquireBound(pool, func(int) (int, error) {
		called = true

		return 1, nil
	})
	if err == nil || errors.Is(err, ErrNoBindablePort) || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("err = %v, want pool exhausted error", err)
	}

	if called {
		t.Error("bind called on empty pool")
	}
}
