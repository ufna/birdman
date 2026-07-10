package agentlink

import (
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"

	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// TestHubSendPushOrderMatchesMutationOrderUnderConcurrency is a regression
// test for a delivery-order inversion (task review, Fix 1 — registries-v1
// branch): Send used to mutate q.pending under h.mu, unlock, and only THEN
// push onto the live session's channel (attach had the same shape, before
// this fix). Two goroutines racing Send on the same node's session could
// therefore push onto the channel in an order that differs from the order
// their mutations were serialized in by the mutex: whichever goroutine's
// unlock-then-push happened to get scheduled first would land on the wire
// first, regardless of which mutation was chronologically last.
//
// This matters specifically for SetRegistries: Hub coalesces it (never holds
// more than one pending SetRegistries per node — removeSetRegistries) and
// the agent applies it as a FULL REPLACE of its credential set. If delivery
// order inverts, the agent's last-applied snapshot can be the STALE one,
// silently, until the next registries change or reconnect.
//
// Fix: push now happens INSIDE the same h.mu critical section as the
// mutation (both in Send and attach) — push is already non-blocking
// (select/default on a buffered channel), so this adds no unbounded lock
// hold time. Because only one goroutine can be inside that critical section
// at a time, the order pushes reach the channel is now, by construction,
// exactly the mutex's own serialization order: state-mutation order becomes
// delivery order.
//
// A deterministic RED against the pre-fix code is not practical here — the
// bug is a goroutine-scheduling race (the unlock→push gap is nanoseconds
// wide), not a bug reachable via a fixed sequence of calls — so this is a
// high-iteration stress test instead: many independent rounds, each firing a
// burst of concurrent Sends at one live (drained) session, then reading the
// hub's OWN post-race state (q.pending's coalesced SetRegistries — the
// ground truth for "whose mutation the mutex serialized last") and checking
// that entry, whenever it was actually delivered (a drop from a full channel
// is expected/acceptable behavior, not a bug — see hub.go's session.push),
// was delivered LAST — never followed by an older, already-superseded
// snapshot. Run with -race; the actual guarantee against the bug is the code
// change, not this test having achieved a deterministic pre-fix failure.
func TestHubSendPushOrderMatchesMutationOrderUnderConcurrency(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	hub := NewHub(log)
	const nodeID = "node-under-test"
	sess := hub.attach(nodeID, nil, false, false)

	const rounds = 200
	const workersPerRound = 24

	setRegistries := func(tag string) *agentlinkv1.MasterMsg {
		return &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_SetRegistries{SetRegistries: &agentlinkv1.SetRegistries{
			Registries: []*agentlinkv1.RegistryCred{{Host: tag, Username: "u", Token: "t"}},
		}}}
	}

	dropped := 0
	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		wg.Add(workersPerRound)
		for w := 0; w < workersPerRound; w++ {
			w := w
			go func() {
				defer wg.Done()
				hub.Send(nodeID, setRegistries(fmt.Sprintf("round-%d-worker-%d.example", r, w)))
			}()
		}
		wg.Wait()

		// Ground truth: with all of this round's Send calls fully complete
		// (mutation AND — with the fix — push, both inside the same critical
		// section), coalescing guarantees q.pending holds exactly one
		// SetRegistries: whichever call the mutex serialized last.
		hub.mu.Lock()
		var wantLast string
		for _, m := range hub.queues[nodeID].pending {
			if sr := m.GetSetRegistries(); sr != nil && len(sr.GetRegistries()) > 0 {
				wantLast = sr.GetRegistries()[0].GetHost()
			}
		}
		hub.mu.Unlock()
		if wantLast == "" {
			t.Fatalf("round %d: no SetRegistries pending after %d concurrent Sends — coalescing invariant broken", r, workersPerRound)
		}

		drained := drainSetRegistries(sess)
		foundAt := -1
		for i, host := range drained {
			if host == wantLast {
				foundAt = i
			}
		}
		if foundAt == -1 {
			// The ground-truth-last message itself was dropped (channel
			// briefly full under this round's burst) — acceptable per
			// session.push's documented drop-on-full behavior, and gives no
			// signal either way for this round.
			dropped++
			continue
		}
		if foundAt != len(drained)-1 {
			t.Fatalf("round %d: delivery order inverted — the hub's own last-mutated SetRegistries (%q) was delivered at position %d/%d, followed by a stale one: %v",
				r, wantLast, foundAt, len(drained)-1, drained)
		}
	}
	t.Logf("rounds=%d workersPerRound=%d ground-truth-dropped=%d (expected: rare, non-fatal — full-channel drops are correct behavior)", rounds, workersPerRound, dropped)
}

// drainSetRegistries reads every currently-buffered SetRegistries host off
// sess.out without blocking, in arrival order — mirrors the non-blocking
// shape of session.push itself.
func drainSetRegistries(sess *session) []string {
	var hosts []string
	for {
		select {
		case m := <-sess.out:
			if sr := m.GetSetRegistries(); sr != nil && len(sr.GetRegistries()) > 0 {
				hosts = append(hosts, sr.GetRegistries()[0].GetHost())
			}
		default:
			return hosts
		}
	}
}
