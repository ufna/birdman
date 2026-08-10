package metrics_test

import (
	"testing"

	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/testdb"
)

// birdman_server_info — the reference series that gives an owner to the
// per-server metrics the AGENT emits (tracker #958).
//
// The hole it fills: birdman_server_tick_ms{server_id} comes from the agent,
// and the agent does not know which project a dedik belongs to — StartServer
// never carried it. TickDegraded, an alert that is project-scoped BY NATURE,
// could therefore not be narrowed at all, and the alerts screen showed the tick
// degradation of project B to the operator of project A.
//
// The master already owns server_id → project in Postgres, so it exports the
// mapping and the rule joins on server_id (`group_left(project)`) — the whole
// fleet gains the label on the next scrape, with no agent, protocol or rollout
// change. The rule side is pinned by promtool cases in
// infra/roles/birdman_monitoring_dev/tests/rules_test.yml.

// TestServerInfoCarriesProjectAndEnv is the join key itself: one live dedik,
// one series, value 1, labels naming its owner. The value matters — the rule
// multiplies the tick sample by it, so anything other than 1 would corrupt the
// alert's $value.
func TestServerInfoCarriesProjectAndEnv(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8) // project "game", dev node, version 1.0.0 (dev)

	id := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)

	m := metrics.New(st, testLog())

	labels := map[string]string{"server_id": id, "project": "game", "env": "dev"}
	if n := countSeries(t, m.Registry, "birdman_server_info", labels); n != 1 {
		t.Fatalf("birdman_server_info series count = %d, want exactly 1 (a duplicate breaks the many-to-one join)", n)
	}
	if got := findGauge(t, m.Registry, "birdman_server_info", labels); got != 1 {
		t.Fatalf("birdman_server_info = %v, want 1 — the rule multiplies the tick sample by it", got)
	}
}

// TestServerInfoCoversEveryLiveState: the tick series exists for as long as the
// container runs, so every non-terminal state needs an owner. Missing one would
// silently drop the project label back off the alert for dediks in that state.
func TestServerInfoCoversEveryLiveState(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)

	ids := map[string]string{}
	for i, state := range []string{"creating", "ready", "allocated", "draining"} {
		ids[state] = f.InsertServer(t, f.NodeID, f.VersionID, state, int32(20010+i), 0)
	}

	m := metrics.New(st, testLog())

	for state, id := range ids {
		if !gaugeSeriesPresent(t, m.Registry, "birdman_server_info", map[string]string{
			"server_id": id, "project": "game", "env": "dev",
		}) {
			t.Errorf("state %q: no birdman_server_info — a live dedik without an owner loses the project label", state)
		}
	}
}

// TestServerInfoSkipsTerminalServers: failed/reaped mean the container is gone,
// so there is no agent series left to join against. Keeping them would let the
// series grow with the lifetime of the database instead of the size of the
// fleet — this is the cardinality bound of the whole approach.
func TestServerInfoSkipsTerminalServers(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)
	ctx := t.Context()

	dead := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20021, 0)
	f.MarkFailed(t, dead, 0)
	reaped := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20022, 0)
	if _, err := st.Pool.Exec(ctx,
		`update servers set state = 'reaped' where id = $1::uuid`, reaped); err != nil {
		t.Fatalf("reap server: %v", err)
	}

	m := metrics.New(st, testLog())

	for name, id := range map[string]string{"failed": dead, "reaped": reaped} {
		if gaugeSeriesPresent(t, m.Registry, "birdman_server_info", map[string]string{
			"server_id": id, "project": "game", "env": "dev",
		}) {
			t.Errorf("%s server still exported an info series — the series must shrink with the fleet", name)
		}
	}
}

// TestServerInfoNotFabricatedOnQueryFailure: the #960 discipline — a broken
// source emits NOTHING rather than an invented answer. Here the missing answer
// is safe by construction: TickDegraded is written as a non-hiding join, so an
// unmatched sample still fires, just platform-scoped (pinned by the promtool
// case "no birdman_server_info — TickDegraded still fires"). Fabricating an
// owner instead would file a real degradation under the wrong project.
func TestServerInfoNotFabricatedOnQueryFailure(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8)
	ctx := t.Context()

	id := f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20031, 0)
	// Break the read the way a real outage would: the query errors out.
	if _, err := st.Pool.Exec(ctx, `alter table servers rename to servers_gone`); err != nil {
		t.Fatalf("rename servers: %v", err)
	}

	m := metrics.New(st, testLog())

	if countSeries(t, m.Registry, "birdman_server_info", map[string]string{"server_id": id}) != 0 {
		t.Fatal("a failed servers query must not manufacture an owner for a dedik")
	}
	// The rest of /metrics must survive: the collector logs-and-continues like
	// its neighbours, so a scrape is never blanked by this one query.
	if !gaugeSeriesPresent(t, m.Registry, "birdman_node_heartbeat_age_seconds", map[string]string{
		"node": "node-1", "region": "eu",
	}) {
		t.Fatal("a failed server-info query blanked the rest of the scrape")
	}
}
