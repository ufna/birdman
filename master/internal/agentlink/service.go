package agentlink

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ufna/birdman/master/internal/store"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// PullSink consumes agent PullReports (the deploy manager waits for `pulled`
// from every fleet node — итерация 3, docs/specs/master.md §5). May be nil.
type PullSink interface {
	HandlePullReport(nodeID string, r *agentlinkv1.PullReport)
}

// Service implements agentlink.v1.AgentLink. Nodes are pre-registered via
// REST (POST /v1/nodes → node_token); Hello{node_token} authenticates the
// stream (v0 auth clarification in docs/specs/protocol.md §Auth).
type Service struct {
	agentlinkv1.UnimplementedAgentLinkServer

	st   *store.Store
	hub  *Hub
	pull PullSink
	logs *LogRouter
	log  *slog.Logger

	// regMu serializes "read the current registries from the store" with
	// "enqueue that snapshot into the hub" — as ONE atomic step — across
	// EVERY caller of that pair: the attach preface (attachWithFreshRegistries,
	// called from Session) and BroadcastRegistries. Without it, two
	// concurrent callers (two broadcasts, or an attach racing a broadcast)
	// can each do their own read-then-enqueue independently; the hub's own
	// hub.mu (hub.go) guarantees that WHICHEVER enqueue happens last is
	// delivered last, but it says nothing about WHICH read that last enqueue
	// carries. Without regMu, an older read can end up enqueued after a
	// newer one — e.g. reconnect-during-edit: attach reads a stale snapshot,
	// is preempted, a concurrent broadcast reads+enqueues the fresh one, and
	// then attach's enqueue (coalescing, full-replace) lands last and wins
	// with the STALE snapshot. Holding regMu around both the DB read and the
	// enqueue for every caller makes the order in which reads are serialized
	// identical to the order in which they are enqueued, so combined with
	// hub.mu's push-order guarantee, whichever caller reads last is also
	// delivered last (task review, Fix 1;
	// TestServiceRegistriesSnapshotReadEnqueueSerializedAcrossAttachAndBroadcast).
	//
	// Lock granularity: this puts a DB round trip (ListRegistryCreds) inside
	// a mutex critical section. Accepted deliberately — registries changes
	// and node (re)connects are both rare, low-frequency, admin/ops-driven
	// events (not a per-request hot path like /v1/allocate), so serializing
	// them fully is cheap in absolute terms even though it is a coarser lock
	// than hub.mu (which only ever guards in-memory state).
	regMu sync.Mutex
}

func NewService(st *store.Store, hub *Hub, pull PullSink, logs *LogRouter, log *slog.Logger) *Service {
	return &Service{st: st, hub: hub, pull: pull, logs: logs, log: log}
}

func (s *Service) Session(stream agentlinkv1.AgentLink_SessionServer) error {
	ctx := stream.Context()

	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first message must be Hello")
	}
	node, err := s.st.AuthNodeToken(ctx, hello.GetNodeToken())
	if err != nil {
		if errors.Is(err, store.ErrBadToken) {
			return status.Error(codes.PermissionDenied, "bad node token")
		}
		s.log.Error("agentlink: auth failed", "err", err)
		return status.Error(codes.Internal, "auth failed")
	}
	if hello.GetRegion() != "" && hello.GetRegion() != node.Region {
		s.log.Warn("agentlink: hello region differs from registration, keeping registered region",
			"node_id", node.ID, "hello_region", hello.GetRegion(), "region", node.Region)
	}
	if err := s.st.HelloSync(ctx, node.ID, hello.GetHostname(), hello.GetCapacitySlots(),
		hello.GetAgentVersion(), toReports(hello.GetServers())); err != nil {
		s.log.Error("agentlink: hello sync failed", "node_id", node.ID, "err", err)
		return status.Error(codes.Internal, "hello sync failed")
	}
	s.log.Info("agentlink: node connected",
		"node_id", node.ID, "hostname", hello.GetHostname(), "agent_version", hello.GetAgentVersion())

	// Attach prefaces the stream with a fresh registries snapshot, computed
	// here BEFORE attach so it lands ahead of any replayed pending command
	// (docs/superpowers/specs/2026-07-09-registries-design.md §2).
	sess := s.attachWithFreshRegistries(ctx, node.ID)
	defer s.hub.detach(node.ID, sess)

	recvErr := make(chan error, 1)
	go func() { recvErr <- s.readLoop(ctx, stream, node.ID) }()
	sendErr := make(chan error, 1)
	go func() { sendErr <- s.writeLoop(ctx, stream, sess) }()

	select {
	case err := <-recvErr:
		return err
	case err := <-sendErr:
		return err
	case <-sess.done:
		return status.Error(codes.Aborted, "session replaced by a new connection")
	}
}

func (s *Service) writeLoop(ctx context.Context, stream agentlinkv1.AgentLink_SessionServer, sess *session) error {
	for {
		select {
		case m := <-sess.out:
			if err := stream.Send(m); err != nil {
				return err
			}
		case <-sess.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *Service) readLoop(ctx context.Context, stream agentlinkv1.AgentLink_SessionServer, nodeID string) error {
	for {
		in, err := stream.Recv()
		if err != nil {
			return err
		}
		// Store errors are logged but do not kill the session: the agent
		// keeps its state and retries via the next heartbeat.
		switch m := in.GetMsg().(type) {
		case *agentlinkv1.AgentMsg_Heartbeat:
			if err := s.st.ApplyHeartbeat(ctx, nodeID, toReports(m.Heartbeat.GetServers())); err != nil {
				s.log.Error("agentlink: heartbeat apply failed", "node_id", nodeID, "err", err)
			}
		case *agentlinkv1.AgentMsg_Event:
			ev := m.Event
			if err := s.st.ApplyServerEvent(ctx, nodeID, ev.GetServerId(), ev.GetKind(), ev.GetDetail()); err != nil {
				s.log.Error("agentlink: server event apply failed",
					"node_id", nodeID, "server_id", ev.GetServerId(), "kind", ev.GetKind(), "err", err)
			}
		case *agentlinkv1.AgentMsg_Ack:
			s.hub.Ack(nodeID, m.Ack.GetCmdId())
		case *agentlinkv1.AgentMsg_Pull:
			s.log.Info("agentlink: pull report", "node_id", nodeID,
				"image_ref", m.Pull.GetImageRef(), "status", m.Pull.GetStatus(), "detail", m.Pull.GetDetail())
			// The deploy manager waits for `pulled` from every fleet node
			// (итерация 3, master.md §5).
			if s.pull != nil {
				s.pull.HandlePullReport(nodeID, m.Pull)
			}
		case *agentlinkv1.AgentMsg_Log:
			// Route TailLogs answer chunks to the REST logs proxy
			// (итерация 4, GET /v1/servers/{id}/logs).
			if s.logs != nil {
				s.logs.Dispatch(m.Log.GetCmdId(), m.Log)
			}
		case *agentlinkv1.AgentMsg_Hello:
			s.log.Warn("agentlink: duplicate Hello ignored", "node_id", nodeID)
		}
	}
}

// attachWithFreshRegistries builds a fresh registries snapshot and attaches
// nodeID's session in one regMu-guarded step — see regMu's doc comment on
// the Service struct for why the read and the enqueue must be atomic
// together, not just each safe on its own. Split out of Session so the
// concurrency regression test can drive it directly, the same way it drives
// BroadcastRegistries, without needing a full gRPC stream per attempt.
func (s *Service) attachWithFreshRegistries(ctx context.Context, nodeID string) *session {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	return s.hub.attach(nodeID, s.registriesSnapshot(ctx, nodeID))
}

// registriesSnapshot builds a fresh SetRegistries command from the current
// database contents — used both as the attach preface (a newly (re)connected
// node must see the current credential set before any replayed command,
// design doc §2) and, via registryCredsToProto, by BroadcastRegistries.
// cmd_id is left blank: the caller (Hub.attach/Send) stamps a fresh one.
// Returns nil on a DB error (logged, without ever formatting the creds
// themselves) rather than failing the attach — the next Hello retries.
func (s *Service) registriesSnapshot(ctx context.Context, nodeID string) *agentlinkv1.MasterMsg {
	creds, err := s.st.ListRegistryCreds(ctx)
	if err != nil {
		s.log.Error("agentlink: registries snapshot failed", "node_id", nodeID, "err", err)
		return nil
	}
	return &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_SetRegistries{
		SetRegistries: &agentlinkv1.SetRegistries{Registries: registryCredsToProto(creds)},
	}}
}

// registryCredsToProto converts store creds (which carry the plaintext
// token) into wire structs. Never log the result or format it with %v/%+v.
func registryCredsToProto(creds []store.RegistryCred) []*agentlinkv1.RegistryCred {
	out := make([]*agentlinkv1.RegistryCred, 0, len(creds))
	for _, c := range creds {
		out = append(out, &agentlinkv1.RegistryCred{Host: c.Host, Username: c.Username, Token: c.Token})
	}
	return out
}

// BroadcastRegistries rebuilds the registries snapshot from the database
// once and Sends a fresh copy (its own cmd_id per node — Hub.Send stamps it)
// to every currently connected node. Wired as the httpapi
// onRegistriesChanged hook (main.go) so a successful POST/DELETE
// /v1/registries immediately refreshes every connected agent (design doc
// §2). It runs synchronously on the HTTP request path by design: Hub.Send
// only touches an in-memory queue and a buffered channel with a
// non-blocking push (hub.go, session.push) — it never waits on network I/O —
// so the only latency this adds is the single ListRegistryCreds query,
// which is no heavier than the insert+event-write the caller already made
// earlier in the same request.
//
// The read and the fan-out are wrapped in regMu (see its doc comment on the
// Service struct) so this call's DB read is atomic, as a whole, with respect
// to every other concurrent BroadcastRegistries/attachWithFreshRegistries
// call — otherwise two overlapping broadcasts (or a broadcast racing an
// attach) could each read-then-send independently and have an older read
// delivered after a newer one (task review, Fix 1;
// TestServiceRegistriesSnapshotReadEnqueueSerializedAcrossAttachAndBroadcast).
func (s *Service) BroadcastRegistries(ctx context.Context) {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	creds, err := s.st.ListRegistryCreds(ctx)
	if err != nil {
		s.log.Error("agentlink: registries broadcast failed", "err", err)
		return
	}
	regs := registryCredsToProto(creds)
	for _, nodeID := range s.hub.ConnectedNodes() {
		s.hub.Send(nodeID, &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_SetRegistries{
			SetRegistries: &agentlinkv1.SetRegistries{Registries: regs},
		}})
	}
}

func toReports(states []*agentlinkv1.ServerState) []store.ServerReport {
	out := make([]store.ServerReport, 0, len(states))
	for _, st := range states {
		out = append(out, store.ServerReport{
			ServerID: st.GetServerId(),
			State:    st.GetState(),
			Players:  st.GetPlayers(),
			TickMS:   st.GetTickMs(),
			MatchID:  st.GetMatchId(),
			Port:     st.GetPort(),
			Version:  st.GetVersion(),
		})
	}
	return out
}
