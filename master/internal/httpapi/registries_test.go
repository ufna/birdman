package httpapi_test

import (
	"context"
	"net/http"
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

// garKey is a minimal well-formed service-account JSON key (object with
// "type":"service_account" + non-empty "private_key").
const garKey = `{"type":"service_account","private_key":"-----BEGIN PRIVATE KEY-----\nMIIB\n-----END PRIVATE KEY-----\n"}`

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

// TestRegistriesAdminGate: only the admin scope may list/create/patch/delete
// registries — readonly gets 403 on every route (unlike alerts/apikeys list,
// GET here is admin-only too: registries are secret-adjacent).
func TestRegistriesAdminGate(t *testing.T) {
	_, _, ro, _ := registryServer(t)
	if code, _ := ro.do("GET", "/v1/registries", nil); code != 403 {
		t.Fatalf("ro list: want 403, got %d", code)
	}
	if code, _ := ro.do("POST", "/v1/registries", map[string]any{
		"host": "ghcr.io", "type": "ghcr", "username": "alice", "token": "tok",
	}); code != 403 {
		t.Fatalf("ro create: want 403, got %d", code)
	}
	if code, _ := ro.do("PATCH", "/v1/registries/"+uuid.NewString(), map[string]any{"note": "x"}); code != 403 {
		t.Fatalf("ro patch: want 403, got %d", code)
	}
	if code, _ := ro.do("DELETE", "/v1/registries/"+uuid.NewString(), nil); code != 403 {
		t.Fatalf("ro delete: want 403, got %d", code)
	}
}

// TestRegistriesCRUD covers the full admin flow: create (201, type echoed, no
// token in the response), list (type present, no token anywhere), upsert-replace
// by host, delete (204/404/400), the audit events (without the token), and the
// onRegistriesChanged hook firing after each successful change only.
func TestRegistriesCRUD(t *testing.T) {
	st, admin, _, hook := registryServer(t)
	ctx := t.Context()

	code, body := admin.do("POST", "/v1/registries", map[string]any{
		"host": "GHCR.io", "type": "ghcr", "username": "alice", "token": "tok-1", "note": "primary",
	})
	if code != 201 {
		t.Fatalf("create: %d %v", code, body)
	}
	reg := body["registry"].(map[string]any)
	regID, _ := reg["id"].(string)
	if regID == "" || reg["host"] != "ghcr.io" || reg["type"] != "ghcr" || reg["username"] != "alice" || reg["note"] != "primary" {
		t.Fatalf("create response shape: %v", reg)
	}
	if _, leaked := reg["token"]; leaked {
		t.Fatalf("create response leaked a token field: %v", reg)
	}
	if hook.count() != 1 {
		t.Fatalf("hook should fire once after create, got %d", hook.count())
	}

	// GET: type present, token never present anywhere in the body (raw substring
	// check — robust against a future field rename).
	code, rawBody := admin.doRaw("GET", "/v1/registries")
	if code != 200 {
		t.Fatalf("list: %d %s", code, rawBody)
	}
	if !strings.Contains(string(rawBody), `"type"`) || !strings.Contains(string(rawBody), "ghcr") {
		t.Fatalf("list response should carry type: %s", rawBody)
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
		"host": "ghcr.io", "type": "ghcr", "username": "alice2", "token": "tok-2", "note": "rotated",
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
	assertNoTokenInEvents(t, ctx, st, "tok-1", "tok-2")
}

// TestRegistriesTypedCreate covers the per-type POST: gar normalizes the
// stored username to _json_key regardless of any supplied username, and each
// type's validation failure is a 400.
func TestRegistriesTypedCreate(t *testing.T) {
	st, admin, _, _ := registryServer(t)
	ctx := t.Context()

	// gar: username is forced to _json_key (input ignored), JSON kept as secret.
	code, body := admin.do("POST", "/v1/registries", map[string]any{
		"host": "europe-docker.pkg.dev", "type": "gar", "username": "ignored", "token": garKey, "note": "gar",
	})
	if code != 201 {
		t.Fatalf("gar create: %d %v", code, body)
	}
	reg := body["registry"].(map[string]any)
	if reg["type"] != "gar" || reg["username"] != "_json_key" {
		t.Fatalf("gar normalization in response: %v", reg)
	}
	if _, leaked := reg["token"]; leaked {
		t.Fatalf("gar create leaked a token: %v", reg)
	}
	if c := credFor(t, ctx, st, "europe-docker.pkg.dev"); c.Username != "_json_key" || c.Token != garKey {
		t.Fatalf("gar stored credential wrong: %+v", c)
	}

	// generic ok.
	if code, body := admin.do("POST", "/v1/registries", map[string]any{
		"host": "reg.example.com", "type": "generic", "username": "bob", "token": "pw",
	}); code != 201 {
		t.Fatalf("generic create: %d %v", code, body)
	}

	// Per-type 400s (host-shape, gar JSON, unknown type, missing token, docker.io).
	bad := []struct {
		name string
		body map[string]any
	}{
		{"ghcr wrong host", map[string]any{"host": "example.com", "type": "ghcr", "username": "a", "token": "t"}},
		{"gar bad json", map[string]any{"host": "europe-docker.pkg.dev", "type": "gar", "token": "not-json"}},
		{"gar no private_key", map[string]any{"host": "europe-docker.pkg.dev", "type": "gar", "token": `{"type":"service_account"}`}},
		{"gar wrong host", map[string]any{"host": "example.com", "type": "gar", "token": garKey}},
		{"unknown type", map[string]any{"host": "example.com", "type": "bogus", "username": "a", "token": "t"}},
		{"missing type", map[string]any{"host": "example.com", "username": "a", "token": "t"}},
		{"generic docker.io", map[string]any{"host": "docker.io", "type": "generic", "username": "a", "token": "t"}},
		{"missing token", map[string]any{"host": "ghcr.io", "type": "ghcr", "username": "a"}},
		{"missing username generic", map[string]any{"host": "example.com", "type": "generic", "token": "t"}},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if code, b := admin.do("POST", "/v1/registries", c.body); code != 400 {
				t.Fatalf("%s: want 400, got %d %v", c.name, code, b)
			}
		})
	}
	// docker.io detail still names docker.io (panel shows a real reason).
	code, body = admin.do("POST", "/v1/registries", map[string]any{
		"host": "docker.io", "type": "generic", "username": "a", "token": "t",
	})
	if code != 400 || !strings.Contains(strings.ToLower(body["detail"].(string)), "docker.io") {
		t.Fatalf("docker.io rejection detail: %d %v", code, body)
	}
}

// TestRegistriesPatch covers PATCH /v1/registries/{id}: non-uuid 400, unknown
// 404, keep-secret vs rotate (verified through the creds read), note/username
// edits, host-in-body ignored (immutable), the registry_updated event without
// a token, and the hook firing on every successful patch only.
func TestRegistriesPatch(t *testing.T) {
	st, admin, ro, hook := registryServer(t)
	ctx := t.Context()

	code, body := admin.do("POST", "/v1/registries", map[string]any{
		"host": "reg.example.com", "type": "generic", "username": "alice", "token": "tok-keep", "note": "n1",
	})
	if code != 201 {
		t.Fatalf("seed: %d %v", code, body)
	}
	id := body["registry"].(map[string]any)["id"].(string)
	base := hook.count()

	// readonly → 403, non-uuid → 400, unknown → 404 (none touch the hook).
	if c, _ := ro.do("PATCH", "/v1/registries/"+id, map[string]any{"note": "x"}); c != 403 {
		t.Fatalf("ro patch: want 403, got %d", c)
	}
	if c, _ := admin.do("PATCH", "/v1/registries/not-a-uuid", map[string]any{"note": "x"}); c != 400 {
		t.Fatalf("non-uuid patch: want 400, got %d", c)
	}
	if c, _ := admin.do("PATCH", "/v1/registries/"+uuid.NewString(), map[string]any{"note": "x"}); c != 404 {
		t.Fatalf("unknown patch: want 404, got %d", c)
	}
	if hook.count() != base {
		t.Fatalf("hook must not fire on rejected patches: %d", hook.count())
	}

	// Keep-secret: patch username+note, omit token → secret intact, no token in
	// the response.
	code, body = admin.do("PATCH", "/v1/registries/"+id, map[string]any{"username": "alice2", "note": "n2"})
	if code != 200 {
		t.Fatalf("patch keep: %d %v", code, body)
	}
	reg := body["registry"].(map[string]any)
	if reg["username"] != "alice2" || reg["note"] != "n2" || reg["host"] != "reg.example.com" || reg["type"] != "generic" {
		t.Fatalf("patch keep response: %v", reg)
	}
	if _, leaked := reg["token"]; leaked {
		t.Fatalf("patch response leaked a token: %v", reg)
	}
	if got := credFor(t, ctx, st, "reg.example.com").Token; got != "tok-keep" {
		t.Fatalf("keep-secret: token must be unchanged, got %q", got)
	}

	// host in body is IGNORED (immutable), not rejected → 200 and host stays.
	code, body = admin.do("PATCH", "/v1/registries/"+id, map[string]any{"host": "evil.example.com", "note": "n3"})
	if code != 200 {
		t.Fatalf("patch host-ignored: %d %v", code, body)
	}
	if body["registry"].(map[string]any)["host"] != "reg.example.com" {
		t.Fatalf("host must be immutable/ignored: %v", body["registry"])
	}

	// Rotate: non-empty token re-encrypts.
	code, body = admin.do("PATCH", "/v1/registries/"+id, map[string]any{"token": "tok-rotated"})
	if code != 200 {
		t.Fatalf("patch rotate: %d %v", code, body)
	}
	if got := credFor(t, ctx, st, "reg.example.com").Token; got != "tok-rotated" {
		t.Fatalf("rotate: token should be tok-rotated, got %q", got)
	}

	// Three successful patches fired the hook three times.
	if hook.count() != base+3 {
		t.Fatalf("hook should fire on each successful patch: base=%d now=%d", base, hook.count())
	}

	// registry_updated events exist and never carry a token.
	if n, _ := st.CountEvents(ctx, store.EventRegistryUpdated); n != 3 {
		t.Fatalf("registry_updated events: want 3, got %d", n)
	}
	assertNoTokenInEvents(t, ctx, st, "tok-keep", "tok-rotated")
}

// TestRegistriesPatchTypeChangeToGARNeedsToken: a type change to gar without a
// fresh token cannot retro-fit the _json_key + SA-JSON invariant → 400.
func TestRegistriesPatchTypeChangeToGARNeedsToken(t *testing.T) {
	_, admin, _, _ := registryServer(t)
	code, body := admin.do("POST", "/v1/registries", map[string]any{
		"host": "reg.example.com", "type": "generic", "username": "alice", "token": "tok-1",
	})
	if code != 201 {
		t.Fatalf("seed: %d %v", code, body)
	}
	id := body["registry"].(map[string]any)["id"].(string)
	if c, b := admin.do("PATCH", "/v1/registries/"+id, map[string]any{"type": "gar"}); c != 400 {
		t.Fatalf("type→gar without token: want 400, got %d %v", c, b)
	}
}

// credFor returns the single credential for host (fatal if absent) — the store
// read that carries the decrypted token, used to prove keep-vs-rotate over HTTP.
func credFor(t *testing.T, ctx context.Context, st *store.Store, host string) store.RegistryCred {
	t.Helper()
	creds, err := st.ListRegistryCreds(ctx)
	if err != nil {
		t.Fatalf("list creds: %v", err)
	}
	for _, c := range creds {
		if c.Host == host {
			return c
		}
	}
	t.Fatalf("no credential for host %q", host)
	return store.RegistryCred{}
}

// assertNoTokenInEvents scans every event payload for any of the given token
// substrings — the write-only guarantee (no token in an audit event).
func assertNoTokenInEvents(t *testing.T, ctx context.Context, st *store.Store, tokens ...string) {
	t.Helper()
	events, err := st.ListEvents(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		for k, v := range e.Payload {
			s, ok := v.(string)
			if !ok {
				continue
			}
			for _, tok := range tokens {
				if strings.Contains(s, tok) {
					t.Fatalf("event %s payload leaked a token in %s: %v", e.Kind, k, s)
				}
			}
		}
	}
}

// ctxRecorder is an onRegistriesChanged hook that captures the exact
// context.Context it was invoked with (not just a count), so a test can
// inspect its cancellation behavior after the fact.
type ctxRecorder struct {
	mu    sync.Mutex
	calls int
	last  context.Context
}

func (c *ctxRecorder) fire(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.last = ctx
}

func (c *ctxRecorder) snapshot() (int, context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.last
}

// TestRegistriesHookSurvivesRequestContextCancellation is a regression test
// for task-review Fix 2 (registries-v1 branch, final-review round):
// onRegistriesChanged used to run with r.Context() itself — a client that
// disconnects right after the DB commit cancels r.Context(), and since
// BroadcastRegistries' own store read (and the Hub sends it triggers) reuse
// that same ctx, the change stays durable in Postgres but is never
// distributed to connected agents until the next change or reconnect. Fix:
// both handleCreateRegistry and handleDeleteRegistry now invoke
// s.onRegistriesChanged(context.WithoutCancel(r.Context())).
//
// This is a synchronous, deterministic reproduction — no goroutines or
// timing races. It drives Server.ServeHTTP directly, in-process (no real
// network round trip, so nothing here depends on scheduler luck), with a
// request whose context it controls; ServeHTTP runs the full handler
// (decode → store write → audit event → hook) to completion and returns
// before the test ever touches the context's cancellation — so whatever the
// hook captured is exactly what production code handed it, not an artifact
// of timing. ONLY AFTER ServeHTTP returns does the test cancel the
// ORIGINAL request context and check whether the hook's captured context
// noticed. Pre-fix, the hook's captured context IS r.Context() (the same
// object) — cancelling the original after the fact also cancels the
// captured one, proving they were wrongly the same context. Post-fix,
// WithoutCancel's returned context is provably detached: Err() must stay
// nil no matter what happens to its parent afterward.
func TestRegistriesHookSurvivesRequestContextCancellation(t *testing.T) {
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	hook := &ctxRecorder{}
	srv := httpapi.New(st, m, mm, dep, nil, nil, "", "", log).WithRegistriesHook(hook.fire)

	ctx := t.Context()
	_, adminSecret, err := st.CreateAPIKey(ctx, "admin", []string{httpapi.ScopeAdmin})
	if err != nil {
		t.Fatal(err)
	}

	// --- POST /v1/registries ---
	reqCtx, cancel := context.WithCancel(context.Background())
	body := strings.NewReader(`{"host":"ghcr.io","type":"ghcr","username":"alice","token":"tok-cancel-1"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/registries", body).WithContext(reqCtx)
	req.Header.Set("Authorization", "Bearer "+adminSecret)
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d: %s", w.Code, w.Body.String())
	}
	calls, hookCtx := hook.snapshot()
	if calls != 1 {
		t.Fatalf("want the hook to fire exactly once after create, got %d", calls)
	}
	if hookCtx == nil {
		t.Fatal("hook did not capture a context")
	}
	if err := hookCtx.Err(); err != nil {
		t.Fatalf("hook context must not be cancelled before the request context is, got %v", err)
	}

	// Simulate the client disconnecting right after the response was
	// written: cancel the ORIGINAL request context now, once the handler
	// (and hook) has already run to completion.
	cancel()
	if reqCtx.Err() == nil {
		t.Fatal("sanity check failed: the original request context should be cancelled now")
	}
	if err := hookCtx.Err(); err != nil {
		t.Fatalf("POST: hook's context must survive the request context's cancellation (context.WithoutCancel), got %v", err)
	}

	// --- DELETE /v1/registries/{id} — same property, the other call site. ---
	regs, err := st.ListRegistries(ctx)
	if err != nil || len(regs) != 1 {
		t.Fatalf("want exactly 1 registry seeded by the POST above: %v (err=%v)", regs, err)
	}
	regID := regs[0].ID

	delCtx, delCancel := context.WithCancel(context.Background())
	delReq := httptest.NewRequest(http.MethodDelete, "/v1/registries/"+regID, nil).WithContext(delCtx)
	delReq.Header.Set("Authorization", "Bearer "+adminSecret)
	delW := httptest.NewRecorder()

	srv.ServeHTTP(delW, delReq)

	if delW.Code != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d: %s", delW.Code, delW.Body.String())
	}
	calls, hookCtx = hook.snapshot()
	if calls != 2 {
		t.Fatalf("want the hook to have fired twice total (create+delete), got %d", calls)
	}
	delCancel()
	if delCtx.Err() == nil {
		t.Fatal("sanity check failed: the delete request context should be cancelled now")
	}
	if err := hookCtx.Err(); err != nil {
		t.Fatalf("DELETE: hook's context must survive the request context's cancellation (context.WithoutCancel), got %v", err)
	}
}
