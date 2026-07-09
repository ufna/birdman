package metrics_test

import (
	"log/slog"
	"os"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestNodeCapacityMetric: with 2 active nodes in region=eu (capacity_slots
// 8+8), birdman_node_capacity_slots{region="eu"} must report the summed
// capacity — the panel's utilization-over-time chart (Statistics v1) reads
// this per region on top of the existing point-in-time
// store.RegionUtilization snapshot. A third, non-active (draining) node must
// NOT count: the gauge is the denominator of true utilization, so it follows
// the same active-nodes-only rule as store.RegionUtilization's capacity CTE.
func TestNodeCapacityMetric(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)
	f.AddNode(t, "node-2", "203.0.113.11", 8)
	draining := f.AddNode(t, "node-3", "203.0.113.12", 100)
	if _, err := st.Pool.Exec(t.Context(),
		`update nodes set state = 'draining' where id = $1::uuid`, draining); err != nil {
		t.Fatalf("drain node-3: %v", err)
	}

	m := metrics.New(st, testLog())

	got := findGauge(t, m.Registry, "birdman_node_capacity_slots", map[string]string{"region": "eu"})
	if got != 16 {
		t.Fatalf("capacity eu = %v, want 16 (active nodes only; draining node-3's 100 slots must not count)", got)
	}
}

// findGauge gathers reg and returns the value of the metric matching name and
// exactly the given label set, failing the test if none matches.
func findGauge(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, met := range mf.GetMetric() {
			if labelsMatch(met.GetLabel(), labels) {
				return met.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("metric %s%v not found", name, labels)
	return 0
}

func labelsMatch(pairs []*dto.LabelPair, want map[string]string) bool {
	if len(pairs) != len(want) {
		return false
	}
	for _, p := range pairs {
		if want[p.GetName()] != p.GetValue() {
			return false
		}
	}
	return true
}
