// birdman-agent — node agent of the birdman platform.
// v0 (iteration 0): local run-once supervision, no master link yet.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ufna/birdman/agent/internal/runonce"
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
  run-once    pull an image, start one dedicated server and supervise it
              until it exits (process exit code = container exit code)
  version     print agent version

Run-once flags:
  --config PATH        agent config (default /etc/birdman/agent.yaml)
  --image REF          image to run (required)
  --port N             host port (default: first free port from the pool)
  --allocate MATCH_ID  send allocated{match_id} after the server reports ready
  --drain-after SEC    send drain SEC seconds after ready
`)
}
