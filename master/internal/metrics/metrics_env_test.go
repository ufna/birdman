package metrics_test

import (
	"testing"

	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/testdb"
)

// TestServerEnvProductionLabels: birdman_servers carries
// {project, env, production, state, region, version} and birdman_versions
// carries {project, env, state} (environments v1 §7). env comes from
// servers.env; production is a join to environments by (project_id, env) — and
// is derived from the SERVER's env, NEVER from the node (I6): moving a node
// between environments must not rewrite the production dimension of a server it
// hosts. A dev server (production="false") and a prod server (production="true")
// of one project are therefore distinct series. Reuses the package harness
// (TestMain, findGauge in metrics_test.go).
func TestServerEnvProductionLabels(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 8) // project "game": dev node + registered version 1.0.0
	ctx := t.Context()

	// A dev server on the dev node/version (env = node env = dev).
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)

	// A prod environment: move a fresh node to prod, register a prod version,
	// put a ready server on it (env = prod).
	prodNode := f.AddNode(t, "node-prod", "203.0.113.30", 8)
	if _, err := st.SetNodeEnv(ctx, prodNode, "prod"); err != nil {
		t.Fatalf("move node to prod: %v", err)
	}
	prodV := f.AddVersion(t, "2.0.0", "prod")
	f.InsertServer(t, prodNode, prodV, "ready", 20002, 0)

	// I6: a prod-env server placed on the DEV node must STILL report
	// production="true" — production joins environments by (project_id,
	// servers.env), not by the node's env. A distinct state keeps it its own
	// series. Were production derived from the node (dev), this would wrongly
	// read production="false".
	if _, err := st.Pool.Exec(ctx, `
		insert into servers (project_id, node_id, version_id, state, port, env)
		select n.project_id, n.id, $2::uuid, 'allocated', 20003, 'prod'
		from nodes n where n.id = $1::uuid`, f.NodeID, prodV); err != nil {
		t.Fatalf("insert cross-env server: %v", err)
	}

	m := metrics.New(st, testLog())

	// dev server → production="false".
	if got := findGauge(t, m.Registry, "birdman_servers", map[string]string{
		"project": "game", "env": "dev", "production": "false",
		"state": "ready", "region": "eu", "version": "1.0.0",
	}); got != 1 {
		t.Fatalf("dev server series = %v, want 1 (production must be false)", got)
	}
	// prod server → production="true".
	if got := findGauge(t, m.Registry, "birdman_servers", map[string]string{
		"project": "game", "env": "prod", "production": "true",
		"state": "ready", "region": "eu", "version": "2.0.0",
	}); got != 1 {
		t.Fatalf("prod server series = %v, want 1 (production must be true)", got)
	}
	// I6 cross-env server on the dev node → production="true" from its own env.
	if got := findGauge(t, m.Registry, "birdman_servers", map[string]string{
		"project": "game", "env": "prod", "production": "true",
		"state": "allocated", "region": "eu", "version": "2.0.0",
	}); got != 1 {
		t.Fatalf("cross-env server = %v, want 1 (production from s.env, never the node — I6)", got)
	}

	// birdman_versions gains env: dev 1.0.0 and prod 2.0.0 (both registered) are
	// distinct series.
	if got := findGauge(t, m.Registry, "birdman_versions", map[string]string{
		"project": "game", "env": "dev", "state": "registered",
	}); got != 1 {
		t.Fatalf("versions{env=dev,state=registered} = %v, want 1", got)
	}
	if got := findGauge(t, m.Registry, "birdman_versions", map[string]string{
		"project": "game", "env": "prod", "state": "registered",
	}); got != 1 {
		t.Fatalf("versions{env=prod,state=registered} = %v, want 1", got)
	}
}
