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

// TestRenderServerScopeLabels (tracker #1008): все ЧЕТЫРЕ пер-серверные серии
// несут пару (project, env) владельца дедика — без неё привязанный к паре ключ
// получает от `GET /v1/metrics/query` пустые графики, потому что master сужает
// его запрос по `extra_label=project=…&extra_label=env=…` (master.md §6).
func TestRenderServerScopeLabels(t *testing.T) {
	s := Sample{
		States: map[string]int{"ready": 2},
		Servers: []ServerSample{
			{ID: "srv-scoped", State: "ready", Players: 4, TickMS: 16,
				HasUsage: true, CPUSeconds: 2.5, MemBytes: 2 << 20,
				Project: "game", Env: "prod"},
			// Дедик, запущенный до появления label'ов: пары нет, серия
			// обязана остаться БАЙТ-В-БАЙТ прежней. Иначе беспарная история
			// поехала бы в новую идентичность серии на ровном месте.
			{ID: "srv-legacy", State: "ready", Players: 1, TickMS: 17,
				HasUsage: true, CPUSeconds: 1, MemBytes: 1 << 20},
		},
	}
	out := Render("test", s)

	for _, want := range []string{
		`birdman_server_players{server_id="srv-scoped",project="game",env="prod"} 4` + "\n",
		`birdman_server_tick_ms{server_id="srv-scoped",project="game",env="prod"} 16` + "\n",
		`birdman_container_cpu_seconds_total{server_id="srv-scoped",project="game",env="prod"} 2.5` + "\n",
		`birdman_container_memory_bytes{server_id="srv-scoped",project="game",env="prod"} 2097152` + "\n",

		`birdman_server_players{server_id="srv-legacy"} 1` + "\n",
		`birdman_server_tick_ms{server_id="srv-legacy"} 17` + "\n",
		`birdman_container_cpu_seconds_total{server_id="srv-legacy"} 1` + "\n",
		`birdman_container_memory_bytes{server_id="srv-legacy"} 1048576` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}

	// Пара ставится ТОЛЬКО парой. Половина (`project` без `env`) под join'ом
	// TickDegraded даёт тот же набор выходных лейблов, что и беспарная серия
	// того же server_id, и правило умирает целиком с `duplicate output
	// timeseries` — замерено на живом VictoriaMetrics v1.102.1. Поэтому
	// половина обязана значить «пары нет», а не «половина лейблов».
	for _, half := range []ServerSample{
		{ID: "srv-half", State: "ready", HasUsage: true, Project: "game"},
		{ID: "srv-half", State: "ready", HasUsage: true, Env: "prod"},
	} {
		got := Render("test", Sample{States: map[string]int{}, Servers: []ServerSample{half}})
		if !strings.Contains(got, `birdman_server_players{server_id="srv-half"} 0`+"\n") {
			t.Fatalf("половина пары (%q, %q) не свелась к беспарной серии:\n%s", half.Project, half.Env, got)
		}
		for _, bad := range []string{`project=`, `env=`} {
			if strings.Contains(got, `{server_id="srv-half",`+bad) {
				t.Fatalf("половина пары стала лейблом (%q):\n%s", bad, got)
			}
		}
	}

	// Платформенные и нодовые серии пары не имеют ПО СУЩЕСТВУ — это данные
	// всего хоста, и привязанному ключу их видеть не положено (карточка #1008,
	// п.4). Появись пара на birdman_agent_disk_*, деления DiskHigh
	// (`used / total`, one-to-one без on()) сломались бы молча.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "birdman_agent_") && strings.Contains(line, "project=") {
			t.Fatalf("нодовая серия получила пару: %q", line)
		}
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

// TestRenderContainerdDisk covers the dual-fs watermark's containerd-root
// gauges (environments v1 §6в): the DiskHigh alert rules are duplicated onto
// this pair, so the exact metric names matter. On a single-filesystem node the
// values equal the data_dir pair — that is expected and documented.
func TestRenderContainerdDisk(t *testing.T) {
	s := sample()
	s.ContainerdDiskUsed = 4096
	s.ContainerdDiskTotal = 8192
	out := Render("test", s)
	for _, want := range []string{
		"# TYPE birdman_agent_containerd_disk_used_bytes gauge\n",
		"birdman_agent_containerd_disk_used_bytes 4096\n",
		"# TYPE birdman_agent_containerd_disk_total_bytes gauge\n",
		"birdman_agent_containerd_disk_total_bytes 8192\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
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
