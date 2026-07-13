// Package reconcile implements the fleet controller loop
// (docs/specs/master.md §2) and the node lease checker
// (docs/specs/protocol.md §1 Lease).
package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/ufna/birdman/master/internal/store"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// Sender dispatches a command to a node's agent (implemented by
// agentlink.Hub; an in-memory fake is used in tests). It must not block:
// commands to offline nodes are queued and replayed on reconnect.
type Sender interface {
	Send(nodeID string, msg *agentlinkv1.MasterMsg) (cmdID string)
}

const (
	// Servers stuck in `creating` without agent progress for this long are
	// failed so the deficit is re-created (уточнено в v0, master.md §2).
	stuckCreatingTimeout = 120 * time.Second

	// Crash-loop policy (master.md §2): ≥3 failed of one (version,node)
	// within 10 min → pause creations of that pair for 15 min.
	crashLoopFails  = 3
	crashLoopWindow = 10 * time.Minute
	crashLoopPause  = 15 * time.Minute

	stopGraceSeconds = 30
)

type Reconciler struct {
	st     *store.Store
	sender Sender
	log    *slog.Logger

	// (version,node) pairs already reported as crash-looping → pausedUntil,
	// to avoid re-emitting the crash_loop event every tick.
	reported map[pairKey]time.Time
}

type pairKey struct{ versionID, nodeID string }

func New(st *store.Store, sender Sender, log *slog.Logger) *Reconciler {
	return &Reconciler{st: st, sender: sender, log: log, reported: map[pairKey]time.Time{}}
}

// Run ticks RunOnce every interval until ctx is done.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
				r.log.Error("reconcile: pass failed", "err", err)
			}
		}
	}
}

// RunOnce performs a single reconcile pass over all fleets.
func (r *Reconciler) RunOnce(ctx context.Context) error {
	// Close expired multi-version windows first (итерация 3, master.md §5):
	// deprecated versions past reap_ttl_min go disabled, so this very pass
	// reaps their buffers and drains their live matches below.
	if disabled, err := r.st.DisableExpiredDeprecated(ctx); err != nil {
		return err
	} else {
		for _, v := range disabled {
			r.log.Info("reconcile: deprecated version disabled by reap_ttl",
				"version_id", v.ID, "semver", v.Semver)
		}
	}

	if n, err := r.st.FailStuckCreating(ctx, stuckCreatingTimeout); err != nil {
		return err
	} else if n > 0 {
		r.log.Warn("reconcile: failed stuck creating servers", "count", n)
	}

	fleets, err := r.st.ListFleetConfigs(ctx)
	if err != nil {
		return err
	}
	for _, f := range fleets {
		if f.ActiveVersion == nil {
			continue
		}
		if err := r.reconcileFleet(ctx, f); err != nil {
			r.log.Error("reconcile: fleet pass failed",
				"project", f.Project, "env", f.Env, "region", f.Region, "err", err)
		}
	}
	return nil
}

func (r *Reconciler) reconcileFleet(ctx context.Context, f store.FleetConfig) error {
	dep, err := r.st.DeprecatedWindowVersion(ctx, f.ProjectID, f.Env)
	if err != nil {
		return err
	}
	paused := map[string][]string{}
	for _, vid := range windowVersionIDs(f, dep) {
		p, err := r.pausedNodes(ctx, vid, f)
		if err != nil {
			return err
		}
		paused[vid] = p
	}
	starts, stops, drains, locked, err := r.st.PlanFleet(ctx, f, dep, paused)
	if err != nil {
		return err
	}
	if !locked {
		return nil // another master holds the fleet lock
	}
	for _, p := range starts {
		cmdID := r.sender.Send(p.NodeID, &agentlinkv1.MasterMsg{
			Msg: &agentlinkv1.MasterMsg_Start{Start: &agentlinkv1.StartServer{
				ServerId: p.ServerID,
				ImageRef: p.ImageRef,
				// BIRDMAN_ENV — игровой сервер знает своё окружение (конфиги/
				// аналитика). Заполнение существующего map-поля, ноль диффов proto.
				Env:  map[string]string{"BIRDMAN_ENV": f.Env},
				Port: 0, // agent picks from its pool
			}},
		})
		r.log.Info("reconcile: start server",
			"server_id", p.ServerID, "node_id", p.NodeID, "image_ref", p.ImageRef, "cmd_id", cmdID)
	}
	for _, p := range stops {
		cmdID := r.sender.Send(p.NodeID, &agentlinkv1.MasterMsg{
			Msg: &agentlinkv1.MasterMsg_Stop{Stop: &agentlinkv1.StopServer{
				ServerId: p.ServerID,
				GraceS:   stopGraceSeconds,
			}},
		})
		r.log.Info("reconcile: stop server (surplus/stale)",
			"server_id", p.ServerID, "node_id", p.NodeID, "cmd_id", cmdID)
	}
	for _, p := range drains {
		cmdID := r.sender.Send(p.NodeID, &agentlinkv1.MasterMsg{
			Msg: &agentlinkv1.MasterMsg_DrainServer{DrainServer: &agentlinkv1.DrainServer{
				ServerId:  p.ServerID,
				DeadlineS: p.DeadlineS,
				Reason:    p.Reason,
			}},
		})
		r.log.Info("reconcile: drain server (live match plays out)",
			"server_id", p.ServerID, "node_id", p.NodeID, "reason", p.Reason, "cmd_id", cmdID)
	}
	return nil
}

func windowVersionIDs(f store.FleetConfig, dep *store.Version) []string {
	ids := []string{*f.ActiveVersion}
	if dep != nil && dep.ID != *f.ActiveVersion {
		ids = append(ids, dep.ID)
	}
	return ids
}

// pausedNodes derives crash-looping (version,node) pairs from recent
// failures. Restart-safe: input is the servers table, not process memory.
func (r *Reconciler) pausedNodes(ctx context.Context, versionID string, f store.FleetConfig) ([]string, error) {
	failures, err := r.st.RecentFailedTimes(ctx, versionID, f.Region, crashLoopWindow+crashLoopPause)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var paused []string
	for nodeID, times := range failures {
		until, looping := crashLoopPausedUntil(times, now)
		if !looping {
			continue
		}
		paused = append(paused, nodeID)
		key := pairKey{versionID, nodeID}
		if prev, ok := r.reported[key]; !ok || now.After(prev) {
			r.reported[key] = until
			nid, vid := nodeID, versionID
			if err := r.st.InsertEvent(ctx, store.EventCrashLoop,
				store.EventRef{NodeID: &nid, VersionID: &vid},
				map[string]any{"project": f.Project, "region": f.Region,
					"paused_until": until.UTC().Format(time.RFC3339)}); err != nil {
				return nil, err
			}
			r.log.Warn("reconcile: crash loop detected, pausing (version,node) pair",
				"node_id", nodeID, "version_id", versionID, "paused_until", until)
		}
	}
	return paused, nil
}

// crashLoopPausedUntil scans failure timestamps (ascending) for any
// crashLoopFails-sized run inside crashLoopWindow; the pause lasts
// crashLoopPause from the last failure of the newest such run.
func crashLoopPausedUntil(times []time.Time, now time.Time) (time.Time, bool) {
	var until time.Time
	for i := 0; i+crashLoopFails-1 < len(times); i++ {
		last := times[i+crashLoopFails-1]
		if last.Sub(times[i]) <= crashLoopWindow {
			if u := last.Add(crashLoopPause); u.After(until) {
				until = u
			}
		}
	}
	return until, until.After(now)
}
