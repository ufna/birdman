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
// buffer is re-created on live nodes. A returning heartbeat flips the node
// back to active (handled in store.ApplyHeartbeat).
type LeaseChecker struct {
	st  *store.Store
	log *slog.Logger
}

func NewLeaseChecker(st *store.Store, log *slog.Logger) *LeaseChecker {
	return &LeaseChecker{st: st, log: log}
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
	return nil
}
