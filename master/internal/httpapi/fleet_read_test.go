package httpapi_test

import (
	"testing"

	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

func TestFleetReadEnforcesProjectEnvironmentAndScope(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 1)
	f.UpsertFleet(t, 1, 1)
	if _, err := st.CreateProject(t.Context(), "neighbour", 4); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{f.Project, "prod"}, {"neighbour", "dev"}} {
		if _, err := st.UpsertFleet(t.Context(), store.UpsertFleetParams{Project: pair[0], Env: pair[1], Region: "eu"}); err != nil {
			t.Fatal(err)
		}
	}
	ts, _, _ := deployServer(t, st)
	key := func(name string, scopes []string, project, env *string) string {
		_, secret, err := st.CreateAPIKey(t.Context(), store.CreateAPIKeyParams{Name: name, Scopes: scopes, Project: project, Env: env})
		if err != nil {
			t.Fatal(err)
		}
		return secret
	}
	global := &client{t: t, base: ts.URL, key: key("global", []string{httpapi.ScopeReadonly}, nil, nil)}
	project, env := f.Project, f.Env
	bound := &client{t: t, base: ts.URL, key: key("bound", []string{httpapi.ScopeReadonly}, &project, &env)}
	for _, tc := range []struct {
		caller *client
		query  string
		count  int
	}{
		{global, "", 3},
		{global, "?project=" + project, 2},
		{global, "?project=" + project + "&env=" + env, 1},
		{bound, "", 1},
		{bound, "?project=" + project + "&env=" + env, 1},
	} {
		code, body := tc.caller.do("GET", "/v1/fleets"+tc.query, nil)
		fleets, ok := body["fleets"].([]any)
		if code != 200 || !ok || len(fleets) != tc.count {
			t.Fatalf("fleet listing %s: status=%d count=%d", tc.query, code, len(fleets))
		}
		if tc.caller == bound {
			for _, raw := range fleets {
				fleet := raw.(map[string]any)
				if fleet["project"] != project || fleet["env"] != env || fleet["buffer_ready"] != float64(1) || fleet["max_servers"] != float64(1) {
					t.Fatal("bound key observed another project/environment or incorrect desired state")
				}
			}
		}
	}
	for _, query := range []string{"?project=neighbour", "?env=prod", "?project=missing", "?env=missing"} {
		if code, _ := bound.do("GET", "/v1/fleets"+query, nil); code != 403 {
			t.Fatalf("bound fleet query %s: got %d, want 403", query, code)
		}
	}
	for _, query := range []string{"?project=missing", "?project=" + project + "&env=missing"} {
		if code, _ := global.do("GET", "/v1/fleets"+query, nil); code != 400 {
			t.Fatalf("invalid global fleet query %s: got %d, want 400", query, code)
		}
	}
	anonymous := &client{t: t, base: ts.URL}
	if code, _ := anonymous.do("GET", "/v1/fleets", nil); code != 401 {
		t.Fatal("anonymous fleet access accepted")
	}
	deployOnly := &client{t: t, base: ts.URL, key: key("deploy", []string{httpapi.ScopeDeploy}, nil, nil)}
	if code, _ := deployOnly.do("GET", "/v1/fleets", nil); code != 403 {
		t.Fatal("deploy-only key read fleet configuration")
	}
}
