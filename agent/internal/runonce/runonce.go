// Package runonce implements `birdman-agent run-once` (итерация 0,
// docs/05-runtime-iterations.md): pull образа, запуск одного дедика под
// containerd, супервизия через liba-сокет, выход с кодом контейнера.
// Master'а ещё нет — allocated/drain играют флаги CLI.
package runonce

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/ufna/birdman/agent/internal/config"
	"github.com/ufna/birdman/agent/internal/lifecycle"
	"github.com/ufna/birdman/agent/internal/ports"
	"github.com/ufna/birdman/agent/internal/runtime"
	"github.com/ufna/birdman/agent/internal/uds"
)

// DefaultSocketDir is where per-server liba sockets live (protocol.md §2).
const DefaultSocketDir = "/run/birdman/servers"

const (
	stopGrace      = 30 * time.Second // SIGTERM → grace → SIGKILL (agent.md §3)
	drainDeadlineS = 30               // deadline_s в drain-фрейме run-once
	pingEvery      = 10 * time.Second
	libaSilenceMax = 15 * time.Second // тишина liba при allocated → unhealthy (agent.md §4)
)

// Options are the run-once CLI parameters.
type Options struct {
	ConfigPath        string
	ImageRef          string
	Port              int           // 0 = взять из пула
	AllocateMatchID   string        // не пусто: отправить allocated после ready
	DrainAfter        time.Duration // >0: отправить drain через N после ready
	ContainerdAddress string        // пусто = runtime.DefaultAddress
	SocketDir         string        // пусто = DefaultSocketDir
}

// Run executes the run-once flow and returns the process exit code
// (container exit code; agent-side errors → 1).
func Run(ctx context.Context, opts Options) int {
	if opts.ContainerdAddress == "" {
		opts.ContainerdAddress = runtime.DefaultAddress
	}
	if opts.SocketDir == "" {
		opts.SocketDir = DefaultSocketDir
	}

	ev := &eventLog{out: os.Stdout} // события — в stdout (контракт run-once)
	diag := log.New(os.Stderr, "", log.LstdFlags)

	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		diag.Printf("config: %v", err)
		return 1
	}

	pool, err := ports.New(cfg.PortRange[0], cfg.PortRange[1])
	if err != nil {
		diag.Printf("port pool: %v", err)
		return 1
	}
	port := opts.Port
	if port == 0 {
		port, err = pool.Acquire()
	} else {
		err = pool.AcquireSpecific(port)
	}
	if err != nil {
		diag.Printf("port: %v", err)
		return 1
	}

	serverID := newServerID()

	logDir := filepath.Join(cfg.LogDir, "servers")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		diag.Printf("log dir: %v", err)
		return 1
	}
	// Per-server socket dir: its parent is bind-mounted (ro) into the
	// container, so the socket path is {socket_dir}/{id}/agent.sock.
	serverSockDir := filepath.Join(opts.SocketDir, serverID)
	if err := os.MkdirAll(serverSockDir, 0o755); err != nil {
		diag.Printf("socket dir: %v", err)
		return 1
	}
	defer os.RemoveAll(serverSockDir)
	logPath := filepath.Join(logDir, serverID+".log")
	sockPath := filepath.Join(serverSockDir, runtime.SocketFileName)

	slog, err := openServerLog(logPath)
	if err != nil {
		diag.Printf("server log: %v", err)
		return 1
	}
	defer slog.Close()

	machine := lifecycle.NewMachine(serverID, func(tr lifecycle.Transition) {
		if tr.Reason != "" {
			ev.printf("state %s -> %s (%s)", tr.From, tr.To, tr.Reason)
		} else {
			ev.printf("state %s -> %s", tr.From, tr.To)
		}
	})
	transition := func(to lifecycle.State, reason string) {
		if err := machine.To(to, reason); err != nil {
			diag.Printf("state: %v", err)
		}
	}

	ev.printf("server %s image=%s port=%d state=%s log=%s socket=%s",
		serverID, opts.ImageRef, port, machine.Current(), logPath, sockPath)

	client, err := runtime.Connect(opts.ContainerdAddress, cfg.ContainerdNamespace)
	if err != nil {
		transition(lifecycle.StateFailed, "containerd connect failed")
		diag.Printf("%v", err)
		return 1
	}
	defer client.Close()

	// run-once is deliberately host-blind (docs/superpowers/specs/2026-07-09-registries-design.md
	// §3): the operator supplies image_ref directly on the command line —
	// there is no attacker-controlled input here, unlike the daemon's
	// master/legacy host-matched chain — so the configured file cred, if
	// any, applies regardless of the ref's host.
	var lookup runtime.CredLookup
	if cfg.RegistryAuth != nil {
		token, err := cfg.RegistryAuth.Token()
		if err != nil {
			transition(lifecycle.StateFailed, "registry token unavailable")
			diag.Printf("%v", err)
			return 1
		}
		username := cfg.RegistryAuth.Username
		lookup = func(string) (string, string, error) { return username, token, nil }
	}

	diag.Printf("ensuring image %s (pull if not local)", opts.ImageRef)
	img, err := client.EnsureImage(ctx, opts.ImageRef, lookup)
	if err != nil {
		transition(lifecycle.StateFailed, "image pull failed")
		diag.Printf("%v", err)
		return 1
	}

	// liba-сокет должен существовать до старта контейнера (источник bind-mount)
	readyCh := make(chan struct{}, 1)
	sock, err := uds.Listen(sockPath, uds.Events{
		OnHello: func(v string) { ev.printf("liba hello sdk_version=%s", v) },
		OnReady: func() {
			select {
			case readyCh <- struct{}{}:
			default:
			}
		},
		OnPlayers:    func(n int) { ev.printf("players count=%d", n) },
		OnMatchStart: func(id string) { ev.printf("match_start match_id=%s", id) },
		OnMatchEnd:   func(id, res string) { ev.printf("match_end match_id=%s result=%s", id, res) },
		OnMetric:     func(name string, v float64) { diag.Printf("metric %s=%v", name, v) },
		OnLog:        func(level, msg string) { slog.AppendLibaLine(level, msg) },
		OnConnect:    func() { diag.Printf("liba connected") },
		OnDisconnect: func() { diag.Printf("liba disconnected") },
	}, diag.Printf)
	if err != nil {
		transition(lifecycle.StateFailed, "agent socket listen failed")
		diag.Printf("%v", err)
		return 1
	}
	defer sock.Close()

	srv, err := client.StartServer(ctx, runtime.ServerSpec{
		ID:         serverID,
		Image:      img,
		ImageRef:   opts.ImageRef,
		Port:       port,
		Region:     cfg.Region,
		SocketPath: sockPath,
		CPUMillis:  cfg.LimitsDefault.CPUMillis,
		MemMB:      cfg.LimitsDefault.MemMB,
	}, slog)
	if err != nil {
		transition(lifecycle.StateFailed, "container start failed")
		diag.Printf("%v", err)
		return 1
	}
	transition(lifecycle.StateStarting, "container started")

	// containerd-операции переживают отмену сигнального ctx (graceful stop)
	opCtx := context.WithoutCancel(ctx)

	graceExpired := make(chan struct{}, 1)
	graceCtx, cancelGrace := context.WithCancel(opCtx)
	defer cancelGrace()
	go lifecycle.WatchReadyGrace(graceCtx, machine, lifecycle.ReadyGrace, func() {
		select {
		case graceExpired <- struct{}{}:
		default:
		}
	})

	pingTicker := time.NewTicker(pingEvery)
	defer pingTicker.Stop()

	exitCh := srv.Wait()
	sigCh := ctx.Done()
	var drainCh, killCh <-chan time.Time

	for {
		select {
		case <-readyCh:
			if machine.Current() != lifecycle.StateStarting {
				continue // повторный ready (реконнект liba) — игнорируем
			}
			transition(lifecycle.StateReady, "liba ready")
			if opts.AllocateMatchID != "" {
				if err := sock.SendAllocated(opts.AllocateMatchID, 0); err != nil {
					diag.Printf("send allocated: %v", err)
				}
				ev.printf("allocated match_id=%s players_expected=0", opts.AllocateMatchID)
				transition(lifecycle.StateAllocated, "run-once --allocate")
			}
			if opts.DrainAfter > 0 {
				drainCh = time.After(opts.DrainAfter)
			}

		case <-graceExpired:
			if machine.Current() != lifecycle.StateStarting {
				continue
			}
			transition(lifecycle.StateFailed, fmt.Sprintf("no ready within %s", lifecycle.ReadyGrace))
			if err := srv.ForceKill(opCtx); err != nil {
				diag.Printf("force kill: %v", err)
			}

		case <-drainCh:
			drainCh = nil
			if cur := machine.Current(); cur != lifecycle.StateReady && cur != lifecycle.StateAllocated {
				continue
			}
			if err := sock.SendDrain(drainDeadlineS, "run-once --drain-after"); err != nil {
				diag.Printf("send drain: %v", err)
			}
			ev.printf("drain deadline_s=%d reason=%q", drainDeadlineS, "run-once --drain-after")
			transition(lifecycle.StateDraining, "run-once --drain-after")

		case <-pingTicker.C:
			if err := sock.SendPing(); err != nil && !errors.Is(err, uds.ErrNotConnected) {
				diag.Printf("ping: %v", err)
			}
			if machine.Current() == lifecycle.StateAllocated {
				if lh := sock.LastHeard(); !lh.IsZero() && time.Since(lh) > libaSilenceMax {
					diag.Printf("warning: liba silent for %s while allocated (unhealthy)",
						time.Since(lh).Round(time.Second))
				}
			}

		case <-sigCh:
			sigCh = nil // обрабатываем один раз
			ev.printf("signal received, stopping container (grace %s)", stopGrace)
			if err := srv.Signal(opCtx, syscall.SIGTERM); err != nil {
				diag.Printf("sigterm: %v", err)
			}
			killCh = time.After(stopGrace)

		case <-killCh:
			killCh = nil
			diag.Printf("stop grace exceeded, sending SIGKILL")
			if err := srv.ForceKill(opCtx); err != nil {
				diag.Printf("force kill: %v", err)
			}

		case st := <-exitCh:
			code, _, werr := st.Result()
			cancelGrace()
			if werr != nil {
				// exit-watch сломался (демон недоступен и т.п.) — код недостоверен
				diag.Printf("exit watch failed: %v", werr)
				transition(lifecycle.StateFailed, "exit watch failed")
				if err := srv.Delete(opCtx); err != nil {
					diag.Printf("cleanup: %v", err)
				}
				sock.Close()
				return 1
			}
			if cur := machine.Current(); !cur.Terminal() {
				if code == 0 {
					transition(lifecycle.StateStopped, "container exited")
				} else {
					transition(lifecycle.StateFailed, fmt.Sprintf("container exited with code %d", code))
				}
			}
			ev.printf("exit code=%d", code)
			if err := srv.Delete(opCtx); err != nil {
				diag.Printf("cleanup: %v", err)
			}
			sock.Close()
			if err := pool.Release(port); err != nil {
				diag.Printf("port release: %v", err)
			}
			if machine.Current() == lifecycle.StateFailed && code == 0 {
				return 1 // failed (нет ready) при коде 0 — не маскируем успехом
			}
			return int(code)
		}
	}
}

func newServerID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("runonce-%d", time.Now().UnixNano())
	}
	return "runonce-" + hex.EncodeToString(b)
}

// eventLog печатает события run-once в stdout одной строкой с UTC-временем.
type eventLog struct {
	mu  sync.Mutex
	out io.Writer
}

func (e *eventLog) printf(format string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	fmt.Fprintf(e.out, "%s %s\n",
		time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), fmt.Sprintf(format, args...))
}

// serverLog — лог дедика {log_dir}/servers/{id}.log: туда пишут и
// stdout/stderr контейнера (через cio-копирование), и log-фреймы liba.
// Записи сериализованы мьютексом; чанк cio может разорвать строку liba-лога
// по границе буфера — для v0 приемлемо.
// TODO(итерация 1+): ротация 100MB×2 на дедик и gzip после stop (agent.md §5).
type serverLog struct {
	mu sync.Mutex
	f  *os.File
}

func openServerLog(path string) (*serverLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &serverLog{f: f}, nil
}

// Write — io.Writer для cio-стримов контейнера.
func (l *serverLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Write(p)
}

// AppendLibaLine пишет структурированное log-событие liba (protocol.md §2).
func (l *serverLog) AppendLibaLine(level, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.f, "%s [liba/%s] %s\n", time.Now().UTC().Format(time.RFC3339), level, msg)
}

func (l *serverLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
