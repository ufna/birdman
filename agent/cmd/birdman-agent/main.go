// birdman-agent — node agent of the birdman platform.
// Iteration 1: `run` daemon mode (gRPC link to master, heartbeat, commands)
// plus the local `run-once` supervision from iteration 0.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ufna/birdman/agent/internal/config"
	"github.com/ufna/birdman/agent/internal/daemon"
	"github.com/ufna/birdman/agent/internal/imagegc"
	"github.com/ufna/birdman/agent/internal/link"
	"github.com/ufna/birdman/agent/internal/logrot"
	"github.com/ufna/birdman/agent/internal/metrics"
	"github.com/ufna/birdman/agent/internal/qosecho"
	"github.com/ufna/birdman/agent/internal/runonce"
	"github.com/ufna/birdman/agent/internal/runtime"
	"github.com/ufna/birdman/agent/internal/stats"
)

// version is injected by build.sh via -ldflags "-X main.version=…".
var version = "dev"

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) < 1 {
		usage()
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Println(version)
		return 0
	case "run":
		return runDaemon(args[1:])
	case "run-once":
		return runOnce(args[1:])
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage()
		return 2
	}
}

// runDaemon implements `birdman-agent run`: connect to master over the
// AgentLink stream and supervise dedicated servers under its command.
// SIGTERM exits gracefully WITHOUT touching running dediks (agent.md §2).
func runDaemon(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", "/etc/birdman/agent.yaml", "path to agent config (YAML)")
	containerdAddr := fs.String("containerd", runtime.DefaultAddress, "containerd socket")
	socketDir := fs.String("socket-dir", daemon.DefaultSocketDir, "per-server liba socket dir")
	_ = fs.Parse(args) // ExitOnError

	logger := log.New(os.Stdout, "", log.LstdFlags|log.LUTC)
	logf := logger.Printf

	cfg, err := config.Load(*configPath)
	if err != nil {
		logf("config: %v", err)
		return 1
	}
	if err := cfg.ValidateRun(); err != nil {
		logf("config: %v", err)
		return 1
	}
	token, err := cfg.MasterToken()
	if err != nil {
		logf("config: %v", err)
		return 1
	}
	hostname, err := os.Hostname()
	if err != nil {
		hostname = ""
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// A successful self-upgrade also ends this context: the agent exits
	// cleanly and systemd restarts the new binary (agent.md §7).
	ctx, cancel := context.WithCancel(sigCtx)
	defer cancel()

	client, err := runtime.Connect(*containerdAddr)
	if err != nil {
		logf("%v", err)
		return 1
	}
	defer client.Close()

	rt := &daemon.ContainerdRuntime{Client: client}
	gc := imagegc.New(imagegc.Options{
		Runtime:   rt,
		DiskUsage: func() (uint64, uint64) { return stats.DiskUsage(cfg.DataDir) },
		Watermark: float64(cfg.DiskGCWatermarkPct) / 100,
		Logf:      logf,
	})

	var upgraded atomic.Bool
	outbox := link.NewOutbox(logf)
	mgr, err := daemon.NewManager(ctx, daemon.Options{
		Config:       cfg,
		Runtime:      rt,
		Sink:         outbox,
		SocketDir:    *socketDir,
		Logf:         logf,
		AgentVersion: version,
		TouchImage:   gc.Touch,
		OnUpgraded: func(v string) {
			logf("self-upgrade to %s complete — exiting for systemd restart", v)
			upgraded.Store(true)
			cancel()
		},
	})
	if err != nil {
		logf("%v", err)
		return 1
	}

	// Log rotation + retention (agent.md §5): 100MB×2 per dedik, gzip after
	// stop, N-day retention for archives.
	rot := logrot.New(logrot.Config{
		Dir:       filepath.Join(cfg.LogDir, "servers"),
		MaxSize:   int64(cfg.LogMaxSizeMB) << 20,
		Retention: time.Duration(cfg.LogRetentionDays) * 24 * time.Hour,
		Logf:      logf,
	}, mgr.LiveServerIDs)
	mgr.SetLogFinalizer(rot.Finalize)

	if err := mgr.Restore(ctx); err != nil {
		logf("restore server map: %v", err)
		return 1
	}

	go rot.Run(ctx.Done())
	go gc.Run(ctx, time.Minute)
	go func() {
		// Metrics endpoint (agent.md §9) — localhost only, scraped by vmagent.
		if err := metrics.Serve(ctx, cfg.MetricsAddr, version, mgr.MetricsSample, logf); err != nil {
			logf("metrics: %v", err)
		}
	}()
	go func() {
		// QoS UDP echo (agent.md §8) — the public ping target of the node.
		if err := qosecho.Serve(ctx, cfg.QoSEchoAddr, logf); err != nil {
			logf("qos echo: %v", err)
		}
	}()

	lc := link.New(link.Config{
		MasterAddr:    cfg.MasterAddr,
		NodeToken:     token,
		Hostname:      hostname,
		Region:        cfg.Region,
		CapacitySlots: int32(cfg.CapacitySlots),
		AgentVersion:  version,
		TLSInsecure:   cfg.TLSInsecure,
		TLSCAFile:     cfg.TLSCAFile,
	}, mgr, mgr, outbox, logf)

	logf("birdman-agent %s: linking to master %s (region %s, %d slots)",
		version, cfg.MasterAddr, cfg.Region, cfg.CapacitySlots)
	if err := lc.Run(ctx); err != nil {
		logf("link: %v", err)
		return 1
	}
	// Graceful shutdown: close liba sockets, leave containers running.
	mgr.Shutdown()
	if upgraded.Load() {
		logf("agent exiting for self-upgrade restart; dedicated servers keep running")
	} else {
		logf("agent stopped; dedicated servers keep running (restart restores the map)")
	}
	return 0
}

func runOnce(args []string) int {
	fs := flag.NewFlagSet("run-once", flag.ExitOnError)
	configPath := fs.String("config", "/etc/birdman/agent.yaml", "path to agent config (YAML)")
	image := fs.String("image", "", "image reference to run (required), e.g. ghcr.io/ufna/birdman-stub-server:latest")
	port := fs.Int("port", 0, "host port for the server (0 = first free from the pool)")
	allocate := fs.String("allocate", "", "send allocated{match_id} once the server reports ready")
	drainAfter := fs.Int("drain-after", 0, "seconds after ready before sending drain (0 = never)")
	_ = fs.Parse(args) // ExitOnError
	if *image == "" {
		fmt.Fprintln(os.Stderr, "run-once: --image is required")
		fs.Usage()
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runonce.Run(ctx, runonce.Options{
		ConfigPath:      *configPath,
		ImageRef:        *image,
		Port:            *port,
		AllocateMatchID: *allocate,
		DrainAfter:      time.Duration(*drainAfter) * time.Second,
	})
}

func usage() {
	fmt.Fprint(os.Stderr, `Usage: birdman-agent <command> [flags]

Commands:
  run         daemon mode: connect to master (gRPC AgentLink) and supervise
              dedicated servers under its command; SIGTERM exits without
              touching running servers
  run-once    pull an image, start one dedicated server and supervise it
              until it exits (process exit code = container exit code)
  version     print agent version

Run flags:
  --config PATH        agent config (default /etc/birdman/agent.yaml);
                       requires master_addr and node_token(_file)
  --containerd PATH    containerd socket (default /run/containerd/containerd.sock)
  --socket-dir PATH    per-server liba socket dir (default /run/birdman/servers)

Run-once flags:
  --config PATH        agent config (default /etc/birdman/agent.yaml)
  --image REF          image to run (required)
  --port N             host port (default: first free port from the pool)
  --allocate MATCH_ID  send allocated{match_id} after the server reports ready
  --drain-after SEC    send drain SEC seconds after ready
`)
}
