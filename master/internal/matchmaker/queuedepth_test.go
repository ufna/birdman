package matchmaker_test

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/testdb"
)

// TestQueueDepthByEnv: birdman_mm_queue_depth is keyed by {region, env}
// (environments v1 §7). Tickets group per (project, env) in the tick, so a dev
// ticket and a prod ticket resting in the same region are DISTINCT series — the
// depth of one env never masks the other. Cancelling both and re-ticking drives
// each (region, env) series to an explicit 0 (the regionsSeen zeroing is keyed
// by (region, env) too).
func TestQueueDepthByEnv(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10) // project "game": dev+prod seeded, no fleet → tickets rest queued
	_ = f
	ctx := context.Background()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)

	// One dev and one prod ticket, both best-region eu. No fleet is set up, so
	// there are no candidates and both tickets stay queued after the tick.
	tDev, err := mm.Submit(ctx, matchmaker.SubmitParams{
		Env: "dev", PlayerID: "d1", ClientVersion: "1.0.0", Regions: regions("eu", 10),
	})
	if err != nil {
		t.Fatalf("dev submit: %v", err)
	}
	tProd, err := mm.Submit(ctx, matchmaker.SubmitParams{
		Env: "prod", PlayerID: "p1", ClientVersion: "1.0.0", Regions: regions("eu", 10),
	})
	if err != nil {
		t.Fatalf("prod submit: %v", err)
	}
	runOnce(t, mm)

	if got, ok := queueDepth(t, m.Registry, "eu", "dev"); !ok || got != 1 {
		t.Fatalf("queue_depth{region=eu,env=dev} = %v (present=%v), want 1", got, ok)
	}
	if got, ok := queueDepth(t, m.Registry, "eu", "prod"); !ok || got != 1 {
		t.Fatalf("queue_depth{region=eu,env=prod} = %v (present=%v), want 1", got, ok)
	}

	// Drain both queues; the next tick emits an explicit 0 for each seen
	// (region, env) — proving the zeroing is per env, not per bare region.
	mm.Cancel(tDev.ID)
	mm.Cancel(tProd.ID)
	runOnce(t, mm)
	if got, ok := queueDepth(t, m.Registry, "eu", "dev"); !ok || got != 0 {
		t.Fatalf("queue_depth{region=eu,env=dev} after drain = %v (present=%v), want an explicit 0", got, ok)
	}
	if got, ok := queueDepth(t, m.Registry, "eu", "prod"); !ok || got != 0 {
		t.Fatalf("queue_depth{region=eu,env=prod} after drain = %v (present=%v), want an explicit 0", got, ok)
	}
}

// queueDepth reads birdman_mm_queue_depth{region,env} from reg, reporting the
// value and whether the series is present.
func queueDepth(t *testing.T, reg *prometheus.Registry, region, env string) (float64, bool) {
	t.Helper()
	want := map[string]string{"region": region, "env": env}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "birdman_mm_queue_depth" {
			continue
		}
		for _, met := range mf.GetMetric() {
			labels := met.GetLabel()
			if len(labels) != len(want) {
				continue
			}
			match := true
			for _, p := range labels {
				if want[p.GetName()] != p.GetValue() {
					match = false
					break
				}
			}
			if match {
				return met.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}
