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
)

// TestUpsertFleetKicksQueuedAutoDeploy (W2-реестр, v6): a version registered in an
// auto_deploy env before any fleet exists is queued (ErrNoFleet, marker not
// moved); creating the first fleet for that env kicks TryAutoDeploy from the PUT
// /v1/fleets handler, so the queued version finally deploys.
func TestUpsertFleetKicksQueuedAutoDeploy(t *testing.T) {
	st := testdb.New(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log))
	t.Cleanup(ts.Close)
	ctx := t.Context()

	_, adminKey, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name: "admin", Scopes: []string{httpapi.ScopeAdmin},
	})
	if err != nil {
		t.Fatal(err)
	}
	admin := &client{t: t, base: ts.URL, key: adminKey}

	// Register a version in dev (auto_deploy) with NO fleet yet → queued.
	code, body := admin.do("POST", "/v1/versions", map[string]any{
		"project": "game", "semver": "1.0.0", "image_ref": "ghcr.io/example/game:1.0.0", "env": "dev",
	})
	if code != 201 {
		t.Fatalf("create version: %d %v", code, body)
	}
	versionID := body["version"].(map[string]any)["id"].(string)
	if body["auto_deploy"] != "queued" {
		t.Fatalf("no fleet yet → want auto_deploy queued, got %v", body["auto_deploy"])
	}
	if v, _ := st.GetVersion(ctx, versionID); v.State != "registered" {
		t.Fatalf("queued version must still be registered, got %s", v.State)
	}

	// Create the FIRST dev fleet → the handler kicks TryAutoDeploy → deploy starts
	// (no live nodes → immediate activate, so the version leaves `registered`).
	code, body = admin.do("PUT", "/v1/fleets/eu", map[string]any{
		"project": "game", "env": "dev", "active_version": versionID, "buffer_ready": 1,
	})
	if code != 200 {
		t.Fatalf("upsert fleet: %d %v", code, body)
	}
	if v, _ := st.GetVersion(ctx, versionID); v.State == "registered" {
		t.Fatal("fleet kick must have started the queued auto-deploy, version still registered")
	}
	if n, _ := st.CountEvents(ctx, store.EventDeployStarted); n < 1 {
		t.Fatalf("want a deploy_started from the fleet kick, got %d", n)
	}
}
