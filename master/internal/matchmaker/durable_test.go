package matchmaker_test

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ufna/birdman/master/internal/matchmaker"
	"github.com/ufna/birdman/master/internal/secrets"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
)

func TestPartyDurableRestartAndOutboxRecovery(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	codec, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	st, dsn := testdb.NewWithCodec(t, codec)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20002, 0)
	p := partyParams("durable", 2)
	mm := newMM(t, st, matchmaker.Config{})
	first, err := mm.Submit(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	// Reopen an independent store before assignment, as after a master crash.
	st2, err := store.Open(t.Context(), dsn, codec)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	restarted := newMM(t, st2, matchmaker.Config{})
	if err = restarted.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	retry, err := restarted.Submit(t.Context(), p)
	if err != nil || retry.ID != first.ID {
		t.Fatalf("queued retry %s: %v", retry.ID, err)
	}
	// No sender simulates a crash after the atomic SQL commit and before any
	// command reaches the in-memory agent hub.
	runOnce(t, restarted)
	assigned := get(t, restarted, first.ID)
	if assigned.Status != matchmaker.StatusMatched {
		t.Fatal("not assigned")
	}
	if got := get(t, mm, first.ID); got.Match.MatchID != assigned.Match.MatchID {
		t.Fatal("old instance ignored SQL assignment")
	}
	var body, metadata, cmdID string
	if err = st.Pool.QueryRow(t.Context(), `select t.body,o.metadata,o.cmd_id::text from matchmaking_tickets t cross join allocation_outbox o where t.id=$1::uuid`, first.ID).Scan(&body, &metadata, &cmdID); err != nil {
		t.Fatal(err)
	}
	for _, envelope := range []string{body, metadata} {
		if !secrets.IsEncrypted(envelope) || strings.Contains(envelope, p.AllocationPayload) {
			t.Fatal("admission payload stored as plaintext")
		}
	}
	// Simulate cache retention passing: the durable idempotency mapping must
	// still survive and must not acquire the remaining ready slot.
	if _, err = st.Pool.Exec(t.Context(), `update matchmaking_tickets set updated_at=now()-interval '1 day'`); err != nil {
		t.Fatal(err)
	}
	third := newMM(t, st2, matchmaker.Config{})
	retry, err = third.Submit(t.Context(), p)
	if err != nil || retry.ID != first.ID || retry.Match.MatchID != assigned.Match.MatchID {
		t.Fatalf("assigned retry: %+v %v", retry, err)
	}
	recorder := &testdb.CommandRecorder{}
	st2.SetCommandSender(recorder)
	runOnce(t, third)
	commands := recorder.Allocates()
	if len(commands) != 1 {
		t.Fatalf("recovered commands=%d", len(commands))
	}
	a := commands[0].Msg.GetAllocate()
	if a.GetCmdId() != cmdID || a.GetMatchId() != assigned.Match.MatchID || a.GetMetadata()["ticket_"+first.ID] != p.AllocationPayload {
		t.Fatal("recovered wrong command/payload")
	}
	// A lost ACK retries the same command id, not another allocation.
	if _, err = st.Pool.Exec(t.Context(), `update allocation_outbox set last_sent_at=now()-interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	if err = st2.DispatchAllocations(t.Context()); err != nil {
		t.Fatal(err)
	}
	commands = recorder.Allocates()
	if len(commands) != 2 || commands[1].Msg.GetAllocate().GetCmdId() != cmdID {
		t.Fatal("retry changed command identity")
	}
	if err = st2.AckAllocation(t.Context(), f.NodeID, cmdID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.Pool.Exec(t.Context(), `update allocation_outbox set last_sent_at=null`); err != nil {
		t.Fatal(err)
	}
	if err = st2.DispatchAllocations(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(recorder.Allocates()) != 2 {
		t.Fatal("ACKed command replayed")
	}
	// Agent process restart loses the UDS frame even when it already ACKed.
	// An authenticated new Hello must re-arm that same active allocation.
	if err = st2.RearmAllocations(t.Context(), f.NodeID); err != nil {
		t.Fatal(err)
	}
	if err = st2.DispatchAllocations(t.Context()); err != nil {
		t.Fatal(err)
	}
	commands = recorder.Allocates()
	if len(commands) != 3 || commands[2].Msg.GetAllocate().GetCmdId() != cmdID || commands[2].Msg.GetAllocate().GetMetadata()["ticket_"+first.ID] != p.AllocationPayload {
		t.Fatal("agent restart lost ACKed allocation payload")
	}
	var allocated int
	if err = st.Pool.QueryRow(t.Context(), `select count(*) from servers where state='allocated'`).Scan(&allocated); err != nil || allocated != 1 {
		t.Fatalf("allocated=%d err=%v", allocated, err)
	}
}

func TestPartyAtomicClaimRollsBackWithAssignment(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	mm := newMM(t, st, matchmaker.Config{})
	p := partyParams("rollback", 2)
	tk, err := mm.Submit(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.Pool.Exec(t.Context(), `create function reject_assignment() returns trigger language plpgsql as $$ begin if NEW.status='matched' then raise exception 'injected assignment failure'; end if; return NEW; end $$; create trigger reject_assignment before update on matchmaking_tickets for each row execute function reject_assignment()`)
	if err != nil {
		t.Fatal(err)
	}
	_ = mm.RunOnce(t.Context())
	var allocated, commands int
	if err = st.Pool.QueryRow(t.Context(), `select (select count(*) from servers where state='allocated'),(select count(*) from allocation_outbox)`).Scan(&allocated, &commands); err != nil {
		t.Fatal(err)
	}
	if allocated != 0 || commands != 0 || get(t, mm, tk.ID).Status != matchmaker.StatusQueued {
		t.Fatal("claim/outbox committed without ticket assignment")
	}
	if _, err = st.Pool.Exec(t.Context(), `drop trigger reject_assignment on matchmaking_tickets`); err != nil {
		t.Fatal(err)
	}
	runOnce(t, mm)
	if get(t, mm, tk.ID).Status != matchmaker.StatusMatched {
		t.Fatal("retry failed after transaction rollback")
	}
}

func TestPartyTwoInstancesCannotDoubleClaimOrResurrectCancel(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	f.UpsertFleet(t, 2, 50)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20001, 0)
	f.InsertServer(t, f.NodeID, f.VersionID, "ready", 20002, 0)
	left, right := newMM(t, st, matchmaker.Config{}), newMM(t, st, matchmaker.Config{})
	p := partyParams("instances", 2)
	tk, err := left.Submit(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, mm := range []*matchmaker.Matchmaker{left, right} {
		wg.Go(func() {
			if err := mm.RunOnce(t.Context()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var count int
	if err = st.Pool.QueryRow(t.Context(), `select count(*) from allocation_outbox`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("commands=%d err=%v", count, err)
	}
	if get(t, left, tk.ID).Match.MatchID != get(t, right, tk.ID).Match.MatchID {
		t.Fatal("instances disagreed on assignment")
	}
	p2 := partyParams("cancel-durable", 1)
	tk2, err := left.Submit(t.Context(), p2)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := right.CancelContext(t.Context(), tk2.ID); err != nil || !ok {
		t.Fatal(err)
	}
	got, err := left.Submit(t.Context(), p2)
	if err != nil || got.Status != matchmaker.StatusCancelled {
		t.Fatal("retry resurrected cancelled ticket across instances")
	}
	// Database errors must surface to callers rather than masquerade as 404.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := left.GetContext(ctx, tk.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("get error=%v", err)
	}
}
