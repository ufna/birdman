package httpapi_test

import (
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// deployServer wires the REST API with a live deploy manager + recorder.
func deployServer(t *testing.T, st *store.Store) (*httptest.Server, *deploy.Manager, *testdb.CommandRecorder) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	rec := &testdb.CommandRecorder{}
	dep := deploy.New(deploy.Options{Store: st, Sender: rec, Log: log})
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log))
	t.Cleanup(ts.Close)
	return ts, dep, rec
}

// POST /v1/deploy + /v1/rollback over HTTP: scopes, status codes and the full
// prepull → flip → rollback cycle (итерация 3, master.md §5–6).
func TestDeployAndRollbackEndpoints(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	v2 := f.AddVersion(t, "1.1.0")
	ts, dep, rec := deployServer(t, st)
	ctx := t.Context()

	_, deployKey, err := st.CreateAPIKey(ctx, "ci", []string{httpapi.ScopeDeploy})
	if err != nil {
		t.Fatal(err)
	}
	_, roKey, err := st.CreateAPIKey(ctx, "ro", []string{httpapi.ScopeReadonly})
	if err != nil {
		t.Fatal(err)
	}
	ci := &client{t: t, base: ts.URL, key: deployKey}
	ro := &client{t: t, base: ts.URL, key: roKey}

	// Scope: deploy required.
	if code, _ := ro.do("POST", "/v1/deploy", map[string]any{"version_id": v2}); code != 403 {
		t.Fatalf("readonly deploy: want 403, got %d", code)
	}
	if code, _ := ro.do("POST", "/v1/rollback", map[string]any{}); code != 403 {
		t.Fatalf("readonly rollback: want 403, got %d", code)
	}

	// Validation.
	if code, _ := ci.do("POST", "/v1/deploy", map[string]any{"version_id": "nope"}); code != 400 {
		t.Fatalf("bad version_id: want 400, got %d", code)
	}
	if code, _ := ci.do("POST", "/v1/deploy",
		map[string]any{"version_id": "00000000-0000-0000-0000-000000000000"}); code != 404 {
		t.Fatalf("unknown version: want 404, got %d", code)
	}
	// Nothing deprecated yet → rollback conflicts.
	if code, body := ci.do("POST", "/v1/rollback", map[string]any{}); code != 409 {
		t.Fatalf("premature rollback: want 409, got %d %v", code, body)
	}

	// Deploy: 202 prepulling, PrePull dispatched to the node.
	code, body := ci.do("POST", "/v1/deploy", map[string]any{"version_id": v2})
	if code != 202 {
		t.Fatalf("deploy: want 202, got %d %v", code, body)
	}
	dp := body["deploy"].(map[string]any)
	if dp["state"] != "prepulling" || dp["pending_nodes"] != float64(1) {
		t.Fatalf("deploy body: %v", body)
	}
	var prepulled bool
	for _, c := range rec.Take() {
		if p := c.Msg.GetPrepull(); p != nil && c.NodeID == f.NodeID {
			prepulled = true
		}
	}
	if !prepulled {
		t.Fatal("PrePull not dispatched")
	}

	// Node reports pulled → flip; repeated deploy → 200 active.
	dep.HandlePullReport(f.NodeID, &agentlinkv1.PullReport{
		ImageRef: "ghcr.io/example/game-server:1.1.0", Status: "pulled",
	})
	code, body = ci.do("POST", "/v1/deploy", map[string]any{"version_id": v2})
	if code != 200 || body["deploy"].(map[string]any)["state"] != "active" {
		t.Fatalf("repeat deploy: want 200 active, got %d %v", code, body)
	}

	// Rollback: one call, 200, old version active again.
	code, body = ci.do("POST", "/v1/rollback", map[string]any{})
	if code != 200 {
		t.Fatalf("rollback: want 200, got %d %v", code, body)
	}
	rb := body["rollback"].(map[string]any)
	if rb["old_semver"] != "1.1.0" {
		t.Fatalf("rollback body: %v", body)
	}
	v, err := st.GetVersion(ctx, f.VersionID)
	if err != nil || v.State != "active" {
		t.Fatalf("rolled-back version: %+v %v", v, err)
	}

	// Unknown region → 404, nothing flipped.
	if code, _ := ci.do("POST", "/v1/rollback", map[string]any{"region": "mars"}); code != 404 {
		t.Fatalf("bad region rollback: want 404, got %d", code)
	}
}
