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
	// How this session authenticated, fixed at attach time (mTLS agentlink v1,
	// docs/superpowers/specs/2026-07-10-mtls-agentlink-design.md §3). certAuth
	// is true when the agent presented a verified client cert; loopback is true
	// when the peer address is loopback. Consumed by a later task: the
	// SetRegistries gate (queue/push only if certAuth || loopback) and the
	// sessions{auth} metric. Recorded here so both are decided once, from the
	// authenticated peer, and never re-derived per command.
	certAuth bool
	loopback bool
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
// pushes it to the live session, if any. A SetRegistries command coalesces
// with (replaces) any older unacked SetRegistries already pending for the
// node — Hub never holds more than one, so a chatty stream of registry
// changes (or a node that never acks) cannot grow the queue unbounded
// (docs/superpowers/specs/2026-07-09-registries-design.md §2). Other command
// kinds are unaffected. Returns the cmd_id.
//
// The push happens INSIDE the same critical section as the pending-queue
// mutation (not after unlocking) so that two goroutines racing Send for the
// same node's live session can never push out of the order their mutations
// were serialized in by h.mu — otherwise the LAST command a full-replace
// consumer (SetRegistries) sees on the wire could be an older one than the
// last one Hub itself considers current (task review, Fix 1;
// TestHubSendPushOrderMatchesMutationOrderUnderConcurrency). push is
// non-blocking (select/default on a buffered channel — hub.go, session.push)
// so this adds no unbounded lock hold time.
func (h *Hub) Send(nodeID string, msg *agentlinkv1.MasterMsg) string {
	cmdID := uuid.NewString()
	stampCmdID(msg, cmdID)

	h.mu.Lock()
	q := h.queue(nodeID)
	if isSetRegistries(msg) {
		q.pending = removeSetRegistries(q.pending)
	}
	q.pending = append(q.pending, msg)
	if q.sess != nil {
		q.sess.push(h.log, nodeID, msg)
	}
	h.mu.Unlock()

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
// any previous one, and replays all pending commands into it. If preface is
// non-nil (built by the caller from a fresh store.ListRegistryCreds read) it
// is stamped with its own cmd_id and inserted at the FRONT of the pending
// queue — coalesced with (replacing) any older unacked SetRegistries the
// same way Send does — so a (re)connecting agent always sees the current
// registries snapshot before any replayed command that might reference a
// private image, e.g. a StartServer (design doc §2). Routing the preface
// through the ordinary pending queue like any other command means the usual
// ack/re-send machinery covers it too: if delivery is lost (the session dies
// mid-replay before the agent acks) it simply stays pending and goes out
// again — still first — on the next attach.
//
// The replay push happens INSIDE the same critical section that swaps in the
// new session and mutates pending — not after unlocking — for the same
// ordering reason as Send (see its doc comment and
// TestHubSendPushOrderMatchesMutationOrderUnderConcurrency): a concurrent
// Send racing this attach must not be able to land its push in a way that
// contradicts h.mu's own serialization order. push is non-blocking, so this
// adds no unbounded lock hold time — it was already O(len(pending)) here
// before this change, via the replay copy this replaces.
func (h *Hub) attach(nodeID string, preface *agentlinkv1.MasterMsg, certAuth, loopback bool) *session {
	s := &session{
		out:      make(chan *agentlinkv1.MasterMsg, 256),
		done:     make(chan struct{}),
		certAuth: certAuth,
		loopback: loopback,
	}
	h.mu.Lock()
	q := h.queue(nodeID)
	if q.sess != nil {
		close(q.sess.done)
	}
	q.sess = s
	if preface != nil {
		stampCmdID(preface, uuid.NewString())
		q.pending = append([]*agentlinkv1.MasterMsg{preface}, removeSetRegistries(q.pending)...)
	}
	for _, m := range q.pending {
		s.push(h.log, nodeID, m)
	}
	h.mu.Unlock()

	return s
}

// ConnectedNodes returns the ids of nodes with a live session — used by
// Service.BroadcastRegistries to fan a fresh snapshot out to every agent
// currently online (design doc §2).
func (h *Hub) ConnectedNodes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.queues))
	for nodeID, q := range h.queues {
		if q.sess != nil {
			out = append(out, nodeID)
		}
	}
	return out
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
	case *agentlinkv1.MasterMsg_Undrain:
		c.Undrain.CmdId = cmdID
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
	case *agentlinkv1.MasterMsg_SetRegistries:
		c.SetRegistries.CmdId = cmdID
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
	case *agentlinkv1.MasterMsg_Undrain:
		return c.Undrain.GetCmdId()
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
	case *agentlinkv1.MasterMsg_SetRegistries:
		return c.SetRegistries.GetCmdId()
	}
	return ""
}

// isSetRegistries reports whether m carries a SetRegistries command.
func isSetRegistries(m *agentlinkv1.MasterMsg) bool {
	_, ok := m.GetMsg().(*agentlinkv1.MasterMsg_SetRegistries)
	return ok
}

// removeSetRegistries returns pending with any SetRegistries command
// filtered out — the coalescing step shared by Send and attach's preface
// insertion (design doc §2: at most one pending SetRegistries per node).
func removeSetRegistries(pending []*agentlinkv1.MasterMsg) []*agentlinkv1.MasterMsg {
	out := make([]*agentlinkv1.MasterMsg, 0, len(pending))
	for _, m := range pending {
		if !isSetRegistries(m) {
			out = append(out, m)
		}
	}
	return out
}
