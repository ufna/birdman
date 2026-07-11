package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/ufna/birdman/master/internal/store"
)

// LeaseChecker enforces node liveness (docs/specs/protocol.md §1 Lease):
// heartbeat older than 10s → quarantine (excluded from allocation and
// placement); 20s more of silence → the node's servers become failed and the
// buffer is re-created on live nodes; a node quarantined for deadAfter
// (node_dead_after_min, итерация 5 follow-up) → dead + node_down, so
// operators/alerts tell a blip from a death. A returning heartbeat flips the
// node back to active from either state (handled in store.ApplyHeartbeat).
type LeaseChecker struct {
	st        *store.Store
	log       *slog.Logger
	deadAfter time.Duration
}

func NewLeaseChecker(st *store.Store, log *slog.Logger, deadAfter time.Duration) *LeaseChecker {
	return &LeaseChecker{st: st, log: log, deadAfter: deadAfter}
}

func (l *LeaseChecker) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := l.RunOnce(ctx); err != nil && ctx.Err() == nil {
				l.log.Error("lease: pass failed", "err", err)
			}
		}
	}
}

func (l *LeaseChecker) RunOnce(ctx context.Context) error {
	if n, err := l.st.QuarantineStaleNodes(ctx); err != nil {
		return err
	} else if n > 0 {
		l.log.Warn("lease: nodes quarantined (heartbeat lost)", "count", n)
	}
	if n, err := l.st.FailQuarantinedServers(ctx); err != nil {
		return err
	} else if n > 0 {
		l.log.Warn("lease: servers failed on silent nodes", "count", n)
	}
	if n, err := l.st.MarkDeadNodes(ctx, l.deadAfter); err != nil {
		return err
	} else if n > 0 {
		l.log.Warn("lease: nodes marked dead (long silence)", "count", n)
	}
	return nil
}
