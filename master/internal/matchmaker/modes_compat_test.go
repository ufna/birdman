package matchmaker

import (
	"crypto/sha256"
	"encoding/json"
	"testing"
)

func TestLegacyRequestFingerprintSurvivesGameModeField(t *testing.T) {
	// This is the exact pre-game_mode schema, including its JSON field order.
	legacy := struct {
		Project, Env, PlayerID, ClientVersion string
		Regions                               []RegionPing
		Members                               []Member
		PartyID                               string
		PartyRevision                         int64
		ClientRequestID, AllocationPayload    string
	}{Project: "game", Env: "dev", PlayerID: "legacy", ClientVersion: "1.0.0", Regions: []RegionPing{{Region: "eu", RTTms: 10}}, ClientRequestID: "test-request"}
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	p := SubmitParams{Project: legacy.Project, Env: legacy.Env, PlayerID: legacy.PlayerID, ClientVersion: legacy.ClientVersion, Regions: legacy.Regions, ClientRequestID: legacy.ClientRequestID}
	if requestFingerprint(p) != sha256.Sum256(b) {
		t.Fatal("legacy durable request fingerprint changed")
	}
	p.GameMode = "vr"
	if requestFingerprint(p) == sha256.Sum256(b) {
		t.Fatal("explicit mode must participate in retry identity")
	}
}
