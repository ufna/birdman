package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// Environments v1 (docs/superpowers/specs/2026-07-13-environments-v1-design.md
// §1–3): seeds on project create, CRUD + guardrails, node-move, sole-env resolve.

// ensureProject seeds dev+prod for any brand-new project on first reference.
func TestEnvironmentsSeededOnProjectCreate(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	if _, _, err := st.CreateNode(ctx, store.CreateNodeParams{
		Project: "newgame", Region: "eu", Hostname: "n1", PublicIP: "203.0.113.1", CapacitySlots: 4,
	}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	envs, err := st.ListEnvironments(ctx, "newgame")
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 2 {
		t.Fatalf("want dev+prod seeded, got %d: %+v", len(envs), envs)
	}
	// Non-production first (panel convention): dev then prod.
	dev, prod := envs[0], envs[1]
	if dev.Name != "dev" || dev.Production || !dev.AutoDeploy || dev.RetentionKeep != 20 {
		t.Fatalf("dev seed wrong: %+v", dev)
	}
	if prod.Name != "prod" || !prod.Production || prod.AutoDeploy || prod.RetentionKeep != 0 {
		t.Fatalf("prod seed wrong: %+v", prod)
	}
	// The seeded node itself entered as dev (never prod implicitly).
	nodes, _ := st.ListNodes(ctx)
	if len(nodes) != 1 || nodes[0].Env != "dev" {
		t.Fatalf("new node must enter as dev: %+v", nodes)
	}
	if got, err := st.GetEnvironment(ctx, "newgame", "dev"); err != nil || got.Project != "newgame" {
		t.Fatalf("get dev: %+v %v", got, err)
	}
	if _, err := st.GetEnvironment(ctx, "newgame", "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get missing env: want ErrNotFound, got %v", err)
	}
}

func TestEnvironmentsCRUD(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	testdb.Seed(t, st, "eu", 10) // project game with dev+prod seeded

	e, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{
		Project: "game", Name: "staging", AutoDeploy: true, RetentionKeep: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "staging" || e.Production || !e.AutoDeploy || e.RetentionKeep != 5 || e.Project != "game" {
		t.Fatalf("created env wrong: %+v", e)
	}
	// Duplicate and re-creating a seeded env → ErrConflict.
	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{Project: "game", Name: "staging"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup env: want ErrConflict, got %v", err)
	}
	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{Project: "game", Name: "dev", AutoDeploy: true}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("recreate seeded dev: want ErrConflict, got %v", err)
	}

	no, keep := false, 9
	e2, err := st.PatchEnvironment(ctx, "game", "staging", store.EnvironmentPatch{AutoDeploy: &no, RetentionKeep: &keep})
	if err != nil {
		t.Fatal(err)
	}
	if e2.AutoDeploy || e2.RetentionKeep != 9 {
		t.Fatalf("patched env wrong: %+v", e2)
	}
	// order by production, name → dev, staging, prod.
	envs, _ := st.ListEnvironments(ctx, "game")
	if len(envs) != 3 || envs[0].Name != "dev" || envs[1].Name != "staging" || envs[2].Name != "prod" {
		t.Fatalf("list order wrong: %+v", envs)
	}
	if _, err := st.PatchEnvironment(ctx, "game", "nope", store.EnvironmentPatch{RetentionKeep: &keep}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("patch missing: want ErrNotFound, got %v", err)
	}
}

// production && auto_deploy is rejected on Create and on Patch, in both field
// orders, and a rejected patch must not mutate the row.
func TestEnvironmentsGuardrails(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	testdb.Seed(t, st, "eu", 10)

	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{
		Project: "game", Name: "prodlike", Production: true, AutoDeploy: true,
	}); err == nil {
		t.Fatal("create production+auto_deploy: want error")
	}

	// Order A: production env, then try to enable auto_deploy.
	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{Project: "game", Name: "live", Production: true}); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := st.PatchEnvironment(ctx, "game", "live", store.EnvironmentPatch{AutoDeploy: &yes}); err == nil {
		t.Fatal("patch auto_deploy on production env: want error")
	}

	// Order B: auto_deploy env, then try to make it production.
	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{Project: "game", Name: "d2", AutoDeploy: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PatchEnvironment(ctx, "game", "d2", store.EnvironmentPatch{Production: &yes}); err == nil {
		t.Fatal("patch production on auto_deploy env: want error")
	}
	if got, _ := st.GetEnvironment(ctx, "game", "d2"); got.Production || !got.AutoDeploy {
		t.Fatalf("rejected patch mutated d2: %+v", got)
	}
}

func TestEnvironmentsNameValidation(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	testdb.Seed(t, st, "eu", 10)
	for _, name := range []string{"all", "global", "DEV", "-x", "", "has space", "way-too-long-environment-name-over-limit"} {
		if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{Project: "game", Name: name}); err == nil {
			t.Fatalf("name %q: want validation error", name)
		}
	}
	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{Project: "game", Name: "qa-1"}); err != nil {
		t.Fatalf("valid name qa-1: %v", err)
	}
}

// An unused env deletes; a used one is a 409 listing the offenders; a missing
// one is a 404.
func TestEnvironmentsDelete(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	testdb.Seed(t, st, "eu", 10) // dev holds the seeded node + version 1.0.0

	if _, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{Project: "game", Name: "temp"}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteEnvironment(ctx, "game", "temp"); err != nil {
		t.Fatalf("delete unused env: %v", err)
	}
	if _, err := st.GetEnvironment(ctx, "game", "temp"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("temp still present after delete: %v", err)
	}

	err := st.DeleteEnvironment(ctx, "game", "dev")
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("delete used env: want ErrConflict, got %v", err)
	}
	if !strings.Contains(err.Error(), "versions") || !strings.Contains(err.Error(), "nodes") {
		t.Fatalf("conflict must list offenders (versions, nodes): %q", err.Error())
	}

	if err := st.DeleteEnvironment(ctx, "game", "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete missing env: want ErrNotFound, got %v", err)
	}
}

// A node moves between envs only when empty and not dead; the move emits
// node_env_changed and is idempotent.
func TestEnvironmentsSetNodeEnv(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10)

	n2 := f.AddNode(t, "node-2", "203.0.113.11", 10)
	node, err := st.SetNodeEnv(ctx, n2, "prod")
	if err != nil {
		t.Fatalf("move empty node: %v", err)
	}
	if node.Env != "prod" {
		t.Fatalf("node env: %s, want prod", node.Env)
	}
	if n, _ := st.CountEvents(ctx, store.EventNodeEnvChanged); n != 1 {
		t.Fatalf("want 1 node_env_changed, got %d", n)
	}
	// Idempotent no-op (prod→prod): ok, no new event.
	if _, err := st.SetNodeEnv(ctx, n2, "prod"); err != nil {
		t.Fatalf("idempotent move: %v", err)
	}
	if n, _ := st.CountEvents(ctx, store.EventNodeEnvChanged); n != 1 {
		t.Fatalf("no-op move must not emit an event, got %d", n)
	}

	// A node with a live server cannot be moved.
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	if _, err := st.SetNodeEnv(ctx, f.NodeID, "prod"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("move node with live server: want ErrConflict, got %v", err)
	}

	// A dead node cannot be moved.
	dead := f.AddNode(t, "node-dead", "203.0.113.12", 10)
	if _, err := st.Pool.Exec(ctx, `update nodes set state = 'dead' where id = $1::uuid`, dead); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetNodeEnv(ctx, dead, "prod"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("move dead node: want ErrConflict, got %v", err)
	}

	// A quarantined, empty node can be moved.
	quar := f.AddNode(t, "node-quar", "203.0.113.13", 10)
	if _, err := st.Pool.Exec(ctx, `update nodes set state = 'quarantine' where id = $1::uuid`, quar); err != nil {
		t.Fatal(err)
	}
	if qn, err := st.SetNodeEnv(ctx, quar, "prod"); err != nil || qn.Env != "prod" {
		t.Fatalf("move quarantined empty node: %+v %v", qn, err)
	}

	// Non-existent target env → ErrNotFound; unknown node → ErrNotFound.
	if _, err := st.SetNodeEnv(ctx, quar, "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("move to missing env: want ErrNotFound, got %v", err)
	}
	if _, err := st.SetNodeEnv(ctx, uuid.NewString(), "prod"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("move unknown node: want ErrNotFound, got %v", err)
	}
}

func TestEnvironmentsSoleEnvWithActiveNodes(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := testdb.Seed(t, st, "eu", 10) // one active dev node, fresh heartbeat

	if env, err := st.SoleEnvWithActiveNodes(ctx, "game"); err != nil || env != "dev" {
		t.Fatalf("sole env: %q %v, want dev", env, err)
	}

	// Two envs with active nodes → ErrConflict.
	n2 := f.AddNode(t, "node-2", "203.0.113.11", 10)
	if _, err := st.SetNodeEnv(ctx, n2, "prod"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SoleEnvWithActiveNodes(ctx, "game"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("two envs with nodes: want ErrConflict, got %v", err)
	}

	// No fresh nodes at all → ErrConflict.
	if _, err := st.Pool.Exec(ctx, `update nodes set last_heartbeat_at = now() - interval '5 minutes'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SoleEnvWithActiveNodes(ctx, "game"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("no fresh nodes: want ErrConflict, got %v", err)
	}
}
