package agentlink_test

// Transport-level tests of the SetRegistries gate (mTLS agentlink v1,
// docs/superpowers/specs/2026-07-10-mtls-agentlink-design.md §3 «Гейт
// SetRegistries», §Тестирование "unit гейт"): registry credentials reach a
// session ONLY if it authenticated with a client cert or its peer is
// loopback. bufconn's peer address is a non-IP ("bufconn") and classifies as
// NOT loopback — precisely the untrusted, remote-link shape — while
// startServer (service_test.go) listens on real 127.0.0.1 TCP, the trusted
// dev-box shape. Hub-level mechanics live in
// registries_gate_internal_test.go.

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/ufna/birdman/master/internal/agentlink"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// startServerNonLoopback runs the service on an INSECURE bufconn listener:
// no TLS (never certAuth) and a non-IP peer address (never loopback) — the
// untrusted-link session shape.
func startServerNonLoopback(t *testing.T, st *store.Store) (*agentlink.Hub, *agentlink.Service, agentlinkv1.AgentLinkClient) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	hub := agentlink.NewHub(log)
	svc := agentlink.NewService(st, hub, nil, nil, agentlink.AuthMixed, log)

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	agentlinkv1.RegisterAgentLinkServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return hub, svc, agentlinkv1.NewAgentLinkClient(conn)
}

// startTLSServerNonLoopback mirrors session_auth_test.go's TLS harness but on
// bufconn (non-loopback peer), and returns the hub/service for gate
// assertions.
func startTLSServerNonLoopback(t *testing.T, st *store.Store, mode agentlink.AuthMode, caCertPEM, caKeyPEM []byte) (*agentlink.Hub, *agentlink.Service, *bufconn.Listener) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	hub := agentlink.NewHub(log)
	svc := agentlink.NewService(st, hub, nil, nil, mode, log)

	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{serverLeaf(t, caCertPEM, caKeyPEM)},
		ClientCAs:    caPool(t, caCertPEM),
		ClientAuth:   tls.VerifyClientCertIfGiven,
		MinVersion:   tls.VersionTLS12,
	})
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(grpc.Creds(creds))
	agentlinkv1.RegisterAgentLinkServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return hub, svc, lis
}

// tlsClientBufconn dials a TLS bufconn listener; a nil clientCert connects
// without one (token-only shape over a verified server link).
func tlsClientBufconn(t *testing.T, lis *bufconn.Listener, caCertPEM []byte, clientCert *tls.Certificate) agentlinkv1.AgentLinkClient {
	t.Helper()
	cfg := &tls.Config{
		RootCAs:    caPool(t, caCertPEM),
		ServerName: "birdman-master",
		MinVersion: tls.VersionTLS12,
	}
	if clientCert != nil {
		cfg.Certificates = []tls.Certificate{*clientCert}
	}
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return agentlinkv1.NewAgentLinkClient(conn)
}

func wireWithheldCounter(hub *agentlink.Hub) *atomic.Int64 {
	var n atomic.Int64
	hub.SetRegistriesWithheldCounter(func() { n.Add(1) })
	return &n
}

func seedRegistry(t *testing.T, st *store.Store, host, token string) {
	t.Helper()
	if _, err := st.UpsertRegistry(context.Background(), host, "generic", "alice", token, ""); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
}

// openStream opens a Session and sends Hello(token) without reading anything.
func openStream(t *testing.T, client agentlinkv1.AgentLinkClient, token string) (agentlinkv1.AgentLink_SessionClient, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	stream, err := client.Session(ctx)
	if err != nil {
		cancel()
		t.Fatalf("open Session: %v", err)
	}
	if err := stream.Send(hello(token)); err != nil {
		cancel()
		t.Fatalf("send hello: %v", err)
	}
	return stream, cancel
}

func waitConnected(t *testing.T, hub *agentlink.Hub, nodeID string) {
	t.Helper()
	eventually(t, "session attached", func() bool {
		return slices.Contains(hub.ConnectedNodes(), nodeID)
	})
}

// The core negative: a token session on a non-loopback link gets NO preface
// and NO broadcast, its pending queue does NOT grow (skip, not enqueue), the
// withheld counter counts both send points — and ordinary commands still
// flow, proving the gate touches nothing but SetRegistries.
func TestGateNonLoopbackTokenSessionWithheld(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	seedRegistry(t, st, "ghcr.io", "tok-must-not-leak")
	hub, svc, client := startServerNonLoopback(t, st)
	withheld := wireWithheldCounter(hub)

	stream, cancel := openStream(t, client, f.NodeToken)
	defer cancel()
	waitConnected(t, hub, f.NodeID)

	// Attach preface: withheld — never enqueued.
	eventually(t, "attach preface withheld", func() bool { return withheld.Load() == 1 })
	if got := hub.PendingCount(f.NodeID); got != 0 {
		t.Fatalf("pending = %d after attach, want 0 (preface skipped, not enqueued)", got)
	}

	// Broadcast fan-out: withheld too, still nothing pending.
	svc.BroadcastRegistries(context.Background())
	if got := withheld.Load(); got != 2 {
		t.Fatalf("withheld counter = %d after broadcast, want 2", got)
	}
	if got := hub.PendingCount(f.NodeID); got != 0 {
		t.Fatalf("pending = %d after broadcast, want 0", got)
	}

	// Probe: the FIRST message this session ever receives is the Stop sent
	// now — nothing SetRegistries-shaped ever hit the wire before it.
	stopID := hub.Send(f.NodeID, &agentlinkv1.MasterMsg{
		Msg: &agentlinkv1.MasterMsg_Stop{Stop: &agentlinkv1.StopServer{ServerId: "srv-1", GraceS: 1}},
	})
	in, err := stream.Recv()
	if err != nil {
		t.Fatalf("probe recv: %v", err)
	}
	if in.GetSetRegistries() != nil {
		t.Fatalf("a SetRegistries reached an untrusted session: %+v", in)
	}
	if in.GetStop().GetCmdId() != stopID {
		t.Fatalf("first delivered message must be the probe Stop %s, got %+v", stopID, in)
	}
}

// A cert-authenticated session on the SAME non-loopback transport gets both
// the preface and the broadcast: certAuth alone is sufficient trust.
func TestGateMTLSNonLoopbackSessionGetsRegistries(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	seedRegistry(t, st, "ghcr.io", "tok-mtls")
	caCert, caKey, err := st.EnsureInternalCA(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	hub, svc, lis := startTLSServerNonLoopback(t, st, agentlink.AuthMixed, caCert, caKey)
	withheld := wireWithheldCounter(hub)
	cert := clientLeaf(t, caCert, caKey, f.NodeID)

	stream, cancel := openStream(t, tlsClientBufconn(t, lis, caCert, &cert), "")
	defer cancel()

	in, err := stream.Recv()
	if err != nil {
		t.Fatalf("preface recv: %v", err)
	}
	snap := in.GetSetRegistries()
	if snap == nil || len(snap.GetRegistries()) != 1 || snap.GetRegistries()[0].GetToken() != "tok-mtls" {
		t.Fatalf("mTLS session must receive the registries preface, got %+v", in)
	}

	seedRegistry(t, st, "second.example", "tok-2")
	svc.BroadcastRegistries(context.Background())
	in, err = stream.Recv()
	if err != nil {
		t.Fatalf("broadcast recv: %v", err)
	}
	if bc := in.GetSetRegistries(); bc == nil || len(bc.GetRegistries()) != 2 {
		t.Fatalf("mTLS session must receive the broadcast, got %+v", in)
	}
	if got := withheld.Load(); got != 0 {
		t.Fatalf("withheld counter = %d for a trusted session, want 0", got)
	}
}

// Dev compatibility: a token session over real loopback TCP still gets the
// preface and broadcasts (startServer listens on 127.0.0.1).
func TestGateLoopbackTokenSessionGetsRegistries(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	seedRegistry(t, st, "ghcr.io", "tok-loopback")
	hub, svc, client := startServer(t, st)
	withheld := wireWithheldCounter(hub)

	stream, cancel := openStream(t, client, f.NodeToken)
	defer cancel()

	in, err := stream.Recv()
	if err != nil {
		t.Fatalf("preface recv: %v", err)
	}
	if snap := in.GetSetRegistries(); snap == nil || len(snap.GetRegistries()) != 1 {
		t.Fatalf("loopback token session must receive the preface, got %+v", in)
	}

	svc.BroadcastRegistries(context.Background())
	in, err = stream.Recv()
	if err != nil {
		t.Fatalf("broadcast recv: %v", err)
	}
	if in.GetSetRegistries() == nil {
		t.Fatalf("loopback token session must receive the broadcast, got %+v", in)
	}
	if got := withheld.Load(); got != 0 {
		t.Fatalf("withheld counter = %d on loopback, want 0", got)
	}
}

// Nothing is lost: a node first connected untrusted (token over non-loopback,
// snapshot withheld) reconnects WITH a cert — the attach preface delivers the
// current snapshot.
func TestGateReconnectWithCertDeliversSnapshot(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	seedRegistry(t, st, "ghcr.io", "tok-reconnect")
	caCert, caKey, err := st.EnsureInternalCA(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	hub, _, lis := startTLSServerNonLoopback(t, st, agentlink.AuthMixed, caCert, caKey)
	withheld := wireWithheldCounter(hub)

	// First connection: token only (no client cert) → snapshot withheld.
	_, cancel1 := openStream(t, tlsClientBufconn(t, lis, caCert, nil), f.NodeToken)
	waitConnected(t, hub, f.NodeID)
	eventually(t, "first attach withheld the preface", func() bool { return withheld.Load() == 1 })
	if got := hub.PendingCount(f.NodeID); got != 0 {
		t.Fatalf("pending = %d after untrusted attach, want 0", got)
	}
	cancel1() // drop the untrusted link

	// Reconnect with a cert: the preface must deliver the full snapshot.
	cert := clientLeaf(t, caCert, caKey, f.NodeID)
	stream2, cancel2 := openStream(t, tlsClientBufconn(t, lis, caCert, &cert), "")
	defer cancel2()
	in, err := stream2.Recv()
	if err != nil {
		t.Fatalf("preface recv after cert reconnect: %v", err)
	}
	snap := in.GetSetRegistries()
	if snap == nil || len(snap.GetRegistries()) != 1 || snap.GetRegistries()[0].GetToken() != "tok-reconnect" {
		t.Fatalf("cert reconnect must deliver the registries snapshot via the preface, got %+v", in)
	}
}

// Downgrade reconnect, end to end: a trusted session leaves an unacked
// SetRegistries in pending; the same node reconnecting UNTRUSTED must not
// have it replayed — attach strips it and the queue shrinks accordingly.
func TestGateUntrustedReconnectStripsStalePendingSnapshot(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	seedRegistry(t, st, "ghcr.io", "tok-stale")
	caCert, caKey, err := st.EnsureInternalCA(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	hub, _, lis := startTLSServerNonLoopback(t, st, agentlink.AuthMixed, caCert, caKey)

	// Trusted session receives the preface but never acks it.
	cert := clientLeaf(t, caCert, caKey, f.NodeID)
	stream1, cancel1 := openStream(t, tlsClientBufconn(t, lis, caCert, &cert), "")
	if in, err := stream1.Recv(); err != nil || in.GetSetRegistries() == nil {
		t.Fatalf("trusted preface: %+v (err=%v)", in, err)
	}
	if got := hub.PendingCount(f.NodeID); got != 1 {
		t.Fatalf("pending = %d with the unacked preface, want 1", got)
	}
	cancel1()

	// Untrusted reconnect (token, no cert): the stale pending snapshot must
	// be stripped, not replayed.
	stream2, cancel2 := openStream(t, tlsClientBufconn(t, lis, caCert, nil), f.NodeToken)
	defer cancel2()
	waitConnected(t, hub, f.NodeID)
	eventually(t, "stale pending SetRegistries stripped", func() bool {
		return hub.PendingCount(f.NodeID) == 0
	})

	// Probe: the first thing this session receives is the Stop, not a replay.
	stopID := hub.Send(f.NodeID, &agentlinkv1.MasterMsg{
		Msg: &agentlinkv1.MasterMsg_Stop{Stop: &agentlinkv1.StopServer{ServerId: "srv-1", GraceS: 1}},
	})
	in, err := stream2.Recv()
	if err != nil {
		t.Fatalf("probe recv: %v", err)
	}
	if in.GetSetRegistries() != nil {
		t.Fatalf("stale SetRegistries replayed to an untrusted session: %+v", in)
	}
	if in.GetStop().GetCmdId() != stopID {
		t.Fatalf("want the probe Stop %s first, got %+v", stopID, in)
	}
}
