package agentlink

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"sync"
	"testing"

	"github.com/ufna/birdman/master/internal/testdb"
)

// TestServiceRegistriesSnapshotReadEnqueueSerializedAcrossAttachAndBroadcast
// is a regression test for task-review Fix 1 (registries-v1 branch,
// final-review round): registriesSnapshot's DB read (store.ListRegistryCreds)
// and the corresponding hub mutation (attach/Send) must be ONE atomic step
// with respect to every OTHER concurrent attach/broadcast on the Service.
//
// The earlier hub-level fix (TestHubSendPushOrderMatchesMutationOrderUnderConcurrency)
// guarantees that whichever hub.attach/hub.Send call the mutex (hub.mu)
// serializes LAST is also delivered last. But that says nothing about WHICH
// read that last call carries: without a Service-level lock, two concurrent
// callers (two broadcasts, or an attach racing a broadcast — the exact
// "reconnect-during-edit" scenario) each do their own read-then-enqueue
// independently. Goroutine A can read an OLDER snapshot, be preempted before
// calling into the hub, while goroutine B's edit commits and B's own
// (read-newer, enqueue) completes first — then A's stale enqueue lands last
// and wins, because SetRegistries coalesces to a full-replace: the hub
// (correctly, per its own contract) delivers whatever was enqueued last, but
// "last enqueued" and "most recent state" have quietly come apart.
//
// Fix: Service.regMu wraps the read+enqueue pair as a whole, in BOTH
// attachWithFreshRegistries and BroadcastRegistries (service.go) — so the
// order in which reads are serialized is, by construction, identical to the
// order in which they are enqueued, and combined with hub.mu's push-order
// guarantee, whichever caller's read is chronologically last is also
// delivered last.
//
// Reproduction strategy (mirrors the hub-level test's high-iteration stress
// style, scaled down from pure-in-memory 200x24 since this one drives real
// Postgres round trips): each round fires a burst of concurrent workers that
// EACH commit a distinct new registry row (so the round's contribution to
// "current state" is unambiguous — different hosts never race each other at
// the row level) and then either broadcast or (one worker per round)
// reconnect the node, simulating reconnect-during-edit. Once wg.Wait()
// returns, EVERY worker's own critical section (read+enqueue) has completed;
// by a happens-before argument (each worker's write commits, by program
// order, strictly before that worker's own read — and every worker's
// critical section is included in the round's total order), the
// mutex-serialized LAST critical section of the round is guaranteed to have
// read a snapshot containing every row committed by any worker in the round.
// So after wg.Wait(), st.ListRegistryCreds(ctx) — sampled from the test
// goroutine — must equal what that last critical section read, and hence
// (given the hub's push-order guarantee) what it delivered last. A
// mismatch is a real staler-after-fresher delivery, not test noise.
func TestServiceRegistriesSnapshotReadEnqueueSerializedAcrossAttachAndBroadcast(t *testing.T) {
	st := testdb.New(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	hub := NewHub(log)
	svc := NewService(st, hub, nil, nil, AuthMixed, log)
	ctx := context.Background()

	const nodeID = "node-under-test"

	var sessMu sync.Mutex
	curSess := svc.attachWithFreshRegistries(ctx, nodeID, false, false)

	const rounds = 60
	const workersPerRound = 16

	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		wg.Add(workersPerRound + 1)

		for w := 0; w < workersPerRound; w++ {
			r, w := r, w
			go func() {
				defer wg.Done()
				host := fmt.Sprintf("r%d-w%d.example", r, w)
				if _, err := st.UpsertRegistry(ctx, host, "u", "t", ""); err != nil {
					t.Errorf("round %d worker %d: upsert: %v", r, w, err)
					return
				}
				svc.BroadcastRegistries(ctx)
			}()
		}
		// One worker per round reconnects the SAME node concurrently with
		// the edits above — reconnect-during-edit, the scenario named in the
		// task review.
		go func() {
			defer wg.Done()
			newSess := svc.attachWithFreshRegistries(ctx, nodeID, false, false)
			sessMu.Lock()
			curSess = newSess
			sessMu.Unlock()
		}()

		wg.Wait()

		want, err := st.ListRegistryCreds(ctx)
		if err != nil {
			t.Fatalf("round %d: ListRegistryCreds: %v", r, err)
		}
		wantHosts := make([]string, 0, len(want))
		for _, c := range want {
			wantHosts = append(wantHosts, c.Host)
		}
		sort.Strings(wantHosts)

		sessMu.Lock()
		sess := curSess
		sessMu.Unlock()

		drained := drainAllSetRegistriesHostSets(sess)
		if len(drained) == 0 {
			t.Fatalf("round %d: no SetRegistries delivered to the current session — coalescing/delivery invariant broken", r)
		}
		last := drained[len(drained)-1]
		if !slices.Equal(last, wantHosts) {
			t.Fatalf("round %d: the LAST delivered snapshot does not match the store's current state "+
				"(stale-after-fresh delivery — task review, Fix 1).\nwant (%d hosts): %v\ngot  (%d hosts): %v\nall delivered this round: %v",
				r, len(wantHosts), wantHosts, len(last), last, drained)
		}
	}
}

// drainAllSetRegistriesHostSets reads every currently-buffered SetRegistries
// message off sess.out without blocking, in arrival order, returning each
// one's sorted host list — mirrors hub_test.go's drainSetRegistries but
// preserves the full per-message registry set (a broadcast/attach here can
// legitimately carry more than one registry) instead of a single tag host.
func drainAllSetRegistriesHostSets(sess *session) [][]string {
	var out [][]string
	for {
		select {
		case m := <-sess.out:
			sr := m.GetSetRegistries()
			if sr == nil {
				continue
			}
			hosts := make([]string, 0, len(sr.GetRegistries()))
			for _, c := range sr.GetRegistries() {
				hosts = append(hosts, c.GetHost())
			}
			sort.Strings(hosts)
			out = append(out, hosts)
		default:
			return out
		}
	}
}
