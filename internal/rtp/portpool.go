package rtp

import (
	"errors"
	"fmt"
	"log"
	"sync"
)

// MaxBindAttempts bounds how many pool ports one AcquireBound call tries
// when binds fail, so a squatted port costs a few extra syscalls rather than
// a scan of the pool.
const MaxBindAttempts = 8

// ErrNoBindablePort reports that every AcquireBound attempt failed to bind.
var ErrNoBindablePort = errors.New("rtp: no bindable port")

// PortPool manages a range of UDP ports for RTP listeners.
// One port is allocated per call.
type PortPool struct {
	inUse map[int]struct{}
	free  []int
	lo    int
	hi    int
	mu    sync.Mutex
}

// NewPortPool builds a pool of even-numbered UDP ports in [lo, hi]. Even
// ports follow the RTP convention (the corresponding odd port is reserved
// for RTCP, though we currently don't use it).
func NewPortPool(lo, hi int) *PortPool {
	p := &PortPool{
		lo:    lo,
		hi:    hi,
		inUse: make(map[int]struct{}),
		free:  make([]int, 0, (hi-lo)/2+1),
	}
	for port := lo; port <= hi; port += 2 {
		p.free = append(p.free, port)
	}

	// Publish initial pool size + capacity to Prometheus. The total is
	// constant for the process lifetime; available is updated on each
	// Acquire / Release.

	return p
}

// Acquire returns the oldest free port, or an error if the pool is
// exhausted. Ports are handed out first-in, first-out and Release
// appends to the back, so a just-released port is reused only after
// every other free port — a delay that keeps a previous call's late
// teardown RTP out of the next call's recording.
func (p *PortPool) Acquire() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.free) == 0 {
		return 0, fmt.Errorf("rtp port pool exhausted (%d-%d)", p.lo, p.hi)
	}

	port := p.free[0]
	p.free = p.free[1:]
	p.inUse[port] = struct{}{}

	return port, nil
}

// Release returns a port to the pool.
func (p *PortPool) Release(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.inUse[port]; !ok {
		return
	}

	delete(p.inUse, port)
	p.free = append(p.free, port)
}

// Demote returns an acquired port to the BACK of the free list, so
// every other free port is handed out before this one again. Used
// when a port is found to be held by another process. With FIFO
// acquisition, appending is the back.
func (p *PortPool) Demote(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.inUse[port]; !ok {
		return
	}

	delete(p.inUse, port)
	p.free = append(p.free, port)
}

// AcquireBound acquires a port and passes it to bind, which typically opens
// the port's socket. When bind fails, for example because another process
// already holds the port, the port is demoted to the back of the queue
// and the next port is tried, up to MaxBindAttempts. An empty
// pool is reported as Acquire reports it; running out of attempts wraps
// ErrNoBindablePort with the last bind error.
func AcquireBound[T any](p *PortPool, bind func(port int) (T, error)) (int, T, error) {
	var (
		zero    T
		lastErr error
	)

	for range MaxBindAttempts {
		port, err := p.Acquire()
		if err != nil {
			return 0, zero, err
		}

		v, err := bind(port)
		if err == nil {
			return port, v, nil
		}

		log.Printf("[rtp] port %d not bindable, trying another: %v", port, err)
		p.Demote(port)

		lastErr = err
	}

	return 0, zero, fmt.Errorf("%w after %d attempts: %w", ErrNoBindablePort, MaxBindAttempts, lastErr)
}

// Available returns the number of free ports.
func (p *PortPool) Available() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return len(p.free)
}
