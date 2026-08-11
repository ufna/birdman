package httpapi_test

import (
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

func newAPIKeyServer(t *testing.T) (*store.Store, *httptest.Server) {
	t.Helper()
	st := testdb.New(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log))
	t.Cleanup(ts.Close)
	return st, ts
}

// TestAPIKeysCRUD covers list/create/revoke, scope validation, the audit
// events (without secret), and the last-admin self-lockout guard.
func TestAPIKeysCRUD(t *testing.T) {
	st, ts := newAPIKeyServer(t)
	ctx := t.Context()

	adminK, adminSecret, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "admin", Scopes: []string{httpapi.ScopeAdmin}})
	if err != nil {
		t.Fatal(err)
	}
	_, roSecret, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ro", Scopes: []string{httpapi.ScopeReadonly}})
	admin := &client{t: t, base: ts.URL, key: adminSecret}
	ro := &client{t: t, base: ts.URL, key: roSecret}

	// readonly cannot manage keys.
	if code, _ := ro.do("GET", "/v1/apikeys", nil); code != 403 {
		t.Fatalf("ro list keys: want 403, got %d", code)
	}
	if code, _ := ro.do("POST", "/v1/apikeys", map[string]any{"name": "x", "scopes": []string{"readonly"}}); code != 403 {
		t.Fatalf("ro create key: want 403, got %d", code)
	}

	// list: keys present, never a secret, revoked_at present (null for active).
	code, body := admin.do("GET", "/v1/apikeys", nil)
	if code != 200 {
		t.Fatalf("list keys: %d %v", code, body)
	}
	items := body["apikeys"].([]any)
	if len(items) != 2 {
		t.Fatalf("want 2 keys, got %d", len(items))
	}
	for _, it := range items {
		m := it.(map[string]any)
		if _, leaked := m["secret"]; leaked {
			t.Fatalf("list leaked a secret: %v", m)
		}
		if _, ok := m["revoked_at"]; !ok {
			t.Fatalf("list item missing revoked_at: %v", m)
		}
		if _, ok := m["created_at"]; !ok {
			t.Fatalf("list item missing created_at: %v", m)
		}
	}

	// create: scopes normalized (deduped/sorted), secret returned once.
	code, body = admin.do("POST", "/v1/apikeys", map[string]any{
		"name": "ci", "scopes": []string{"readonly", "deploy", "readonly"}})
	if code != 201 {
		t.Fatalf("create key: %d %v", code, body)
	}
	secret, _ := body["secret"].(string)
	if !strings.HasPrefix(secret, "bmk_") {
		t.Fatalf("create did not return a bmk_ secret: %v", body)
	}
	newKey := body["key"].(map[string]any)
	newKeyID := newKey["id"].(string)
	gotScopes := newKey["scopes"].([]any)
	if len(gotScopes) != 2 || gotScopes[0] != "deploy" || gotScopes[1] != "readonly" {
		t.Fatalf("scopes not normalized: %v", gotScopes)
	}

	// bad requests.
	if code, _ := admin.do("POST", "/v1/apikeys", map[string]any{"name": "z", "scopes": []string{"superuser"}}); code != 400 {
		t.Fatalf("unknown scope: want 400, got %d", code)
	}
	if code, _ := admin.do("POST", "/v1/apikeys", map[string]any{"name": "z", "scopes": []string{}}); code != 400 {
		t.Fatalf("empty scopes: want 400, got %d", code)
	}
	if code, _ := admin.do("POST", "/v1/apikeys", map[string]any{"name": "", "scopes": []string{"readonly"}}); code != 400 {
		t.Fatalf("empty name: want 400, got %d", code)
	}

	// revoke invalidates auth immediately: prime the cache with the new key,
	// revoke it, the very next request with it must 401.
	fresh := &client{t: t, base: ts.URL, key: secret}
	if code, _ := fresh.do("GET", "/v1/nodes", nil); code != 200 {
		t.Fatalf("new key should authenticate: got %d", code)
	}
	code, body = admin.do("DELETE", "/v1/apikeys/"+newKeyID, nil)
	if code != 200 {
		t.Fatalf("revoke: %d %v", code, body)
	}
	if body["key"].(map[string]any)["revoked_at"] == nil {
		t.Fatalf("revoked key should carry revoked_at: %v", body)
	}
	if code, _ := fresh.do("GET", "/v1/nodes", nil); code != 401 {
		t.Fatalf("revoked key should stop authenticating at once: got %d", code)
	}

	// revoke edge cases.
	if code, _ := admin.do("DELETE", "/v1/apikeys/not-a-uuid", nil); code != 400 {
		t.Fatalf("bad uuid revoke: want 400, got %d", code)
	}
	if code, _ := admin.do("DELETE", "/v1/apikeys/"+uuid.NewString(), nil); code != 404 {
		t.Fatalf("unknown key revoke: want 404, got %d", code)
	}

	// last-admin guard: adminK is the only admin key → 409.
	if code, _ := admin.do("DELETE", "/v1/apikeys/"+adminK.ID, nil); code != 409 {
		t.Fatalf("revoke last admin: want 409, got %d", code)
	}
	// add a second admin, then the first can be revoked (by the second).
	code, body = admin.do("POST", "/v1/apikeys", map[string]any{"name": "admin2", "scopes": []string{"admin"}})
	if code != 201 {
		t.Fatalf("create admin2: %d %v", code, body)
	}
	admin2 := &client{t: t, base: ts.URL, key: body["secret"].(string)}
	admin2ID := body["key"].(map[string]any)["id"].(string)
	if code, _ := admin2.do("DELETE", "/v1/apikeys/"+adminK.ID, nil); code != 200 {
		t.Fatalf("revoke first admin with a second present: want 200, got %d", code)
	}
	// now admin2 is the only admin again → guarded.
	if code, _ := admin2.do("DELETE", "/v1/apikeys/"+admin2ID, nil); code != 409 {
		t.Fatalf("revoke last remaining admin: want 409, got %d", code)
	}

	// audit events written, and no secret ever landed in a payload.
	if n, err := st.CountEvents(ctx, store.EventAPIKeyCreated); err != nil || n < 2 {
		t.Fatalf("apikey_created events = %d err=%v, want >=2", n, err)
	}
	if n, err := st.CountEvents(ctx, store.EventAPIKeyRevoked); err != nil || n < 2 {
		t.Fatalf("apikey_revoked events = %d err=%v, want >=2", n, err)
	}
	events, err := st.ListEvents(ctx, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		for k, v := range e.Payload {
			if s, ok := v.(string); ok && strings.HasPrefix(s, "bmk_") {
				t.Fatalf("event %s payload leaked a secret in %s: %v", e.Kind, k, s)
			}
		}
	}
}

// TestAPIKeyRevokeIdempotent: revoking an already-revoked key is a no-op 200
// (no self-lockout error for a non-admin key), and does not emit a new event
// beyond the first.
func TestAPIKeyRevokeIdempotent(t *testing.T) {
	st, ts := newAPIKeyServer(t)
	ctx := t.Context()
	_, adminSecret, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "admin", Scopes: []string{httpapi.ScopeAdmin}})
	k, _, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "temp", Scopes: []string{httpapi.ScopeReadonly}})
	admin := &client{t: t, base: ts.URL, key: adminSecret}

	if code, _ := admin.do("DELETE", "/v1/apikeys/"+k.ID, nil); code != 200 {
		t.Fatalf("first revoke: want 200, got %d", code)
	}
	if code, _ := admin.do("DELETE", "/v1/apikeys/"+k.ID, nil); code != 200 {
		t.Fatalf("second revoke (idempotent): want 200, got %d", code)
	}
	if n, _ := st.CountEvents(ctx, store.EventAPIKeyRevoked); n != 1 {
		t.Fatalf("idempotent revoke should emit exactly 1 event, got %d", n)
	}
}

// TestAPIKeyPurge covers ?purge=true (registries v1 design §6,
// docs/superpowers/specs/2026-07-09-registries-design.md): hard-delete is
// admin-gated exactly like revoke, applies ONLY to an already-revoked key
// (never revokes on its own behalf), and a retried purge on the now-gone id
// is a plain 404 — no destructive escalation from a retry/double-click.
// Values other than purge=true|1 (including its absence) take the
// byte-identical plain-DELETE path.
func TestAPIKeyPurge(t *testing.T) {
	st, ts := newAPIKeyServer(t)
	ctx := t.Context()
	_, adminSecret, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "admin", Scopes: []string{httpapi.ScopeAdmin}})
	_, roSecret, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ro", Scopes: []string{httpapi.ScopeReadonly}})
	admin := &client{t: t, base: ts.URL, key: adminSecret}
	ro := &client{t: t, base: ts.URL, key: roSecret}

	active, _, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "active-key", Scopes: []string{httpapi.ScopeReadonly}})
	revoked, _, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "revoked-key", Scopes: []string{httpapi.ScopeReadonly}})

	// readonly cannot purge — same admin-only gate as revoke.
	if code, _ := ro.do("DELETE", "/v1/apikeys/"+revoked.ID+"?purge=true", nil); code != 403 {
		t.Fatalf("ro purge: want 403, got %d", code)
	}

	// purge on a never-revoked key: 409 not_revoked, purge never revokes.
	code, body := admin.do("DELETE", "/v1/apikeys/"+active.ID+"?purge=true", nil)
	if code != 409 {
		t.Fatalf("purge active: want 409, got %d %v", code, body)
	}
	if body["error"] != "not_revoked" {
		t.Fatalf("purge active: want error=not_revoked, got %v", body)
	}
	if !listHasKey(t, admin, active.ID) {
		t.Fatalf("purge active must not delete the row")
	}

	// revoke, then purge: 204, event recorded, row gone from the list.
	if code, _ := admin.do("DELETE", "/v1/apikeys/"+revoked.ID, nil); code != 200 {
		t.Fatalf("revoke before purge: want 200, got %d", code)
	}
	code, body = admin.do("DELETE", "/v1/apikeys/"+revoked.ID+"?purge=true", nil)
	if code != 204 {
		t.Fatalf("purge revoked: want 204, got %d %v", code, body)
	}
	if listHasKey(t, admin, revoked.ID) {
		t.Fatalf("purged key still present in the list")
	}
	if n, err := st.CountEvents(ctx, store.EventAPIKeyPurged); err != nil || n != 1 {
		t.Fatalf("apikey_purged events = %d err=%v, want 1", n, err)
	}

	// retry: the row is already gone → 404, not a repeated 204/destructive
	// escalation.
	if code, _ := admin.do("DELETE", "/v1/apikeys/"+revoked.ID+"?purge=true", nil); code != 404 {
		t.Fatalf("retry purge: want 404, got %d", code)
	}

	// purge of an unknown id → 404; non-uuid id → 400 (same guard as revoke).
	if code, _ := admin.do("DELETE", "/v1/apikeys/"+uuid.NewString()+"?purge=true", nil); code != 404 {
		t.Fatalf("purge unknown: want 404, got %d", code)
	}
	if code, _ := admin.do("DELETE", "/v1/apikeys/not-a-uuid?purge=true", nil); code != 400 {
		t.Fatalf("purge bad uuid: want 400, got %d", code)
	}

	// Anything other than purge=true|1 takes the byte-identical plain-DELETE
	// path: it revokes (200), it does not hard-delete.
	other, _, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "other-key", Scopes: []string{httpapi.ScopeReadonly}})
	if code, body := admin.do("DELETE", "/v1/apikeys/"+other.ID+"?purge=false", nil); code != 200 {
		t.Fatalf("purge=false: want 200 (plain revoke), got %d %v", code, body)
	}
	if !listHasKey(t, admin, other.ID) {
		t.Fatalf("purge=false must not hard-delete the row")
	}
}

func listHasKey(t *testing.T, c *client, id string) bool {
	t.Helper()
	code, body := c.do("GET", "/v1/apikeys", nil)
	if code != 200 {
		t.Fatalf("list keys: %d %v", code, body)
	}
	for _, it := range body["apikeys"].([]any) {
		if it.(map[string]any)["id"] == id {
			return true
		}
	}
	return false
}
