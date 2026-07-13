package testdb

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/store"
)

// Fixture is a seeded project + node + version for integration tests.
type Fixture struct {
	St        *store.Store
	Project   string
	Region    string
	Env       string // окружение фикстуры (environments v1): всё живёт в dev
	NodeID    string
	NodeToken string
	VersionID string
}

// Seed creates project "game", one active node with a fresh heartbeat and a
// registered version 1.0.0 in the seeded dev environment (environments v1:
// ensureProject seeds dev+prod, the node/version enter as dev).
func Seed(t *testing.T, st *store.Store, region string, capacity int32) *Fixture {
	t.Helper()
	ctx := context.Background()
	node, token, err := st.CreateNode(ctx, store.CreateNodeParams{
		Project:       "game",
		Region:        region,
		Hostname:      "node-1",
		PublicIP:      "203.0.113.10",
		CapacitySlots: capacity,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	v, err := st.CreateVersion(ctx, store.CreateVersionParams{
		Project:  "game",
		Semver:   "1.0.0",
		ImageRef: "ghcr.io/example/game-server:1.0.0",
		Env:      "dev",
	})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	f := &Fixture{St: st, Project: "game", Region: region, Env: "dev", NodeID: node.ID, NodeToken: token, VersionID: v.ID}
	f.SetHeartbeatAge(t, node.ID, 0)
	return f
}

// AddNode registers one more active node with a fresh heartbeat.
func (f *Fixture) AddNode(t *testing.T, hostname, ip string, capacity int32) string {
	t.Helper()
	node, _, err := f.St.CreateNode(context.Background(), store.CreateNodeParams{
		Project:       f.Project,
		Region:        f.Region,
		Hostname:      hostname,
		PublicIP:      ip,
		CapacitySlots: capacity,
	})
	if err != nil {
		t.Fatalf("add node: %v", err)
	}
	f.SetHeartbeatAge(t, node.ID, 0)
	return node.ID
}

// AddVersion registers another version for the project in the given env
// (environments v1: pass "dev" to preserve v0 single-env behaviour).
func (f *Fixture) AddVersion(t *testing.T, semver, env string) string {
	t.Helper()
	v, err := f.St.CreateVersion(context.Background(), store.CreateVersionParams{
		Project:  f.Project,
		Semver:   semver,
		ImageRef: "ghcr.io/example/game-server:" + semver,
		Env:      env,
	})
	if err != nil {
		t.Fatalf("add version: %v", err)
	}
	return v.ID
}

// SetHeartbeatAge pins nodes.last_heartbeat_at to now()-age.
func (f *Fixture) SetHeartbeatAge(t *testing.T, nodeID string, age time.Duration) {
	t.Helper()
	_, err := f.St.Pool.Exec(context.Background(),
		`update nodes set last_heartbeat_at = now() - $2::interval where id = $1::uuid`,
		nodeID, fmt.Sprintf("%d milliseconds", age.Milliseconds()))
	if err != nil {
		t.Fatalf("set heartbeat age: %v", err)
	}
}

// InsertServer inserts a server row directly (bypassing reconcile).
func (f *Fixture) InsertServer(t *testing.T, nodeID, versionID, state string, port int32, age time.Duration) string {
	t.Helper()
	var id string
	err := f.St.Pool.QueryRow(context.Background(), `
		insert into servers (project_id, node_id, version_id, state, port, created_at, updated_at)
		select n.project_id, n.id, $2::uuid, $3, $4, now() - $5::interval, now() - $5::interval
		from nodes n where n.id = $1::uuid
		returning id::text`,
		nodeID, versionID, state, port, fmt.Sprintf("%d milliseconds", age.Milliseconds())).Scan(&id)
	if err != nil {
		t.Fatalf("insert server: %v", err)
	}
	return id
}

// MarkFailed flips a server to failed with updated_at = now()-age, writing
// the server_failed event the way the real failure paths do (reason
// agent_report) — crash-loop detection feeds on these events.
func (f *Fixture) MarkFailed(t *testing.T, serverID string, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	age_ := fmt.Sprintf("%d milliseconds", age.Milliseconds())
	_, err := f.St.Pool.Exec(ctx,
		`update servers set state = 'failed', updated_at = now() - $2::interval where id = $1::uuid`,
		serverID, age_)
	if err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	_, err = f.St.Pool.Exec(ctx, `
		insert into events (ts, kind, server_id, node_id, version_id, payload)
		select now() - $2::interval, 'server_failed', s.id, s.node_id, s.version_id,
		       '{"reason": "agent_report"}'::jsonb
		from servers s where s.id = $1::uuid`,
		serverID, age_)
	if err != nil {
		t.Fatalf("mark failed event: %v", err)
	}
}

// UpsertFleet sets the fleet config for the fixture project/region.
func (f *Fixture) UpsertFleet(t *testing.T, buffer, maxServers int32) {
	t.Helper()
	_, err := f.St.UpsertFleet(context.Background(), store.UpsertFleetParams{
		Project:       f.Project,
		Region:        f.Region,
		ActiveVersion: &f.VersionID,
		BufferReady:   &buffer,
		MaxServers:    &maxServers,
	})
	if err != nil {
		t.Fatalf("upsert fleet: %v", err)
	}
}

// ServerStates returns state → count for the fixture project.
func (f *Fixture) ServerStates(t *testing.T) map[string]int {
	t.Helper()
	rows, err := f.St.Pool.Query(context.Background(), `
		select s.state, count(*)::int from servers s
		join projects p on p.id = s.project_id
		where p.slug = $1 group by 1`, f.Project)
	if err != nil {
		t.Fatalf("server states: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[state] = n
	}
	return out
}
