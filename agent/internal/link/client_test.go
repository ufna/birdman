package link

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

const goodToken = "bnt_test.secret"

// fakeMaster is a scriptable AgentLink server: it records everything the
// agent sends and lets the test push commands into the live session.
type fakeMaster struct {
	agentlinkv1.UnimplementedAgentLinkServer
	t *testing.T

	mu         sync.Mutex
	hellos     []*agentlinkv1.Hello
	heartbeats []*agentlinkv1.Heartbeat
	events     []*agentlinkv1.ServerEvent
	pulls      []*agentlinkv1.PullReport
	acks       []string
	sessions   int
	stream     agentlinkv1.AgentLink_SessionServer
	kill       chan struct{}
}

func (f *fakeMaster) Session(stream agentlinkv1.AgentLink_SessionServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first message must be Hello")
	}
	if hello.GetNodeToken() != goodToken {
		return status.Error(codes.PermissionDenied, "bad node token")
	}
	kill := make(chan struct{})
	f.mu.Lock()
	f.hellos = append(f.hellos, hello)
	f.sessions++
	f.stream = stream
	f.kill = kill
	f.mu.Unlock()

	recvErr := make(chan error, 1)
	go func() {
		for {
			in, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			f.mu.Lock()
			switch m := in.GetMsg().(type) {
			case *agentlinkv1.AgentMsg_Heartbeat:
				f.heartbeats = append(f.heartbeats, m.Heartbeat)
			case *agentlinkv1.AgentMsg_Event:
				f.events = append(f.events, m.Event)
			case *agentlinkv1.AgentMsg_Pull:
				f.pulls = append(f.pulls, m.Pull)
			case *agentlinkv1.AgentMsg_Ack:
				f.acks = append(f.acks, m.Ack.GetCmdId())
			}
			f.mu.Unlock()
		}
	}()
	select {
	case err := <-recvErr:
		return err
	case <-kill:
		return status.Error(codes.Unavailable, "fake master dropped the session")
	}
}

// push sends a command into the current session.
func (f *fakeMaster) push(t *testing.T, msg *agentlinkv1.MasterMsg) {
	t.Helper()
	f.mu.Lock()
	stream := f.stream
	f.mu.Unlock()
	if stream == nil {
		t.Fatal("no live session to push into")
	}
	if err := stream.Send(msg); err != nil {
		t.Fatalf("push: %v", err)
	}
}

func (f *fakeMaster) dropSession() {
	f.mu.Lock()
	kill := f.kill
	f.kill = nil
	f.mu.Unlock()
	if kill != nil {
		close(kill)
	}
}

func (f *fakeMaster) counts() (sessions, heartbeats, events, acks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions, len(f.heartbeats), len(f.events), len(f.acks)
}

// fakeHandler records dispatched commands.
type fakeHandler struct {
	mu          sync.Mutex
	starts      []*agentlinkv1.StartServer
	stops       []*agentlinkv1.StopServer
	prepulls    []*agentlinkv1.PrePull
	drains      []*agentlinkv1.Drain
	unsupported []string
}

func (h *fakeHandler) Start(_ context.Context, c *agentlinkv1.StartServer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.starts = append(h.starts, c)
}
func (h *fakeHandler) Stop(_ context.Context, c *agentlinkv1.StopServer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stops = append(h.stops, c)
}
func (h *fakeHandler) PrePull(_ context.Context, c *agentlinkv1.PrePull) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prepulls = append(h.prepulls, c)
}
func (h *fakeHandler) Drain(_ context.Context, c *agentlinkv1.Drain) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.drains = append(h.drains, c)
}
func (h *fakeHandler) Unsupported(_ context.Context, kind, cmdID, serverID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unsupported = append(h.unsupported, kind)
}
func (h *fakeHandler) startCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.starts)
}

// fakeSource serves a fixed server map.
type fakeSource struct {
	mu      sync.Mutex
	servers []*agentlinkv1.ServerState
}

func (s *fakeSource) Snapshot() []*agentlinkv1.ServerState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*agentlinkv1.ServerState(nil), s.servers...)
}

func (s *fakeSource) NodeStats() *agentlinkv1.NodeStats {
	return &agentlinkv1.NodeStats{Cpu: 0.5, MemTotal: 1 << 30}
}

// harness wires a fake master behind a swappable bufconn listener so tests
// can simulate a full master restart.
type harness struct {
	t      *testing.T
	fake   *fakeMaster
	lis    atomic.Pointer[bufconn.Listener]
	server *grpc.Server
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, fake: &fakeMaster{t: t}}
	h.startServer()
	t.Cleanup(func() { h.server.Stop() })
	return h
}

func (h *harness) startServer() {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	agentlinkv1.RegisterAgentLinkServer(srv, h.fake)
	go func() { _ = srv.Serve(lis) }()
	h.lis.Store(lis)
	h.server = srv
}

// restart simulates a master restart: the old server dies with all streams,
// a fresh one takes over the same "address".
func (h *harness) restart() {
	h.server.Stop()
	h.startServer()
}

func (h *harness) clientConfig() Config {
	return Config{
		MasterAddr:    "passthrough:///bufconn",
		NodeToken:     goodToken,
		Hostname:      "test-node",
		Region:        "dev",
		CapacitySlots: 8,
		AgentVersion:  "test",

		HeartbeatEvery: 30 * time.Millisecond,
		BackoffMin:     20 * time.Millisecond,
		BackoffMax:     100 * time.Millisecond,

		DialOptions: []grpc.DialOption{
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return h.lis.Load().DialContext(ctx)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met in time: %s", what)
}

func startClient(t *testing.T, h *harness, handler Handler, src Source, outbox *Outbox) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	c := New(h.clientConfig(), handler, src, outbox, t.Logf)
	go func() {
		defer close(done)
		_ = c.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func TestHelloCarriesServerMapAndHeartbeats(t *testing.T) {
	h := newHarness(t)
	src := &fakeSource{servers: []*agentlinkv1.ServerState{
		{ServerId: "s1", State: "ready", Port: 20001, Version: "img:1"},
		{ServerId: "s2", State: "allocated", Port: 20002, MatchId: "m1", Version: "img:1"},
	}}
	startClient(t, h, &fakeHandler{}, src, NewOutbox(t.Logf))

	eventually(t, "hello received", func() bool {
		h.fake.mu.Lock()
		defer h.fake.mu.Unlock()
		return len(h.fake.hellos) == 1
	})
	h.fake.mu.Lock()
	hello := h.fake.hellos[0]
	h.fake.mu.Unlock()
	if hello.GetHostname() != "test-node" || hello.GetRegion() != "dev" ||
		hello.GetCapacitySlots() != 8 || hello.GetAgentVersion() != "test" {
		t.Fatalf("hello fields: %+v", hello)
	}
	if len(hello.GetServers()) != 2 || hello.GetServers()[0].GetServerId() != "s1" ||
		hello.GetServers()[1].GetMatchId() != "m1" {
		t.Fatalf("hello server map: %+v", hello.GetServers())
	}

	eventually(t, "heartbeats flow", func() bool {
		_, hb, _, _ := h.fake.counts()
		return hb >= 2
	})
	h.fake.mu.Lock()
	hb := h.fake.heartbeats[0]
	h.fake.mu.Unlock()
	if hb.GetNode().GetCpu() != 0.5 || len(hb.GetServers()) != 2 || hb.GetTsUnixMs() == 0 {
		t.Fatalf("heartbeat: %+v", hb)
	}
}

func TestCommandDispatchAckAndIdempotency(t *testing.T) {
	h := newHarness(t)
	handler := &fakeHandler{}
	startClient(t, h, handler, &fakeSource{}, NewOutbox(t.Logf))
	eventually(t, "connected", func() bool { s, _, _, _ := h.fake.counts(); return s == 1 })

	start := func() *agentlinkv1.MasterMsg {
		return &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_Start{Start: &agentlinkv1.StartServer{
			ServerId: "s1", ImageRef: "img:1", CmdId: "cmd-1",
		}}}
	}
	h.fake.push(t, start())
	eventually(t, "start dispatched and acked", func() bool {
		_, _, _, acks := h.fake.counts()
		return handler.startCount() == 1 && acks == 1
	})

	// The same cmd_id again: re-acked, NOT re-executed (at-least-once
	// delivery, idempotent handling — protocol.md §1).
	h.fake.push(t, start())
	eventually(t, "duplicate re-acked", func() bool {
		_, _, _, acks := h.fake.counts()
		return acks == 2
	})
	if handler.startCount() != 1 {
		t.Fatalf("duplicate cmd_id executed twice: %d starts", handler.startCount())
	}
	h.fake.mu.Lock()
	acks := append([]string(nil), h.fake.acks...)
	h.fake.mu.Unlock()
	if acks[0] != "cmd-1" || acks[1] != "cmd-1" {
		t.Fatalf("acks: %v", acks)
	}

	// Other command kinds reach their handlers (unsupported ones included).
	h.fake.push(t, &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_Stop{Stop: &agentlinkv1.StopServer{
		ServerId: "s1", GraceS: 5, CmdId: "cmd-2",
	}}})
	h.fake.push(t, &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_Tail{Tail: &agentlinkv1.TailLogs{
		ServerId: "s1", CmdId: "cmd-3",
	}}})
	eventually(t, "stop and tail handled", func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return len(handler.stops) == 1 && len(handler.unsupported) == 1
	})
	handler.mu.Lock()
	if handler.unsupported[0] != "tail_logs" {
		t.Fatalf("unsupported: %v", handler.unsupported)
	}
	handler.mu.Unlock()
}

func TestReconnectCycle(t *testing.T) {
	h := newHarness(t)
	src := &fakeSource{servers: []*agentlinkv1.ServerState{{ServerId: "s1", State: "ready"}}}
	startClient(t, h, &fakeHandler{}, src, NewOutbox(t.Logf))
	eventually(t, "first session", func() bool { s, _, _, _ := h.fake.counts(); return s == 1 })

	// Master drops the stream: the client must reconnect with a fresh Hello
	// carrying the current map.
	h.fake.dropSession()
	eventually(t, "second session after drop", func() bool {
		s, _, _, _ := h.fake.counts()
		return s == 2
	})
	h.fake.mu.Lock()
	if len(h.fake.hellos[1].GetServers()) != 1 {
		t.Fatalf("hello after reconnect lost the map: %+v", h.fake.hellos[1])
	}
	h.fake.mu.Unlock()

	// Full master restart (new listener, new server instance).
	h.restart()
	eventually(t, "third session after master restart", func() bool {
		s, _, _, _ := h.fake.counts()
		return s == 3
	})
	eventually(t, "heartbeats resume", func() bool {
		h.fake.mu.Lock()
		defer h.fake.mu.Unlock()
		return len(h.fake.heartbeats) > 0
	})
}

func TestOutboxSurvivesDisconnect(t *testing.T) {
	h := newHarness(t)
	outbox := NewOutbox(t.Logf)
	// Events queued before any session exist must arrive after connect.
	outbox.ServerEvent("s1", "failed", "container exited with code 137")
	outbox.PullReport("cmd-9", "img:1", "pulled", "")
	startClient(t, h, &fakeHandler{}, &fakeSource{}, outbox)

	eventually(t, "queued event and pull report delivered", func() bool {
		h.fake.mu.Lock()
		defer h.fake.mu.Unlock()
		return len(h.fake.events) == 1 && len(h.fake.pulls) == 1
	})
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if h.fake.events[0].GetKind() != "failed" || h.fake.pulls[0].GetStatus() != "pulled" {
		t.Fatalf("delivered: %+v %+v", h.fake.events[0], h.fake.pulls[0])
	}
	if outbox.Len() != 0 {
		t.Fatalf("outbox not drained: %d", outbox.Len())
	}
}

func TestBadTokenBacksOffToMax(t *testing.T) {
	h := newHarness(t)
	cfg := h.clientConfig()
	cfg.NodeToken = "bnt_wrong.token"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New(cfg, &fakeHandler{}, &fakeSource{}, NewOutbox(t.Logf), t.Logf)
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx) }()

	// The client keeps retrying (never authenticates) without spinning.
	time.Sleep(300 * time.Millisecond)
	if c.Connected() {
		t.Fatal("client must not stay connected with a bad token")
	}
	s, _, _, _ := h.fake.counts()
	if s != 0 {
		t.Fatalf("bad token must never create a session, got %d", s)
	}
	cancel()
	<-done
}

func TestCmdCache(t *testing.T) {
	c := newCmdCache(2)
	if !c.add("a") || !c.add("b") {
		t.Fatal("fresh ids must be new")
	}
	if c.add("a") {
		t.Fatal("duplicate within window must be rejected")
	}
	if !c.add("c") { // evicts "a"
		t.Fatal("new id must be accepted")
	}
	if !c.add("a") {
		t.Fatal("evicted id is treated as new again (window slid)")
	}
}
