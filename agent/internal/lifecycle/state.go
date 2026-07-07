// Package lifecycle implements the per-server state machine of the agent
// (docs/specs/agent.md §3): pulling → starting → ready → allocated →
// draining → stopped|failed, plus the readiness grace watchdog.
package lifecycle

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// State of a dedicated server as tracked by the agent.
type State string

const (
	StatePulling   State = "pulling"
	StateStarting  State = "starting"
	StateReady     State = "ready"
	StateAllocated State = "allocated"
	StateDraining  State = "draining"
	StateStopped   State = "stopped"
	StateFailed    State = "failed"
)

// ReadyGrace is how long a server may stay in `starting` before the agent
// declares it failed (spec §3: 30s from container start to liba `ready`).
const ReadyGrace = 30 * time.Second

// Terminal reports whether no further transitions are allowed from s.
func (s State) Terminal() bool { return s == StateStopped || s == StateFailed }

// next lists the legal transitions. `stopped`/`failed` are reachable from any
// non-terminal state (operator stop, container exit, errors).
var next = map[State]map[State]bool{
	StatePulling:   {StateStarting: true, StateStopped: true, StateFailed: true},
	StateStarting:  {StateReady: true, StateStopped: true, StateFailed: true},
	StateReady:     {StateAllocated: true, StateDraining: true, StateStopped: true, StateFailed: true},
	StateAllocated: {StateDraining: true, StateStopped: true, StateFailed: true},
	StateDraining:  {StateStopped: true, StateFailed: true},
	StateStopped:   {},
	StateFailed:    {},
}

// Transition is one recorded state change.
type Transition struct {
	ServerID string
	From     State
	To       State
	Reason   string
	At       time.Time
}

// Machine is the in-memory state machine for a single server.
type Machine struct {
	serverID string
	onChange func(Transition) // transition log hook; must not call back into Machine

	mu   sync.Mutex
	cur  State
	subs []chan Transition
}

// NewMachine creates a machine in StatePulling. onChange (may be nil) is
// invoked synchronously on every successful transition.
func NewMachine(serverID string, onChange func(Transition)) *Machine {
	return &Machine{serverID: serverID, onChange: onChange, cur: StatePulling}
}

// Current returns the current state.
func (m *Machine) Current() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur
}

// To performs a transition; illegal moves return an error and change nothing.
func (m *Machine) To(to State, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !next[m.cur][to] {
		return fmt.Errorf("invalid transition %s -> %s (server %s)", m.cur, to, m.serverID)
	}
	tr := Transition{ServerID: m.serverID, From: m.cur, To: to, Reason: reason, At: time.Now()}
	m.cur = to
	for _, ch := range m.subs {
		select {
		case ch <- tr:
		default: // slow subscriber: drop; watchers re-check Current()
		}
	}
	if m.onChange != nil {
		m.onChange(tr)
	}
	return nil
}

// Subscribe returns a channel receiving future transitions. The channel is
// buffered; a slow consumer may miss events and should re-check Current().
// Subscriptions live for the lifetime of the machine (one server run).
func (m *Machine) Subscribe() <-chan Transition {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := make(chan Transition, 16)
	m.subs = append(m.subs, ch)
	return ch
}

// WatchReadyGrace calls onExpire if m is still in StateStarting after grace.
// It returns as soon as the machine leaves `starting`, the grace expires, or
// ctx is done. Run it in its own goroutine right after the transition to
// `starting`; the caller decides what "expire" means (fail + stop).
func WatchReadyGrace(ctx context.Context, m *Machine, grace time.Duration, onExpire func()) {
	if m.Current() != StateStarting {
		return
	}
	sub := m.Subscribe()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case tr := <-sub:
			if tr.To != StateStarting {
				return
			}
		case <-timer.C:
			if m.Current() == StateStarting {
				onExpire()
			}
			return
		}
	}
}
