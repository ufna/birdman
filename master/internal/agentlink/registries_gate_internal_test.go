package agentlink

// Hub-level mechanics of the SetRegistries transport gate (mTLS agentlink v1,
// docs/superpowers/specs/2026-07-10-mtls-agentlink-design.md §3 «Гейт
// SetRegistries»): a SetRegistries command is enqueued/pushed ONLY for a
// session with certAuth || loopback. Otherwise it is skipped ENTIRELY — never
// enqueued — because the secret must not sit in pending on an untrusted link;
// a WARN is logged and the withheld counter (wired to
// birdman_agentlink_registries_withheld_total) is bumped. Commands of every
// other kind are unaffected. The transport-level (gRPC) shape of the same
// rules is covered in registries_gate_test.go.

import (
	"log/slog"
	"os"
	"sync/atomic"
	"testing"

	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

func gateHub(t *testing.T) (*Hub, *atomic.Int64) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	hub := NewHub(log)
	var withheld atomic.Int64
	hub.SetRegistriesWithheldCounter(func() { withheld.Add(1) })
	return hub, &withheld
}

func gateSetRegistries(host string) *agentlinkv1.MasterMsg {
	return &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_SetRegistries{SetRegistries: &agentlinkv1.SetRegistries{
		Registries: []*agentlinkv1.RegistryCred{{Host: host, Username: "u", Token: "t"}},
	}}}
}

func gateStop(serverID string) *agentlinkv1.MasterMsg {
	return &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_Stop{Stop: &agentlinkv1.StopServer{ServerId: serverID}}}
}

// drainKinds empties sess.out non-blockingly and reports which command kinds
// were delivered, in order.
func drainKinds(sess *session) []string {
	var out []string
	for {
		select {
		case m := <-sess.out:
			switch m.GetMsg().(type) {
			case *agentlinkv1.MasterMsg_SetRegistries:
				out = append(out, "set_registries")
			case *agentlinkv1.MasterMsg_Stop:
				out = append(out, "stop")
			default:
				out = append(out, "other")
			}
		default:
			return out
		}
	}
}

// An untrusted session (no cert, non-loopback): SetRegistries is skipped —
// not enqueued, not pushed — and counted; any OTHER command kind still flows
// (the "no mTLS ⇒ no work at all" policy is agentlink_auth=mtls, not this
// gate).
func TestHubSendWithholdsSetRegistriesFromUntrustedSession(t *testing.T) {
	hub, withheld := gateHub(t)
	sess := hub.attach("node-untrusted", nil, false, false)

	if cmdID := hub.Send("node-untrusted", gateSetRegistries("ghcr.io")); cmdID != "" {
		t.Fatalf("withheld SetRegistries must return no cmd_id, got %q", cmdID)
	}
	if got := hub.PendingCount("node-untrusted"); got != 0 {
		t.Fatalf("pending = %d after withheld SetRegistries, want 0 (skip, not enqueue)", got)
	}
	if got := withheld.Load(); got != 1 {
		t.Fatalf("withheld counter = %d, want 1", got)
	}

	if cmdID := hub.Send("node-untrusted", gateStop("srv-1")); cmdID == "" {
		t.Fatal("a non-registries command must still be sent to an untrusted session")
	}
	if got := hub.PendingCount("node-untrusted"); got != 1 {
		t.Fatalf("pending = %d after Stop, want 1 (other kinds unaffected)", got)
	}
	if kinds := drainKinds(sess); len(kinds) != 1 || kinds[0] != "stop" {
		t.Fatalf("delivered kinds = %v, want exactly [stop]", kinds)
	}
	if got := withheld.Load(); got != 1 {
		t.Fatalf("withheld counter = %d after Stop, want still 1", got)
	}
}

// No live session at all: SetRegistries is also skipped — a registries secret
// is never parked in an offline node's pending queue; the attach preface
// rebuilds the snapshot from the DB when the node (re)connects trusted.
func TestHubSendWithholdsSetRegistriesWithNoSession(t *testing.T) {
	hub, withheld := gateHub(t)

	if cmdID := hub.Send("node-offline", gateSetRegistries("ghcr.io")); cmdID != "" {
		t.Fatalf("want no cmd_id for a session-less SetRegistries, got %q", cmdID)
	}
	if got := hub.PendingCount("node-offline"); got != 0 {
		t.Fatalf("pending = %d, want 0 — the secret must not wait in pending", got)
	}
	if got := withheld.Load(); got != 1 {
		t.Fatalf("withheld counter = %d, want 1", got)
	}
}

// Trusted sessions — via client cert OR via loopback — receive SetRegistries
// exactly as before the gate (enqueued + pushed).
func TestHubSendDeliversSetRegistriesToTrustedSessions(t *testing.T) {
	hub, withheld := gateHub(t)

	for name, sess := range map[string]*session{
		"cert-non-loopback": hub.attach("node-cert", nil, true, false),
		"token-loopback":    hub.attach("node-loop", nil, false, true),
	} {
		nodeID := map[string]string{"cert-non-loopback": "node-cert", "token-loopback": "node-loop"}[name]
		if cmdID := hub.Send(nodeID, gateSetRegistries("ghcr.io")); cmdID == "" {
			t.Fatalf("%s: SetRegistries must be sent to a trusted session", name)
		}
		if got := hub.PendingCount(nodeID); got != 1 {
			t.Fatalf("%s: pending = %d, want 1", name, got)
		}
		if kinds := drainKinds(sess); len(kinds) != 1 || kinds[0] != "set_registries" {
			t.Fatalf("%s: delivered kinds = %v, want [set_registries]", name, kinds)
		}
	}
	if got := withheld.Load(); got != 0 {
		t.Fatalf("withheld counter = %d for trusted sessions, want 0", got)
	}
}

// Downgrade reconnect: a SetRegistries left pending by a trusted session
// (delivered but never acked) must NOT be replayed into a session that
// reconnects untrusted — attach strips it, while other pending kinds replay
// normally. Without the strip, the replay loop would push the stale secret
// onto the untrusted link.
func TestHubAttachStripsPendingSetRegistriesForUntrustedSession(t *testing.T) {
	hub, withheld := gateHub(t)

	hub.attach("node-x", nil, true, false)
	hub.Send("node-x", gateSetRegistries("ghcr.io"))
	hub.Send("node-x", gateStop("srv-1"))
	if got := hub.PendingCount("node-x"); got != 2 {
		t.Fatalf("pending = %d before downgrade, want 2", got)
	}
	if got := withheld.Load(); got != 0 {
		t.Fatalf("withheld counter = %d before downgrade, want 0 (nothing withheld yet)", got)
	}

	// Same node reconnects with neither cert nor loopback.
	sess2 := hub.attach("node-x", nil, false, false)
	if got := hub.PendingCount("node-x"); got != 1 {
		t.Fatalf("pending = %d after untrusted attach, want 1 (SetRegistries stripped, Stop kept)", got)
	}
	if kinds := drainKinds(sess2); len(kinds) != 1 || kinds[0] != "stop" {
		t.Fatalf("replayed kinds = %v, want exactly [stop]", kinds)
	}
	// The strip is a withhold point too: it must bump
	// birdman_agentlink_registries_withheld_total for metric consistency with
	// the Send/attach-preface skips (T5 review).
	if got := withheld.Load(); got != 1 {
		t.Fatalf("withheld counter = %d after the untrusted reconnect stripped the stale snapshot, want 1", got)
	}
}

// Backstop: attach must refuse a non-nil preface for an untrusted session
// even though the service already pre-gates it (structural invariant: an
// untrusted session's queue never holds a SetRegistries).
func TestHubAttachDropsPrefaceForUntrustedSession(t *testing.T) {
	hub, _ := gateHub(t)

	sess := hub.attach("node-y", gateSetRegistries("ghcr.io"), false, false)
	if got := hub.PendingCount("node-y"); got != 0 {
		t.Fatalf("pending = %d, want 0 — untrusted attach must drop the preface", got)
	}
	if kinds := drainKinds(sess); len(kinds) != 0 {
		t.Fatalf("delivered kinds = %v, want none", kinds)
	}
}
