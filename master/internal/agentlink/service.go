package agentlink

import (
	"context"
	"errors"
	"log/slog"

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

	sess := s.hub.attach(node.ID)
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
