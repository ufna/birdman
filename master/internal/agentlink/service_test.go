package agentlink_test

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/ufna/birdman/master/internal/agentlink"
	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }

// startServer wires a Hub+Service pair behind a bufconn gRPC server. It
// returns the Hub (to seed pending commands / inspect coalescing straight
// from a test, as the existing replay test already does) and the Service
// (T3 adds BroadcastRegistries, called directly here the same way main.go
// wires it to the httpapi onRegistriesChanged hook) alongside the client.
func startServer(t *testing.T, st *store.Store) (*agentlink.Hub, *agentlink.Service, agentlinkv1.AgentLinkClient) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return startServerWithLog(t, st, log)
}

// startServerWithLog is startServer with an injectable logger —
// TestRegistriesTokenNeverLogged uses it to capture every log line (not just
// stderr at Error level) into a buffer it can grep for a leaked token.
func startServerWithLog(t *testing.T, st *store.Store, log *slog.Logger) (*agentlink.Hub, *agentlink.Service, agentlinkv1.AgentLinkClient) {
	t.Helper()
	hub := agentlink.NewHub(log)
	svc := agentlink.NewService(st, hub, nil, nil, log)

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

func hello(token string) *agentlinkv1.AgentMsg {
	return &agentlinkv1.AgentMsg{Msg: &agentlinkv1.AgentMsg_Hello{Hello: &agentlinkv1.Hello{
		NodeToken:     token,
		Hostname:      "node-1",
		Region:        "eu",
		CapacitySlots: 10,
		AgentVersion:  "0.1.0",
	}}}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met in time: %s", what)
}

func TestSessionRejectsBadToken(t *testing.T) {
	st := testdb.New(t)
	testdb.Seed(t, st, "eu", 10)
	_, _, client := startServer(t, st)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(hello("bnt_bogus")); err != nil {
		t.Fatal(err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
}

func TestSessionRequiresHelloFirst(t *testing.T) {
	st := testdb.New(t)
	_, _, client := startServer(t, st)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&agentlinkv1.AgentMsg{Msg: &agentlinkv1.AgentMsg_Heartbeat{
		Heartbeat: &agentlinkv1.Heartbeat{TsUnixMs: time.Now().UnixMilli()},
	}}); err != nil {
		t.Fatal(err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

// Full happy path: Hello auth → heartbeat lands in Postgres → command
// delivery with cmd_id → Ack clears the pending queue → unacked commands are
// replayed on reconnect.
func TestSessionHeartbeatCommandsAndReplay(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	// Wipe the fixture's fresh heartbeat so we observe the agent's own.
	if _, err := st.Pool.Exec(context.Background(),
		`update nodes set last_heartbeat_at = null where id = $1::uuid`, f.NodeID); err != nil {
		t.Fatal(err)
	}
	hub, _, client := startServer(t, st)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stream, err := client.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(hello(f.NodeToken)); err != nil {
		t.Fatal(err)
	}

	// Attach now prefaces every stream with a (possibly empty) registries
	// snapshot before anything else, ahead of any replayed/new command
	// (docs/superpowers/specs/2026-07-09-registries-design.md §2, T3) — drain
	// and ack it (as a well-behaved agent would) so the rest of this test's
	// Recv calls and PendingCount==0 assertions line up with the commands it
	// actually sends below. No registries are configured in this fixture, so
	// the snapshot is empty.
	preface, err := stream.Recv()
	if err != nil || preface.GetSetRegistries() == nil {
		t.Fatalf("want the attach registries preface first, got %+v (err=%v)", preface, err)
	}
	if err := stream.Send(&agentlinkv1.AgentMsg{Msg: &agentlinkv1.AgentMsg_Ack{
		Ack: &agentlinkv1.Ack{CmdId: preface.GetSetRegistries().GetCmdId()},
	}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "attach registries preface acked", func() bool { return hub.PendingCount(f.NodeID) == 0 })

	// Heartbeat with a server report: creating → ready.
	serverID := f.InsertServer(t, f.NodeID, f.VersionID, "creating", 0, 0)
	if err := stream.Send(&agentlinkv1.AgentMsg{Msg: &agentlinkv1.AgentMsg_Heartbeat{
		Heartbeat: &agentlinkv1.Heartbeat{
			TsUnixMs: time.Now().UnixMilli(),
			Servers: []*agentlinkv1.ServerState{{
				ServerId: serverID, State: "ready", Port: 22001,
			}},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "heartbeat applied", func() bool {
		sv, err := st.GetServer(context.Background(), serverID)
		return err == nil && sv.State == "ready" && sv.Port == 22001
	})
	eventually(t, "node heartbeat recorded", func() bool {
		nodes, err := st.ListNodes(context.Background())
		return err == nil && len(nodes) == 1 && nodes[0].LastHeartbeatAt != nil
	})

	// Command dispatch: agent receives StartServer with a cmd_id.
	sentCmdID := hub.Send(f.NodeID, &agentlinkv1.MasterMsg{
		Msg: &agentlinkv1.MasterMsg_Start{Start: &agentlinkv1.StartServer{
			ServerId: serverID, ImageRef: "ghcr.io/example/game-server:1.0.0",
		}},
	})
	in, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	start := in.GetStart()
	if start == nil || start.GetCmdId() != sentCmdID {
		t.Fatalf("want StartServer with cmd_id %s, got %+v", sentCmdID, in)
	}

	// Ack removes it from the pending queue.
	if err := stream.Send(&agentlinkv1.AgentMsg{Msg: &agentlinkv1.AgentMsg_Ack{
		Ack: &agentlinkv1.Ack{CmdId: sentCmdID},
	}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "ack processed", func() bool { return hub.PendingCount(f.NodeID) == 0 })

	// Queue a command that will NOT be acked, drop the connection, then
	// reconnect: the command must be replayed.
	unacked := hub.Send(f.NodeID, &agentlinkv1.MasterMsg{
		Msg: &agentlinkv1.MasterMsg_Stop{Stop: &agentlinkv1.StopServer{
			ServerId: serverID, GraceS: 5,
		}},
	})
	if in, err := stream.Recv(); err != nil || in.GetStop().GetCmdId() != unacked {
		t.Fatalf("want StopServer %s on live stream, got %+v (%v)", unacked, in, err)
	}
	cancel() // hard-drop the client connection without acking

	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()
	stream2, err := client.Session(ctx2)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream2.Send(hello(f.NodeToken)); err != nil {
		t.Fatal(err)
	}
	// Reconnect re-attaches, so a fresh registries preface goes out again
	// first, ahead of the replayed StopServer — drain and ack it too, same as
	// on the initial connect above.
	preface2, err := stream2.Recv()
	if err != nil || preface2.GetSetRegistries() == nil {
		t.Fatalf("want the attach registries preface first on reconnect, got %+v (err=%v)", preface2, err)
	}
	if err := stream2.Send(&agentlinkv1.AgentMsg{Msg: &agentlinkv1.AgentMsg_Ack{
		Ack: &agentlinkv1.Ack{CmdId: preface2.GetSetRegistries().GetCmdId()},
	}}); err != nil {
		t.Fatal(err)
	}
	in, err = stream2.Recv()
	if err != nil {
		t.Fatalf("replay recv: %v", err)
	}
	if in.GetStop().GetCmdId() != unacked {
		t.Fatalf("want replayed StopServer %s, got %+v", unacked, in)
	}
	if err := stream2.Send(&agentlinkv1.AgentMsg{Msg: &agentlinkv1.AgentMsg_Ack{
		Ack: &agentlinkv1.Ack{CmdId: unacked},
	}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "replayed command acked", func() bool { return hub.PendingCount(f.NodeID) == 0 })
}

// A second connection for the same node displaces the first one.
func TestSessionReplacedByNewConnection(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	_, _, client := startServer(t, st)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	first, err := client.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Send(hello(f.NodeToken)); err != nil {
		t.Fatal(err)
	}
	// Attach prefaces the stream with a registries snapshot before anything
	// else (T3) — drain it now so the Recv below observes the abort, not
	// this leftover buffered message.
	if _, err := first.Recv(); err != nil {
		t.Fatalf("attach registries preface: %v", err)
	}
	// Wait until the first session is attached (its Hello is processed).
	eventually(t, "first session attached", func() bool {
		nodes, err := st.ListNodes(context.Background())
		return err == nil && nodes[0].AgentVersion == "0.1.0"
	})

	second, err := client.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Send(hello(f.NodeToken)); err != nil {
		t.Fatal(err)
	}
	_, err = first.Recv()
	if status.Code(err) != codes.Aborted {
		t.Fatalf("first session must be aborted by the second, got %v", err)
	}
}

// --- T3: registries distribution over agentlink (docs/superpowers/specs/2026-07-09-registries-design.md §2) ---

// TestHubCoalescesSetRegistries: enqueueing a new SetRegistries for a node
// removes any older unacked SetRegistries from its pending queue — Hub never
// holds more than one, so a chatty stream of registry changes (or a node
// that never acks) cannot grow the queue unbounded. Coalescing is scoped to
// SetRegistries only: a pending command of another kind is untouched by it.
// Pure Hub-level test — no store/gRPC needed.
func TestHubCoalescesSetRegistries(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	hub := agentlink.NewHub(log)
	const nodeID = "node-under-test"

	setRegistries := func(username string) *agentlinkv1.MasterMsg {
		return &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_SetRegistries{SetRegistries: &agentlinkv1.SetRegistries{
			Registries: []*agentlinkv1.RegistryCred{{Host: "ghcr.io", Username: username, Token: "tok"}},
		}}}
	}

	hub.Send(nodeID, setRegistries("alice"))
	if got := hub.PendingCount(nodeID); got != 1 {
		t.Fatalf("after 1st SetRegistries: want pending=1, got %d", got)
	}

	// Two consecutive registries changes must coalesce to exactly one
	// pending entry, not accumulate.
	hub.Send(nodeID, setRegistries("bob"))
	if got := hub.PendingCount(nodeID); got != 1 {
		t.Fatalf("after 2nd SetRegistries: want coalesced pending=1, got %d", got)
	}

	// A different command kind is unaffected by SetRegistries coalescing: it
	// survives a further registries change, which only removes its own kind.
	hub.Send(nodeID, &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_Start{Start: &agentlinkv1.StartServer{
		ServerId: "srv-1",
	}}})
	hub.Send(nodeID, setRegistries("carol"))
	if got := hub.PendingCount(nodeID); got != 2 {
		t.Fatalf("want 2 pending (1 Start + 1 coalesced SetRegistries), got %d", got)
	}
}

// TestAttachSendsRegistriesSnapshotBeforePendingReplay: a fresh registries
// snapshot built from the store must reach a (re)connecting node BEFORE any
// replayed pending command — otherwise a replayed StartServer referencing a
// private image could race ahead of its credentials and trigger a noisy
// transient anonymous pull (design doc §2). The snapshot also rides the
// normal pending/ack machinery: it has its own cmd_id and acking it clears
// only it, leaving the older pending command intact — proving a lost stream
// would still get it re-delivered on the next attach.
func TestAttachSendsRegistriesSnapshotBeforePendingReplay(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()
	if _, err := st.UpsertRegistry(ctx, "ghcr.io", "alice", "tok-secret", "primary"); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	hub, _, client := startServer(t, st)

	// Queue a command for the node BEFORE it ever connects — it must still
	// be replayed on attach, but only after the snapshot.
	serverID := f.InsertServer(t, f.NodeID, f.VersionID, "creating", 0, 0)
	startCmdID := hub.Send(f.NodeID, &agentlinkv1.MasterMsg{
		Msg: &agentlinkv1.MasterMsg_Start{Start: &agentlinkv1.StartServer{
			ServerId: serverID, ImageRef: "ghcr.io/example/game-server:1.0.0",
		}},
	})
	if got := hub.PendingCount(f.NodeID); got != 1 {
		t.Fatalf("want 1 pending before attach, got %d", got)
	}

	ctxS, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.Session(ctxS)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(hello(f.NodeToken)); err != nil {
		t.Fatal(err)
	}

	// First message: the fresh snapshot, not the older pending StartServer.
	in, err := stream.Recv()
	if err != nil {
		t.Fatalf("snapshot recv: %v", err)
	}
	snap := in.GetSetRegistries()
	if snap == nil {
		t.Fatalf("want SetRegistries first, got %+v", in)
	}
	if snap.GetCmdId() == "" {
		t.Fatalf("snapshot must carry a cmd_id")
	}
	regs := snap.GetRegistries()
	if len(regs) != 1 || regs[0].GetHost() != "ghcr.io" || regs[0].GetUsername() != "alice" || regs[0].GetToken() != "tok-secret" {
		t.Fatalf("unexpected snapshot contents: %+v", regs)
	}
	snapCmdID := snap.GetCmdId()

	// Second message: the older pending StartServer, in its original order.
	in, err = stream.Recv()
	if err != nil {
		t.Fatalf("replay recv: %v", err)
	}
	if in.GetStart().GetCmdId() != startCmdID {
		t.Fatalf("want replayed StartServer %s after the snapshot, got %+v", startCmdID, in)
	}

	// Both are tracked in pending; acking the snapshot clears only it.
	if got := hub.PendingCount(f.NodeID); got != 2 {
		t.Fatalf("want 2 pending (snapshot + StartServer) after attach, got %d", got)
	}
	if err := stream.Send(&agentlinkv1.AgentMsg{Msg: &agentlinkv1.AgentMsg_Ack{
		Ack: &agentlinkv1.Ack{CmdId: snapCmdID},
	}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "snapshot ack clears only the snapshot", func() bool {
		return hub.PendingCount(f.NodeID) == 1
	})
}

// TestAttachSendsEmptyRegistriesSnapshotWhenNoneConfigured: an empty
// registries set is still a valid snapshot and is sent on attach — it means
// "everything was deleted", not "nothing to say" (design doc §2).
func TestAttachSendsEmptyRegistriesSnapshotWhenNoneConfigured(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	_, _, client := startServer(t, st)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(hello(f.NodeToken)); err != nil {
		t.Fatal(err)
	}
	in, err := stream.Recv()
	if err != nil {
		t.Fatalf("snapshot recv: %v", err)
	}
	snap := in.GetSetRegistries()
	if snap == nil {
		t.Fatalf("want an (empty) SetRegistries on attach, got %+v", in)
	}
	if snap.GetCmdId() == "" {
		t.Fatalf("empty snapshot must still carry a cmd_id")
	}
	if len(snap.GetRegistries()) != 0 {
		t.Fatalf("want zero registries, got %+v", snap.GetRegistries())
	}
}

// TestBroadcastRegistriesReachesAllConnectedNodes: BroadcastRegistries
// rebuilds the snapshot from the store once and Sends a fresh copy (its own
// cmd_id per node) to every currently connected node. This is what the
// httpapi onRegistriesChanged hook (wired in main.go) calls after every
// successful POST/DELETE /v1/registries.
func TestBroadcastRegistriesReachesAllConnectedNodes(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()
	_, token2, err := st.CreateNode(ctx, store.CreateNodeParams{
		Project: f.Project, Region: f.Region, Hostname: "node-2", PublicIP: "203.0.113.11", CapacitySlots: 10,
	})
	if err != nil {
		t.Fatalf("create second node: %v", err)
	}

	_, svc, client := startServer(t, st)

	ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel1()
	s1, err := client.Session(ctx1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Send(hello(f.NodeToken)); err != nil {
		t.Fatal(err)
	}
	if in, err := s1.Recv(); err != nil || in.GetSetRegistries() == nil {
		t.Fatalf("node1 attach preface: %+v (err=%v)", in, err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	s2, err := client.Session(ctx2)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.Send(hello(token2)); err != nil {
		t.Fatal(err)
	}
	if in, err := s2.Recv(); err != nil || in.GetSetRegistries() == nil {
		t.Fatalf("node2 attach preface: %+v (err=%v)", in, err)
	}

	if _, err := st.UpsertRegistry(ctx, "ghcr.io", "alice", "tok-broadcast", ""); err != nil {
		t.Fatalf("upsert registry: %v", err)
	}
	svc.BroadcastRegistries(ctx)

	for name, stream := range map[string]agentlinkv1.AgentLink_SessionClient{"node1": s1, "node2": s2} {
		in, err := stream.Recv()
		if err != nil {
			t.Fatalf("%s broadcast recv: %v", name, err)
		}
		snap := in.GetSetRegistries()
		if snap == nil {
			t.Fatalf("%s: want a broadcast SetRegistries, got %+v", name, in)
		}
		regs := snap.GetRegistries()
		if len(regs) != 1 || regs[0].GetHost() != "ghcr.io" || regs[0].GetToken() != "tok-broadcast" {
			t.Fatalf("%s: want broadcast snapshot with the new registry, got %+v", name, regs)
		}
	}
}

// TestRegistriesTokenNeverLogged: the hub/service must never print a
// registry token, in the ordinary attach+broadcast path (design doc §2:
// "токен... никогда: ... в логах"). Captures ALL log levels into a buffer
// (unlike the other tests here, which only surface Error to stderr) and
// asserts a distinctive token string never appears in it, even though it
// legitimately travels in the wire messages themselves.
func TestRegistriesTokenNeverLogged(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	ctx := context.Background()
	const secretToken = "s3cr3t-registry-token-should-never-be-logged"
	if _, err := st.UpsertRegistry(ctx, "ghcr.io", "alice", secretToken, ""); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	hub, svc, client := startServerWithLog(t, st, log)

	ctxS, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.Session(ctxS)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(hello(f.NodeToken)); err != nil {
		t.Fatal(err)
	}
	in, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	snap := in.GetSetRegistries()
	if snap == nil || len(snap.GetRegistries()) != 1 || snap.GetRegistries()[0].GetToken() != secretToken {
		t.Fatalf("want the token to reach the agent over the wire: %+v", in)
	}

	// A broadcast exercises Service.BroadcastRegistries' own log paths too.
	svc.BroadcastRegistries(ctx)
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("broadcast recv: %v", err)
	}
	if got := hub.PendingCount(f.NodeID); got != 1 {
		t.Fatalf("want the broadcast to coalesce with the attach preface, got %d pending", got)
	}

	if strings.Contains(logBuf.String(), secretToken) {
		t.Fatalf("registry token leaked into logs: %s", logBuf.String())
	}
}
