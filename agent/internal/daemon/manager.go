package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/ufna/birdman/agent/internal/config"
	"github.com/ufna/birdman/agent/internal/lifecycle"
	"github.com/ufna/birdman/agent/internal/ports"
	"github.com/ufna/birdman/agent/internal/stats"
	"github.com/ufna/birdman/agent/internal/uds"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// DefaultSocketDir hosts the per-server socket dirs (protocol.md §2:
// {socket_dir}/{server_id}/agent.sock, the dir is bind-mounted ro).
const DefaultSocketDir = "/run/birdman/servers"

const (
	defaultStopGraceS = 30               // agent.md §3
	pingEvery         = 10 * time.Second // liba keepalive
	libaSilenceMax    = 15 * time.Second // agent.md §4
	// tombstoneTTL keeps terminal servers in heartbeats long enough for the
	// master to observe the failed/stopped state even across a short link
	// outage; then they are evicted from the map.
	tombstoneTTL = 60 * time.Second
)

// Options configure the Manager.
type Options struct {
	Config    *config.Config
	Runtime   Runtime
	Sink      Sink
	SocketDir string // default DefaultSocketDir
	Logf      func(string, ...any)
}

// Manager supervises the node's dedicated servers: executes master commands,
// tracks per-server lifecycle (agent.md §3) and produces the state snapshot
// for Hello/heartbeats. It implements link.Handler and link.Source.
type Manager struct {
	ctx     context.Context // daemon lifetime; commands must not die with a session
	cfg     *config.Config
	rt      Runtime
	sink    Sink
	logf    func(string, ...any)
	stats   *stats.Collector
	pool    *ports.Pool
	sockDir string
	logDir  string

	mu      sync.Mutex
	servers map[string]*server
}

// server is one supervised dedicated server.
type server struct {
	id       string
	imageRef string
	machine  *lifecycle.Machine
	readyCh  chan struct{}
	stopCh   chan int32 // grace seconds

	mu            sync.Mutex
	port          int
	sock          *uds.Server
	handle        Handle
	players       int32
	tickMS        float32
	matchID       string
	stopRequested bool
	doneAt        time.Time
}

// NewManager creates the manager. ctx bounds all runtime operations — it
// must outlive link sessions (commands keep executing across reconnects).
func NewManager(ctx context.Context, o Options) (*Manager, error) {
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.SocketDir == "" {
		o.SocketDir = DefaultSocketDir
	}
	pool, err := ports.New(o.Config.PortRange[0], o.Config.PortRange[1])
	if err != nil {
		return nil, fmt.Errorf("port pool: %w", err)
	}
	logDir := filepath.Join(o.Config.LogDir, "servers")
	for _, dir := range []string{o.SocketDir, logDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return &Manager{
		ctx:     ctx,
		cfg:     o.Config,
		rt:      o.Runtime,
		sink:    o.Sink,
		logf:    o.Logf,
		stats:   stats.New(o.Config.DataDir),
		pool:    pool,
		sockDir: o.SocketDir,
		logDir:  logDir,
		servers: map[string]*server{},
	}, nil
}

// Restore rebuilds the server map from the runtime (agent.md §2): running
// containers are re-adopted at their recorded state (live matches survive an
// agent restart); dead ones are reported failed and cleaned up.
func (m *Manager) Restore(ctx context.Context) error {
	restored, err := m.rt.Restore(ctx)
	if err != nil {
		return err
	}
	for _, r := range restored {
		if !r.Running {
			m.logf("[daemon] restore %s: container dead (state label %q, exit %d) — cleaning up",
				r.ID, r.State, r.ExitCode)
			if r.State != string(lifecycle.StateDraining) {
				m.sink.ServerEvent(r.ID, "failed",
					fmt.Sprintf("found dead on agent restart (exit %d)", r.ExitCode))
			}
			if err := r.Handle.Delete(m.ctx); err != nil {
				m.logf("[daemon] restore %s: cleanup: %v", r.ID, err)
			}
			continue
		}

		state := restoredState(r.State)
		srv := m.newServer(r.ID, r.ImageRef, state)
		srv.matchID = r.MatchID
		srv.port = r.Port
		srv.handle = r.Handle
		if err := m.pool.AcquireSpecific(r.Port); err != nil {
			m.logf("[daemon] restore %s: port %d not tracked in pool: %v", r.ID, r.Port, err)
		}
		sock, err := m.listenSocket(srv)
		if err != nil {
			m.logf("[daemon] restore %s: socket: %v (liba link lost, container kept)", r.ID, err)
		} else {
			srv.sock = sock
		}

		m.mu.Lock()
		m.servers[r.ID] = srv
		m.mu.Unlock()
		m.logf("[daemon] restored %s state=%s port=%d image=%s", r.ID, state, r.Port, r.ImageRef)
		go m.supervise(srv)
	}
	return nil
}

// restoredState maps a recorded label state onto the machine start state.
// Unknown/early states restart the readiness grace.
func restoredState(label string) lifecycle.State {
	switch lifecycle.State(label) {
	case lifecycle.StateReady, lifecycle.StateAllocated, lifecycle.StateDraining:
		return lifecycle.State(label)
	default:
		return lifecycle.StateStarting
	}
}

// Shutdown closes liba sockets and stops supervision bookkeeping. Containers
// are NOT touched: dediks survive agent restarts (agent.md §2); the caller
// cancels the manager context right after.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, srv := range m.servers {
		srv.mu.Lock()
		if srv.sock != nil {
			srv.sock.Close()
		}
		srv.mu.Unlock()
	}
}

// --- link.Source ---

// Snapshot returns the current server map for Hello/heartbeats and evicts
// expired terminal tombstones.
func (m *Manager) Snapshot() []*agentlinkv1.ServerState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*agentlinkv1.ServerState, 0, len(m.servers))
	for id, srv := range m.servers {
		st := srv.machine.Current()
		srv.mu.Lock()
		if st.Terminal() && !srv.doneAt.IsZero() && time.Since(srv.doneAt) > tombstoneTTL {
			srv.mu.Unlock()
			delete(m.servers, id)
			continue
		}
		out = append(out, &agentlinkv1.ServerState{
			ServerId: id,
			State:    string(st),
			Players:  srv.players,
			TickMs:   srv.tickMS,
			MatchId:  srv.matchID,
			Port:     int32(srv.port),
			Version:  srv.imageRef,
		})
		srv.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ServerId < out[j].ServerId })
	return out
}

// NodeStats samples the node for the heartbeat.
func (m *Manager) NodeStats() *agentlinkv1.NodeStats {
	s := m.stats.Sample()
	return &agentlinkv1.NodeStats{
		Cpu:       s.CPU,
		MemUsed:   s.MemUsed,
		MemTotal:  s.MemTotal,
		DiskUsed:  s.DiskUsed,
		DiskTotal: s.DiskTotal,
		Load1:     s.Load1,
	}
}

// --- link.Handler ---

// Start handles StartServer. Idempotent by server_id: a replayed command for
// a known server (including one restored after an agent restart) is a no-op.
func (m *Manager) Start(_ context.Context, cmd *agentlinkv1.StartServer) {
	id := cmd.GetServerId()
	if id == "" {
		m.logf("[daemon] start: empty server_id ignored")
		return
	}
	m.mu.Lock()
	if _, ok := m.servers[id]; ok {
		m.mu.Unlock()
		m.logf("[daemon] start %s: already supervised (idempotent)", id)
		return
	}
	srv := m.newServer(id, cmd.GetImageRef(), lifecycle.StatePulling)
	m.servers[id] = srv
	m.mu.Unlock()
	go m.launch(srv, cmd)
}

// Stop handles StopServer{grace_s}: SIGTERM → grace → SIGKILL → delete.
func (m *Manager) Stop(_ context.Context, cmd *agentlinkv1.StopServer) {
	m.mu.Lock()
	srv, ok := m.servers[cmd.GetServerId()]
	m.mu.Unlock()
	if !ok {
		m.logf("[daemon] stop %s: unknown server (idempotent no-op)", cmd.GetServerId())
		return
	}
	grace := cmd.GetGraceS()
	if grace <= 0 {
		grace = defaultStopGraceS
	}
	select {
	case srv.stopCh <- grace:
	default: // stop already requested
	}
}

// Allocate handles AllocateServer (итерация 2): the master claimed this
// server for a match — forward `allocated{match_id, players_expected}` to
// liba (the UDS server caches the frame and replays it to a (re)connecting
// liba), move ready → allocated and persist it to the container labels so an
// agent restart keeps the match. Idempotent: a replayed command for an
// already-allocated server only refreshes the frame/labels.
func (m *Manager) Allocate(_ context.Context, cmd *agentlinkv1.AllocateServer) {
	id := cmd.GetServerId()
	m.mu.Lock()
	srv, ok := m.servers[id]
	m.mu.Unlock()
	if !ok {
		m.logf("[daemon] allocate %s: unknown server ignored (match %s)", id, cmd.GetMatchId())
		return
	}

	srv.mu.Lock()
	srv.matchID = cmd.GetMatchId()
	sock := srv.sock
	srv.mu.Unlock()

	switch cur := srv.machine.Current(); cur {
	case lifecycle.StateReady:
		m.transition(srv, lifecycle.StateAllocated,
			fmt.Sprintf("allocated by master (match %s)", cmd.GetMatchId()))
	case lifecycle.StateAllocated:
		// Replay after an agent restart (cmd_id cache is gone) — make sure
		// the labels carry the match id even if the first run missed it.
		m.logf("[daemon] allocate %s: already allocated (idempotent)", id)
		m.storeState(srv)
	default:
		// Master allocates only DB-ready servers; a mismatch is a stale or
		// racing command. Still deliver the frame — liba owns its own view —
		// while heartbeats keep reporting the real state.
		m.logf("[daemon] allocate %s: unexpected state %s (match %s)", id, cur, cmd.GetMatchId())
	}

	if sock == nil {
		m.logf("[daemon] allocate %s: no liba socket, frame undeliverable", id)
		return
	}
	if err := sock.SendAllocated(cmd.GetMatchId(), int(cmd.GetPlayersExpected())); err != nil {
		// ErrNotConnected is fine: the frame is cached and replayed when liba
		// (re)connects (protocol.md §2).
		m.logf("[daemon] allocate %s: send allocated: %v", id, err)
	}
}

// PrePull warms the image cache, reporting progress (protocol.md §1).
func (m *Manager) PrePull(_ context.Context, cmd *agentlinkv1.PrePull) {
	go func() {
		ref := cmd.GetImageRef()
		m.sink.PullReport(cmd.GetCmdId(), ref, "pulling", "")
		if err := m.rt.Pull(m.ctx, ref); err != nil {
			m.logf("[daemon] prepull %s: %v", ref, err)
			m.sink.PullReport(cmd.GetCmdId(), ref, "failed", err.Error())
			return
		}
		m.sink.PullReport(cmd.GetCmdId(), ref, "pulled", "")
	}()
}

// Drain acknowledges the node-level drain. Full drain behavior (stop
// creating, let matches finish, report drained) is iteration 4 — TODO.
func (m *Manager) Drain(_ context.Context, cmd *agentlinkv1.Drain) {
	m.logf("[daemon] drain requested (reason %q) — TODO iteration 4, ack only", cmd.GetReason())
}

// Unsupported handles commands not implemented in v0 (UpgradeAgent,
// TailLogs): ack + event so the master log shows why nothing happened.
func (m *Manager) Unsupported(_ context.Context, kind, cmdID, serverID string) {
	m.logf("[daemon] command %s (cmd_id %s) unsupported in v0 — TODO", kind, cmdID)
	if serverID != "" {
		m.sink.ServerEvent(serverID, "unsupported", kind+" is not implemented in agent v0")
	}
}

// --- internals ---

func (m *Manager) newServer(id, imageRef string, at lifecycle.State) *server {
	srv := &server{
		id:       id,
		imageRef: imageRef,
		readyCh:  make(chan struct{}, 1),
		stopCh:   make(chan int32, 1),
	}
	srv.machine = lifecycle.NewMachineAt(id, at, func(tr lifecycle.Transition) {
		if tr.Reason != "" {
			m.logf("[daemon] server %s: %s -> %s (%s)", id, tr.From, tr.To, tr.Reason)
		} else {
			m.logf("[daemon] server %s: %s -> %s", id, tr.From, tr.To)
		}
	})
	return srv
}

func (m *Manager) socketPath(id string) string {
	return filepath.Join(m.sockDir, id, "agent.sock")
}

// listenSocket (re)creates the per-server liba socket. The parent dir is
// what containers see via the ro bind mount, so recreating the socket file
// inside it survives agent restarts.
func (m *Manager) listenSocket(srv *server) (*uds.Server, error) {
	path := m.socketPath(srv.id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return uds.Listen(path, uds.Events{
		OnHello: func(v string) { m.logf("[daemon] server %s: liba hello sdk_version=%s", srv.id, v) },
		OnReady: func() {
			select {
			case srv.readyCh <- struct{}{}:
			default:
			}
		},
		OnPlayers: func(n int) {
			srv.mu.Lock()
			srv.players = int32(n)
			srv.mu.Unlock()
		},
		OnMetric: func(name string, v float64) {
			if name == "tick_ms" {
				srv.mu.Lock()
				srv.tickMS = float32(v)
				srv.mu.Unlock()
			}
		},
		OnMatchStart: func(matchID string) {
			srv.mu.Lock()
			srv.matchID = matchID
			srv.mu.Unlock()
			m.sink.ServerEvent(srv.id, "match_start", matchID)
			m.storeState(srv)
		},
		OnMatchEnd: func(matchID, result string) {
			m.sink.ServerEvent(srv.id, "match_end", matchID+" "+result)
		},
		OnLog: func(level, msg string) { m.appendLibaLog(srv.id, level, msg) },
		OnConnect: func() {
			m.logf("[daemon] server %s: liba connected", srv.id)
		},
		OnDisconnect: func() {
			m.logf("[daemon] server %s: liba disconnected", srv.id)
		},
	}, m.logf)
}

// launch runs the full StartServer path in its own goroutine: port, socket,
// image, container — then hands over to supervise.
func (m *Manager) launch(srv *server, cmd *agentlinkv1.StartServer) {
	var err error
	port := int(cmd.GetPort())
	if port > 0 {
		err = m.pool.AcquireSpecific(port)
	} else {
		port, err = m.pool.Acquire()
	}
	if err != nil {
		m.fail(srv, fmt.Sprintf("no host port: %v", err))
		return
	}
	srv.mu.Lock()
	srv.port = port
	srv.mu.Unlock()

	sock, err := m.listenSocket(srv)
	if err != nil {
		m.pool.Release(port)
		m.fail(srv, fmt.Sprintf("agent socket: %v", err))
		return
	}
	srv.mu.Lock()
	srv.sock = sock
	srv.mu.Unlock()

	cpu, mem := m.cfg.LimitsDefault.CPUMillis, m.cfg.LimitsDefault.MemMB
	if l := cmd.GetLimits(); l != nil {
		if l.GetCpuMillis() > 0 {
			cpu = int(l.GetCpuMillis())
		}
		if l.GetMemMb() > 0 {
			mem = int(l.GetMemMb())
		}
	}

	handle, err := m.rt.Start(m.ctx, StartSpec{
		ID:         srv.id,
		ImageRef:   srv.imageRef,
		Port:       port,
		Region:     m.cfg.Region,
		SocketPath: m.socketPath(srv.id),
		LogPath:    filepath.Join(m.logDir, srv.id+".log"),
		CPUMillis:  cpu,
		MemMB:      mem,
		Env:        cmd.GetEnv(),
	})
	if err != nil {
		sock.Close()
		os.RemoveAll(filepath.Dir(m.socketPath(srv.id)))
		m.pool.Release(port)
		m.fail(srv, fmt.Sprintf("container start: %v", err))
		return
	}
	srv.mu.Lock()
	srv.handle = handle
	srv.mu.Unlock()
	m.transition(srv, lifecycle.StateStarting, "container started")
	m.supervise(srv)
}

// supervise owns one server until it terminates or the agent shuts down.
func (m *Manager) supervise(srv *server) {
	srv.mu.Lock()
	handle := srv.handle
	sock := srv.sock
	srv.mu.Unlock()

	graceExpired := make(chan struct{}, 1)
	graceCtx, cancelGrace := context.WithCancel(m.ctx)
	defer cancelGrace()
	if srv.machine.Current() == lifecycle.StateStarting {
		go lifecycle.WatchReadyGrace(graceCtx, srv.machine, lifecycle.ReadyGrace, func() {
			select {
			case graceExpired <- struct{}{}:
			default:
			}
		})
	}

	pingTicker := time.NewTicker(pingEvery)
	defer pingTicker.Stop()
	var killCh <-chan time.Time

	for {
		select {
		case <-m.ctx.Done():
			return // agent shutdown: the container lives on (agent.md §2)

		case <-srv.readyCh:
			if srv.machine.Current() != lifecycle.StateStarting {
				continue // repeated ready (liba reconnect) — ignore
			}
			m.transition(srv, lifecycle.StateReady, "liba ready")

		case <-graceExpired:
			if srv.machine.Current() != lifecycle.StateStarting {
				continue
			}
			m.transition(srv, lifecycle.StateFailed,
				fmt.Sprintf("no ready within %s", lifecycle.ReadyGrace))
			if err := handle.Kill(m.ctx); err != nil {
				m.logf("[daemon] server %s: force kill: %v", srv.id, err)
			}

		case grace := <-srv.stopCh:
			srv.mu.Lock()
			already := srv.stopRequested
			srv.stopRequested = true
			srv.mu.Unlock()
			if already {
				continue
			}
			if cur := srv.machine.Current(); cur == lifecycle.StateReady || cur == lifecycle.StateAllocated {
				m.transition(srv, lifecycle.StateDraining, "stop requested by master")
			}
			m.logf("[daemon] server %s: stopping (grace %ds)", srv.id, grace)
			if err := handle.Signal(m.ctx, syscall.SIGTERM); err != nil {
				m.logf("[daemon] server %s: sigterm: %v", srv.id, err)
			}
			killCh = time.After(time.Duration(grace) * time.Second)

		case <-killCh:
			killCh = nil
			m.logf("[daemon] server %s: stop grace exceeded, SIGKILL", srv.id)
			if err := handle.Kill(m.ctx); err != nil {
				m.logf("[daemon] server %s: force kill: %v", srv.id, err)
			}

		case <-pingTicker.C:
			if sock == nil {
				continue
			}
			_ = sock.SendPing()
			if srv.machine.Current() == lifecycle.StateAllocated {
				if lh := sock.LastHeard(); !lh.IsZero() && time.Since(lh) > libaSilenceMax {
					m.logf("[daemon] server %s: liba silent for %s while allocated (unhealthy)",
						srv.id, time.Since(lh).Round(time.Second))
				}
			}

		case exit := <-handle.Wait():
			cancelGrace()
			m.finish(srv, handle, sock, exit)
			return
		}
	}
}

// finish handles container exit: final state, master event, cleanup.
func (m *Manager) finish(srv *server, handle Handle, sock *uds.Server, exit Exit) {
	srv.mu.Lock()
	intentional := srv.stopRequested
	port := srv.port
	srv.mu.Unlock()

	cur := srv.machine.Current()
	switch {
	case exit.Err != nil:
		m.logf("[daemon] server %s: exit watch failed: %v", srv.id, exit.Err)
		if !cur.Terminal() {
			m.transition(srv, lifecycle.StateFailed, "exit watch failed")
		}
	case cur.Terminal():
		// already failed (readiness grace) — keep it
	case intentional || exit.Code == 0:
		// master-requested stop or a clean self-exit (одноразовый дедик
		// after match_end) → stopped; the master reaps it.
		m.transition(srv, lifecycle.StateStopped, fmt.Sprintf("container exited (code %d)", exit.Code))
	default:
		m.transition(srv, lifecycle.StateFailed, fmt.Sprintf("container exited with code %d", exit.Code))
	}

	if err := handle.Delete(m.ctx); err != nil {
		m.logf("[daemon] server %s: cleanup: %v", srv.id, err)
	}
	if sock != nil {
		sock.Close()
	}
	os.RemoveAll(filepath.Dir(m.socketPath(srv.id)))
	if port > 0 {
		if err := m.pool.Release(port); err != nil {
			m.logf("[daemon] server %s: port release: %v", srv.id, err)
		}
	}
	srv.mu.Lock()
	srv.doneAt = time.Now()
	srv.mu.Unlock()
}

// fail marks a server failed before it ever ran (port/socket/start errors).
// The entry stays as a tombstone so heartbeats report the failure too.
func (m *Manager) fail(srv *server, reason string) {
	m.transition(srv, lifecycle.StateFailed, reason)
	srv.mu.Lock()
	srv.doneAt = time.Now()
	srv.mu.Unlock()
}

// transition moves the machine, persists the state label and emits the
// ready/failed ServerEvents the master lives by (protocol.md §1).
func (m *Manager) transition(srv *server, to lifecycle.State, reason string) {
	if err := srv.machine.To(to, reason); err != nil {
		m.logf("[daemon] server %s: %v", srv.id, err)
		return
	}
	switch to {
	case lifecycle.StateReady:
		m.sink.ServerEvent(srv.id, "ready", reason)
	case lifecycle.StateFailed:
		m.sink.ServerEvent(srv.id, "failed", reason)
	}
	if !to.Terminal() { // terminal containers are deleted right after
		m.storeState(srv)
	}
}

// storeState persists the current state (+match id) into container labels.
func (m *Manager) storeState(srv *server) {
	srv.mu.Lock()
	handle := srv.handle
	matchID := srv.matchID
	srv.mu.Unlock()
	if handle == nil {
		return
	}
	if err := handle.SetState(m.ctx, string(srv.machine.Current()), matchID); err != nil {
		m.logf("[daemon] server %s: set state label: %v", srv.id, err)
	}
}

// appendLibaLog appends a structured liba log frame to the server log file
// (the container stdout/stderr go there via the shim).
func (m *Manager) appendLibaLog(id, level, msg string) {
	path := filepath.Join(m.logDir, id+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		m.logf("[daemon] server %s: liba log: %v", id, err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s [liba/%s] %s\n", time.Now().UTC().Format(time.RFC3339), level, msg)
}
