package matchmaker

import (
	"testing"
	"time"

	"github.com/ufna/birdman/master/internal/store"
)

func TestPartyMinimaxRegionAndPackingFairness(t *testing.T) {
	now := time.Now()
	p := SubmitParams{PartyID: "00000000-0000-0000-0000-000000000001", PartyRevision: 1, ClientRequestID: "00000000-0000-0000-0000-000000000002", Members: []Member{
		{PlayerID: "a", Regions: []RegionPing{{Region: "eu", RTTms: 5}, {Region: "us", RTTms: 80}}},
		{PlayerID: "b", Regions: []RegionPing{{Region: "eu", RTTms: 150}, {Region: "us", RTTms: 90}}},
	}}
	_, regs, err := normalizeParty(p)
	if err != nil {
		t.Fatal(err)
	}
	mm := &Matchmaker{cfg: Config{}.withDefaults()}
	pool := map[string]qt{"party": {id: "party", players: 2, party: true, created: now, regions: regs}}
	verFor := map[string]store.RegionVersion{"eu": {}, "us": {}}
	region, group := mm.bestGroup(pool, 2, verFor, nil, now, QueuePolicy{MinimumSize: 2})
	if region != "us" || len(group) != 1 {
		t.Fatalf("minimax region=%s group=%v", region, group)
	}
	// A fast anchor must not hide the slowest participant when regions widen.
	region, _ = mm.bestGroup(pool, 2, verFor, nil, now.Add(time.Minute), QueuePolicy{MinimumSize: 2})
	if region != "us" {
		t.Fatalf("widened minimax region=%s", region)
	}
	delete(verFor, "us")
	region, _ = mm.bestGroup(pool, 2, verFor, nil, now.Add(time.Minute), QueuePolicy{MinimumSize: 2, MaxRTTms: 120, ExpandedMaxRTTms: 140})
	if region != "" {
		t.Fatal("slow member bypassed hard RTT cap")
	}

	eligible := []qt{
		{id: "old-three", players: 3, created: now.Add(-time.Minute)},
		{id: "two-a", players: 2, created: now.Add(-time.Second)},
		{id: "two-b", players: 2, created: now},
		{id: "solo", players: 1, created: now},
	}
	g := pack(eligible, 4, QueuePolicy{MinimumSize: 4}, now)
	if len(g) != 2 || g[0].id != "old-three" || g[1].id != "solo" {
		t.Fatalf("old party starved: %v", g)
	}
	// A partial group cannot discard a party to fill the target exactly.
	g = pack(eligible[:1], 4, QueuePolicy{MinimumSize: 1, PartialWait: 30 * time.Second}, now)
	if len(g) != 1 || g[0].players != 3 {
		t.Fatalf("partial split: %v", g)
	}
	if g = pack(eligible[:1], 4, QueuePolicy{MinimumSize: 4, PartialWait: 30 * time.Second}, now); g != nil {
		t.Fatal("partial bypassed minimum")
	}
}

func TestQueuePolicyValidation(t *testing.T) {
	base := QueuePolicy{Project: "game", Env: "dev", MinimumSize: 1, PartialWait: time.Second, MaxRTTms: 120, ExpandedMaxRTTms: 150}
	if err := ValidatePolicies([]QueuePolicy{base}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []QueuePolicy{{}, {Project: "game", Env: "dev", MinimumSize: 0}, {Project: "game", Env: "dev", MinimumSize: 1, PartialWait: -1}, {Project: "game", Env: "dev", MinimumSize: 1, MaxRTTms: 150, ExpandedMaxRTTms: 120}} {
		if err := ValidatePolicies([]QueuePolicy{p}); err == nil {
			t.Fatalf("accepted invalid policy: %+v", p)
		}
	}
	if err := ValidatePolicies([]QueuePolicy{base, base}); err == nil {
		t.Fatal("accepted duplicate policy")
	}
}
