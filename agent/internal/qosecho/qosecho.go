// Package qosecho implements the UDP echo responder for client QoS probes
// (docs/specs/agent.md §8): every datagram is answered back to the sender,
// capped at 64 bytes. Clients measure rtt to regions against the node list
// from GET /v1/qos.
package qosecho

import (
	"context"
	"errors"
	"net"
)

// MaxEcho caps the echoed payload (agent.md §8: ≤64 bytes).
const MaxEcho = 64

// Serve answers UDP datagrams on addr until ctx is done. The read buffer is
// larger than MaxEcho so oversized probes are consumed (and truncated) rather
// than left in the socket.
func Serve(ctx context.Context, addr string, logf func(string, ...any)) error {
	var lc net.ListenConfig
	pc, err := lc.ListenPacket(ctx, "udp", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
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
