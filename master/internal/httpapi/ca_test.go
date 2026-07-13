package httpapi_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

// caTestServer wires a fresh server with admin / readonly / deploy clients.
// deploy has neither readonly nor admin, so it exercises the scope gate on the
// readonly /v1/ca route.
func caTestServer(t *testing.T) (*store.Store, *client, *client, *client) {
	t.Helper()
	st := testdb.New(t)
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log))
	t.Cleanup(ts.Close)
	ctx := t.Context()
	_, adminKey, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "admin", Scopes: []string{httpapi.ScopeAdmin}})
	_, roKey, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ro", Scopes: []string{httpapi.ScopeReadonly}})
	_, deployKey, _ := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "ci", Scopes: []string{httpapi.ScopeDeploy}})
	admin := &client{t: t, base: ts.URL, key: adminKey}
	ro := &client{t: t, base: ts.URL, key: roKey}
	deploy := &client{t: t, base: ts.URL, key: deployKey}
	return st, admin, ro, deploy
}

// rawGet issues an authenticated GET and returns status, Content-Type and body
// — the /v1/ca body is text/plain PEM, not a JSON object, and the test needs
// the header.
func rawGet(t *testing.T, c *client) (int, string, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.base+"/v1/ca", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
}

// TestCA covers GET /v1/ca: scope gate, the text/plain PEM bundle equal to the
// active CA cert, and — the security-load-bearing assertion — that the CA
// private key can NEVER surface through this endpoint (ActiveCAs reads cert_pem
// alone; design §5).
func TestCA(t *testing.T) {
	st, _, ro, deploy := caTestServer(t)
	ctx := t.Context()

	// Anon → 401.
	anon := &client{t: t, base: ro.base}
	if code, _, _ := rawGet(t, anon); code != http.StatusUnauthorized {
		t.Fatalf("anon /v1/ca: want 401, got %d", code)
	}
	// A key without readonly/admin (deploy) → 403.
	if code, _, _ := rawGet(t, deploy); code != http.StatusForbidden {
		t.Fatalf("deploy /v1/ca: want 403, got %d", code)
	}
	// Before any CA exists → 503, not 500 and not an empty 200 (ansible must
	// never write an empty ca.pem).
	if code, _, _ := rawGet(t, ro); code != http.StatusServiceUnavailable {
		t.Fatalf("no CA yet: want 503, got %d", code)
	}

	// Seed the internal CA — exactly what master does at startup.
	caCert, caKey, err := st.EnsureInternalCA(ctx)
	if err != nil {
		t.Fatal(err)
	}

	code, ctype, body := rawGet(t, ro)
	if code != http.StatusOK {
		t.Fatalf("readonly /v1/ca: want 200, got %d", code)
	}
	if !strings.HasPrefix(ctype, "text/plain") {
		t.Fatalf("content-type: want text/plain, got %q", ctype)
	}
	if !strings.Contains(body, "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("body is not a PEM certificate bundle: %.80q", body)
	}
	if !strings.Contains(body, string(caCert)) {
		t.Fatalf("body does not contain the active CA cert")
	}
	// The CA private key must be unreachable through /v1/ca by construction.
	if strings.Contains(body, "PRIVATE KEY") {
		t.Fatalf("/v1/ca leaked a private key block")
	}
	// Belt-and-braces: the exact key PEM issued alongside the cert must not
	// appear anywhere in the response.
	if strings.Contains(body, string(caKey)) {
		t.Fatalf("/v1/ca leaked the CA signing key")
	}
}

// TestNodesCertFields locks the additive mTLS cert fields in GET /v1/nodes:
// absent (omitempty) for a fresh node, present once the node enrolls
// (design §5). The struct plumbing landed with T2; this pins the REST contract.
func TestNodesCertFields(t *testing.T) {
	st, admin, ro, _ := caTestServer(t)
	ctx := t.Context()

	code, body := admin.do("POST", "/v1/nodes", map[string]any{
		"project": "game", "region": "eu", "hostname": "n1",
		"public_ip": "203.0.113.10", "capacity_slots": 4,
	})
	if code != 201 {
		t.Fatalf("create node: %d %v", code, body)
	}
	nodeID := body["node"].(map[string]any)["id"].(string)

	// Fresh node: none of the three cert fields present.
	code, body = ro.do("GET", "/v1/nodes", nil)
	if code != 200 {
		t.Fatalf("list nodes: %d %v", code, body)
	}
	node := body["nodes"].([]any)[0].(map[string]any)
	for _, f := range []string{"cert_serial", "cert_not_after", "enrolled_at"} {
		if v, ok := node[f]; ok {
			t.Fatalf("fresh node must omit %q, got %v", f, v)
		}
	}

	// Enroll it (token→cert exchange records serial/not_after/enrolled_at).
	notAfter := time.Now().Add(90 * 24 * time.Hour).UTC().Truncate(time.Second)
	if err := st.SetNodeCert(ctx, nodeID, "AABBCCDD", notAfter, store.EventNodeEnrolled, "v1.2.3"); err != nil {
		t.Fatal(err)
	}

	code, body = ro.do("GET", "/v1/nodes", nil)
	if code != 200 {
		t.Fatalf("list nodes (enrolled): %d %v", code, body)
	}
	node = body["nodes"].([]any)[0].(map[string]any)
	if node["cert_serial"] != "AABBCCDD" {
		t.Fatalf("cert_serial: want AABBCCDD, got %v", node["cert_serial"])
	}
	if s, ok := node["cert_not_after"].(string); !ok || s == "" {
		t.Fatalf("cert_not_after should be a non-empty timestamp, got %v", node["cert_not_after"])
	}
	if s, ok := node["enrolled_at"].(string); !ok || s == "" {
		t.Fatalf("enrolled_at should be a non-empty timestamp, got %v", node["enrolled_at"])
	}
}
