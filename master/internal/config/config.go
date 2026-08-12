// Package config loads master.yaml (docs/specs/master.md §7; dev defaults
// clarified in v0: API :8100, gRPC :8444).
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ufna/birdman/master/internal/secrets"
)

type TLS struct {
	// CertFile/KeyFile: external PEM pair for the gRPC AgentLink listener.
	// When both are empty, the master issues its own server leaf from the
	// internal CA (in memory, hot-rotated) instead — mTLS agentlink v1,
	// docs/superpowers/specs/2026-07-10-mtls-agentlink-design.md §1.
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// AutoCertDir is accepted for config compatibility but no longer used:
	// the retired self-signed autogen wrote its pair here (design §1). Kept so
	// existing master.yaml files with this key still load.
	AutoCertDir string `yaml:"auto_cert_dir"`
	// ExtraSANs are appended to the internally issued server leaf's SANs
	// (design §1) — extra DNS names or IPs for external probes. Optional; each
	// entry is classified as IP or DNS by tlsutil.IssueServerLeaf. Agents
	// verify the master by ServerName "birdman-master", so they never need
	// these; ignored entirely when an external CertFile/KeyFile is set.
	ExtraSANs []string `yaml:"extra_sans"`
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
	// VictoriaLogsURL is the base URL of the VictoriaLogs HTTP API
	// (e.g. http://127.0.0.1:9428); empty → the logs proxy returns 503.
	VictoriaLogsURL string `yaml:"victorialogs_url"`
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
	// AlertmanagerURL — base URL alertmanager api/v2 для зеркалирования
	// mute → настоящий silence (ops.md §1, tracker #238). Пусто →
	// зеркалирование выключено, mute работает в чистой v0-семантике
	// (аннотация muted:true), что нормально для self-host без мониторинг-стека.
	AlertmanagerURL string `yaml:"alertmanager_url"`
}

// Backups — исполнение бекапов master'ом (Backups v1). Настройки политики
// (интервал/ретеншны/S3) живут в БД и правятся из панели; здесь только
// деплой-концерны: куда писать дампы и где pg_dump.
type Backups struct {
	Dir        string `yaml:"dir"`          // каталог дампов, деф. /var/lib/birdman/backups
	PgDumpPath string `yaml:"pg_dump_path"` // бинарь pg_dump, деф. "pg_dump" (PATH)
}

type Config struct {
	DSN        string `yaml:"dsn"`
	ListenAPI  string `yaml:"listen_api"`
	ListenGRPC string `yaml:"listen_grpc"`
	// ListenMetrics — ОТДЕЛЬНЫЙ адрес прометеевской экспозиции `/metrics`
	// (tracker #1003). На `listen_api` этой ручки нет: реестр не пер-тенантный
	// и отдаётся без аутентификации, поэтому единственная работающая граница —
	// АДРЕС. Дефолт `127.0.0.1:9102` (9101 занят агентом на том же боксе).
	//
	// Семантика пустого значения отличается от `listen_api` СОЗНАТЕЛЬНО:
	// отсутствующий ключ в конфиге — это дефолт (апгрейд со старым файлом не
	// теряет метрики молча, они лишь переезжают с API-порта на loopback), а
	// ЯВНЫЙ `listen_metrics: ""` выключает экспозицию совсем — оператору, у
	// которого скрейпера нет, не за что платить открытым портом. Env-override
	// BIRDMAN_LISTEN_METRICS.
	ListenMetrics string `yaml:"listen_metrics"`
	TLS           TLS    `yaml:"tls"`
	// AgentlinkAuth selects how the gRPC AgentLink listener authenticates
	// agents (mTLS agentlink v1, design §3): "token" (client certs ignored —
	// pre-mTLS behaviour, byte-identical; emergency rollback), "mixed"
	// (Session accepts a verified client cert OR a node_token; the
	// post-release default and transition mode) or "mtls" (Session requires a
	// verified client cert; token-only Hello → PermissionDenied, though
	// Enroll-by-token stays possible). Env override BIRDMAN_AGENTLINK_AUTH.
	AgentlinkAuth string      `yaml:"agentlink_auth"`
	Matchmaking   Matchmaking `yaml:"matchmaking"`
	Compat        Compat      `yaml:"compat"`
	Metrics       Metrics     `yaml:"metrics"`
	Alerts        Alerts      `yaml:"alerts"`
	// StatsRollupInterval is how often the rollup-maintenance job
	// (internal/statsrollup, «Статистика v1» T9) recomputes the trailing
	// two UTC days of match_stats_daily/match_ccu_daily. A human-readable
	// duration string (e.g. "2m", "30s") — yaml.v3 parses time.Duration via
	// time.ParseDuration natively.
	StatsRollupInterval time.Duration `yaml:"stats_rollup_interval"`
	// NodeDownAfterMin is how long a quarantined node may stay silent before
	// the lease checker marks it down (итерация 5 follow-up, ревизия):
	// operators/alerts tell «моргнула» (quarantine) from «лежит давно» (down).
	// A heartbeat of a live agent session lifts down → active. 'dead' is NOT
	// set here — it is the manual revocation terminal (agentlink refuses dead
	// nodes a session), and auto-dead would lock a node out permanently.
	// Default 10, must be >= 1.
	NodeDownAfterMin int `yaml:"node_down_after_min"`
	// Backups holds deploy-time backup concerns (Backups v1); the policy
	// (interval/retentions/S3) lives in the DB (backup_settings), edited from
	// the panel — see the Backups type.
	Backups Backups `yaml:"backups"`
	// SecretsKeyFile is the path to the master's at-rest encryption key —
	// base64 of 32 random bytes, one line, 0600 (secrets-encryption design §2).
	// The ansible role provisions /etc/birdman/secrets.key here. Env override
	// BIRDMAN_SECRETS_KEY_FILE (path); the dev/test env value override is
	// BIRDMAN_SECRETS_KEY (the base64 key itself, for dev-compose/CI where the
	// container has no /etc/birdman). Resolved to key bytes by SecretsKey().
	SecretsKeyFile string `yaml:"secrets_key_file"`
}

func defaults() Config {
	return Config{
		ListenAPI:  ":8100",
		ListenGRPC: ":8444",
		// Loopback — не «рекомендация развёртывания», а дефолт кода: реестр
		// перечисляет проекты, окружения и живые server_id (tracker #1003).
		ListenMetrics: "127.0.0.1:9102",
		TLS:           TLS{AutoCertDir: "certs"},
		// Post-release default (design §3): accept both cert-auth and
		// token-auth during the transition to strict mTLS.
		AgentlinkAuth: "mixed",
		// VictoriaMetrics/VictoriaLogs of the same box (ops.md §1 recommended
		// stack); the proxies return 502 if the upstream is not running, 503
		// only when the URL is unset.
		Metrics: Metrics{VictoriaMetricsURL: "http://127.0.0.1:8428", VictoriaLogsURL: "http://127.0.0.1:9428"},
		// vmalert + alert sink on the same box (ops.md §1); the alerts
		// endpoints degrade gracefully (503 unset / 502 unreachable / [] no log).
		// alertmanager of the same box mirrors mutes into real silences
		// (tracker #245); best-effort — an unreachable AM never breaks mute.
		Alerts: Alerts{
			VmalertURL:      "http://127.0.0.1:8880",
			LogPath:         "/var/log/birdman/alerts.log",
			AlertmanagerURL: "http://127.0.0.1:9093",
		},
		Matchmaking: Matchmaking{
			TickMS:      500,
			WidenAfterS: 30,
			TicketTTLS:  120,
		},
		StatsRollupInterval: 2 * time.Minute,
		Backups:             Backups{Dir: "/var/lib/birdman/backups", PgDumpPath: "pg_dump"},
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
	if v := os.Getenv("BIRDMAN_LISTEN_METRICS"); v != "" {
		cfg.ListenMetrics = v
	}
	if v := os.Getenv("BIRDMAN_MM_JOIN_SECRET"); v != "" {
		cfg.Matchmaking.JoinToken.Secret = v
	}
	if v := os.Getenv("BIRDMAN_VM_URL"); v != "" {
		cfg.Metrics.VictoriaMetricsURL = v
	}
	if v := os.Getenv("BIRDMAN_VL_URL"); v != "" {
		cfg.Metrics.VictoriaLogsURL = v
	}
	if v := os.Getenv("BIRDMAN_VMALERT_URL"); v != "" {
		cfg.Alerts.VmalertURL = v
	}
	if v := os.Getenv("BIRDMAN_ALERTMANAGER_URL"); v != "" {
		cfg.Alerts.AlertmanagerURL = v
	}
	if v := os.Getenv("BIRDMAN_AGENTLINK_AUTH"); v != "" {
		cfg.AgentlinkAuth = v
	}
	if v := os.Getenv("BIRDMAN_SECRETS_KEY_FILE"); v != "" {
		cfg.SecretsKeyFile = v
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
	if cfg.AgentlinkAuth == "" {
		cfg.AgentlinkAuth = "mixed"
	}
	switch cfg.AgentlinkAuth {
	case "token", "mixed", "mtls":
	default:
		return cfg, fmt.Errorf("agentlink_auth %q is not supported (token|mixed|mtls)", cfg.AgentlinkAuth)
	}
	if cfg.NodeDownAfterMin == 0 {
		cfg.NodeDownAfterMin = 10
	}
	if cfg.NodeDownAfterMin < 1 {
		return cfg, fmt.Errorf("node_down_after_min must be >= 1")
	}
	if cfg.Backups.Dir == "" {
		cfg.Backups.Dir = "/var/lib/birdman/backups"
	}
	if cfg.Backups.PgDumpPath == "" {
		cfg.Backups.PgDumpPath = "pg_dump"
	}
	return cfg, nil
}

// SecretsKey resolves the 32-byte at-rest master key (secrets-encryption design
// §2) from exactly ONE source, deliberately choosing it so a config-file
// default path and a dev env value cannot trip secrets.LoadKey's both-sources
// ambiguity guard:
//
//   - BIRDMAN_SECRETS_KEY (a base64 key VALUE) WINS when set — the dev-compose/
//     CI override — even if SecretsKeyFile also holds a (default) path. This is
//     the normal dev case: master.example.yaml ships a secrets_key_file path,
//     but a container with no /etc/birdman injects the key by env instead, and
//     that must not read as "two conflicting sources".
//   - Otherwise the file at SecretsKeyFile (env BIRDMAN_SECRETS_KEY_FILE
//     overrides the yaml value, applied in Load) is the single source.
//
// Because exactly one source ever reaches LoadKey, its ambiguity error fires
// only for a genuinely broken direct caller, never for a dev override layered
// over a default path. The env VALUE is read here rather than stored on Config
// so a raw key never lands in the marshalled struct. This never generates a
// key: a missing/invalid source is a hard error (fail-loud; master must not
// start able to decrypt nothing) — the caller reports it and refuses to boot.
func (c Config) SecretsKey() ([]byte, error) {
	if v := os.Getenv("BIRDMAN_SECRETS_KEY"); v != "" {
		return secrets.LoadKey("", v)
	}
	return secrets.LoadKey(c.SecretsKeyFile, "")
}

// SecretsKeyFileInUse reports whether the resolved key source is the file (not
// the BIRDMAN_SECRETS_KEY env value), and the path — so the caller can WARN on
// an over-permissive file mode only when a file is actually read (design §2).
func (c Config) SecretsKeyFileInUse() (string, bool) {
	if os.Getenv("BIRDMAN_SECRETS_KEY") != "" {
		return "", false
	}
	return c.SecretsKeyFile, c.SecretsKeyFile != ""
}
