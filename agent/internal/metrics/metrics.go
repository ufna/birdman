// Package metrics exposes the agent Prometheus endpoint
// (docs/specs/agent.md §9): localhost:9101/metrics, scraped by the vmagent of
// the same node. The exporter is hand-rolled Prometheus text (gauges and one
// counter) — no client library needed for a scrape-time snapshot.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ServerSample is one supervised server in a Sample.
type ServerSample struct {
	ID      string
	State   string
	Players int32
	TickMS  float32

	// Project/Env is the owner pair of this dedik (tracker #1008). It reaches
	// the agent in StartServer.env (BIRDMAN_PROJECT/BIRDMAN_ENV) and survives
	// an agent restart in container labels. Both halves set — the per-server
	// series carry them as labels and a (project, env)-bound API key sees its
	// own dediks' charts through the master metrics proxy (master.md §6).
	// Either half empty — the series are emitted exactly as before, with
	// server_id alone; that dedik was started before the pair was stamped and
	// stays invisible to a bound key until it is recycled.
	Project string
	Env     string

	// cgroups v2 usage; HasUsage is false when the cgroup could not be read
	// (dead container, non-Linux dev host).
	HasUsage   bool
	CPUSeconds float64
	MemBytes   uint64
}

// Sample is a point-in-time snapshot of the agent, produced on scrape.
type Sample struct {
	Draining bool
	States   map[string]int // servers per lifecycle state (all states, 0 included)
	Servers  []ServerSample

	PortsUsed  int
	PortsTotal int

	DiskUsed  uint64
	DiskTotal uint64

	// ContainerdDiskUsed/Total sample the containerd-root filesystem — where
	// images live (environments v1 §6в). On a node where containerd-root shares
	// the data_dir filesystem these equal DiskUsed/DiskTotal; that is expected.
	ContainerdDiskUsed  uint64
	ContainerdDiskTotal uint64

	// CertExpiryUnix is the NotAfter (unix seconds) of the loaded agentlink
	// client certificate, or 0 when none is loaded (token/insecure session).
	// The node-local half of the CertExpiry alert (mTLS agentlink v1, design §4).
	CertExpiryUnix int64
}

// Handler serves GET /metrics from sample().
func Handler(version string, sample func() Sample) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := sample()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Write([]byte(Render(version, s)))
	})
}

// Render formats a Sample as Prometheus text exposition (deterministic order).
func Render(version string, s Sample) string {
	var b strings.Builder

	gauge := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	}
	counter := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	}

	gauge("birdman_agent_up", "1 while the agent is running.")
	fmt.Fprintf(&b, "birdman_agent_up 1\n")

	gauge("birdman_agent_info", "Agent build info; the value is always 1.")
	fmt.Fprintf(&b, "birdman_agent_info{version=%q} 1\n", escape(version))

	gauge("birdman_agent_draining", "1 while the node-level drain is in effect.")
	fmt.Fprintf(&b, "birdman_agent_draining %d\n", boolToInt(s.Draining))

	gauge("birdman_agent_servers", "Supervised servers per lifecycle state.")
	states := make([]string, 0, len(s.States))
	for st := range s.States {
		states = append(states, st)
	}
	sort.Strings(states)
	for _, st := range states {
		fmt.Fprintf(&b, "birdman_agent_servers{state=%q} %d\n", escape(st), s.States[st])
	}

	servers := append([]ServerSample(nil), s.Servers...)
	sort.Slice(servers, func(i, j int) bool { return servers[i].ID < servers[j].ID })

	gauge("birdman_server_players", "Live player count reported by liba, per server.")
	for _, sv := range servers {
		fmt.Fprintf(&b, "birdman_server_players%s %d\n", serverLabels(sv), sv.Players)
	}
	gauge("birdman_server_tick_ms", "Last tick duration reported by liba, per server (ms).")
	for _, sv := range servers {
		fmt.Fprintf(&b, "birdman_server_tick_ms%s %s\n", serverLabels(sv), formatFloat(float64(sv.TickMS)))
	}

	counter("birdman_container_cpu_seconds_total", "Cumulative CPU time of the server container (cgroups v2).")
	for _, sv := range servers {
		if sv.HasUsage {
			fmt.Fprintf(&b, "birdman_container_cpu_seconds_total%s %s\n", serverLabels(sv), formatFloat(sv.CPUSeconds))
		}
	}
	gauge("birdman_container_memory_bytes", "Current memory usage of the server container (cgroups v2).")
	for _, sv := range servers {
		if sv.HasUsage {
			fmt.Fprintf(&b, "birdman_container_memory_bytes%s %d\n", serverLabels(sv), sv.MemBytes)
		}
	}

	gauge("birdman_agent_disk_used_bytes", "Used bytes on the data_dir filesystem.")
	fmt.Fprintf(&b, "birdman_agent_disk_used_bytes %d\n", s.DiskUsed)
	gauge("birdman_agent_disk_total_bytes", "Total bytes on the data_dir filesystem.")
	fmt.Fprintf(&b, "birdman_agent_disk_total_bytes %d\n", s.DiskTotal)

	// Containerd-root filesystem — where images live; the dual-fs watermark and
	// its DiskHigh alert rule read this pair (environments v1 §6в). Equals the
	// data_dir pair when containerd-root is not a separate mount.
	gauge("birdman_agent_containerd_disk_used_bytes", "Used bytes on the containerd-root filesystem (image store).")
	fmt.Fprintf(&b, "birdman_agent_containerd_disk_used_bytes %d\n", s.ContainerdDiskUsed)
	gauge("birdman_agent_containerd_disk_total_bytes", "Total bytes on the containerd-root filesystem (image store).")
	fmt.Fprintf(&b, "birdman_agent_containerd_disk_total_bytes %d\n", s.ContainerdDiskTotal)

	gauge("birdman_agent_ports_used", "Host ports handed out of the pool.")
	fmt.Fprintf(&b, "birdman_agent_ports_used %d\n", s.PortsUsed)
	gauge("birdman_agent_ports_total", "Size of the host port pool.")
	fmt.Fprintf(&b, "birdman_agent_ports_total %d\n", s.PortsTotal)

	// Only emitted with a loaded client cert: a token/insecure session has no
	// expiry to report, and a 0 sample would look like the epoch.
	if s.CertExpiryUnix > 0 {
		gauge("birdman_agent_cert_expiry_timestamp_seconds", "NotAfter of the loaded agentlink client certificate (unix seconds).")
		fmt.Fprintf(&b, "birdman_agent_cert_expiry_timestamp_seconds %d\n", s.CertExpiryUnix)
	}

	return b.String()
}

// Serve runs the metrics HTTP server on addr until ctx is done. The addr must
// stay on localhost (agent.md §9): the vmagent of the same node scrapes it.
func Serve(ctx context.Context, addr, version string, sample func() Sample, logf func(string, ...any)) error {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", Handler(version, sample))
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logf("[metrics] serving on %s", addr)
	select {
	case <-ctx.Done():
		shCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// serverLabels renders the label set shared by every per-server series
// (tracker #1008). The owner pair is emitted ONLY as a pair, and that is
// load-bearing rather than tidy: the TickDegraded rule joins these series with
// birdman_server_info through `on (server_id) group_left (project)`, so a
// series carrying `project` WITHOUT `env` collapses onto the same output label
// set as an unlabelled one and the whole rule dies with `duplicate output
// timeseries` (measured against VictoriaMetrics v1.102.1, not read off the
// docs). With both halves the output sets differ by `env` and the rule holds.
//
// Label ORDER inside the braces is irrelevant to Prometheus (it parses into a
// set), but is kept deterministic for the golden test and for a human reading
// a scrape by eye.
func serverLabels(sv ServerSample) string {
	if sv.Project == "" || sv.Env == "" {
		return fmt.Sprintf("{server_id=%q}", escape(sv.ID))
	}
	return fmt.Sprintf("{server_id=%q,project=%q,env=%q}", escape(sv.ID), escape(sv.Project), escape(sv.Env))
}

// escape renders a label value for %q-quoted output; Go's %q already escapes
// quotes, backslashes and newlines the way Prometheus expects.
func escape(s string) string { return s }

func formatFloat(v float64) string {
	out := fmt.Sprintf("%g", v)
	return out
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
