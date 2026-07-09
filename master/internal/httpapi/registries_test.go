package httpapi_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// hookRecorder counts onRegistriesChanged firings. T3 wires this hook to a
// real agentlink broadcast; here it just proves the httpapi layer calls it
// after every successful change and never on a failed one.
type hookRecorder struct {
	mu sync.Mutex
	n  int
}

func (h *hookRecorder) fire(context.Context) {
	h.mu.Lock()
	h.n++
	h.mu.Unlock()
}

func (h *hookRecorder) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// registryServer wires a server with the registries hook recorder attached,
// plus admin/readonly clients.
func registryServer(t *testing.T) (*store.Store, *client, *client, *hookRecorder) {
	t.Helper()
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	hook := &hookRecorder{}
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log).WithRegistriesHook(hook.fire))
	t.Cleanup(ts.Close)
	ctx := t.Context()
	_, adminSecret, _ := st.CreateAPIKey(ctx, "admin", []string{httpapi.ScopeAdmin})
	_, roSecret, _ := st.CreateAPIKey(ctx, "ro", []string{httpapi.ScopeReadonly})
	admin := &client{t: t, base: ts.URL, key: adminSecret}
	ro := &client{t: t, base: ts.URL, key: roSecret}
	return st, admin, ro, hook
}

// TestRegistriesAdminGate: only the admin scope may list/create/delete
// registries — readonly gets 403 on every route (unlike alerts/apikeys list,
// GET here is admin-only too: registries are secret-adjacent).
func TestRegistriesAdminGate(t *testing.T) {
	_, _, ro, _ := registryServer(t)
	if code, _ := ro.do("GET", "/v1/registries", nil); code != 403 {
		t.Fatalf("ro list: want 403, got %d", code)
	}
	if code, _ := ro.do("POST", "/v1/registries", map[string]any{
		"host": "ghcr.io", "username": "alice", "token": "tok",
	}); code != 403 {
		t.Fatalf("ro create: want 403, got %d", code)
	}
	if code, _ := ro.do("DELETE", "/v1/registries/"+uuid.NewString(), nil); code != 403 {
		t.Fatalf("ro delete: want 403, got %d", code)
	}
}

// TestRegistriesCRUD covers the full admin flow: create (201, no token in the
// response), list (no token anywhere in the body), upsert-replace by host,
// delete (204/404/400), the audit events (without the token), and the
// onRegistriesChanged hook firing after each successful change only.
func TestRegistriesCRUD(t *testing.T) {
	st, admin, _, hook := registryServer(t)
	ctx := t.Context()

	code, body := admin.do("POST", "/v1/registries", map[string]any{
		"host": "GHCR.io", "username": "alice", "token": "tok-1", "note": "primary",
	})
	if code != 201 {
		t.Fatalf("create: %d %v", code, body)
	}
	reg := body["registry"].(map[string]any)
	regID, _ := reg["id"].(string)
	if regID == "" || reg["host"] != "ghcr.io" || reg["username"] != "alice" || reg["note"] != "primary" {
		t.Fatalf("create response shape: %v", reg)
	}
	if _, leaked := reg["token"]; leaked {
		t.Fatalf("create response leaked a token field: %v", reg)
	}
	if hook.count() != 1 {
		t.Fatalf("hook should fire once after create, got %d", hook.count())
	}

	// GET: token never present anywhere in the body (raw substring check —
	// robust against a future field rename).
	code, rawBody := admin.doRaw("GET", "/v1/registries")
	if code != 200 {
		t.Fatalf("list: %d %s", code, rawBody)
	}
	if strings.Contains(string(rawBody), "tok-1") {
		t.Fatalf("list response leaked the token: %s", rawBody)
	}
	if strings.Contains(string(rawBody), `"token"`) {
		t.Fatalf("list response should never carry a token field: %s", rawBody)
	}

	// Upsert-replace: POST the same (case-insensitive) host again with a new
	// token/username — same id, no duplicate row.
	code, body = admin.do("POST", "/v1/registries", map[string]any{
		"host": "ghcr.io", "username": "alice2", "token": "tok-2", "note": "rotated",
	})
	if code != 201 {
		t.Fatalf("re-create (upsert): %d %v", code, body)
	}
	reg2 := body["registry"].(map[string]any)
	if reg2["id"] != regID {
		t.Fatalf("upsert should keep the same id: %v vs %s", reg2["id"], regID)
	}
	if reg2["username"] != "alice2" || reg2["note"] != "rotated" {
		t.Fatalf("upsert should replace username/note: %v", reg2)
	}
	if hook.count() != 2 {
		t.Fatalf("hook should fire again after the upsert, got %d", hook.count())
	}

	code, body = admin.do("GET", "/v1/registries", nil)
	if code != 200 {
		t.Fatalf("list: %d %v", code, body)
	}
	if items := body["registries"].([]any); len(items) != 1 {
		t.Fatalf("upsert should not duplicate the row: %v", items)
	}

	// Delete: bad uuid → 400, unknown → 404, real → 204, repeat → 404.
	if code, _ := admin.do("DELETE", "/v1/registries/not-a-uuid", nil); code != 400 {
		t.Fatalf("bad uuid delete: want 400, got %d", code)
	}
	if code, _ := admin.do("DELETE", "/v1/registries/"+uuid.NewString(), nil); code != 404 {
		t.Fatalf("unknown delete: want 404, got %d", code)
	}
	hookBefore := hook.count()
	if code, _ := admin.do("DELETE", "/v1/registries/"+regID, nil); code != 204 {
		t.Fatalf("delete: want 204, got %d", code)
	}
	if hook.count() != hookBefore+1 {
		t.Fatalf("hook should fire after a real delete: before=%d after=%d", hookBefore, hook.count())
	}
	if code, _ := admin.do("DELETE", "/v1/registries/"+regID, nil); code != 404 {
		t.Fatalf("repeat delete: want 404, got %d", code)
	}
	// A failed (404/400) call must not re-fire the hook.
	if hook.count() != hookBefore+1 {
		t.Fatalf("hook should not fire on a no-op delete: %d", hook.count())
	}

	code, body = admin.do("GET", "/v1/registries", nil)
	if code != 200 || len(body["registries"].([]any)) != 0 {
		t.Fatalf("list after delete: %d %v", code, body)
	}

	// Audit events recorded, without the token ever leaking into a payload.
	if n, _ := st.CountEvents(ctx, store.EventRegistryUpserted); n != 2 {
		t.Fatalf("registry_upserted events: want 2, got %d", n)
	}
	if n, _ := st.CountEvents(ctx, store.EventRegistryRemoved); n != 1 {
		t.Fatalf("registry_removed events: want 1, got %d", n)
	}
	events, err := st.ListEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		for k, v := range e.Payload {
			if s, ok := v.(string); ok && (strings.Contains(s, "tok-1") || strings.Contains(s, "tok-2")) {
				t.Fatalf("event %s payload leaked a token in %s: %v", e.Kind, k, s)
			}
		}
	}
}

// TestRegistriesValidation covers POST field/host validation, including the
// docker.io rejection with a clear detail message, and that a validation
// failure never fires the hook.
func TestRegistriesValidation(t *testing.T) {
	_, admin, _, hook := registryServer(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing host", map[string]any{"username": "a", "token": "t"}},
		{"missing username", map[string]any{"host": "ghcr.io", "token": "t"}},
		{"missing token", map[string]any{"host": "ghcr.io", "username": "a"}},
		{"scheme host", map[string]any{"host": "https://ghcr.io", "username": "a", "token": "t"}},
		{"slash host", map[string]any{"host": "ghcr.io/foo", "username": "a", "token": "t"}},
		{"docker.io rejected", map[string]any{"host": "docker.io", "username": "a", "token": "t"}},
		{"index.docker.io rejected", map[string]any{"host": "index.docker.io", "username": "a", "token": "t"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := admin.do("POST", "/v1/registries", c.body)
			if code != 400 {
				t.Fatalf("%s: want 400, got %d %v", c.name, code, body)
			}
		})
	}
	// The docker.io case gets a detail that actually names docker.io, so the
	// panel/CLI can show a clear reason instead of a generic bad_request.
	code, body := admin.do("POST", "/v1/registries", map[string]any{
		"host": "docker.io", "username": "a", "token": "t",
	})
	if code != 400 || !strings.Contains(strings.ToLower(body["detail"].(string)), "docker.io") {
		t.Fatalf("docker.io rejection detail: %d %v", code, body)
	}
	if hook.count() != 0 {
		t.Fatalf("hook must not fire on a validation failure, got %d", hook.count())
	}
}
