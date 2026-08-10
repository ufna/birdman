package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Ready-buffer explicit zeros (tracker #960).
//
// BufferEmptyReadyProd/NonProd are `sum by (region, project)
// (birdman_servers{state="ready", …}) == 0`. A grouped SQL count emits NOTHING
// for a combination with no rows, and a Prometheus aggregation over a missing
// series yields an EMPTY result, not 0 — so the condition never held and both
// alerts were dead. Verified by running the rendered rules through
// `promtool test rules` (see infra/roles/birdman_monitoring_dev/tests):
// without an explicit zero the alert does not fire; with one it does.
//
// The fix emits an explicit 0 for every fleet_config that actually wants a warm
// buffer (buffer_ready > 0), which is what these tests pin down.

// TestReadyBufferZeroForConfiguredFleet is the exact case from the card: a
// prod fleet asks for a warm buffer, has servers in other states and ZERO
// ready ones. The ready series must exist and read 0 — otherwise `== 0` can
// never hold.
func TestReadyBufferZeroForConfiguredFleet(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8) // project "game", dev node, version 1.0.0 (dev)
	ctx := t.Context()

	prodV := f.AddVersion(t, "2.0.0", "prod")
	prodNode := f.AddNode(t, "node-prod", "203.0.113.30", 8)
	if _, err := st.SetNodeEnv(ctx, prodNode, "prod"); err != nil {
		t.Fatalf("move node to prod: %v", err)
	}
	// Servers exist — just none of them ready. Without the zero-fill the whole
	// ready dimension of this fleet is silently absent.
	f.InsertServer(t, prodNode, prodV, "allocated", 20002, 0)
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "prod", Region: "eu",
		ActiveVersion: &prodV, BufferReady: i32(2),
	}); err != nil {
		t.Fatalf("upsert prod fleet: %v", err)
	}

	m := metrics.New(st, testLog())

	if got := findGauge(t, m.Registry, "birdman_servers", map[string]string{
		"project": "game", "env": "prod", "production": "true",
		"state": "ready", "region": "eu", "version": "2.0.0",
	}); got != 0 {
		t.Fatalf("ready series = %v, want an explicit 0 (BufferEmptyReadyProd cannot fire without it)", got)
	}
}

// TestReadyBufferZeroSkipsBufferlessFleet: a fleet that deliberately keeps no
// warm buffer (buffer_ready = 0) must NOT get an explicit zero — an empty
// ready buffer is its normal state and paging on it would be noise. This is
// also what keeps the alert expr free of a buffer_ready join: the filter lives
// in the metric.
func TestReadyBufferZeroSkipsBufferlessFleet(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)
	ctx := t.Context()

	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "dev", Region: "eu",
		ActiveVersion: &f.VersionID, BufferReady: i32(0),
	}); err != nil {
		t.Fatalf("upsert bufferless fleet: %v", err)
	}

	m := metrics.New(st, testLog())

	if gaugeSeriesPresent(t, m.Registry, "birdman_servers", map[string]string{
		"project": "game", "env": "dev", "production": "false",
		"state": "ready", "region": "eu", "version": "1.0.0",
	}) {
		t.Fatal("bufferless fleet (buffer_ready=0) must not get an explicit ready zero")
	}
}

// TestReadyBufferZeroKeepsRealCount: a fleet WITH ready servers must report
// the real count exactly once. A zero emitted on top of a real row would be a
// duplicate labelset — Gather fails on those, which would blank the whole
// /metrics endpoint.
func TestReadyBufferZeroKeepsRealCount(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)
	ctx := t.Context()

	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "dev", Region: "eu",
		ActiveVersion: &f.VersionID, BufferReady: i32(2),
	}); err != nil {
		t.Fatalf("upsert dev fleet: %v", err)
	}

	m := metrics.New(st, testLog())

	labels := map[string]string{
		"project": "game", "env": "dev", "production": "false",
		"state": "ready", "region": "eu", "version": "1.0.0",
	}
	if n := countSeries(t, m.Registry, "birdman_servers", labels); n != 1 {
		t.Fatalf("ready series count = %d, want exactly 1 (a zero must never duplicate a real row)", n)
	}
	if got := findGauge(t, m.Registry, "birdman_servers", labels); got != 1 {
		t.Fatalf("ready series = %v, want the real count 1", got)
	}
}

// TestReadyBufferZeroWithoutActiveVersion: a fleet that wants a buffer but has
// nothing deployed yet is exactly the "next player gets no dedic" state, so it
// gets a zero too — with an empty version label (nothing is active).
func TestReadyBufferZeroWithoutActiveVersion(t *testing.T) {
	st := testdb.New(t)
	testdb.Seed(t, st, "eu", 8)
	ctx := t.Context()

	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "prod", Region: "eu", BufferReady: i32(3),
	}); err != nil {
		t.Fatalf("upsert version-less fleet: %v", err)
	}

	m := metrics.New(st, testLog())

	if got := findGauge(t, m.Registry, "birdman_servers", map[string]string{
		"project": "game", "env": "prod", "production": "true",
		"state": "ready", "region": "eu", "version": "",
	}); got != 0 {
		t.Fatalf("ready series = %v, want an explicit 0 for a fleet with no active version", got)
	}
}

// TestReadyBufferZeroNotFabricatedOnQueryFailure: the zeros are DERIVED from a
// successful server count. If the count itself fails, emitting zeros would
// turn a database hiccup into a false critical page ("no ready servers
// anywhere"), so nothing must be emitted at all.
func TestReadyBufferZeroNotFabricatedOnQueryFailure(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)
	ctx := t.Context()

	if _, err := st.UpsertFleet(ctx, store.UpsertFleetParams{
		Project: "game", Env: "dev", Region: "eu",
		ActiveVersion: &f.VersionID, BufferReady: i32(2),
	}); err != nil {
		t.Fatalf("upsert dev fleet: %v", err)
	}
	// Break the servers count the way a real outage would: the query errors out.
	if _, err := st.Pool.Exec(ctx, `alter table servers rename to servers_gone`); err != nil {
		t.Fatalf("rename servers: %v", err)
	}

	m := metrics.New(st, testLog())

	if gaugeSeriesPresent(t, m.Registry, "birdman_servers", map[string]string{
		"project": "game", "env": "dev", "production": "false",
		"state": "ready", "region": "eu", "version": "1.0.0",
	}) {
		t.Fatal("a failed servers query must not manufacture a ready zero (that is a false critical)")
	}
}

// TestEventsCounterZeroBaseline: birdman_events_total{kind} is DB-derived, so
// the series for a kind springs into existence at 1 the moment the first such
// event lands — and `increase(...[5m])` over a series that has only ever read
// 1 is 0, so CrashLoop/AgentUpgradeFailed missed the FIRST event of their kind
// entirely (verified with promtool, see the infra rules tests). An explicit 0
// baseline for the two alert-feeding kinds gives increase() something to rise
// from; it costs exactly two series.
func TestEventsCounterZeroBaseline(t *testing.T) {
	st := testdb.New(t)
	testdb.Seed(t, st, "eu", 8) // seeds node/version events, никаких crash_loop

	m := metrics.New(st, testLog())

	for _, kind := range []string{store.EventCrashLoop, store.EventAgentUpgradeFailed} {
		if got := findCounter(t, m.Registry, "birdman_events_total", map[string]string{"kind": kind}); got != 0 {
			t.Fatalf("events_total{kind=%q} = %v, want an explicit 0 baseline", kind, got)
		}
	}
}

func i32(v int32) *int32 { return &v }

// countSeries counts the series of a metric family matching exactly the given
// label set — the duplicate check a plain lookup cannot make.
func countSeries(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) int {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	n := 0
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, met := range mf.GetMetric() {
			if labelsMatch(met.GetLabel(), labels) {
				n++
			}
		}
	}
	return n
}
