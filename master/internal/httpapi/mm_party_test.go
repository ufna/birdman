package httpapi_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ufna/birdman/master/internal/httpapi"
	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

func TestPartyRESTBindingRetryAndPayloadIsolation(t *testing.T) {
	st := testdb.New(t)
	testdb.Seed(t, st, "eu", 10)
	ts := mmServer(t, st, matchmaker.Config{Tick: time.Hour})
	project, env := "game", "dev"
	_, key, err := st.CreateAPIKey(t.Context(), store.CreateAPIKeyParams{Name: "party-backend", Scopes: []string{httpapi.ScopeMatchmaking}, Project: &project, Env: &env})
	if err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, base: ts.URL, key: key}
	body := map[string]any{
		"party_id": uuid.NewString(), "party_revision": 1, "client_request_id": uuid.NewString(), "client_version": "1.0.0", "allocation_payload": "private-admission-fixture",
		"members": []map[string]any{{"player_id": "a", "regions": []map[string]any{{"region": "eu", "rtt_ms": 10}}}, {"player_id": "b", "regions": []map[string]any{{"region": "eu", "rtt_ms": 20}}}},
	}
	code, got := c.do("POST", "/v1/matchmaking/tickets", body)
	if code != 201 || got["project"] != "game" || got["env"] != "dev" {
		t.Fatalf("bound party: %d %v", code, got)
	}
	id := got["ticket_id"].(string)
	code, retry := c.do("POST", "/v1/matchmaking/tickets", body)
	if code != 201 || retry["ticket_id"] != id {
		t.Fatalf("retry: %d %v", code, retry)
	}
	raw, _ := json.Marshal(retry)
	if strings.Contains(string(raw), "private-admission") || strings.Contains(string(raw), "allocation_payload") {
		t.Fatal("payload exposed by REST")
	}
	body["allocation_payload"] = "changed"
	if code, got := c.do("POST", "/v1/matchmaking/tickets", body); code != 409 || got["error"] != "request_conflict" {
		t.Fatalf("conflict: %d %v", code, got)
	}
	otherEnv := "prod"
	_, foreignKey, err := st.CreateAPIKey(t.Context(), store.CreateAPIKeyParams{Name: "foreign-party-backend", Scopes: []string{httpapi.ScopeMatchmaking}, Project: &project, Env: &otherEnv})
	if err != nil {
		t.Fatal(err)
	}
	foreign := &client{t: t, base: ts.URL, key: foreignKey}
	for _, method := range []string{"GET", "DELETE"} {
		if code, _ := foreign.do(method, "/v1/matchmaking/tickets/"+id, nil); code != 404 {
			t.Fatalf("foreign %s: %d", method, code)
		}
	}
	body["env"] = "dev"
	if code, _ := foreign.do("POST", "/v1/matchmaking/tickets", body); code != 403 {
		t.Fatalf("foreign submit: %d", code)
	}
}
