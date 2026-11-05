package httpapi_test

import (
	"net/http/httptest"
	"testing"

	"github.com/ufna/birdman/master/internal/deploy"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/metrics"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

func TestNodeAddressAPIRequiresAdminAndExplicitReplacement(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 4)
	ctx := t.Context()
	log := opsLog()
	m := metrics.New(st, log)
	mm := matchmaker.New(st, m, matchmaker.Config{}, log)
	dep := deploy.New(deploy.Options{Store: st, Sender: &testdb.CommandRecorder{}, Log: log})
	ts := httptest.NewServer(httpapi.New(st, m, mm, dep, nil, nil, "", "", log))
	defer ts.Close()
	_, adminKey, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "admin", Scopes: []string{httpapi.ScopeAdmin}})
	if err != nil {
		t.Fatal(err)
	}
	_, roKey, err := st.CreateAPIKey(ctx, store.CreateAPIKeyParams{Name: "readonly", Scopes: []string{httpapi.ScopeReadonly}})
	if err != nil {
		t.Fatal(err)
	}
	admin := client{t: t, base: ts.URL, key: adminKey}
	ro := client{t: t, base: ts.URL, key: roKey}
	path := "/v1/nodes/" + f.NodeID + "/reconcile-address"
	body := map[string]any{"project": "game", "env": "dev", "region": "eu", "hostname": "node-1", "expected_public_ip": "203.0.113.10", "public_ip": "198.51.100.22"}
	if code, _ := ro.do("POST", path, body); code != 403 {
		t.Fatalf("readonly=%d", code)
	}
	if code, _ := admin.do("POST", path, body); code != 409 {
		t.Fatalf("active=%d", code)
	}
	if _, err = st.DrainNode(ctx, f.NodeID); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", "not-an-ip", "0.0.0.0", "127.0.0.1", "224.0.0.1", "::", "::1"} {
		body["public_ip"] = invalid
		if code, _ := admin.do("POST", path, body); code != 400 {
			t.Fatalf("invalid %q=%d", invalid, code)
		}
	}
	body["public_ip"] = "198.51.100.22"
	if code, _ := admin.do("POST", path, body); code != 200 {
		t.Fatalf("repair=%d", code)
	}
	if code, _ := admin.do("POST", path, body); code != 200 {
		t.Fatalf("replay=%d", code)
	}
}
