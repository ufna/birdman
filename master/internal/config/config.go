// Package config loads master.yaml (docs/specs/master.md §7; dev defaults
// clarified in v0: API :8100, gRPC :8444).
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type TLS struct {
	// CertFile/KeyFile: PEM pair used for the gRPC AgentLink listener.
	// When empty, a self-signed pair is generated into AutoCertDir on
	// first start (dev mode; see docs/specs/protocol.md §Auth).
	CertFile    string `yaml:"cert_file"`
	KeyFile     string `yaml:"key_file"`
	AutoCertDir string `yaml:"auto_cert_dir"`
}

// JoinToken configures the optional HMAC join token
// (docs/specs/master.md §4; off by default in v0).
type JoinToken struct {
	Enabled bool `yaml:"enabled"`
	// Secret should come from the environment (BIRDMAN_MM_JOIN_SECRET), not
	// from the config file. Empty with Enabled → an ephemeral secret is
	// generated at start (tokens do not survive a master restart).
	Secret string `yaml:"secret"`
}

// Matchmaking configures the v0 matchmaker (docs/specs/master.md §4).
type Matchmaking struct {
	TickMS         int       `yaml:"tick_ms"`         // queue scan period, default 500
	WidenAfterS    int       `yaml:"widen_after_s"`   // widen to next region, default 30
	TicketTTLS     int       `yaml:"ticket_ttl_s"`    // queued → expired, default 120
	DefaultProject string    `yaml:"default_project"` // empty → sole project in DB
	JoinToken      JoinToken `yaml:"join_token"`
}

// CompatOverride is one compat.overrides row (docs/specs/ops.md §3):
// clients matching Client may ALSO play on servers matching Servers
// (additive to the default MAJOR.MINOR rule — migration windows).
type CompatOverride struct {
	Client  string   `yaml:"client"`  // e.g. "1.4.x"
	Servers []string `yaml:"servers"` // e.g. ["1.4.x", "1.5.x"]
}

// Compat is the client↔server version compatibility policy (ops.md §3).
type Compat struct {
	// Default rule; only "major.minor" (or empty) is supported.
	Default   string           `yaml:"default"`
	Overrides []CompatOverride `yaml:"overrides"`
}

// Metrics configures the read-only metrics query proxy
// (docs/specs/panel.md §1.3, ops.md §1): the panel reads graphs from
// VictoriaMetrics through master, never touching the TSDB directly.
type Metrics struct {
	// VictoriaMetricsURL is the base URL of the VictoriaMetrics HTTP API
	// (e.g. http://127.0.0.1:8428); empty → the query proxy returns 503.
	VictoriaMetricsURL string `yaml:"victoriametrics_url"`
}

// Alerts configures the panel П2 Alerts endpoints (docs/specs/panel.md §3,
// ops.md §1): rules and live firing state come from the vmalert HTTP API,
// history from the alert-sink log the vmalert stack writes on the box.
type Alerts struct {
	// VmalertURL is the base URL of the vmalert HTTP API (e.g.
	// http://127.0.0.1:8880); empty → /v1/alerts/rules and /active return 503.
	VmalertURL string `yaml:"vmalert_url"`
	// LogPath is the alert-sink log (alertmanager-v2 JSON lines); missing file
	// → /v1/alerts/history returns an empty list, not 500.
	LogPath string `yaml:"log_path"`
}

type Config struct {
	DSN         string      `yaml:"dsn"`
	ListenAPI   string      `yaml:"listen_api"`
	ListenGRPC  string      `yaml:"listen_grpc"`
	TLS         TLS         `yaml:"tls"`
	Matchmaking Matchmaking `yaml:"matchmaking"`
	Compat      Compat      `yaml:"compat"`
	Metrics     Metrics     `yaml:"metrics"`
	Alerts      Alerts      `yaml:"alerts"`
}

func defaults() Config {
	return Config{
		ListenAPI:  ":8100",
		ListenGRPC: ":8444",
		TLS:        TLS{AutoCertDir: "certs"},
		// VictoriaMetrics of the same box (ops.md §1 recommended stack); the
		// metrics proxy returns 502 if it is not running, 503 only when unset.
		Metrics: Metrics{VictoriaMetricsURL: "http://127.0.0.1:8428"},
		// vmalert + alert sink on the same box (ops.md §1); the alerts
		// endpoints degrade gracefully (503 unset / 502 unreachable / [] no log).
		Alerts: Alerts{
			VmalertURL: "http://127.0.0.1:8880",
			LogPath:    "/var/log/birdman/alerts.log",
		},
		Matchmaking: Matchmaking{
			TickMS:      500,
			WidenAfterS: 30,
			TicketTTLS:  120,
		},
	}
}

// Load reads the YAML config at path (optional — pass "" to use defaults)
// and applies environment overrides: BIRDMAN_DSN, BIRDMAN_LISTEN_API,
// BIRDMAN_LISTEN_GRPC, BIRDMAN_MM_JOIN_SECRET.
func Load(path string) (Config, error) {
	cfg := defaults()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	if v := os.Getenv("BIRDMAN_DSN"); v != "" {
		cfg.DSN = v
	}
	if v := os.Getenv("BIRDMAN_LISTEN_API"); v != "" {
		cfg.ListenAPI = v
	}
	if v := os.Getenv("BIRDMAN_LISTEN_GRPC"); v != "" {
		cfg.ListenGRPC = v
	}
	if v := os.Getenv("BIRDMAN_MM_JOIN_SECRET"); v != "" {
		cfg.Matchmaking.JoinToken.Secret = v
	}
	if v := os.Getenv("BIRDMAN_VM_URL"); v != "" {
		cfg.Metrics.VictoriaMetricsURL = v
	}
	if v := os.Getenv("BIRDMAN_VMALERT_URL"); v != "" {
		cfg.Alerts.VmalertURL = v
	}
	if cfg.ListenAPI == "" {
		cfg.ListenAPI = ":8100"
	}
	if cfg.ListenGRPC == "" {
		cfg.ListenGRPC = ":8444"
	}
	if cfg.DSN == "" {
		return cfg, fmt.Errorf("dsn is required (config `dsn` or env BIRDMAN_DSN)")
	}
	if d := cfg.Compat.Default; d != "" && d != "major.minor" {
		return cfg, fmt.Errorf("compat.default %q is not supported (only \"major.minor\")", d)
	}
	return cfg, nil
}
