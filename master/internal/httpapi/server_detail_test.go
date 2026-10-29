package httpapi_test

import (
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

func TestServerDetailPreservesBoundEnvironmentAndTerminalState(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	id := f.InsertServer(t, f.NodeID, f.VersionID, "failed", 20001, 0)
	ts := mmServer(t, st, matchmaker.Config{Tick: time.Hour})
	project, dev, prod := "game", "dev", "prod"
	makeClient := func(env string) *client {
		_, key, err := st.CreateAPIKey(t.Context(), store.CreateAPIKeyParams{Name: "detail-" + env, Scopes: []string{httpapi.ScopeReadonly}, Project: &project, Env: &env})
		if err != nil {
			t.Fatal(err)
		}
		return &client{t: t, base: ts.URL, key: key}
	}
	if code, body := makeClient(dev).do("GET", "/v1/servers/"+id, nil); code != 200 || body["server"].(map[string]any)["state"] != "failed" {
		t.Fatalf("server detail: %d %v", code, body)
	}
	foreign := makeClient(prod)
	code, foreignBody := foreign.do("GET", "/v1/servers/"+id, nil)
	missingCode, missingBody := foreign.do("GET", "/v1/servers/00000000-0000-0000-0000-000000000001", nil)
	if code != 404 || missingCode != 404 || foreignBody["detail"] != missingBody["detail"] {
		t.Fatal("foreign server existence leaked")
	}
}
