package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ufna/birdman/agent/internal/cgroups"
	"github.com/ufna/birdman/agent/internal/config"
	"github.com/ufna/birdman/agent/internal/lifecycle"
	"github.com/ufna/birdman/agent/internal/logrot"
	"github.com/ufna/birdman/agent/internal/metrics"
	"github.com/ufna/birdman/agent/internal/ports"
	"github.com/ufna/birdman/agent/internal/runtime"
	"github.com/ufna/birdman/agent/internal/stats"
	"github.com/ufna/birdman/agent/internal/uds"
	"github.com/ufna/birdman/agent/internal/upgrade"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// legacyDefaultRegistryHost is the host assumed for a legacy agent.yaml
// registry_auth block that does not set host (docs/superpowers/specs/2026-07-09-registries-design.md
// §3): the pre-registries-v1 fallback only ever talked to ghcr.io.
const legacyDefaultRegistryHost = "ghcr.io"

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

	// AgentVersion is the running build (Hello/UpgradeAgent no-op detection).
	AgentVersion string
	// BinaryPath is the file UpgradeAgent replaces; default os.Executable().
	BinaryPath string
	// OnUpgraded fires after a successful self-upgrade download+swap: the
	// caller shuts the agent down cleanly and systemd restarts the new
	// binary (agent.md §7). Containers survive (§2).
	OnUpgraded func(version string)
	// TouchImage feeds the image GC protected set on StartServer/PrePull
	// (agent.md §6). May be nil.
	TouchImage func(imageRef string)
	// UntouchImage drops a ref from the image GC protected set after RemoveImage
	// retires it (environments v1 §6б, РЕВИЗИЯ M12): a dead ref must not keep
	// occupying a protection slot. May be nil.
	UntouchImage func(imageRef string)
	// ContainerdDiskUsage samples the containerd-root filesystem for the
	// birdman_agent_containerd_disk_* metrics (dual-fs watermark, §6в). May be
	// nil (metrics then report zero for the containerd pair).
	ContainerdDiskUsage func() (used, total uint64)
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

	agentVersion string
	binaryPath   string
	onUpgraded   func(string)
	touchImage   func(string)
	untouchImage func(string)
	// containerdDiskUsage samples the containerd-root filesystem for the
	// dual-fs watermark metrics (§6в); may be nil.
	containerdDiskUsage func() (used, total uint64)
	// applyUpgrade is upgrade.Apply, injectable for tests.
	applyUpgrade func(ctx context.Context, url, sha256hex, dest string) error
	// finalizeLog gzips a stopped server's logs (logrot.Rotator.Finalize).
	finalizeLog func(serverID string)
	// diskUsage samples the data_dir filesystem (stats.DiskUsage in prod).
	diskUsage func() (used, total uint64)
	// diskFullFrac refuses StartServer above this usage fraction (§6);
	// 0 disables the guard (configs built without defaults, tests).
	diskFullFrac float64

	// registries is the in-memory master registry-credential snapshot
	// (registries v1, §2/§3): SetRegistries replaces it wholesale, never
	// persisted to disk.
	registries *registryStore
	// warnLegacyHostOnce guards the one-time WARN emitted the first time the
	// legacy registry_auth fallback is consulted without an explicit host
	// (§3) — logged once per process, not once per pull.
	warnLegacyHostOnce sync.Once

	// draining is the node-level drain flag (итерация 4): while set, new
	// StartServer commands are rejected with a failed event. The master's
	// nodes.state stays authoritative — the flag only closes the race of
	// commands already in flight when the drain landed.
	draining atomic.Bool

	mu      sync.Mutex
	servers map[string]*server

	tailsMu sync.Mutex
	tails   map[string]context.CancelFunc // TailLogs cmd_id → cancel
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
	// logPath — файл, в который шим пишет вывод этого дедика. С #994 он
	// зависит от пары (project, env), поэтому запоминается: у liba-кадров
	// частота строк игрового лога, и резолвить каталог globом на каждый кадр
	// было бы платой на горячем пути.
	logPath string
	// project/env — пара владельца дедика (tracker #1008). Проставляется при
	// старте из StartServer.env и ВОССТАНАВЛИВАЕТСЯ из label'ов контейнера
	// после рестарта агента, поэтому пер-серверные серии /metrics несут её и у
	// пережившего рестарт дедика. Пусты у дедика, запущенного до появления
	// label'ов, — его серии остаются беспарными до перекрутки.
	project string
	env     string
}

// logFilePath — путь лога дедика, разрешаемый один раз. Для запущенного этим
// агентом сервера он проставлен при старте; для восстановленного после
// рестарта агента (пары в label'ах контейнера нет) резолвится по файловой
// системе — файл уже создан шимом в момент старта контейнера, так что глоб его
// находит и вывод не раздваивается по двум путям.
func (s *server) logFilePath(root string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logPath == "" {
		s.logPath = filepath.Join(logrot.ServerDir(root, s.id), s.id+".log")
	}
	return s.logPath
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
	if o.TouchImage == nil {
		o.TouchImage = func(string) {}
	}
	if o.UntouchImage == nil {
		o.UntouchImage = func(string) {}
	}
	if o.OnUpgraded == nil {
		o.OnUpgraded = func(string) {}
	}
	if o.BinaryPath == "" {
		if exe, err := os.Executable(); err == nil {
			o.BinaryPath = exe
		}
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
	dataDir := o.Config.DataDir
	return &Manager{
		ctx:                 ctx,
		cfg:                 o.Config,
		rt:                  o.Runtime,
		sink:                o.Sink,
		logf:                o.Logf,
		stats:               stats.New(dataDir),
		pool:                pool,
		sockDir:             o.SocketDir,
		logDir:              logDir,
		agentVersion:        o.AgentVersion,
		binaryPath:          o.BinaryPath,
		onUpgraded:          o.OnUpgraded,
		touchImage:          o.TouchImage,
		untouchImage:        o.UntouchImage,
		containerdDiskUsage: o.ContainerdDiskUsage,
		applyUpgrade:        upgrade.Apply,
		finalizeLog:         func(string) {},
		diskUsage:           func() (uint64, uint64) { return stats.DiskUsage(dataDir) },
		registries:          newRegistryStore(),
		servers:             map[string]*server{},
		tails:               map[string]context.CancelFunc{},
	}, nil
}

// SetLogFinalizer wires the log gzip hook called after a server container is
// gone (logrot.Rotator.Finalize). Call before Restore/first command.
func (m *Manager) SetLogFinalizer(fn func(serverID string)) {
	if fn != nil {
		m.finalizeLog = fn
	}
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
			go m.finalizeLog(r.ID)
			continue
		}

		state := restoredState(r.State)
		srv := m.newServer(r.ID, r.ImageRef, state)
		srv.matchID = r.MatchID
		srv.port = r.Port
		srv.handle = r.Handle
		// Пара владельца переживает рестарт агента ТЕМ ЖЕ путём, что порт,
		// образ и состояние, — она лежит в label'ах контейнера (tracker
		// #1008). Алфавит проверяется и здесь: label мог быть отредактирован
		// на ноде руками, а отсюда значение уходит прямо в лейбл серии
		// Prometheus. Не прошло — дедик просто беспарный, как до апгрейда.
		srv.project, srv.env, _ = scopePair(r.ScopeProject, r.ScopeEnv)
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
		Draining:  m.draining.Load(),
	}
}

// LiveServerIDs lists non-terminal servers (log rotation targets).
func (m *Manager) LiveServerIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.servers))
	for id, srv := range m.servers {
		if !srv.machine.Current().Terminal() {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// MetricsSample snapshots the agent for the /metrics endpoint (agent.md §9).
func (m *Manager) MetricsSample() metrics.Sample {
	s := metrics.Sample{
		Draining: m.draining.Load(),
		States: map[string]int{
			string(lifecycle.StatePulling): 0, string(lifecycle.StateStarting): 0,
			string(lifecycle.StateReady): 0, string(lifecycle.StateAllocated): 0,
			string(lifecycle.StateDraining): 0, string(lifecycle.StateStopped): 0,
			string(lifecycle.StateFailed): 0,
		},
	}
	m.mu.Lock()
	type live struct {
		sample metrics.ServerSample
		handle Handle
	}
	var servers []live
	for id, srv := range m.servers {
		st := srv.machine.Current()
		s.States[string(st)]++
		if st.Terminal() {
			continue // tombstones carry no live gauges
		}
		srv.mu.Lock()
		servers = append(servers, live{
			sample: metrics.ServerSample{
				ID: id, State: string(st), Players: srv.players, TickMS: srv.tickMS,
				Project: srv.project, Env: srv.env,
			},
			handle: srv.handle,
		})
		srv.mu.Unlock()
	}
	m.mu.Unlock()

	for _, lv := range servers {
		if lv.handle != nil {
			if pid := lv.handle.Pid(); pid > 0 {
				if u, err := cgroups.Read(pid); err == nil {
					lv.sample.HasUsage = true
					lv.sample.CPUSeconds = u.CPUSeconds
					lv.sample.MemBytes = u.MemBytes
				}
			}
		}
		s.Servers = append(s.Servers, lv.sample)
	}
	s.PortsUsed = m.pool.InUse()
	s.PortsTotal = m.pool.Capacity()
	s.DiskUsed, s.DiskTotal = m.diskUsage()
	// Containerd-root filesystem for the dual-fs watermark (§6в). When it is
	// not a separate mount this equals the data_dir pair above — expected.
	if m.containerdDiskUsage != nil {
		s.ContainerdDiskUsed, s.ContainerdDiskTotal = m.containerdDiskUsage()
	}
	return s
}

// --- link.Handler ---

// SetRegistries replaces the in-memory master registry-credential snapshot
// (registries v1, docs/superpowers/specs/2026-07-09-registries-design.md
// §2/§3): every delivery is a FULL replace, never a diff. The Ack itself
// travels through the existing link.Client machinery (recvLoop acks any
// dispatched cmd_id) — nothing extra to do here beyond storing the set.
func (m *Manager) SetRegistries(_ context.Context, cmd *agentlinkv1.SetRegistries) {
	m.registries.Set(cmd.GetRegistries())
	m.logf("[daemon] set_registries: %d credential(s) in memory", len(cmd.GetRegistries()))
}

// Start handles StartServer. Idempotent by server_id: a replayed command for
// a known server (including one restored after an agent restart) is a no-op.
// A draining node and a full disk reject the start with a failed event
// (agent.md §6, итерация 4 node drain).
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

	if m.draining.Load() {
		m.fail(srv, "node draining — start rejected")
		return
	}
	if used, total := m.diskUsage(); total > 0 {
		if frac := float64(used) / float64(total); frac >= float64(m.cfg.DiskFullWatermarkPct)/100 {
			detail := fmt.Sprintf("data_dir disk %.0f%% >= %d%%", frac*100, m.cfg.DiskFullWatermarkPct)
			m.sink.ServerEvent(id, "disk_full", detail)
			m.fail(srv, "disk full — start rejected ("+detail+")")
			return
		}
	}
	m.touchImage(cmd.GetImageRef())
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
	if err := sock.SendAllocated(cmd.GetMatchId(), int(cmd.GetPlayersExpected()), cmd.GetMetadata()); err != nil {
		// ErrNotConnected is fine: the frame is cached and replayed when liba
		// (re)connects (protocol.md §2).
		m.logf("[daemon] allocate %s: send allocated: %v", id, err)
	}
}

// PrePull warms the image cache, reporting progress (protocol.md §1).
func (m *Manager) PrePull(_ context.Context, cmd *agentlinkv1.PrePull) {
	m.touchImage(cmd.GetImageRef())
	go func() {
		ref := cmd.GetImageRef()
		m.sink.PullReport(cmd.GetCmdId(), ref, "pulling", "")
		if err := m.rt.Pull(m.ctx, ref, m.pullLookup(ref)); err != nil {
			m.logf("[daemon] prepull %s: %v", ref, err)
			m.sink.PullReport(cmd.GetCmdId(), ref, "failed", err.Error())
			return
		}
		m.sink.PullReport(cmd.GetCmdId(), ref, "pulled", "")
	}()
}

// RemoveImage retires a disabled version's image from the node (environments
// v1 §6б, РЕВИЗИЯ I1). Like PrePull, dispatch returns immediately (the plain
// Ack confirms receipt via the recv loop — RemoveImage does NOT extend Ack)
// and the runtime work runs in a goroutine: synchronous work here would block
// the recv loop that acks every command. Branches:
//   - image backs a live container → skipped (the version's servers are still
//     draining; the watermark GC reclaims it once the container exits);
//   - image absent → no-op (idempotent under at-least-once replay);
//   - otherwise → SynchronousDelete, and the ref is dropped from the GC
//     protected set (РЕВИЗИЯ M12: a dead ref must not hold a protection slot).
//
// EVERY branch reports its outcome back with ImageReport{status} (removed |
// absent | busy | error) — the master needs the RESULT, not just the Ack: it
// stamps versions.image_cleanup_at only when every target node reported
// removed|absent, and re-sends RemoveImage otherwise (a busy image means the
// containers had not exited yet — by the next cleanup subtick they have).
//
// A deprecated version's image is never targeted (the master's guard keeps the
// rollback window warm); a missed removal self-heals via EnsureImage on the
// next StartServer.
func (m *Manager) RemoveImage(_ context.Context, cmd *agentlinkv1.RemoveImage) {
	cmdID, ref := cmd.GetCmdId(), cmd.GetImageRef()
	if ref == "" {
		// Master never sends this (its dispatcher skips empty refs) — report it as
		// an error rather than staying silent: exactly one report per command keeps
		// the master's «жду отчёта от каждой ноды» bookkeeping unambiguous.
		m.logf("[daemon] remove_image: empty image_ref ignored")
		m.sink.ImageReport(cmdID, ref, "error", "empty image_ref")
		return
	}
	go func() {
		used, err := m.rt.UsedImageRefs(m.ctx)
		if err != nil {
			m.logf("[daemon] remove_image %s: list used refs: %v — skipped (watermark GC is the backstop)", ref, err)
			m.sink.ImageReport(cmdID, ref, "error", err.Error())
			return
		}
		if used[ref] {
			m.logf("[daemon] remove_image %s: in use by a live container — skipped (watermark GC reclaims it later)", ref)
			m.sink.ImageReport(cmdID, ref, "busy", "")
			return
		}
		present, err := m.rt.ImagePresent(m.ctx, ref)
		if err != nil {
			m.logf("[daemon] remove_image %s: image lookup: %v — skipped (watermark GC is the backstop)", ref, err)
			m.sink.ImageReport(cmdID, ref, "error", err.Error())
			return
		}
		if !present {
			// Already gone (replay, or the watermark GC beat us to it): drop any
			// stale protection slot and treat as a clean no-op.
			m.untouchImage(ref)
			m.logf("[daemon] remove_image %s: not present — no-op (idempotent)", ref)
			m.sink.ImageReport(cmdID, ref, "absent", "")
			return
		}
		if err := m.rt.DeleteImage(m.ctx, ref); err != nil {
			m.logf("[daemon] remove_image %s: delete: %v — skipped (watermark GC is the backstop)", ref, err)
			m.sink.ImageReport(cmdID, ref, "error", err.Error())
			return
		}
		m.untouchImage(ref)
		m.logf("[daemon] remove_image %s: removed", ref)
		m.sink.ImageReport(cmdID, ref, "removed", "")
	}()
}

// Drain applies the node-level drain (итерация 4, master.md §6): mark the
// node draining — new StartServer commands are rejected, running servers
// play out (the master's reconcile reaps/переносит the rest). Heartbeats
// report the flag (NodeStats.draining).
func (m *Manager) Drain(_ context.Context, cmd *agentlinkv1.Drain) {
	if m.draining.CompareAndSwap(false, true) {
		m.logf("[daemon] node drain: rejecting new servers (reason %q)", cmd.GetReason())
	} else {
		m.logf("[daemon] node drain: already draining (idempotent)")
	}
}

// Undrain lifts the node-level drain (итерация 4).
func (m *Manager) Undrain(_ context.Context, _ *agentlinkv1.Undrain) {
	if m.draining.CompareAndSwap(true, false) {
		m.logf("[daemon] node undrain: accepting servers again")
	} else {
		m.logf("[daemon] node undrain: was not draining (idempotent)")
	}
}

// Draining reports the node-level drain flag (tests).
func (m *Manager) Draining() bool { return m.draining.Load() }

// Upgrade handles UpgradeAgent (agent.md §7): download → sha256 → atomic
// rename, then OnUpgraded — the caller exits cleanly and systemd restarts
// the new binary (containers survive, §2). A replayed command for the
// already-running version is a no-op: that breaks the restart loop when the
// Ack raced the restart.
func (m *Manager) Upgrade(_ context.Context, cmd *agentlinkv1.UpgradeAgent) {
	version := cmd.GetVersion()
	if version != "" && version == m.agentVersion {
		m.logf("[daemon] upgrade to %s: already running it (no-op)", version)
		return
	}
	if m.binaryPath == "" {
		m.logf("[daemon] upgrade to %s: binary path unknown — refusing", version)
		return
	}
	go func() {
		m.logf("[daemon] upgrade to %s: downloading %s", version, cmd.GetUrl())
		if err := m.applyUpgrade(m.ctx, cmd.GetUrl(), cmd.GetSha256(), m.binaryPath); err != nil {
			// The master's watchdog raises agent_upgrade_failed when the new
			// version does not Hello within 60s (ops.md §1).
			m.logf("[daemon] upgrade to %s failed: %v (binary untouched)", version, err)
			return
		}
		m.logf("[daemon] upgrade to %s: binary swapped — restarting (dediks keep running)", version)
		m.onUpgraded(version)
	}()
}

// TailLogs streams a server log to the master (agent.md §5): LogChunk frames
// answer cmd_id until eof/cancel. Works for dead (reaped/failed) servers too —
// the log files (or their gzip archives) stay on disk for the retention
// period. A message carrying cancel_cmd_id stops that running tail instead.
func (m *Manager) TailLogs(_ context.Context, cmd *agentlinkv1.TailLogs) {
	if cc := cmd.GetCancelCmdId(); cc != "" {
		m.tailsMu.Lock()
		cancel, ok := m.tails[cc]
		m.tailsMu.Unlock()
		if ok {
			cancel()
			m.logf("[daemon] tail %s cancelled by master", cc)
		}
		return
	}
	cmdID, serverID := cmd.GetCmdId(), cmd.GetServerId()
	tctx, cancel := context.WithCancel(m.ctx)
	m.tailsMu.Lock()
	m.tails[cmdID] = cancel
	m.tailsMu.Unlock()

	go func() {
		defer func() {
			cancel()
			m.tailsMu.Lock()
			delete(m.tails, cmdID)
			m.tailsMu.Unlock()
		}()
		err := logrot.Stream(tctx, m.logDir, serverID, int(cmd.GetTailLines()), cmd.GetFollow(),
			func(b []byte) error {
				if !m.sink.LogChunk(tctx, cmdID, serverID, b, false) {
					return fmt.Errorf("log chunk backpressure — cancelling tail")
				}
				return nil
			})
		switch {
		case err == nil:
		case err == logrot.ErrNoLogs:
			m.sink.LogChunk(tctx, cmdID, serverID, []byte("no logs for server "+serverID+"\n"), false)
		case tctx.Err() != nil:
			// cancelled (master/agent shutdown) — eof below is best effort
		default:
			m.logf("[daemon] tail %s (%s): %v", cmdID, serverID, err)
		}
		m.sink.LogChunk(tctx, cmdID, serverID, nil, true)
		m.logf("[daemon] tail %s (%s) finished", cmdID, serverID)
	}()
}

// DrainServer handles the per-server drain (итерация 3: reap deprecated
// versions on deploy, docs/specs/master.md §5): move the server to
// `draining`, forward `drain{deadline_s, reason}` to liba (the UDS server
// caches the frame and replays it to a (re)connecting liba) and let the dedik
// finish its match and exit on its own — no signals, unlike StopServer.
// Idempotent: a replayed command only re-delivers the frame.
func (m *Manager) DrainServer(_ context.Context, cmd *agentlinkv1.DrainServer) {
	id := cmd.GetServerId()
	m.mu.Lock()
	srv, ok := m.servers[id]
	m.mu.Unlock()
	if !ok {
		m.logf("[daemon] drain %s: unknown server (idempotent no-op)", id)
		return
	}

	switch cur := srv.machine.Current(); cur {
	case lifecycle.StateReady, lifecycle.StateAllocated:
		m.transition(srv, lifecycle.StateDraining,
			fmt.Sprintf("drain requested by master (%s)", cmd.GetReason()))
	case lifecycle.StateDraining:
		m.logf("[daemon] drain %s: already draining (idempotent)", id)
	default:
		// Too early (starting) or already terminal — deliver the frame anyway,
		// liba owns its own view; heartbeats keep reporting the real state.
		m.logf("[daemon] drain %s: unexpected state %s", id, cur)
	}

	srv.mu.Lock()
	sock := srv.sock
	srv.mu.Unlock()
	if sock == nil {
		m.logf("[daemon] drain %s: no liba socket, frame undeliverable", id)
		return
	}
	if err := sock.SendDrain(int(cmd.GetDeadlineS()), cmd.GetReason()); err != nil {
		// ErrNotConnected is fine: the frame is cached and replayed when liba
		// (re)connects (protocol.md §2).
		m.logf("[daemon] drain %s: send drain: %v", id, err)
	}
}

// --- internals ---

// pullLookup builds the CredLookup for one pull attempt (StartServer's
// launch or PrePull) and logs its predicted host+source ONCE, upfront: `host=
// … source=master|legacy|anonymous`, never the token — "why was this pull
// anonymous" is then debuggable from the agent log alone
// (docs/superpowers/specs/2026-07-09-registries-design.md §3). The returned
// lookup re-resolves the chain against whatever host containerd's docker
// authorizer actually calls back with (the resolver-provided host — not
// necessarily identical to the one predicted here for an unusual ref, but
// that is the value that actually gates the credential).
func (m *Manager) pullLookup(imageRef string) runtime.CredLookup {
	host, _ := runtime.HostFromRef(imageRef) // ok=false → host="" → chain falls through to anonymous
	_, _, source, _ := m.resolveRegistryAuth(host)
	// Advisory only: this log reflects HostFromRef's own parse of imageRef
	// and can diverge from containerd's own resolver parsing on edge refs —
	// the callback below is what actually enforces the match, against
	// containerd's own resolver-provided host.
	m.logf("[daemon] pull %s: host=%s source=%s", imageRef, host, source)
	return func(cbHost string) (string, string, error) {
		u, t, _, err := m.resolveRegistryAuth(strings.ToLower(cbHost))
		return u, t, err
	}
}

// resolveRegistryAuth resolves a pull credential for host (normalized
// lowercase) through the chain: master snapshot (exact host match) → legacy
// agent.yaml file cred (also host-scoped) → anonymous
// (docs/superpowers/specs/2026-07-09-registries-design.md §3). source is for
// the per-pull observability log ONLY — never log username/token alongside
// it. err != nil means host matched the legacy registry_auth block but its
// token_file could not be read: the caller (the CredLookup closure returned
// by pullLookup) must fail the pull with err, never silently degrade to
// anonymous — a broken token_file is a day-one config typo that deserves an
// actionable error, not a confusing registry 401 down the line.
func (m *Manager) resolveRegistryAuth(host string) (username, token, source string, err error) {
	if u, t, found := m.registries.Lookup(host); found {
		return u, t, "master", nil
	}
	u, t, found, lerr := m.legacyCred(host)
	if lerr != nil {
		return "", "", "legacy", lerr
	}
	if found {
		return u, t, "legacy", nil
	}
	return "", "", "anonymous", nil
}

// legacyCred matches host against the legacy agent.yaml registry_auth block
// (host-scoped fallback, §3): an attacker-controlled image_ref on a foreign
// host must not receive this credential either. token is read fresh from
// TokenFile on every call so a rotated file is picked up without a restart
// (matches the pre-registries-v1 behavior of ContainerdRuntime.creds). A
// host match whose token_file can't be read returns found=false and a
// non-nil err — resolveRegistryAuth turns that into a failed pull rather
// than an anonymous one (see there).
func (m *Manager) legacyCred(host string) (username, token string, found bool, err error) {
	a := m.cfg.RegistryAuth
	if a == nil {
		return "", "", false, nil
	}
	if m.legacyRegistryHost() != host {
		return "", "", false, nil
	}
	tok, err := a.Token()
	if err != nil {
		return "", "", false, err
	}
	return a.Username, tok, true, nil
}

// legacyRegistryHost returns the configured registry_auth.host, defaulting
// to ghcr.io (the only host the pre-registries-v1 fallback ever talked to)
// when the operator's config predates the field. The default's use is
// logged exactly once per process (sync.Once), not once per pull — it is a
// silent behavior change only for the hypothetical non-ghcr legacy config
// (docs/superpowers/specs/2026-07-09-registries-design.md §3). Callers must
// only reach this once m.cfg.RegistryAuth is known non-nil.
func (m *Manager) legacyRegistryHost() string {
	if h := strings.ToLower(m.cfg.RegistryAuth.Host); h != "" {
		return h
	}
	m.warnLegacyHostOnce.Do(func() {
		m.logf("[daemon] WARN registry_auth без host — считаю ghcr.io")
	})
	return legacyDefaultRegistryHost
}

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
		OnLog: func(level, msg string) { m.appendLibaLog(srv, level, msg) },
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

	// Пара владельца разбирается ОДИН раз на дедика и расходится в два
	// потребителя: label'ы контейнера (метрики, #1008) и путь файла лога
	// (логи, #994). Второй дополнительно гейтится log_scope_dirs.
	project, envName := m.serverScope(srv.id, cmd.GetEnv())
	logPath := m.serverLogPath(srv.id, cmd.GetEnv())
	srv.mu.Lock()
	srv.logPath = logPath
	srv.project, srv.env = project, envName
	srv.mu.Unlock()

	handle, err := m.rt.Start(m.ctx, StartSpec{
		ID:         srv.id,
		ImageRef:   srv.imageRef,
		Port:       port,
		Region:     m.cfg.Region,
		SocketPath: m.socketPath(srv.id),
		LogPath:    logPath,
		CPUMillis:  cpu,
		MemMB:      mem,
		Env:        cmd.GetEnv(),
		Lookup:     m.pullLookup(srv.imageRef),

		ScopeProject: project,
		ScopeEnv:     envName,
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
	// The shim is gone with the container — gzip the log files (agent.md §5).
	// Async: compressing up to 200MB must not stall supervision.
	go m.finalizeLog(srv.id)
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

// scopeLabelRe — алфавит слага проекта и имени окружения на стороне master
// (store.projectSlugRe / store.envNameRe). Пара приезжает от master'а в
// env-мапе StartServer и становится КАТАЛОГОМ на диске, поэтому проверяется
// здесь ещё раз: агент не обязан доверять тому, что в паре нет `..` или `/`.
var scopeLabelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// scopePair пропускает пару (project, env) ТОЛЬКО целиком: обе половины должны
// пройти алфавит слага, иначе возвращается ("", "", false) и дедик остаётся
// беспарным. Всё-или-ничего здесь несущее, а не аккуратность (tracker #1008):
// половина пары на пер-серверной серии убивает правило TickDegraded целиком
// (`duplicate output timeseries` — замерено на живом VictoriaMetrics v1.102.1),
// потому что join `group_left (project)` схлопывает беспарную и полупарную
// серии одного server_id в один и тот же набор лейблов.
func scopePair(project, env string) (string, string, bool) {
	if !scopeLabelRe.MatchString(project) || !scopeLabelRe.MatchString(env) {
		return "", "", false
	}
	return project, env, true
}

// serverScope достаёт пару владельца из env-мапы StartServer. Это ЕДИНСТВЕННОЕ
// место, где пара входит в агента: дальше она едет в label'ах контейнера
// (метрики, tracker #1008) и в пути файла лога (логи, tracker #994). Флага здесь
// нет намеренно — `log_scope_dirs` гейтит только раскладку логов, у которой есть
// внешняя зависимость (glob в конфиге vector'а кладёт ansible); у метрик такой
// зависимости нет, лейблы экспортёра нодовый vmagent прокидывает как есть.
func (m *Manager) serverScope(id string, env map[string]string) (string, string) {
	project, envName, ok := scopePair(env["BIRDMAN_PROJECT"], env["BIRDMAN_ENV"])
	if !ok {
		if env["BIRDMAN_PROJECT"] != "" || env["BIRDMAN_ENV"] != "" {
			m.logf("[daemon] server %s: scope (%q, %q) is not a valid label pair — dedik stays unlabelled",
				id, env["BIRDMAN_PROJECT"], env["BIRDMAN_ENV"])
		}
		return "", ""
	}
	return project, envName
}

// serverLogPath — куда шим пишет вывод дедика. С tracker #994 путь несёт пару
// (project, env), которую master кладёт в env-мапу StartServer
// (BIRDMAN_PROJECT/BIRDMAN_ENV): vector парсит пару из ПУТИ и лейблует ей стрим
// в VictoriaLogs, а master сужает по этим лейблам запрос привязанного ключа
// (docs/specs/master.md §6). Почему путь, а не статический лейбл в конфиге
// шиппера: конфиг рендерит ansible по ХОСТУ, а окружение ноды меняется через
// API без перерендера — инвариант I6 запрещает выводить окружение истории из
// текущей строки ноды. Пара, приехавшая вместе с дедиком, этой болезнью не
// болеет: она чеканится в момент старта и потом не меняется.
//
// Пары нет или она не проходит алфавит (старый master, run-once, мусор) →
// прежний плоский путь: логи не теряются, просто остаются без лейблов.
func (m *Manager) serverLogPath(id string, env map[string]string) string {
	if !m.cfg.LogScopeDirs {
		// Выключено по умолчанию: бинарь агента обновляется сам, а конфиг
		// шиппера кладёт ansible — они приезжают НЕ вместе. Подробности и
		// цена — config.LogScopeDirs.
		return filepath.Join(m.logDir, id+".log")
	}
	// Тот же гейт всё-или-ничего, что у label'ов контейнера (scopePair). Про
	// негодную пару предупреждает serverScope — она зовётся из launch раньше и
	// одна на дедика, чтобы одна и та же пара не логировалась дважды.
	project, envName, ok := scopePair(env["BIRDMAN_PROJECT"], env["BIRDMAN_ENV"])
	if !ok {
		return filepath.Join(m.logDir, id+".log")
	}
	dir := filepath.Join(m.logDir, project, envName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.logf("[daemon] server %s: log dir %s: %v — log stays unlabelled", id, dir, err)
		return filepath.Join(m.logDir, id+".log")
	}
	return filepath.Join(dir, id+".log")
}

// appendLibaLog appends a structured liba log frame to the server log file
// (the container stdout/stderr go there via the shim).
func (m *Manager) appendLibaLog(srv *server, level, msg string) {
	// Кадры liba обязаны попадать в ТОТ ЖЕ файл, что и вывод шима, — иначе лог
	// дедика раздвоится по двум путям. Путь берётся у сервера (см.
	// logFilePath): он известен с момента старта, а после рестарта агента
	// резолвится по файловой системе, потому что пары в label'ах контейнера нет.
	id := srv.id
	path := srv.logFilePath(m.logDir)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		m.logf("[daemon] server %s: liba log: %v", id, err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s [liba/%s] %s\n", time.Now().UTC().Format(time.RFC3339), level, msg)
}
