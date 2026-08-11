// Package config loads the agent YAML config (docs/specs/agent.md §10).
//
// Unknown keys are ignored on purpose (forward compatibility): configs
// written for newer agents must stay loadable by older ones.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Limits are per-server cgroup limits.
type Limits struct {
	CPUMillis int `yaml:"cpu_millis"`
	MemMB     int `yaml:"mem_mb"`
}

// RegistryAuth configures pulls from a private registry (GHCR by default).
// The token itself lives only in TokenFile — never in the config or code.
// This is the legacy (pre-registries-v1) fallback: the primary path is the
// master's Admin → Registries (agent-side host-match pull auth,
// docs/superpowers/specs/2026-07-09-registries-design.md §3).
type RegistryAuth struct {
	Username  string `yaml:"username"`
	TokenFile string `yaml:"token_file"`
	// Host scopes this credential to one registry host (host-matched
	// against image_ref, same as the master-supplied creds — §3): an
	// attacker-controlled image_ref on a foreign host must not receive this
	// token either. Optional; defaults to ghcr.io (with a one-time WARN,
	// agent/internal/daemon.Manager) for configs written before this field
	// existed.
	Host string `yaml:"host"`
}

// Token reads the registry token from TokenFile. The value must never be
// logged or persisted anywhere else.
func (a *RegistryAuth) Token() (string, error) {
	b, err := os.ReadFile(a.TokenFile)
	if err != nil {
		return "", fmt.Errorf("read registry token: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("registry token file %s is empty", a.TokenFile)
	}
	return tok, nil
}

// Config is the v0 subset of /etc/birdman/agent.yaml.
//
// master_addr / node_token(_file) / tls_* power the `run` daemon mode
// (iteration 1); run-once ignores them.
type Config struct {
	Region        string `yaml:"region"`
	CapacitySlots int    `yaml:"capacity_slots"`
	PortRange     []int  `yaml:"port_range"`
	LimitsDefault Limits `yaml:"limits_default"`
	LogDir        string `yaml:"log_dir"`
	DataDir       string `yaml:"data_dir"`
	// ContainerdRoot is the containerd data root where images live (dual-fs
	// watermark, environments v1 §6в). Defaults to /var/lib/containerd. When it
	// is a separate mount from data_dir, the image GC watermark and the
	// birdman_agent_containerd_disk_* metrics sample it in addition to data_dir.
	ContainerdRoot string        `yaml:"containerd_root"`
	RegistryAuth   *RegistryAuth `yaml:"registry_auth"`

	// MasterAddr is the gRPC AgentLink endpoint (host:port, always TLS).
	MasterAddr string `yaml:"master_addr"`
	// NodeToken authenticates the agent in Hello (protocol.md §Auth, v0).
	// Prefer NodeTokenFile: like the registry token, the secret then lives
	// only in a 0600 file, never in the config.
	NodeToken     string `yaml:"node_token"`
	NodeTokenFile string `yaml:"node_token_file"`
	// TLSInsecure skips master certificate verification — DEV ONLY, and only
	// on a loopback master_addr (validated below). Production: tls_ca_file +
	// enrollment (mTLS agentlink v1, design §4).
	TLSInsecure bool `yaml:"tls_insecure"`
	// TLSCAFile pins the bootstrap trust root delivered by ansible (a public
	// CA cert). The effective trust pool is this file UNION {tls_cert_dir}/ca.pem.
	TLSCAFile string `yaml:"tls_ca_file"`
	// TLSCertDir is the agent-owned directory (0700) holding the enrolled mTLS
	// material: client.key (0600, generated locally — never leaves the node),
	// client.crt and ca.pem (the trust bundle from EnrollResponse). Defaults to
	// {data_dir}/tls.
	TLSCertDir string `yaml:"tls_cert_dir"`
	// TLSServerName overrides the SAN verified on the master's leaf. The master
	// leaf always carries DNS SAN birdman-master (design §1); pinning the name
	// keeps verification IP-independent. Defaults to birdman-master.
	TLSServerName string `yaml:"tls_server_name"`

	// --- iteration 4: observability + ops (docs/specs/agent.md §5–§9) ---

	// MetricsAddr serves Prometheus text metrics (agent.md §9). Keep it on
	// localhost: the vmagent of the same node is the only consumer.
	MetricsAddr string `yaml:"metrics_addr"`
	// QoSEchoAddr is the public UDP echo for client QoS probes (agent.md §8).
	QoSEchoAddr string `yaml:"qos_echo_addr"`
	// LogScopeDirs makes the agent write a dedik's log into
	// {log_dir}/servers/{project}/{env}/{id}.log instead of the flat
	// {log_dir}/servers/{id}.log (agent.md §5, tracker #994): the pair rides in
	// the PATH so the shipper can label the VictoriaLogs stream with the owner
	// of the output, which is what lets master narrow a bound key's query.
	//
	// DEFAULT false, AND THAT IS A ROLLOUT DECISION, not a preference. The
	// agent binary upgrades itself (POST /v1/agent-upgrade, dev-стенд катает
	// его на каждый пуш), while the shipper config is rendered by ansible —
	// the two do NOT arrive together. An agent that started writing into
	// subdirectories before its vector got the matching `include` would ship
	// NOTHING: the old glob (`servers/*.log`) does not match the new path, and
	// the fleet's logs would silently stop reaching VictoriaLogs. So the switch
	// is turned on by the same ansible role that installs the new shipper
	// config (`birdman_log_scope_dirs`, роль birdman_agent_dev), and a bare
	// binary upgrade keeps the old, safe layout.
	LogScopeDirs bool `yaml:"log_scope_dirs"`
	// LogMaxSizeMB rotates a dedik log above this size (100MB × 2, §5).
	LogMaxSizeMB int `yaml:"log_max_size_mb"`
	// LogRetentionDays removes gzipped dedik logs older than this (§5).
	LogRetentionDays int `yaml:"log_retention_days"`
	// DiskGCWatermarkPct triggers image GC above this data_dir usage (§6).
	DiskGCWatermarkPct int `yaml:"disk_gc_watermark_pct"`
	// DiskFullWatermarkPct refuses StartServer above this usage (§6).
	DiskFullWatermarkPct int `yaml:"disk_full_watermark_pct"`
}

// MasterToken returns the node token: the inline value if set, otherwise the
// contents of node_token_file. Must never be logged.
func (c *Config) MasterToken() (string, error) {
	if c.NodeToken != "" {
		return c.NodeToken, nil
	}
	if c.NodeTokenFile == "" {
		return "", fmt.Errorf("node_token or node_token_file is required")
	}
	b, err := os.ReadFile(c.NodeTokenFile)
	if err != nil {
		return "", fmt.Errorf("read node token: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("node token file %s is empty", c.NodeTokenFile)
	}
	return tok, nil
}

// ValidateRun checks the extra fields required by the `run` daemon mode.
func (c *Config) ValidateRun() error {
	if c.MasterAddr == "" {
		return fmt.Errorf("master_addr is required for run mode")
	}
	if c.NodeToken == "" && c.NodeTokenFile == "" {
		return fmt.Errorf("node_token or node_token_file is required for run mode")
	}
	return nil
}

// masterAddrIsLoopback reports whether master_addr points at the local host:
// an explicit "localhost" or an IP literal that IsLoopback (127.0.0.0/8, ::1).
// A bare hostname or a routable IP is treated as non-loopback (fail closed for
// the tls_insecure gate). A missing port is tolerated (host taken as-is).
func masterAddrIsLoopback(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// Load reads, defaults and validates the config at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if len(c.PortRange) == 0 {
		c.PortRange = []int{20000, 29999} // spec default (agent.md §3)
	}
	if c.CapacitySlots == 0 {
		c.CapacitySlots = 1 // unused by v0 run-once
	}
	if c.LogDir == "" {
		c.LogDir = "/var/log/birdman"
	}
	if c.DataDir == "" {
		c.DataDir = "/var/lib/birdman"
	}
	if c.ContainerdRoot == "" {
		c.ContainerdRoot = "/var/lib/containerd" // dual-fs watermark (§6в)
	}
	// tls_cert_dir tracks data_dir by default (must run after DataDir is set).
	if c.TLSCertDir == "" {
		c.TLSCertDir = filepath.Join(c.DataDir, "tls")
	}
	if c.TLSServerName == "" {
		c.TLSServerName = "birdman-master" // master leaf DNS SAN (design §1)
	}
	if c.MetricsAddr == "" {
		c.MetricsAddr = "127.0.0.1:9101" // agent.md §9: localhost only
	}
	if c.QoSEchoAddr == "" {
		c.QoSEchoAddr = ":19999" // agent.md §8: public QoS port
	}
	if c.LogMaxSizeMB == 0 {
		c.LogMaxSizeMB = 100 // agent.md §5
	}
	if c.LogRetentionDays == 0 {
		c.LogRetentionDays = 7 // agent.md §5
	}
	if c.DiskGCWatermarkPct == 0 {
		c.DiskGCWatermarkPct = 80 // agent.md §6
	}
	if c.DiskFullWatermarkPct == 0 {
		c.DiskFullWatermarkPct = 90 // agent.md §6
	}
}

func (c *Config) validate() error {
	if c.Region == "" {
		return fmt.Errorf("region is required")
	}
	if len(c.PortRange) != 2 {
		return fmt.Errorf("port_range must be [from, to], got %v", c.PortRange)
	}
	if c.PortRange[0] <= 0 || c.PortRange[1] > 65535 || c.PortRange[0] > c.PortRange[1] {
		return fmt.Errorf("invalid port_range [%d, %d]", c.PortRange[0], c.PortRange[1])
	}
	if c.CapacitySlots < 0 {
		return fmt.Errorf("capacity_slots must be positive, got %d", c.CapacitySlots)
	}
	if c.LimitsDefault.CPUMillis <= 0 || c.LimitsDefault.MemMB <= 0 {
		return fmt.Errorf("limits_default: cpu_millis and mem_mb must be positive")
	}
	if a := c.RegistryAuth; a != nil {
		if a.Username == "" || a.TokenFile == "" {
			return fmt.Errorf("registry_auth: username and token_file are required")
		}
		// A legacy docker.io host can never match a real pull: containerd
		// resolves docker.io/index.docker.io to registry-1.docker.io before
		// the host-match runs (daemon.Manager.legacyCred), so this cred would
		// silently never fire — the same reason master's
		// store.NormalizeRegistryHost rejects it on POST /v1/registries
		// (docs/superpowers/specs/2026-07-09-registries-design.md §3, task
		// review Fix 4). Config-time validation error, not just a runtime
		// WARN: a misconfig here should not boot silently.
		if h := strings.ToLower(strings.TrimSpace(a.Host)); h == "docker.io" || h == "index.docker.io" {
			return fmt.Errorf("registry_auth: host %q is not supported — docker.io/index.docker.io resolves to registry-1.docker.io, so an exact host-match cred can never fire", a.Host)
		}
	}
	// tls_insecure skips master verification and is the agent half of the
	// iteration-5 gate: it is legal ONLY on a loopback master_addr (dev/debug).
	// A non-loopback link with verification disabled would let a MITM steal the
	// node_token and spoof UpgradeAgent/StartServer (RCE), so the agent must
	// refuse to boot rather than run it silently — the same fail-closed stance
	// as the docker.io host reject above (design §4/§Безопасность).
	if c.TLSInsecure && c.MasterAddr != "" && !masterAddrIsLoopback(c.MasterAddr) {
		return fmt.Errorf("tls_insecure: true is only allowed with a loopback master_addr (dev); master_addr %q is not loopback — deliver a CA to tls_ca_file and drop tls_insecure for mTLS", c.MasterAddr)
	}
	if c.LogMaxSizeMB < 0 || c.LogRetentionDays < 0 {
		return fmt.Errorf("log_max_size_mb and log_retention_days must be positive")
	}
	if c.DiskGCWatermarkPct < 1 || c.DiskGCWatermarkPct > 100 ||
		c.DiskFullWatermarkPct < 1 || c.DiskFullWatermarkPct > 100 ||
		c.DiskGCWatermarkPct > c.DiskFullWatermarkPct {
		return fmt.Errorf("disk watermarks must satisfy 1 <= gc (%d) <= full (%d) <= 100",
			c.DiskGCWatermarkPct, c.DiskFullWatermarkPct)
	}
	return nil
}
