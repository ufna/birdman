package agentlink

import (
	"io"
	"log/slog"
	"testing"

	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

func TestDurableCommandKeepsIDCoalescesAndObservesAckOutsideLock(t *testing.T) {
	h := NewHub(slog.New(slog.NewTextHandler(io.Discard, nil)))
	const node, cmd = "node", "stable-cmd-id"
	msg := func() *agentlinkv1.MasterMsg {
		return &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_Allocate{Allocate: &agentlinkv1.AllocateServer{ServerId: "server", MatchId: "match"}}}
	}
	h.SendDurable(node, cmd, msg())
	h.SendDurable(node, cmd, msg())
	if h.PendingCount(node) != 1 {
		t.Fatal("durable retries grew pending queue")
	}
	observed := false
	h.SetAckObserver(func(n, c string) {
		if n != node || c != cmd {
			t.Error("wrong ACK identity")
		}
		observed = true
		if h.PendingCount(node) != 0 {
			t.Error("observer ran before pending removal")
		}
	})
	h.Ack(node, cmd)
	if !observed {
		t.Fatal("durable ACK not observed")
	}
	h.SendDurable(node, cmd, msg())
	if commandID(h.queues[node].pending[0]) != cmd {
		t.Fatal("recovery changed command id")
	}
}
