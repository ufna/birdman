package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }

type client struct {
	t    *testing.T
	base string
	key  string
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(raw) > 0 && strings.Contains(resp.Header.Get("Content-Type"), "json") {
		if err := json.Unmarshal(raw, &out); err != nil {
			c.t.Fatalf("bad json (%d): %s", resp.StatusCode, raw)
		}
	}
	return resp.StatusCode, out
}

func TestRESTFlow(t *testing.T) {
	st := testdb.New(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ts := httptest.NewServer(httpapi.New(st, metrics.New(st, log), log))
	t.Cleanup(ts.Close)
	ctx := t.Context()

	_, adminKey, err := st.CreateAPIKey(ctx, "admin", []string{httpapi.ScopeAdmin})
	if err != nil {
		t.Fatal(err)
	}
	_, roKey, err := st.CreateAPIKey(ctx, "ro", []string{httpapi.ScopeReadonly})
	if err != nil {
		t.Fatal(err)
	}
	_, deployKey, err := st.CreateAPIKey(ctx, "ci", []string{httpapi.ScopeDeploy})
	if err != nil {
		t.Fatal(err)
	}
	_, allocKey, err := st.CreateAPIKey(ctx, "mm", []string{httpapi.ScopeAllocate})
	if err != nil {
		t.Fatal(err)
	}

	admin := &client{t: t, base: ts.URL, key: adminKey}
	ro := &client{t: t, base: ts.URL, key: roKey}
	deploy := &client{t: t, base: ts.URL, key: deployKey}
	alloc := &client{t: t, base: ts.URL, key: allocKey}
	anon := &client{t: t, base: ts.URL}

	// healthz and metrics are public.
	if code, body := anon.do("GET", "/healthz", nil); code != 200 || body["status"] != "ok" {
		t.Fatalf("healthz: %d %v", code, body)
	}
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("metrics: %v %d", err, resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), "birdman_allocation_failures_total") &&
		!strings.Contains(string(raw), "go_goroutines") {
		t.Fatalf("metrics output looks empty: %.200s", raw)
	}

	// Auth matrix.
	if code, _ := anon.do("GET", "/v1/nodes", nil); code != 401 {
		t.Fatalf("anon list nodes: want 401, got %d", code)
	}
	if code, _ := ro.do("POST", "/v1/nodes", map[string]any{}); code != 403 {
		t.Fatalf("readonly create node: want 403, got %d", code)
	}
	if code, _ := deploy.do("GET", "/v1/nodes", nil); code != 403 {
		t.Fatalf("deploy key listing nodes: want 403, got %d", code)
	}

	// Node lifecycle.
	code, body := admin.do("POST", "/v1/nodes", map[string]any{
		"project": "game", "region": "eu", "hostname": "n1",
		"public_ip": "203.0.113.10", "capacity_slots": 10,
	})
	if code != 201 {
		t.Fatalf("create node: %d %v", code, body)
	}
	token, _ := body["node_token"].(string)
	if !strings.HasPrefix(token, "bnt_") {
		t.Fatalf("node_token missing: %v", body)
	}
	nodeID := body["node"].(map[string]any)["id"].(string)
	if code, body := ro.do("GET", "/v1/nodes", nil); code != 200 || len(body["nodes"].([]any)) != 1 {
		t.Fatalf("list nodes: %d %v", code, body)
	}

	// Versions.
	code, body = deploy.do("POST", "/v1/versions", map[string]any{
		"project": "game", "semver": "1.0.0",
		"image_ref": "ghcr.io/example/game:1.0.0", "channel": "prod",
	})
	if code != 201 {
		t.Fatalf("create version: %d %v", code, body)
	}
	versionID := body["version"].(map[string]any)["id"].(string)
	if code, _ = deploy.do("POST", "/v1/versions", map[string]any{
		"project": "game", "semver": "1.0.0",
		"image_ref": "ghcr.io/example/game:1.0.0", "channel": "prod",
	}); code != 409 {
		t.Fatalf("duplicate version: want 409, got %d", code)
	}
	if code, body = ro.do("GET", "/v1/versions", nil); code != 200 || len(body["versions"].([]any)) != 1 {
		t.Fatalf("list versions: %d %v", code, body)
	}

	// Fleet config.
	code, body = admin.do("PUT", "/v1/fleets/eu", map[string]any{
		"project": "game", "active_version": versionID, "buffer_ready": 2,
	})
	if code != 200 {
		t.Fatalf("upsert fleet: %d %v", code, body)
	}
	if code, _ = admin.do("PUT", "/v1/fleets/eu", map[string]any{
		"project": "game", "active_version": uuid.NewString(),
	}); code != 404 {
		t.Fatalf("fleet with unknown version: want 404, got %d", code)
	}

	// Allocation: empty pool → 409 no_capacity with the exact error shape.
	matchID := uuid.NewString()
	code, body = alloc.do("POST", "/v1/allocate", map[string]any{
		"project": "game", "region": "eu", "match_id": matchID,
	})
	if code != 409 || body["error"] != "no_capacity" {
		t.Fatalf("allocate empty: want 409 no_capacity, got %d %v", code, body)
	}

	// Seed one ready server directly, node heartbeat fresh.
	if _, err := st.Pool.Exec(ctx,
		`update nodes set last_heartbeat_at = now() where id = $1::uuid`, nodeID); err != nil {
		t.Fatal(err)
	}
	var serverID string
	if err := st.Pool.QueryRow(ctx, `
		insert into servers (project_id, node_id, version_id, state, port)
		select project_id, id, $2::uuid, 'ready', 22222 from nodes where id = $1::uuid
		returning id::text`, nodeID, versionID).Scan(&serverID); err != nil {
		t.Fatal(err)
	}

	code, body = alloc.do("POST", "/v1/allocate", map[string]any{
		"project": "game", "region": "eu", "match_id": matchID,
	})
	if code != 200 || body["server_id"] != serverID || body["host"] != "203.0.113.10" {
		t.Fatalf("allocate: %d %v", code, body)
	}
	if port := body["port"].(float64); int(port) != 22222 {
		t.Fatalf("allocate port: %v", body)
	}
	// Idempotent repeat over REST.
	code, body2 := alloc.do("POST", "/v1/allocate", map[string]any{
		"project": "game", "region": "eu", "match_id": matchID,
	})
	if code != 200 || body2["server_id"] != serverID {
		t.Fatalf("idempotent allocate: %d %v", code, body2)
	}
	// readonly key cannot allocate.
	if code, _ = ro.do("POST", "/v1/allocate", map[string]any{
		"project": "game", "region": "eu", "match_id": uuid.NewString(),
	}); code != 403 {
		t.Fatalf("readonly allocate: want 403, got %d", code)
	}
	// bad match_id.
	if code, _ = alloc.do("POST", "/v1/allocate", map[string]any{
		"project": "game", "region": "eu", "match_id": "not-a-uuid",
	}); code != 400 {
		t.Fatalf("bad match_id: want 400, got %d", code)
	}

	// Events feed captured the story.
	code, body = ro.do("GET", "/v1/events?limit=50", nil)
	if code != 200 {
		t.Fatalf("events: %d %v", code, body)
	}
	kinds := map[string]bool{}
	for _, e := range body["events"].([]any) {
		kinds[fmt.Sprint(e.(map[string]any)["kind"])] = true
	}
	for _, want := range []string{store.EventNodeCreated, store.EventVersionRegistered,
		store.EventFleetUpdated, store.EventAllocationFailed} {
		if !kinds[want] {
			t.Fatalf("missing event %s in %v", want, kinds)
		}
	}
}
