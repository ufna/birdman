package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadFull(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "ghcr.token")
	if err := os.WriteFile(tokenFile, []byte(" ghp_secret123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(write(t, `
region: "eu"
capacity_slots: 24
port_range: [21000, 21999]
limits_default: { cpu_millis: 3500, mem_mb: 4096 }
log_dir: /tmp/birdman-logs
data_dir: /tmp/birdman-data
registry_auth:
  username: "ufna"
  token_file: `+tokenFile+`
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Region != "eu" || cfg.CapacitySlots != 24 {
		t.Fatalf("%+v", cfg)
	}
	if cfg.PortRange[0] != 21000 || cfg.PortRange[1] != 21999 {
		t.Fatalf("port_range: %v", cfg.PortRange)
	}
	if cfg.LimitsDefault.CPUMillis != 3500 || cfg.LimitsDefault.MemMB != 4096 {
		t.Fatalf("limits: %+v", cfg.LimitsDefault)
	}
	if cfg.LogDir != "/tmp/birdman-logs" || cfg.DataDir != "/tmp/birdman-data" {
		t.Fatalf("dirs: %+v", cfg)
	}
	if cfg.RegistryAuth == nil || cfg.RegistryAuth.Username != "ufna" {
		t.Fatalf("registry_auth: %+v", cfg.RegistryAuth)
	}
	tok, err := cfg.RegistryAuth.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "ghp_secret123" {
		t.Fatalf("token = %q", tok)
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(write(t, `
region: local
limits_default: { cpu_millis: 1000, mem_mb: 512 }
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PortRange[0] != 20000 || cfg.PortRange[1] != 29999 {
		t.Fatalf("default port_range: %v", cfg.PortRange)
	}
	if cfg.LogDir != "/var/log/birdman" || cfg.DataDir != "/var/lib/birdman" {
		t.Fatalf("default dirs: %+v", cfg)
	}
	if cfg.CapacitySlots != 1 {
		t.Fatalf("default capacity_slots = %d", cfg.CapacitySlots)
	}
	if cfg.RegistryAuth != nil {
		t.Fatal("registry_auth must be nil when absent")
	}
}

// Незнакомые ключи (конфиги будущих агентов) парсятся без ошибок.
// TestRegistryAuthHostField covers the optional registry_auth.host field
// (registries v1, docs/superpowers/specs/2026-07-09-registries-design.md
// §3): present when set, empty (not defaulted here — the daemon.Manager
// legacy fallback applies the ghcr.io default lazily, with a one-time WARN)
// when absent.
func TestRegistryAuthHostField(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tokenFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(write(t, `
region: eu
limits_default: { cpu_millis: 1000, mem_mb: 512 }
registry_auth:
  username: "ufna"
  token_file: `+tokenFile+`
  host: "registry.example.com:5000"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RegistryAuth.Host != "registry.example.com:5000" {
		t.Fatalf("registry_auth.host = %q, want registry.example.com:5000", cfg.RegistryAuth.Host)
	}

	// Absent host: config.go leaves it empty (no static default) — the
	// ghcr.io default + one-time WARN is the daemon.Manager's job.
	cfg2, err := Load(write(t, `
region: eu
limits_default: { cpu_millis: 1000, mem_mb: 512 }
registry_auth:
  username: "ufna"
  token_file: `+tokenFile+`
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.RegistryAuth.Host != "" {
		t.Fatalf("registry_auth.host must stay empty when absent, got %q", cfg2.RegistryAuth.Host)
	}
}

// TestRegistryAuthHostDockerIORejected: registry_auth.host normalizing to
// docker.io/index.docker.io must fail Load with a clear, actionable error
// (task review, Fix 4) rather than boot silently — containerd resolves
// docker.io/index.docker.io to registry-1.docker.io before
// daemon.Manager's host-match ever runs, so this cred could never fire for a
// real pull, exactly the reason store.NormalizeRegistryHost rejects it on
// POST /v1/registries master-side (docs/superpowers/specs/2026-07-09-registries-design.md
// §3). Covers case-insensitivity and incidental whitespace, matching the
// normalization the runtime path (daemon.Manager.legacyRegistryHost) applies.
func TestRegistryAuthHostDockerIORejected(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tokenFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"docker.io", "index.docker.io", "Docker.IO", "  docker.io  "} {
		t.Run(host, func(t *testing.T) {
			_, err := Load(write(t, `
region: eu
limits_default: { cpu_millis: 1000, mem_mb: 512 }
registry_auth:
  username: "ufna"
  token_file: `+tokenFile+`
  host: "`+host+`"
`))
			if err == nil {
				t.Fatalf("want a validation error for registry_auth.host %q, got nil", host)
			}
			if !strings.Contains(err.Error(), "docker.io") {
				t.Fatalf("error should name docker.io for a clear, actionable reason, got: %v", err)
			}
		})
	}

	// A real, non-docker.io host must still load fine (no false positive).
	if _, err := Load(write(t, `
region: eu
limits_default: { cpu_millis: 1000, mem_mb: 512 }
registry_auth:
  username: "ufna"
  token_file: `+tokenFile+`
  host: "ghcr.io"
`)); err != nil {
		t.Fatalf("ghcr.io host must not be rejected: %v", err)
	}
}

func TestUnknownKeysIgnored(t *testing.T) {
	if _, err := Load(write(t, `
region: eu
limits_default: { cpu_millis: 1000, mem_mb: 512 }
some_future_key: true
`)); err != nil {
		t.Fatal(err)
	}
}

func TestMasterLinkConfig(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "node.token")
	if err := os.WriteFile(tokenFile, []byte("bnt_id.secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(write(t, `
region: dev
limits_default: { cpu_millis: 1000, mem_mb: 512 }
master_addr: "127.0.0.1:8444"
node_token_file: `+tokenFile+`
tls_insecure: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MasterAddr != "127.0.0.1:8444" || !cfg.TLSInsecure || cfg.TLSCAFile != "" {
		t.Fatalf("%+v", cfg)
	}
	if err := cfg.ValidateRun(); err != nil {
		t.Fatal(err)
	}
	tok, err := cfg.MasterToken()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "bnt_id.secret" {
		t.Fatalf("token = %q", tok)
	}

	// Inline token wins over the file.
	cfg.NodeToken = "bnt_inline.secret"
	if tok, _ := cfg.MasterToken(); tok != "bnt_inline.secret" {
		t.Fatalf("inline token must win, got %q", tok)
	}
}

func TestValidateRunErrors(t *testing.T) {
	base := `
region: dev
limits_default: { cpu_millis: 1000, mem_mb: 512 }
`
	cfg, err := Load(write(t, base))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateRun(); err == nil {
		t.Fatal("run mode without master_addr must fail validation")
	}
	cfg, err = Load(write(t, base+`master_addr: "127.0.0.1:8444"`))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateRun(); err == nil {
		t.Fatal("run mode without node_token must fail validation")
	}
	cfg.NodeTokenFile = filepath.Join(t.TempDir(), "missing")
	if err := cfg.ValidateRun(); err != nil {
		t.Fatalf("token file existence is not validated at load: %v", err)
	}
	if _, err := cfg.MasterToken(); err == nil {
		t.Fatal("missing node token file must fail")
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]string{
		"no region":      "limits_default: { cpu_millis: 1, mem_mb: 1 }",
		"no limits":      "region: eu",
		"bad range len":  "region: eu\nlimits_default: { cpu_millis: 1, mem_mb: 1 }\nport_range: [1000]",
		"reversed range": "region: eu\nlimits_default: { cpu_millis: 1, mem_mb: 1 }\nport_range: [2000, 1000]",
		"port too big":   "region: eu\nlimits_default: { cpu_millis: 1, mem_mb: 1 }\nport_range: [1000, 70000]",
		"auth wo token":  "region: eu\nlimits_default: { cpu_millis: 1, mem_mb: 1 }\nregistry_auth: { username: u }",
		"negative slots": "region: eu\nlimits_default: { cpu_millis: 1, mem_mb: 1 }\ncapacity_slots: -1",
	}
	for name, content := range cases {
		if _, err := Load(write(t, content)); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

func TestTokenErrors(t *testing.T) {
	a := &RegistryAuth{Username: "u", TokenFile: filepath.Join(t.TempDir(), "missing")}
	if _, err := a.Token(); err == nil {
		t.Fatal("missing token file must fail")
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.TokenFile = empty
	if _, err := a.Token(); err == nil {
		t.Fatal("empty token file must fail")
	}
}
