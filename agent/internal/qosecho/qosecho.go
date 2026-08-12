// Package qosecho implements the UDP echo responder for client QoS probes
// (docs/specs/agent.md §8): every datagram is answered back to the sender,
// capped at 64 bytes. Clients measure rtt to regions against the node list
// from GET /v1/qos.
//
// The echo is a resource of the HOST, not of a node (tracker #1065): the
// responder is address-less — it carries neither node nor project identity —
// and what it measures is the network path to the BOX, the same for every node
// standing on it. On a box carrying several birdman nodes the port is
// therefore held by exactly ONE agent at a time. Who exactly holds it is NOT
// decided in advance (tracker #1068): every agent of the box contends for the
// port and keeps re-contending, so the holder is simply whoever is up. An
// owner fixed by config would take the box's ping target into the grave with
// itself — dark for the NEIGHBOUR project's nodes too, which GET /v1/qos keeps
// advertising (one target per (region, ip)) while their own agents are alive.
package qosecho

import (
	"context"
	"errors"
	"net"
	"time"
)

// MaxEcho caps the echoed payload (agent.md §8: ≤64 bytes).
const MaxEcho = 64

// DefaultRetry is how often an agent that lost the race re-attempts the bind.
// It bounds the window in which a box has no responder after its holder died:
// an order of magnitude below the 30s heartbeat freshness that keeps the box
// in GET /v1/qos at all (docs/specs/ops.md §1), so the takeover finishes long
// before the target could be handed out with nobody answering on it.
const DefaultRetry = 5 * time.Second

// Serve holds the host's QoS echo on addr for as long as this process can: it
// answers datagrams while it owns the port and keeps re-contending for it
// (every DefaultRetry) while another agent of the same box owns it. It returns
// only when ctx is done — losing the race is a normal state here, not an error.
func Serve(ctx context.Context, addr string, logf func(string, ...any)) error {
	return ServeWithRetry(ctx, addr, DefaultRetry, logf)
}

// ServeWithRetry is Serve with an explicit contention interval (tests).
func ServeWithRetry(ctx context.Context, addr string, retry time.Duration, logf func(string, ...any)) error {
	// Two things are worth saying out loud and neither is worth repeating every
	// tick: "I took the port" and "somebody else has it". A busy port is the
	// steady state of every non-holding agent of the box, so a line per attempt
	// would be a stream reporting that nothing changed.
	busyAnnounced := false
	for {
		err := serveOnce(ctx, addr, retry, logf, &busyAnnounced)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			// The socket itself died (not a lost bind): say so once, then fall
			// back into contention like any other agent of the box.
			logf("[qos] echo on %s stopped: %v — re-contending", addr, err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retry):
		}
	}
}

// serveOnce tries to take the port and, having taken it, answers until the
// socket dies or ctx is done. A bind failure is not an error: it means another
// agent of this box currently owns the echo.
func serveOnce(ctx context.Context, addr string, retry time.Duration, logf func(string, ...any), busyAnnounced *bool) error {
	var lc net.ListenConfig
	pc, err := lc.ListenPacket(ctx, "udp", addr)
	if err != nil {
		if !*busyAnnounced && ctx.Err() == nil {
			logf("[qos] %s is held by another agent of this box (%v) — re-contending every %s",
				addr, err, retry)
			*busyAnnounced = true
		}
		return nil
	}
	*busyAnnounced = false
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		pc.Close()
	}()
	logf("[qos] UDP echo on %s", pc.LocalAddr())

	buf := make([]byte, 2048)
	for {
		n, raddr, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if n > MaxEcho {
			n = MaxEcho
		}
		if _, err := pc.WriteTo(buf[:n], raddr); err != nil && ctx.Err() == nil {
			logf("[qos] echo to %s: %v", raddr, err)
		}
	}
}
