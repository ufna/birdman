package reconcile_test

import (
	"io"
	"log/slog"
	"testing"

	"github.com/ufna/birdman/master/internal/reconcile"
	"github.com/ufna/birdman/master/internal/testdb"
)

func TestRuntimeVariablesReachOnlyTheirProjectEnvironment(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 1, 10)
	sender := &fakeSender{}
	vars := map[string]map[string]string{"game/dev": {"CSO_LOBBY_URL": "https://dev.example", "BIRDMAN_ENV": "wrong"}, "game/prod": {"CSO_LOBBY_URL": "https://prod.example"}}
	r := reconcile.New(st, sender, slog.New(slog.NewTextHandler(io.Discard, nil))).WithRuntimeVariables(vars)
	vars["game/dev"]["CSO_LOBBY_URL"] = "mutated"
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, c := range sender.take() {
		if start := c.Msg.GetStart(); start != nil {
			if start.GetEnv()["CSO_LOBBY_URL"] != "https://dev.example" || start.GetEnv()["BIRDMAN_ENV"] != "dev" {
				t.Fatal("runtime variables leaked or overrode owned scope")
			}
			return
		}
	}
	t.Fatal("no StartServer command")
}
