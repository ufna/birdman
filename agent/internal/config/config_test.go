package config

import (
	"os"
	"path/filepath"
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

// Полный спек-конфиг (agent.md §10) содержит master_addr/node_token —
// v0 их не использует, но обязан парсить без ошибок.
func TestUnknownKeysIgnored(t *testing.T) {
	if _, err := Load(write(t, `
master_addr: "master.birdman.internal:8443"
node_token: "bootstrap-token"
region: eu
limits_default: { cpu_millis: 1000, mem_mb: 512 }
`)); err != nil {
		t.Fatal(err)
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
