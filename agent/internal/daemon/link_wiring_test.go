package daemon

// End-to-end agent wiring over bufconn: real Manager (fake runtime) + real
// link.Client + real Outbox against a scripted master stream. Covers the
// iteration-1 acceptance chain: Hello with the restored map → heartbeats →
// StartServer → Ack → runtime start → liba ready → ServerEvent + ready in
// heartbeat → duplicate cmd_id re-acked without a second start.

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/ufna/birdman/agent/internal/link"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

type scriptedMaster struct {
	agentlinkv1.UnimplementedAgentLinkServer

	mu         sync.Mutex
	hellos     []*agentlinkv1.Hello
	heartbeats []*agentlinkv1.Heartbeat
	events     []*agentlinkv1.ServerEvent
	acks       []string
	stream     agentlinkv1.AgentLink_SessionServer
}

func (f *scriptedMaster) Session(stream agentlinkv1.AgentLink_SessionServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "hello first")
	}
	f.mu.Lock()
	f.hellos = append(f.hellos, hello)
	f.stream = stream
	f.mu.Unlock()
	for {
		in, err := stream.Recv()
		if err != nil {
			return err
		}
		f.mu.Lock()
		switch m := in.GetMsg().(type) {
		case *agentlinkv1.AgentMsg_Heartbeat:
			f.heartbeats = append(f.heartbeats, m.Heartbeat)
		case *agentlinkv1.AgentMsg_Event:
			f.events = append(f.events, m.Event)
		case *agentlinkv1.AgentMsg_Ack:
			f.acks = append(f.acks, m.Ack.GetCmdId())
		}
		f.mu.Unlock()
	}
}

func (f *scriptedMaster) push(t *testing.T, msg *agentlinkv1.MasterMsg) {
	t.Helper()
	f.mu.Lock()
	stream := f.stream
	f.mu.Unlock()
	if stream == nil {
		t.Fatal("no session")
	}
	if err := stream.Send(msg); err != nil {
		t.Fatal(err)
	}
}

func (f *scriptedMaster) lastHeartbeatState(serverID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.heartbeats) - 1; i >= 0; i-- {
		for _, s := range f.heartbeats[i].GetServers() {
			if s.GetServerId() == serverID {
				return s.GetState()
			}
		}
	}
	return ""
}

func (f *scriptedMaster) hasEvent(serverID, kind string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events {
		if e.GetServerId() == serverID && e.GetKind() == kind {
			return true
		}
	}
	return false
}

func (f *scriptedMaster) ackCount(cmdID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, a := range f.acks {
		if a == cmdID {
			n++
		}
	}
	return n
}

func TestAgentLinkEndToEnd(t *testing.T) {
	// Fake runtime with one already-running restored server.
	rt := newFakeRuntime()
	restoredHandle := newFakeHandle()
	rt.restored = []RestoredServer{{
		Handle: restoredHandle, ID: "old-1", Port: 20003,
		ImageRef: "img:1", State: "ready", Running: true,
	}}
	m, _, _ := testManager(t, rt)
	if err := m.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}

	fake := &scriptedMaster{}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	agentlinkv1.RegisterAgentLinkServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	outbox := link.NewOutbox(t.Logf)
	// The manager was built with a fakeSink in testManager; rewire it to the
	// real outbox so events travel the stream.
	m.sink = outbox

	client := link.New(link.Config{
		MasterAddr:     "passthrough:///bufconn",
		NodeToken:      "bnt_x.y",
		Hostname:       "n1",
		Region:         "dev",
		CapacitySlots:  4,
		AgentVersion:   "test",
		HeartbeatEvery: 30 * time.Millisecond,
		BackoffMin:     20 * time.Millisecond,
		BackoffMax:     100 * time.Millisecond,
		DialOptions: []grpc.DialOption{
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
	}, m, m, outbox, t.Logf)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = client.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	// 1. Hello carries the restored map.
	eventually(t, "hello with restored map", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.hellos) == 1 && len(fake.hellos[0].GetServers()) == 1 &&
			fake.hellos[0].GetServers()[0].GetServerId() == "old-1" &&
			fake.hellos[0].GetServers()[0].GetState() == "ready"
	})

	// 2. Heartbeats flow with node stats.
	eventually(t, "heartbeats", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.heartbeats) >= 2 && fake.heartbeats[0].GetNode() != nil
	})

	// 3. StartServer → Ack → container in the fake runtime.
	fake.push(t, &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_Start{Start: &agentlinkv1.StartServer{
		ServerId: "new-1", ImageRef: "img:2", CmdId: "cmd-start-1",
	}}})
	eventually(t, "start acked", func() bool { return fake.ackCount("cmd-start-1") == 1 })
	eventually(t, "container started", func() bool { return rt.startCount() == 1 })

	// 4. liba ready → ServerEvent + ready state in heartbeats.
	rt.mu.Lock()
	sockPath := rt.started[0].SocketPath
	rt.mu.Unlock()
	liba := dialLiba(t, sockPath)
	frames := liba.readFrames()
	liba.send("hello", map[string]any{"sdk_version": "stub/1"})
	liba.send("ready", nil)
	eventually(t, "ready event reached master", func() bool { return fake.hasEvent("new-1", "ready") })
	eventually(t, "heartbeat shows ready", func() bool {
		return fake.lastHeartbeatState("new-1") == "ready"
	})

	// 5. Duplicate cmd_id: re-acked, no second container.
	fake.push(t, &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_Start{Start: &agentlinkv1.StartServer{
		ServerId: "new-1", ImageRef: "img:2", CmdId: "cmd-start-1",
	}}})
	eventually(t, "duplicate re-acked", func() bool { return fake.ackCount("cmd-start-1") == 2 })
	if rt.startCount() != 1 {
		t.Fatalf("duplicate cmd started %d containers", rt.startCount())
	}

	// 6. AllocateServer over the stream → Ack, `allocated` frame in liba,
	// allocated state (+match id) in heartbeats (итерация 2).
	fake.push(t, &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_Allocate{Allocate: &agentlinkv1.AllocateServer{
		ServerId: "new-1", MatchId: "m-77", PlayersExpected: 2, CmdId: "cmd-alloc-1",
	}}})
	eventually(t, "allocate acked", func() bool { return fake.ackCount("cmd-alloc-1") == 1 })
	fr := awaitFrame(t, frames, "allocated")
	if fr.Data["match_id"] != "m-77" || fr.Data["players_expected"] != float64(2) {
		t.Fatalf("allocated frame: %+v", fr)
	}
	eventually(t, "heartbeat shows allocated with match id", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for i := len(fake.heartbeats) - 1; i >= 0; i-- {
			for _, s := range fake.heartbeats[i].GetServers() {
				if s.GetServerId() == "new-1" {
					return s.GetState() == "allocated" && s.GetMatchId() == "m-77"
				}
			}
		}
		return false
	})

	// 7. Match ends, the dedik exits 0: match events + a `stopped` state in
	// heartbeats — the master reaps it, no failure anywhere.
	liba.send("match_start", map[string]any{"match_id": "m-77"})
	liba.send("match_end", map[string]any{"match_id": "m-77", "result": "completed"})
	eventually(t, "match events reached master", func() bool {
		return fake.hasEvent("new-1", "match_start") && fake.hasEvent("new-1", "match_end")
	})
	rt.handle("new-1").exit <- Exit{Code: 0}
	eventually(t, "heartbeat shows stopped", func() bool {
		return fake.lastHeartbeatState("new-1") == "stopped"
	})
	if fake.hasEvent("new-1", "failed") {
		t.Fatal("clean exit after match_end must not report failed")
	}
}
