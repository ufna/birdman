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

// TestContainerdRootDefault covers the dual-fs watermark config
// (environments v1 §6в): containerd_root defaults to /var/lib/containerd and
// is overridable for a node whose image store lives on a separate mount.
func TestContainerdRootDefault(t *testing.T) {
	def, err := Load(write(t, `
region: local
limits_default: { cpu_millis: 1000, mem_mb: 512 }
`))
	if err != nil {
		t.Fatal(err)
	}
	if def.ContainerdRoot != "/var/lib/containerd" {
		t.Fatalf("default containerd_root = %q, want /var/lib/containerd", def.ContainerdRoot)
	}

	set, err := Load(write(t, `
region: local
limits_default: { cpu_millis: 1000, mem_mb: 512 }
containerd_root: /mnt/images/containerd
`))
	if err != nil {
		t.Fatal(err)
	}
	if set.ContainerdRoot != "/mnt/images/containerd" {
		t.Fatalf("containerd_root = %q, want the configured path", set.ContainerdRoot)
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

// TestTLSCertDefaults: tls_cert_dir defaults to {data_dir}/tls and
// tls_server_name to "birdman-master" (mTLS agentlink v1, design §4). Explicit
// values win; the cert dir default tracks a custom data_dir.
func TestTLSCertDefaults(t *testing.T) {
	cfg, err := Load(write(t, `
region: dev
limits_default: { cpu_millis: 1000, mem_mb: 512 }
data_dir: /tmp/bm
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLSCertDir != "/tmp/bm/tls" {
		t.Fatalf("tls_cert_dir default = %q, want /tmp/bm/tls", cfg.TLSCertDir)
	}
	if cfg.TLSServerName != "birdman-master" {
		t.Fatalf("tls_server_name default = %q, want birdman-master", cfg.TLSServerName)
	}

	// Explicit values are honored.
	cfg, err = Load(write(t, `
region: dev
limits_default: { cpu_millis: 1000, mem_mb: 512 }
data_dir: /tmp/bm
tls_cert_dir: /etc/birdman/tls
tls_server_name: master.internal
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLSCertDir != "/etc/birdman/tls" || cfg.TLSServerName != "master.internal" {
		t.Fatalf("explicit tls_cert_dir/tls_server_name not honored: %+v", cfg)
	}
}

// TestTLSInsecureNonLoopbackRejected: tls_insecure: true is the agent half of
// the iteration-5 gate (design §4/§Безопасность). It is legal ONLY on a
// loopback master_addr (dev/debug); with a non-loopback master_addr the config
// must fail to LOAD (the agent must not boot a MITM-exposed link silently —
// same fail-closed principle as the docker.io host reject).
func TestTLSInsecureNonLoopbackRejected(t *testing.T) {
	base := `
region: dev
limits_default: { cpu_millis: 1000, mem_mb: 512 }
node_token: "bnt_x.y"
tls_insecure: true
`
	for _, addr := range []string{
		"master.example.com:8444",
		"10.0.0.5:8444",
		"[2001:db8::1]:8444",
		"8.8.8.8:8444",
	} {
		t.Run("reject/"+addr, func(t *testing.T) {
			_, err := Load(write(t, base+"master_addr: \""+addr+"\"\n"))
			if err == nil {
				t.Fatalf("tls_insecure with non-loopback master_addr %q must fail to load", addr)
			}
			if !strings.Contains(err.Error(), "tls_insecure") {
				t.Fatalf("error should name tls_insecure, got: %v", err)
			}
		})
	}

	// Loopback master_addr keeps tls_insecure legal (dev).
	for _, addr := range []string{
		"127.0.0.1:8444",
		"127.0.0.5:8444",
		"[::1]:8444",
		"localhost:8444",
	} {
		t.Run("allow/"+addr, func(t *testing.T) {
			cfg, err := Load(write(t, base+"master_addr: \""+addr+"\"\n"))
			if err != nil {
				t.Fatalf("tls_insecure on loopback master_addr %q must load: %v", addr, err)
			}
			if !cfg.TLSInsecure {
				t.Fatalf("tls_insecure not parsed: %+v", cfg)
			}
		})
	}

	// tls_insecure without master_addr at all (e.g. run-once configs) is not a
	// run-mode concern and must not block Load.
	if _, err := Load(write(t, base)); err != nil {
		t.Fatalf("tls_insecure without master_addr must still load: %v", err)
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

// --- несколько нод birdman на одном хосте (tracker #1065) ---
//
// Три ключа делают агента ужимаемым до ОДНОЙ ноды бокса, на котором их
// несколько. Тесты держат ровно один инвариант на каждый: отсутствие ключа =
// прежнее поведение односерверного бокса, присутствие = изоляция от соседа.

// node_name — имя ноды в мастере. Пусто → бинарь берёт хостнейм ОС (это
// делает main, не config: здесь важно, что пустое поле остаётся пустым и не
// подменяется чем-то по дороге).
func TestNodeName(t *testing.T) {
	def, err := Load(write(t, `
region: local
limits_default: { cpu_millis: 1000, mem_mb: 512 }
`))
	if err != nil {
		t.Fatal(err)
	}
	if def.NodeName != "" {
		t.Fatalf("node_name без ключа = %q, ожидалось пусто (фоллбэк на хостнейм ОС — в main)", def.NodeName)
	}

	set, err := Load(write(t, `
region: local
limits_default: { cpu_millis: 1000, mem_mb: 512 }
node_name: server-cqp-khl
`))
	if err != nil {
		t.Fatal(err)
	}
	if set.NodeName != "server-cqp-khl" {
		t.Fatalf("node_name = %q", set.NodeName)
	}
}

// containerd_namespace — граница, за которой Restore()/image-GC агента видят
// ТОЛЬКО свои объекты. Пусто = прежний общий namespace (runtime.Connect
// подставляет DefaultNamespace); значение с разделителем — отказ на загрузке,
// а не невнятная ошибка containerd на первом вызове.
func TestContainerdNamespace(t *testing.T) {
	def, err := Load(write(t, `
region: local
limits_default: { cpu_millis: 1000, mem_mb: 512 }
`))
	if err != nil {
		t.Fatal(err)
	}
	if def.ContainerdNamespace != "" {
		t.Fatalf("containerd_namespace без ключа = %q, ожидалось пусто", def.ContainerdNamespace)
	}

	set, err := Load(write(t, `
region: local
limits_default: { cpu_millis: 1000, mem_mb: 512 }
containerd_namespace: birdman-khl
`))
	if err != nil {
		t.Fatal(err)
	}
	if set.ContainerdNamespace != "birdman-khl" {
		t.Fatalf("containerd_namespace = %q", set.ContainerdNamespace)
	}

	for _, bad := range []string{"birdman/khl", "birdman khl", "birdman\tkhl"} {
		if _, err := Load(write(t, `
region: local
limits_default: { cpu_millis: 1000, mem_mb: 512 }
containerd_namespace: `+"\""+bad+"\""+`
`)); err == nil {
			t.Fatalf("containerd_namespace %q принят, ожидался отказ", bad)
		}
	}
}

// qos_echo_addr: off — единственный способ сказать «эхо на этом хосте держит
// СОСЕД». Отсутствие ключа обязано остаться :19999 (иначе тихо погас бы
// ping-таргет всех существующих нод), а сентинел не должен зависеть от
// регистра и пробелов — его пишет шаблон роли, а читают люди.
func TestQoSEchoOff(t *testing.T) {
	def, err := Load(write(t, `
region: local
limits_default: { cpu_millis: 1000, mem_mb: 512 }
`))
	if err != nil {
		t.Fatal(err)
	}
	if def.QoSEchoAddr != ":19999" || !def.QoSEchoEnabled() {
		t.Fatalf("дефолт qos_echo_addr = %q enabled=%v, ожидалось :19999/true", def.QoSEchoAddr, def.QoSEchoEnabled())
	}

	custom, err := Load(write(t, `
region: local
limits_default: { cpu_millis: 1000, mem_mb: 512 }
qos_echo_addr: ":19998"
`))
	if err != nil {
		t.Fatal(err)
	}
	if !custom.QoSEchoEnabled() {
		t.Fatal("явный адрес эха выключил респондер")
	}

	for _, off := range []string{"off", "OFF", " off "} {
		cfg, err := Load(write(t, `
region: local
limits_default: { cpu_millis: 1000, mem_mb: 512 }
qos_echo_addr: `+"\""+off+"\""+`
`))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.QoSEchoEnabled() {
			t.Fatalf("qos_echo_addr %q не выключил эхо", off)
		}
	}
}
