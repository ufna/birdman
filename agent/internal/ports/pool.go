// Package ports manages the host port pool of a node: one port per dedicated
// server (used for both TCP and UDP), handed out from the configured range
// (docs/specs/agent.md §3).
package ports

import (
	"errors"
	"fmt"
	"sync"
)

// ErrExhausted is returned by Acquire when every port in the range is taken.
var ErrExhausted = errors.New("port pool exhausted")

// Pool is an in-memory pool of host ports [from..to], safe for concurrent use.
//
// v0 is pure bookkeeping: the pool does not probe the OS. A foreign process
// squatting on a pool port surfaces as the game failing to bind, which the
// readiness grace turns into `failed`.
type Pool struct {
	mu    sync.Mutex
	from  int
	to    int
	inUse map[int]bool
}

// New creates a pool over the inclusive range [from, to].
func New(from, to int) (*Pool, error) {
	if from <= 0 || to > 65535 || from > to {
		return nil, fmt.Errorf("invalid port range [%d, %d]", from, to)
	}
	return &Pool{from: from, to: to, inUse: make(map[int]bool)}, nil
}

// Acquire hands out the lowest free port in the range.
func (p *Pool) Acquire() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for port := p.from; port <= p.to; port++ {
		if !p.inUse[port] {
			p.inUse[port] = true
			return port, nil
		}
	}
	return 0, ErrExhausted
}

// AcquireSpecific claims an exact port (run-once --port). The port must lie
// inside the pool range and be free.
func (p *Pool) AcquireSpecific(port int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if port < p.from || port > p.to {
		return fmt.Errorf("port %d outside pool range [%d, %d]", port, p.from, p.to)
	}
	if p.inUse[port] {
		return fmt.Errorf("port %d already in use", port)
	}
	p.inUse[port] = true
	return nil
}

// Release returns a previously acquired port to the pool.
func (p *Pool) Release(port int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if port < p.from || port > p.to {
		return fmt.Errorf("port %d outside pool range [%d, %d]", port, p.from, p.to)
	}
	if !p.inUse[port] {
		return fmt.Errorf("port %d is not acquired", port)
	}
	delete(p.inUse, port)
	return nil
}

// InUse reports how many ports are currently handed out.
func (p *Pool) InUse() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.inUse)
}
