package config

import "testing"

func TestGameRuntimeReservesRuntimeContractAndProjectIsolation(t *testing.T) {
	withDSN(t)
	valid := "game_runtime:\n  - project: game\n    env: dev\n    variables:\n      CSO_LOBBY_URL: https://example.test\n"
	cfg, err := Load(writeConfig(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GameRuntimeVariables()["game/dev"]["CSO_LOBBY_URL"] != "https://example.test" {
		t.Fatal("runtime config lost")
	}
	for _, bad := range []string{
		"game_runtime:\n  - project: game\n    env: dev\n    variables:\n      BIRDMAN_ENV: prod\n",
		"game_runtime:\n  - project: game\n    env: dev\n  - project: game\n    env: dev\n",
		"game_runtime:\n  - project: game\n    env: dev\n    variables:\n      1BAD: x\n",
	} {
		if _, err := Load(writeConfig(t, bad)); err == nil {
			t.Fatal("accepted invalid runtime config")
		}
	}
}
