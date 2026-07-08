package panelui_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ufna/birdman/master/internal/panelui"
)

const indexHTML = `<!doctype html><div id="root"></div><script src="/assets/app-4f2a.js"></script>`

func builtFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":         {Data: []byte(indexHTML)},
		"assets/app-4f2a.js": {Data: []byte("console.log('panel')")},
	}
}

func get(t *testing.T, h http.Handler, method, path string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	resp := rec.Result()
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(raw)
}

func TestServesBuiltPanel(t *testing.T) {
	h := panelui.NewHandler(builtFS())

	// Root serves the SPA entry.
	resp, body := get(t, h, "GET", "/")
	if resp.StatusCode != 200 || !strings.Contains(body, `id="root"`) {
		t.Fatalf("GET /: %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("GET / content-type: %s", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("index cache-control: %q", cc)
	}

	// Hashed assets are served with immutable caching.
	resp, body = get(t, h, "GET", "/assets/app-4f2a.js")
	if resp.StatusCode != 200 || !strings.Contains(body, "panel") {
		t.Fatalf("asset: %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("asset content-type: %s", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("asset cache-control: %q", cc)
	}

	// Client-side routes fall back to index.html (SPA).
	for _, path := range []string{"/fleet", "/matches", "/deep/link"} {
		if resp, body = get(t, h, "GET", path); resp.StatusCode != 200 || !strings.Contains(body, `id="root"`) {
			t.Fatalf("SPA fallback %s: %d %q", path, resp.StatusCode, body)
		}
	}

	// Missing files with extensions still fall back (vite-style SPA), but
	// the API namespace never turns into HTML.
	resp, body = get(t, h, "GET", "/v1/unknown")
	if resp.StatusCode != 404 || !strings.Contains(body, `"not_found"`) {
		t.Fatalf("/v1 guard: %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("/v1 guard content-type: %s", ct)
	}

	// Static content is read-only.
	if resp, _ = get(t, h, "POST", "/"); resp.StatusCode != 405 {
		t.Fatalf("POST /: want 405, got %d", resp.StatusCode)
	}

	// Path traversal cannot escape the FS root.
	if resp, _ = get(t, h, "GET", "/../panelui.go"); resp.StatusCode != 200 {
		t.Fatalf("dotdot: want index fallback 200, got %d", resp.StatusCode)
	}
}

// TestPlaceholderWithoutBuild: a master built without the panel serves the
// built-in placeholder page (go build must not require node).
func TestPlaceholderWithoutBuild(t *testing.T) {
	h := panelui.NewHandler(fstest.MapFS{})
	resp, body := get(t, h, "GET", "/")
	if resp.StatusCode != 200 || !strings.Contains(body, "birdman") ||
		!strings.Contains(body, "panel/build.sh") {
		t.Fatalf("placeholder: %d %q", resp.StatusCode, body)
	}
	if resp, body = get(t, h, "GET", "/fleet"); resp.StatusCode != 200 || !strings.Contains(body, "birdman") {
		t.Fatalf("placeholder fallback: %d %q", resp.StatusCode, body)
	}
}

// TestEmbeddedHandler: the real embed serves HTML at `/` in both tree
// states — placeholder (fresh clone, static/ holds only .gitkeep) and a
// populated panel build.
func TestEmbeddedHandler(t *testing.T) {
	resp, body := get(t, panelui.Handler(), "GET", "/")
	if resp.StatusCode != 200 || !strings.Contains(body, "birdman") {
		t.Fatalf("embedded /: %d %.120q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("embedded / content-type: %s", ct)
	}
}
