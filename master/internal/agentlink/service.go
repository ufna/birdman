package agentlink

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/tlsutil"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// AuthMode selects how Session authenticates an agent (mTLS agentlink v1,
// docs/superpowers/specs/2026-07-10-mtls-agentlink-design.md §3). The config
// enum agentlink_auth maps 1:1 onto these values.
type AuthMode string

const (
	// AuthToken ignores client certs entirely — behaviour is byte-identical to
	// the pre-mTLS code (emergency rollback).
	AuthToken AuthMode = "token"
	// AuthMixed accepts a verified client cert OR a node_token — the
	// post-release default and transition mode.
	AuthMixed AuthMode = "mixed"
	// AuthMTLS requires a verified client cert for Session; a token-only Hello
	// is PermissionDenied (Enroll-by-token stays possible — a later task).
	AuthMTLS AuthMode = "mtls"
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
	mode AuthMode
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

// NewService wires the AgentLink service. mode selects the Session auth policy
// (design §3); an empty mode normalises to AuthMixed so a zero-value
// construction preserves the post-release default.
func NewService(st *store.Store, hub *Hub, pull PullSink, logs *LogRouter, mode AuthMode, log *slog.Logger) *Service {
	if mode == "" {
		mode = AuthMixed
	}
	return &Service{st: st, hub: hub, pull: pull, logs: logs, mode: mode, log: log}
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
	node, certAuth, loopback, err := s.authenticate(ctx, hello)
	if err != nil {
		return err
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
	sess := s.attachWithFreshRegistries(ctx, node.ID, certAuth, loopback)
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

// Enroll swaps a node credential for a signed client leaf (mTLS agentlink v1,
// design §3 "Enroll handler"). Authentication is EITHER the node_token
// (bootstrap/recovery: bcrypt via AuthNodeToken — its ~100ms is a natural
// anti-brute-force, and the agent's PermissionDenied max-backoff already
// applies) OR a live verified client cert on the connection (renewal: node id
// from the chain's CN, token empty). If both are present they must resolve to
// the same node (confused-deputy guard, same rule as Session). Authorization
// is always re-read from the DB: the node must exist and not be dead.
//
// Enroll is deliberately independent of s.mode: agentlink_auth gates Session
// admission, not enrollment. Enroll-by-token must survive even mtls mode (a
// new node has no cert yet — design §3), and cert renewal keeps working
// during a token-mode emergency rollback so certs stay fresh for the flip
// back.
//
// The nodes.cert_* update and the audit event (node_enrolled | ...renewed,
// payload {serial, not_after, agent_version} — no token, no key bytes) are
// one transaction (store.SetNodeCert). Never log the token or any key
// material here.
func (s *Service) Enroll(ctx context.Context, req *agentlinkv1.EnrollRequest) (*agentlinkv1.EnrollResponse, error) {
	var node store.Node
	var eventKind string
	if leaf := verifiedLeaf(ctx); leaf != nil {
		// Renewal over a live cert; a token, if also sent, must name the SAME
		// node (authorizeCert parses it without a bcrypt run — the verified
		// cert is proof enough).
		n, err := s.authorizeCert(ctx, leaf, req.GetNodeToken())
		if err != nil {
			return nil, err
		}
		node, eventKind = n, store.EventNodeCertRenewed
	} else {
		if req.GetNodeToken() == "" {
			return nil, status.Error(codes.PermissionDenied, "enrollment requires a node token or a verified client certificate")
		}
		n, err := s.st.AuthNodeToken(ctx, req.GetNodeToken())
		if err != nil {
			if errors.Is(err, store.ErrBadToken) {
				return nil, status.Error(codes.PermissionDenied, "bad node token")
			}
			s.log.Error("agentlink: enroll auth failed", "err", err)
			return nil, status.Error(codes.Internal, "auth failed")
		}
		if n.State == "dead" {
			return nil, status.Error(codes.PermissionDenied, "node is dead")
		}
		node, eventKind = n, store.EventNodeEnrolled
	}

	caCert, caKey, err := s.st.EnsureInternalCA(ctx)
	if err != nil {
		s.log.Error("agentlink: enroll: internal CA unavailable", "node_id", node.ID, "err", err)
		return nil, status.Error(codes.Internal, "internal CA unavailable")
	}
	certPEM, leaf, err := tlsutil.IssueClientLeafFromCSR(caCert, caKey, node.ID, req.GetCsrPem())
	if err != nil {
		if errors.Is(err, tlsutil.ErrBadCSR) {
			return nil, status.Error(codes.InvalidArgument, "invalid CSR")
		}
		// tlsutil never puts key material into its errors, so logging err is safe.
		s.log.Error("agentlink: enroll: client leaf issue failed", "node_id", node.ID, "err", err)
		return nil, status.Error(codes.Internal, "certificate issue failed")
	}
	serial := leaf.SerialNumber.Text(16)

	if err := s.st.SetNodeCert(ctx, node.ID, serial, leaf.NotAfter, eventKind, req.GetAgentVersion()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The node vanished between authentication and the write.
			return nil, status.Error(codes.PermissionDenied, "unknown node")
		}
		s.log.Error("agentlink: enroll: cert record failed", "node_id", node.ID, "err", err)
		return nil, status.Error(codes.Internal, "enrollment failed")
	}

	cas, err := s.st.ActiveCAs(ctx)
	if err != nil {
		s.log.Error("agentlink: enroll: active CA bundle read failed", "node_id", node.ID, "err", err)
		return nil, status.Error(codes.Internal, "enrollment failed")
	}
	s.log.Info("agentlink: node enrolled",
		"node_id", node.ID, "serial", serial, "not_after", leaf.NotAfter,
		"agent_version", req.GetAgentVersion(), "renewal", eventKind == store.EventNodeCertRenewed)
	return &agentlinkv1.EnrollResponse{
		CertPem:      certPEM,
		CaBundlePem:  bytes.Join(cas, nil),
		NotAfterUnix: leaf.NotAfter.Unix(),
	}, nil
}

// authenticate resolves the node behind a Session stream and how it proved its
// identity (design §3). It returns the node, whether a verified client cert was
// used (certAuth) and whether the peer address is loopback (loopback) — the
// last two are recorded on the session for a later task (the SetRegistries gate
// and the sessions{auth} metric). Returned errors are already gRPC status
// errors, ready to fail the stream.
//
// Policy by mode:
//   - token: client certs ignored entirely; token auth, byte-identical to
//     pre-mTLS.
//   - mixed: a verified client cert authenticates (cert path); otherwise token
//     auth.
//   - mtls: a verified client cert is required; a token-only Hello (no verified
//     cert) is PermissionDenied.
func (s *Service) authenticate(ctx context.Context, hello *agentlinkv1.Hello) (store.Node, bool, bool, error) {
	loopback := peerIsLoopback(ctx)

	if s.mode != AuthToken {
		if leaf := verifiedLeaf(ctx); leaf != nil {
			node, err := s.authorizeCert(ctx, leaf, hello.GetNodeToken())
			return node, true, loopback, err
		}
		if s.mode == AuthMTLS {
			return store.Node{}, false, loopback, status.Error(codes.PermissionDenied, "client certificate required")
		}
	}

	// token path: mode token|mixed with no verified client cert.
	node, err := s.st.AuthNodeToken(ctx, hello.GetNodeToken())
	if err != nil {
		if errors.Is(err, store.ErrBadToken) {
			return store.Node{}, false, loopback, status.Error(codes.PermissionDenied, "bad node token")
		}
		s.log.Error("agentlink: auth failed", "err", err)
		return store.Node{}, false, loopback, status.Error(codes.Internal, "auth failed")
	}
	// Revocation applies to the token path too (design §Безопасность): a dead
	// node must never link, in ANY mode. AuthNodeToken carries no not-dead check
	// — it is also used by Enroll-by-token, which enforces its own — so the
	// Session token path enforces it here, mirroring authorizeCert on the cert
	// path. A non-dead node is unaffected, so the token-mode regression stays
	// byte-identical for every live node.
	if node.State == "dead" {
		return store.Node{}, false, loopback, status.Error(codes.PermissionDenied, "node is dead")
	}
	return node, false, loopback, nil
}

// authorizeCert authorizes a Session whose client cert already chained to the
// listener's ClientCAs (the internal CA) during the handshake (design §3). The
// identity is the cert CN — a node_id; the node must exist and not be dead
// (identity from the cert, authorization always re-read from the DB, so a valid
// cert for a dead/removed node is useless). A node_token, if the agent still
// sends one alongside its cert, MUST name the SAME node (confused-deputy guard)
// — it is only parsed, never bcrypt-checked: the verified cert is proof enough,
// so no bcrypt runs on this path.
func (s *Service) authorizeCert(ctx context.Context, leaf *x509.Certificate, token string) (store.Node, error) {
	nodeID := leaf.Subject.CommonName
	if nodeID == "" {
		return store.Node{}, status.Error(codes.PermissionDenied, "client certificate has no subject CN")
	}
	if token != "" {
		tokID, err := store.ParseNodeTokenID(token)
		if err != nil || tokID != nodeID {
			return store.Node{}, status.Error(codes.PermissionDenied, "node token does not match client certificate")
		}
	}
	node, err := s.st.GetNode(ctx, nodeID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Node{}, status.Error(codes.PermissionDenied, "unknown node")
	}
	if err != nil {
		s.log.Error("agentlink: node lookup failed", "node_id", nodeID, "err", err)
		return store.Node{}, status.Error(codes.Internal, "auth failed")
	}
	if node.State == "dead" {
		return store.Node{}, status.Error(codes.PermissionDenied, "node is dead")
	}
	return node, nil
}

// verifiedLeaf returns VerifiedChains[0][0] — the client leaf that chained to
// the listener's ClientCAs (the internal CA) — or nil when the peer presented
// no cert or no chain verified. With the listener on VerifyClientCertIfGiven a
// non-empty VerifiedChains means the handshake already validated the cert
// against the internal CA; Session then only has to authorize the identity.
func verifiedLeaf(ctx context.Context) *x509.Certificate {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil
	}
	if len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return nil
	}
	return tlsInfo.State.VerifiedChains[0][0]
}

// peerIsLoopback reports whether the gRPC peer's address is a loopback IP
// (design §3: the SetRegistries gate treats loopback links as trusted, dev
// compatibility). A missing peer, a non-IP address (e.g. bufconn) or an
// unparseable host is treated as not-loopback.
func peerIsLoopback(ctx context.Context) bool {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return false
	}
	host, _, err := net.SplitHostPort(p.Addr.String())
	if err != nil {
		host = p.Addr.String()
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
//
// Registries gate (design §3): a session that is neither cert-authenticated
// nor loopback gets NO preface — skipped entirely (WARN + withheld counter),
// before the DB read, so the plaintext creds are not even materialized for a
// link that must not carry them. The Hub strips any stale pending
// SetRegistries on such an attach (hub.attach) and withholds broadcasts
// (Hub.Send), so the invariant holds at every send point. When the node
// reconnects with a cert, the attach preface delivers the then-current
// snapshot — nothing is lost.
func (s *Service) attachWithFreshRegistries(ctx context.Context, nodeID string, certAuth, loopback bool) *session {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	if !certAuth && !loopback {
		s.hub.noteRegistriesWithheld(nodeID, "attach-preface")
		return s.hub.attach(nodeID, nil, certAuth, loopback)
	}
	return s.hub.attach(nodeID, s.registriesSnapshot(ctx, nodeID), certAuth, loopback)
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
//
// Per-node trust is enforced inside Hub.Send (registries gate, design §3):
// an untrusted session is skipped entirely — not enqueued — with a WARN and
// the withheld counter, so this fan-out needs no trust knowledge of its own.
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
