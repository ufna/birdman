// Package agentlink implements the master side of the AgentLink gRPC
// contract (docs/specs/protocol.md §1): session handling, per-node command
// dispatch with cmd_id/Ack tracking and re-send on reconnect.
package agentlink

import (
	"log/slog"
	"sync"

	"github.com/google/uuid"

	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// Hub tracks one command queue per node. Commands stay pending until the
// agent Acks them; on (re)connect all pending commands are replayed, so
// delivery is at-least-once (agents deduplicate by cmd_id).
//
// Pending commands live in memory: if master restarts they are lost, and the
// reconcile loop re-derives the desired state from Postgres (stuck `creating`
// servers are failed by timeout and re-created).
type Hub struct {
	log *slog.Logger

	mu     sync.Mutex
	queues map[string]*nodeQueue
}

type nodeQueue struct {
	pending []*agentlinkv1.MasterMsg // unacked commands, oldest first
	sess    *session
}

type session struct {
	out  chan *agentlinkv1.MasterMsg
	done chan struct{} // closed when the session is replaced
}

func NewHub(log *slog.Logger) *Hub {
	return &Hub{log: log, queues: map[string]*nodeQueue{}}
}

func (h *Hub) queue(nodeID string) *nodeQueue {
	q, ok := h.queues[nodeID]
	if !ok {
		q = &nodeQueue{}
		h.queues[nodeID] = q
	}
	return q
}

// Send stamps a fresh cmd_id into the command, queues it for the node and
// pushes it to the live session, if any. Returns the cmd_id.
func (h *Hub) Send(nodeID string, msg *agentlinkv1.MasterMsg) string {
	cmdID := uuid.NewString()
	stampCmdID(msg, cmdID)

	h.mu.Lock()
	q := h.queue(nodeID)
	q.pending = append(q.pending, msg)
	sess := q.sess
	h.mu.Unlock()

	if sess != nil {
		sess.push(h.log, nodeID, msg)
	}
	return cmdID
}

// Ack removes the command from the node's pending queue.
func (h *Hub) Ack(nodeID, cmdID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	q, ok := h.queues[nodeID]
	if !ok {
		return
	}
	for i, m := range q.pending {
		if commandID(m) == cmdID {
			q.pending = append(q.pending[:i], q.pending[i+1:]...)
			return
		}
	}
}

// PendingCount reports the number of unacked commands for a node.
func (h *Hub) PendingCount(nodeID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if q, ok := h.queues[nodeID]; ok {
		return len(q.pending)
	}
	return 0
}

// attach registers a new session for the node, replacing (and terminating)
// any previous one, and replays all pending commands into it.
func (h *Hub) attach(nodeID string) *session {
	s := &session{
		out:  make(chan *agentlinkv1.MasterMsg, 256),
		done: make(chan struct{}),
	}
	h.mu.Lock()
	q := h.queue(nodeID)
	if q.sess != nil {
		close(q.sess.done)
	}
	q.sess = s
	replay := make([]*agentlinkv1.MasterMsg, len(q.pending))
	copy(replay, q.pending)
	h.mu.Unlock()

	for _, m := range replay {
		s.push(h.log, nodeID, m)
	}
	return s
}

// detach removes the session if it is still the current one.
func (h *Hub) detach(nodeID string, s *session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if q, ok := h.queues[nodeID]; ok && q.sess == s {
		q.sess = nil
	}
}

// push never blocks: on a full channel the command is skipped — it stays
// pending and will be replayed on the next reconnect.
func (s *session) push(log *slog.Logger, nodeID string, m *agentlinkv1.MasterMsg) {
	select {
	case s.out <- m:
	default:
		log.Warn("agentlink: session channel full, command deferred to reconnect",
			"node_id", nodeID, "cmd_id", commandID(m))
	}
}

// stampCmdID sets the cmd_id field of whichever command the oneof carries.
func stampCmdID(m *agentlinkv1.MasterMsg, cmdID string) {
	switch c := m.GetMsg().(type) {
	case *agentlinkv1.MasterMsg_Start:
		c.Start.CmdId = cmdID
	case *agentlinkv1.MasterMsg_Stop:
		c.Stop.CmdId = cmdID
	case *agentlinkv1.MasterMsg_Prepull:
		c.Prepull.CmdId = cmdID
	case *agentlinkv1.MasterMsg_Drain:
		c.Drain.CmdId = cmdID
	case *agentlinkv1.MasterMsg_Upgrade:
		c.Upgrade.CmdId = cmdID
	case *agentlinkv1.MasterMsg_Tail:
		c.Tail.CmdId = cmdID
	case *agentlinkv1.MasterMsg_Ack:
		c.Ack.CmdId = cmdID
	case *agentlinkv1.MasterMsg_Allocate:
		c.Allocate.CmdId = cmdID
	case *agentlinkv1.MasterMsg_DrainServer:
		c.DrainServer.CmdId = cmdID
	}
}

func commandID(m *agentlinkv1.MasterMsg) string {
	switch c := m.GetMsg().(type) {
	case *agentlinkv1.MasterMsg_Start:
		return c.Start.GetCmdId()
	case *agentlinkv1.MasterMsg_Stop:
		return c.Stop.GetCmdId()
	case *agentlinkv1.MasterMsg_Prepull:
		return c.Prepull.GetCmdId()
	case *agentlinkv1.MasterMsg_Drain:
		return c.Drain.GetCmdId()
	case *agentlinkv1.MasterMsg_Upgrade:
		return c.Upgrade.GetCmdId()
	case *agentlinkv1.MasterMsg_Tail:
		return c.Tail.GetCmdId()
	case *agentlinkv1.MasterMsg_Ack:
		return c.Ack.GetCmdId()
	case *agentlinkv1.MasterMsg_Allocate:
		return c.Allocate.GetCmdId()
	case *agentlinkv1.MasterMsg_DrainServer:
		return c.DrainServer.GetCmdId()
	}
	return ""
}
