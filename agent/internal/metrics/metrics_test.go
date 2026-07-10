package metrics

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func sample() Sample {
	return Sample{
		Draining: true,
		States:   map[string]int{"ready": 2, "allocated": 1, "failed": 0},
		Servers: []ServerSample{
			{ID: "srv-b", State: "allocated", Players: 2, TickMS: 16.5, HasUsage: true, CPUSeconds: 1.25, MemBytes: 1 << 20},
			{ID: "srv-a", State: "ready", Players: 0, TickMS: 17},
		},
		PortsUsed: 3, PortsTotal: 51,
		DiskUsed: 100, DiskTotal: 1000,
	}
}

func TestRender(t *testing.T) {
	out := Render(`v1.2.3"quote`, sample())
	for _, want := range []string{
		"birdman_agent_up 1\n",
		"birdman_agent_info{version=\"v1.2.3\\\"quote\"} 1\n",
		"birdman_agent_draining 1\n",
		"birdman_agent_servers{state=\"allocated\"} 1\n",
		"birdman_agent_servers{state=\"failed\"} 0\n",
		"birdman_agent_servers{state=\"ready\"} 2\n",
		"birdman_server_players{server_id=\"srv-a\"} 0\n",
		"birdman_server_players{server_id=\"srv-b\"} 2\n",
		"birdman_server_tick_ms{server_id=\"srv-b\"} 16.5\n",
		"# TYPE birdman_container_cpu_seconds_total counter\n",
		"birdman_container_cpu_seconds_total{server_id=\"srv-b\"} 1.25\n",
		"birdman_container_memory_bytes{server_id=\"srv-b\"} 1048576\n",
		"birdman_agent_disk_used_bytes 100\n",
		"birdman_agent_disk_total_bytes 1000\n",
		"birdman_agent_ports_used 3\n",
		"birdman_agent_ports_total 51\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
	// srv-a has no cgroup usage — its container series must be absent.
	if strings.Contains(out, "birdman_container_cpu_seconds_total{server_id=\"srv-a\"}") {
		t.Fatal("unexpected cpu series for a server without usage")
	}
	// Deterministic ordering: srv-a before srv-b.
	if strings.Index(out, `server_id="srv-a"`) > strings.Index(out, `server_id="srv-b"`) {
		t.Fatal("server series are not sorted")
	}
}

// TestRenderCertExpiry: the client-cert expiry gauge (mTLS agentlink v1,
// design §4) is emitted only when a cert is loaded (CertExpiryUnix > 0) and is
// absent otherwise (token/insecure sessions have no client cert).
func TestRenderCertExpiry(t *testing.T) {
	s := sample()
	s.CertExpiryUnix = 1893456000 // 2030-01-01T00:00:00Z
	out := Render("test", s)
	for _, want := range []string{
		"# TYPE birdman_agent_cert_expiry_timestamp_seconds gauge\n",
		"birdman_agent_cert_expiry_timestamp_seconds 1893456000\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}

	// No cert loaded → the series must be absent entirely (not a zero sample).
	if strings.Contains(Render("test", sample()), "birdman_agent_cert_expiry_timestamp_seconds") {
		t.Fatal("cert-expiry gauge must be absent when no client cert is loaded")
	}
}

func TestServe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, addr, "test", sample, t.Logf) }()

	var resp *http.Response
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err = http.Get("http://" + addr + "/metrics")
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "birdman_agent_up 1") {
		t.Fatalf("scrape: %d %s", resp.StatusCode, body)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
}
