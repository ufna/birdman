package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/testdb"
)

// browser mimics the panel: session cookie + CSRF header, no Bearer.
type browser struct {
	t      *testing.T
	base   string
	cookie *http.Cookie
	csrf   bool
}

func (b *browser) do(method, path string, body any) (int, map[string]any, *http.Response) {
	b.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			b.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, b.base+path, rd)
	if err != nil {
		b.t.Fatal(err)
	}
	if b.cookie != nil {
		req.AddCookie(b.cookie)
	}
	if b.csrf {
		req.Header.Set("X-Birdman-Csrf", "1")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(raw) > 0 && strings.Contains(resp.Header.Get("Content-Type"), "json") {
		if err := json.Unmarshal(raw, &out); err != nil {
			b.t.Fatalf("bad json (%d): %s", resp.StatusCode, raw)
		}
	}
	return resp.StatusCode, out, resp
}

func sessionCookieOf(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == "birdman_session" {
			return c
		}
	}
	t.Fatalf("no birdman_session cookie in %v", resp.Header["Set-Cookie"])
	return nil
}

// TestSessionAuth: login → cookie → cookie-authenticated API calls with CSRF
// enforcement → logout (docs/specs/panel.md §1 п.5).
func TestSessionAuth(t *testing.T) {
	st := testdb.New(t)
	ts := apiServer(t, st)
	adminKey := scopedKey(t, st, "admin", httpapi.ScopeAdmin)
	roKey := scopedKey(t, st, "ro", httpapi.ScopeReadonly)

	// Login rejects invalid keys, GET /v1/session without auth is 401.
	b := &browser{t: t, base: ts.URL}
	if code, _, _ := b.do("POST", "/v1/session", map[string]any{"api_key": "bmk_bogus"}); code != 401 {
		t.Fatalf("bad key login: want 401, got %d", code)
	}
	if code, _, _ := b.do("GET", "/v1/session", nil); code != 401 {
		t.Fatalf("anon session probe: want 401, got %d", code)
	}

	// Login sets a hardened HttpOnly cookie and returns the scopes.
	code, body, resp := b.do("POST", "/v1/session", map[string]any{"api_key": roKey})
	if code != 200 {
		t.Fatalf("login: %d %v", code, body)
	}
	if scopes, _ := body["scopes"].([]any); len(scopes) != 1 || scopes[0] != "readonly" {
		t.Fatalf("login scopes: %v", body)
	}
	c := sessionCookieOf(t, resp)
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Fatalf("cookie flags: %+v", c)
	}
	b.cookie = c

	// Cookie authenticates GETs; scopes are inherited from the key.
	if code, body, _ := b.do("GET", "/v1/session", nil); code != 200 || body["name"] != "ro" {
		t.Fatalf("session probe: %d %v", code, body)
	}
	if code, _, _ := b.do("GET", "/v1/nodes", nil); code != 200 {
		t.Fatalf("cookie GET /v1/nodes: want 200, got %d", code)
	}
	// readonly session cannot mutate even with the CSRF header.
	b.csrf = true
	if code, _, _ := b.do("POST", "/v1/nodes", map[string]any{}); code != 403 {
		t.Fatalf("readonly cookie POST: want 403, got %d", code)
	}

	// Admin session: non-GET without the CSRF header → 403 csrf_required.
	ab := &browser{t: t, base: ts.URL}
	_, _, resp = ab.do("POST", "/v1/session", map[string]any{"api_key": adminKey})
	ab.cookie = sessionCookieOf(t, resp)
	code, body, _ = ab.do("POST", "/v1/nodes", map[string]any{
		"project": "game", "region": "eu", "hostname": "n1",
		"public_ip": "203.0.113.10", "capacity_slots": 4,
	})
	if code != 403 || body["error"] != "csrf_required" {
		t.Fatalf("cookie POST without CSRF: want 403 csrf_required, got %d %v", code, body)
	}
	ab.csrf = true
	if code, body, _ = ab.do("POST", "/v1/nodes", map[string]any{
		"project": "game", "region": "eu", "hostname": "n1",
		"public_ip": "203.0.113.10", "capacity_slots": 4,
	}); code != 201 {
		t.Fatalf("cookie POST with CSRF: %d %v", code, body)
	}
	// Bearer requests never need the CSRF header (unchanged behavior).
	admin := &client{t: t, base: ts.URL, key: adminKey}
	if code, _ := admin.do("POST", "/v1/nodes", map[string]any{
		"project": "game", "region": "eu", "hostname": "n2",
		"public_ip": "203.0.113.11", "capacity_slots": 4,
	}); code != 201 {
		t.Fatalf("bearer POST: want 201, got %d", code)
	}

	// Logout requires CSRF, kills the session and expires the cookie.
	b.csrf = false
	if code, _, _ := b.do("DELETE", "/v1/session", nil); code != 403 {
		t.Fatalf("logout without CSRF: want 403, got %d", code)
	}
	b.csrf = true
	code, _, resp = b.do("DELETE", "/v1/session", nil)
	if code != 204 {
		t.Fatalf("logout: want 204, got %d", code)
	}
	if c := sessionCookieOf(t, resp); c.Value != "" || c.MaxAge >= 0 {
		t.Fatalf("logout cookie not expired: %+v", c)
	}
	if code, _, _ := b.do("GET", "/v1/nodes", nil); code != 401 {
		t.Fatalf("dead session GET: want 401, got %d", code)
	}
	// Logout is idempotent without a session.
	if code, _, _ := (&browser{t: t, base: ts.URL}).do("DELETE", "/v1/session", nil); code != 204 {
		t.Fatalf("anon logout: want 204, got %d", code)
	}
}

// TestSessionExpiry: sessions honor their TTL (in-memory store).
func TestSessionExpiry(t *testing.T) {
	ss := httpapi.NewSessionStoreForTest(30 * time.Millisecond)
	id, err := ss.CreateForTest("k1", []string{"readonly"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ss.GetForTest(id); !ok {
		t.Fatal("fresh session must resolve")
	}
	time.Sleep(60 * time.Millisecond)
	if _, ok := ss.GetForTest(id); ok {
		t.Fatal("expired session must not resolve")
	}
}
