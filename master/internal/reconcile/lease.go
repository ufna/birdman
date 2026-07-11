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
// buffer is re-created on live nodes; third step — a node quarantined for
// downAfter (node_down_after_min, итерация 5 follow-up) → down + node_down,
// so operators/alerts tell a blip from a long outage. down self-heals: a
// heartbeat of a live agent session flips the node back to active from
// quarantine or down (handled in store.ApplyHeartbeat). 'dead' — the manual
// revocation terminal — is never set here.
type LeaseChecker struct {
	st        *store.Store
	log       *slog.Logger
	downAfter time.Duration
}

func NewLeaseChecker(st *store.Store, log *slog.Logger, downAfter time.Duration) *LeaseChecker {
	return &LeaseChecker{st: st, log: log, downAfter: downAfter}
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
	if n, err := l.st.MarkDownNodes(ctx, l.downAfter); err != nil {
		return err
	} else if n > 0 {
		l.log.Warn("lease: nodes marked down (long silence)", "count", n)
	}
	return nil
}
