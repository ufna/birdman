package matchmaker_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/testdb"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

func partyParams(prefix string, n int) matchmaker.SubmitParams {
	p := matchmaker.SubmitParams{Project: "game", Env: "dev", ClientVersion: "1.0.0",
		PartyID: uuid.NewString(), PartyRevision: 1, ClientRequestID: uuid.NewString(), AllocationPayload: "admission-fixture-" + prefix}
	for i := 0; i < n; i++ {
		p.Members = append(p.Members, matchmaker.Member{PlayerID: fmt.Sprintf("%s-%d", prefix, i), Regions: regions("eu", 10)})
	}
	return p
}

func TestPartyPackingPartitions(t *testing.T) {
	for _, partition := range [][]int{{4}, {3, 1}, {2, 2}, {2, 1, 1}, {1, 1, 1, 1}, {2, 3, 1}} {
		t.Run(fmt.Sprint(partition), func(t *testing.T) {
			st := testdb.New(t)
			f := testdb.Seed(t, st, "eu", 10)
			if _, err := st.SetProjectMatchSize(t.Context(), "game", 4); err != nil {
				t.Fatal(err)
			}
			f.UpsertFleet(t, 2, 50)
			f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
			rec := &testdb.CommandRecorder{}
			st.SetCommandSender(rec)
			mm := newMM(t, st, matchmaker.Config{})
			var tickets []matchmaker.Ticket
			for i, n := range partition {
				tk, err := mm.Submit(t.Context(), partyParams(fmt.Sprint(i), n))
				if err != nil {
					t.Fatal(err)
				}
				tickets = append(tickets, tk)
			}
			runOnce(t, mm)
			matchID := ""
			seats := 0
			matched := 0
			for i, tk := range tickets {
				got := get(t, mm, tk.ID)
				if got.Status != matchmaker.StatusMatched {
					continue
				}
				seats += partition[i]
				matched++
				if matchID != "" && matchID != got.Match.MatchID {
					t.Fatal("party split across allocations")
				}
				matchID = got.Match.MatchID
				if got.Match.ServerID == "" || got.Match.VersionID != f.VersionID {
					t.Fatalf("missing assignment: %+v", got.Match)
				}
				raw, _ := json.Marshal(got)
				if strings.Contains(string(raw), "admission-fixture") || strings.Contains(string(raw), "allocation_payload") {
					t.Fatal("payload leaked in ticket")
				}
				// Returned slices must never allow a caller to mutate queue state.
				got.Members[0].Regions[0].RTTms = 999
				got.Match.TicketIDs[0] = "changed"
				if fresh := get(t, mm, tk.ID); fresh.Members[0].Regions[0].RTTms == 999 || fresh.Match.TicketIDs[0] == "changed" {
					t.Fatal("mutable snapshot")
				}
			}
			if seats != 4 {
				t.Fatalf("matched seats=%d, want 4", seats)
			}
			allocs := rec.Allocates()
			if len(allocs) != 1 {
				t.Fatalf("allocations=%d", len(allocs))
			}
			a := allocs[0].Msg.GetAllocate()
			if a.GetPlayersExpected() != 4 || len(a.GetMetadata()) != matched {
				t.Fatalf("bad SDK payload: %v", a)
			}
			for _, tk := range tickets {
				got := get(t, mm, tk.ID)
				if got.Status == matchmaker.StatusMatched {
					if len(got.Match.TicketIDs) != matched || a.GetMetadata()["ticket_"+tk.ID] == "" {
						t.Fatal("missing party reference or private allocation payload")
					}
				}
			}
		})
	}
}

func TestPartyIdempotencyValidationAndWholeCancellation(t *testing.T) {
	st := testdb.New(t)
	testdb.Seed(t, st, "eu", 10)
	if _, err := st.SetProjectMatchSize(t.Context(), "game", 4); err != nil {
		t.Fatal(err)
	}
	mm := newMM(t, st, matchmaker.Config{})
	p := partyParams("party", 3)
	first, err := mm.Submit(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			tk, e := mm.Submit(t.Context(), p)
			if e != nil || tk.ID != first.ID {
				t.Errorf("retry: %s %v", tk.ID, e)
			}
		})
	}
	wg.Wait()
	changed := p
	changed.AllocationPayload = "different"
	if _, err := mm.Submit(t.Context(), changed); !errors.Is(err, matchmaker.ErrRequestConflict) {
		t.Fatalf("changed request: %v", err)
	}
	for name, mutate := range map[string]func(*matchmaker.SubmitParams){
		"mixed":     func(p *matchmaker.SubmitParams) { p.PlayerID = "solo" },
		"duplicate": func(p *matchmaker.SubmitParams) { p.Members = append(p.Members, p.Members[0]) },
		"oversized": func(p *matchmaker.SubmitParams) {
			for i := 0; i < 2; i++ {
				p.Members = append(p.Members, matchmaker.Member{PlayerID: fmt.Sprint(i), Regions: regions("eu", 1)})
			}
		},
		"payload":          func(p *matchmaker.SubmitParams) { p.AllocationPayload = strings.Repeat("x", 513) },
		"no-common-region": func(p *matchmaker.SubmitParams) { p.Members[0].Regions = regions("us", 1) },
		"empty-members":    func(p *matchmaker.SubmitParams) { p.Members = []matchmaker.Member{} },
		"missing-request":  func(p *matchmaker.SubmitParams) { p.ClientRequestID = "" },
		"invalid-request":  func(p *matchmaker.SubmitParams) { p.ClientRequestID = "not-uuid" },
		"bad-revision":     func(p *matchmaker.SubmitParams) { p.PartyRevision = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := partyParams("invalid", 3)
			mutate(&bad)
			if _, err := mm.Submit(t.Context(), bad); !errors.Is(err, matchmaker.ErrInvalid) {
				t.Fatalf("want invalid, got %v", err)
			}
			if get(t, mm, first.ID).Status != matchmaker.StatusQueued {
				t.Fatal("invalid request cancelled accepted party")
			}
		})
	}
	// An overlap cancels the entire old party, freeing every other member.
	p2 := partyParams("replacement", 1)
	p2.Members[0].PlayerID = p.Members[1].PlayerID
	replacement, err := mm.Submit(t.Context(), p2)
	if err != nil {
		t.Fatal(err)
	}
	if get(t, mm, first.ID).Status != matchmaker.StatusCancelled {
		t.Fatal("overlap did not cancel whole party")
	}
	retried, err := mm.Submit(t.Context(), p)
	if err != nil || retried.ID != first.ID || retried.Status != matchmaker.StatusCancelled {
		t.Fatal("retry resurrected cancelled party")
	}
	if get(t, mm, replacement.ID).Status != matchmaker.StatusQueued {
		t.Fatal("retry cancelled replacement")
	}
	// A new revision with a disjoint roster still cancels the old party.
	p3 := partyParams("new-roster", 1)
	p3.PartyID = p2.PartyID
	p3.PartyRevision = 2
	if _, err := mm.Submit(t.Context(), p3); err != nil {
		t.Fatal(err)
	}
	if get(t, mm, replacement.ID).Status != matchmaker.StatusCancelled {
		t.Fatal("revision failed to cancel disjoint old roster")
	}
	stale := p2
	stale.ClientRequestID = uuid.NewString()
	if _, err := mm.Submit(t.Context(), stale); !errors.Is(err, matchmaker.ErrRequestConflict) {
		t.Fatalf("stale revision: %v", err)
	}
}

func TestPartyPartialStartAndRTTCaps(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	if _, err := st.SetProjectMatchSize(t.Context(), "game", 4); err != nil {
		t.Fatal(err)
	}
	f.UpsertFleet(t, 2, 50)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	wait := 80 * time.Millisecond
	mm := newMM(t, st, matchmaker.Config{WidenAfter: wait, Policies: []matchmaker.QueuePolicy{{Project: "game", Env: "dev", MinimumSize: 1, PartialWait: wait, MaxRTTms: 120, ExpandedMaxRTTms: 150}}})
	p := partyParams("slow", 2)
	p.Members[1].Regions = regions("eu", 140)
	tk, err := mm.Submit(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	runOnce(t, mm)
	if get(t, mm, tk.ID).Status != matchmaker.StatusQueued {
		t.Fatal("started early or exceeded initial RTT cap")
	}
	time.Sleep(2 * wait)
	runOnce(t, mm)
	if got := get(t, mm, tk.ID); got.Status != matchmaker.StatusMatched {
		t.Fatal("partial did not start after wait and RTT expansion")
	}
	// Full-only remains the default even for parties that have waited.
	legacy := newMM(t, st, matchmaker.Config{})
	tk, err = legacy.Submit(t.Context(), partyParams("default", 1))
	if err != nil {
		t.Fatal(err)
	}
	runOnce(t, legacy)
	if get(t, legacy, tk.ID).Status != matchmaker.StatusQueued {
		t.Fatal("default partial start enabled")
	}
}

func TestPartyConcurrentTicksClaimExactlyOnce(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20002, 0)
	rec := &testdb.CommandRecorder{}
	st.SetCommandSender(rec)
	mm := newMM(t, st, matchmaker.Config{})
	tk, err := mm.Submit(t.Context(), partyParams("pair", 2))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if err := mm.RunOnce(t.Context()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if get(t, mm, tk.ID).Status != matchmaker.StatusMatched || len(rec.Allocates()) != 1 {
		t.Fatal("concurrent ticks allocated a party twice")
	}
	var allocated int
	if err := st.Pool.QueryRow(t.Context(), "select count(*) from servers where state='allocated'").Scan(&allocated); err != nil || allocated != 1 {
		t.Fatalf("allocated=%d err=%v", allocated, err)
	}
}

type gatedAllocationSender struct {
	entered chan struct{}
	release chan struct{}
}

func (g *gatedAllocationSender) Send(_ string, msg *agentlinkv1.MasterMsg) string {
	if msg.GetAllocate() != nil {
		close(g.entered)
		<-g.release
	}
	return uuid.NewString()
}

func TestPartyCancelCannotSplitInFlightClaim(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	gate := &gatedAllocationSender{entered: make(chan struct{}), release: make(chan struct{})}
	st.SetCommandSender(gate)
	mm := newMM(t, st, matchmaker.Config{})
	tk, err := mm.Submit(t.Context(), partyParams("fenced", 2))
	if err != nil {
		t.Fatal(err)
	}
	// A bounded gate pins the exact window after the SQL claim and before
	// its result reaches the queue, rather than hoping for a timing race.
	var once sync.Once
	defer once.Do(func() { close(gate.release) })
	done := make(chan error, 1)
	go func() { done <- mm.RunOnce(t.Context()) }()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("claim did not reach sender")
	}
	cancelled := make(chan matchmaker.Ticket, 1)
	go func() { snap, _ := mm.Cancel(tk.ID); cancelled <- snap }()
	select {
	case <-cancelled:
		t.Fatal("cancel crossed the allocation fence")
	case <-time.After(20 * time.Millisecond):
	}
	once.Do(func() { close(gate.release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case snap := <-cancelled:
		if snap.Status != matchmaker.StatusMatched || len(snap.Members) != 2 {
			t.Fatalf("cancel split assigned party: %+v", snap)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel stayed blocked after claim")
	}
}
