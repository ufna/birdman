package agentlink_test

import (
	"context"
	"log/slog"
	"net"
	"os"
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

func startServer(t *testing.T, st *store.Store) (*agentlink.Hub, agentlinkv1.AgentLinkClient) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	hub := agentlink.NewHub(log)
	svc := agentlink.NewService(st, hub, log)

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
	return hub, agentlinkv1.NewAgentLinkClient(conn)
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
	_, client := startServer(t, st)

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
	_, client := startServer(t, st)

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
	hub, client := startServer(t, st)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stream, err := client.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(hello(f.NodeToken)); err != nil {
		t.Fatal(err)
	}

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
	_, client := startServer(t, st)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	first, err := client.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Send(hello(f.NodeToken)); err != nil {
		t.Fatal(err)
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
