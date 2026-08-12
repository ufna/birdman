package config

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

// withDSN sets BIRDMAN_DSN so Load() clears its "dsn is required" gate and we
// can exercise the rest of the validation. Load reads env unconditionally, so
// this is enough whether path is "" or a file that omits dsn.
func withDSN(t *testing.T) {
	t.Helper()
	t.Setenv("BIRDMAN_DSN", "postgres://x/db?sslmode=disable")
}

// mTLS agentlink v1 (design §3): agentlink_auth selects the listener's auth
// mode; the post-release default is "mixed".
func TestLoadAgentlinkAuthDefaultsToMixed(t *testing.T) {
	withDSN(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AgentlinkAuth != "mixed" {
		t.Fatalf("default agentlink_auth = %q, want mixed", cfg.AgentlinkAuth)
	}
}

func TestLoadAgentlinkAuthAcceptsValidModes(t *testing.T) {
	withDSN(t)
	for _, mode := range []string{"token", "mixed", "mtls"} {
		path := writeConfig(t, "agentlink_auth: \""+mode+"\"\n")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load(%s): %v", mode, err)
		}
		if cfg.AgentlinkAuth != mode {
			t.Fatalf("agentlink_auth = %q, want %q", cfg.AgentlinkAuth, mode)
		}
	}
}

func TestLoadAgentlinkAuthRejectsUnknownMode(t *testing.T) {
	withDSN(t)
	path := writeConfig(t, "agentlink_auth: \"insecure\"\n")
	if _, err := Load(path); err == nil {
		t.Fatal("Load must reject an unknown agentlink_auth value")
	}
}

func TestLoadAgentlinkAuthEnvOverride(t *testing.T) {
	withDSN(t)
	// The file says mixed; the env must win.
	path := writeConfig(t, "agentlink_auth: \"mixed\"\n")
	t.Setenv("BIRDMAN_AGENTLINK_AUTH", "mtls")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AgentlinkAuth != "mtls" {
		t.Fatalf("env override agentlink_auth = %q, want mtls", cfg.AgentlinkAuth)
	}
}

// An invalid env override is validated too — the enum gate runs after env is
// applied.
func TestLoadAgentlinkAuthEnvOverrideValidated(t *testing.T) {
	withDSN(t)
	t.Setenv("BIRDMAN_AGENTLINK_AUTH", "bogus")
	if _, err := Load(""); err == nil {
		t.Fatal("Load must reject an unknown BIRDMAN_AGENTLINK_AUTH value")
	}
}

// tls.extra_sans is optional and, when present, parsed into a string slice
// (design §1: extra DNS/IP SANs for the internally issued server leaf).
func TestLoadTLSExtraSANs(t *testing.T) {
	withDSN(t)
	path := writeConfig(t, "tls:\n  extra_sans:\n    - \"master.internal\"\n    - \"10.0.0.5\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.TLS.ExtraSANs) != 2 || cfg.TLS.ExtraSANs[0] != "master.internal" || cfg.TLS.ExtraSANs[1] != "10.0.0.5" {
		t.Fatalf("extra_sans = %v, want [master.internal 10.0.0.5]", cfg.TLS.ExtraSANs)
	}
}

// secrets-encryption design §2: secrets_key_file is loaded from yaml and
// overridden by BIRDMAN_SECRETS_KEY_FILE.
func TestLoadSecretsKeyFileEnvOverride(t *testing.T) {
	withDSN(t)
	path := writeConfig(t, "secrets_key_file: \"/etc/birdman/secrets.key\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SecretsKeyFile != "/etc/birdman/secrets.key" {
		t.Fatalf("secrets_key_file from yaml = %q", cfg.SecretsKeyFile)
	}
	t.Setenv("BIRDMAN_SECRETS_KEY_FILE", "/run/keys/other.key")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SecretsKeyFile != "/run/keys/other.key" {
		t.Fatalf("env override secrets_key_file = %q, want /run/keys/other.key", cfg.SecretsKeyFile)
	}
}

// TestConfigSecretsKeyEnvValueWinsOverFile is the precedence crux (design §2):
// with BOTH a secrets_key_file (a valid key file) AND the BIRDMAN_SECRETS_KEY
// dev env VALUE set, SecretsKey() must resolve to the ENV value and must NOT
// raise LoadKey's both-sources ambiguity error — a dev override layered over a
// shipped default path is the normal dev case, not a misconfiguration.
func TestConfigSecretsKeyEnvValueWinsOverFile(t *testing.T) {
	fileKey := bytes.Repeat([]byte{0x11}, 32)
	envKey := bytes.Repeat([]byte{0x22}, 32)
	path := writeKeyFile(t, fileKey)

	cfg := Config{SecretsKeyFile: path}
	t.Setenv("BIRDMAN_SECRETS_KEY", base64.StdEncoding.EncodeToString(envKey))

	got, err := cfg.SecretsKey()
	if err != nil {
		t.Fatalf("SecretsKey with both sources must NOT be ambiguous (env wins): %v", err)
	}
	if !bytes.Equal(got, envKey) {
		t.Fatal("SecretsKey returned the file key; the env value must win as a dev override")
	}
	// And a file is then NOT in use (no mode WARN to do).
	if _, inUse := cfg.SecretsKeyFileInUse(); inUse {
		t.Fatal("SecretsKeyFileInUse must be false when the env value is the source")
	}
}

// With no env value, the file is the single source and is reported in use.
func TestConfigSecretsKeyFromFile(t *testing.T) {
	fileKey := bytes.Repeat([]byte{0x33}, 32)
	path := writeKeyFile(t, fileKey)
	cfg := Config{SecretsKeyFile: path}

	got, err := cfg.SecretsKey()
	if err != nil {
		t.Fatalf("SecretsKey(file): %v", err)
	}
	if !bytes.Equal(got, fileKey) {
		t.Fatal("SecretsKey returned the wrong key from the file")
	}
	if p, inUse := cfg.SecretsKeyFileInUse(); !inUse || p != path {
		t.Fatalf("SecretsKeyFileInUse = (%q, %v), want (%q, true)", p, inUse, path)
	}
}

// No source at all → fail loud (master must not start).
func TestConfigSecretsKeyMissing(t *testing.T) {
	cfg := Config{}
	if _, err := cfg.SecretsKey(); err == nil {
		t.Fatal("SecretsKey with neither a file nor the env value must error")
	}
}

// Iteration-5 follow-up (ревизия): node_down_after_min bounds how long a
// quarantined node stays silent before it is marked down (NOT dead — dead is
// the manual revocation terminal). Defaults to 10 (0/missing → default),
// rejects < 1.
func TestLoadNodeDownAfterMinDefault(t *testing.T) {
	withDSN(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NodeDownAfterMin != 10 {
		t.Fatalf("default node_down_after_min = %d, want 10", cfg.NodeDownAfterMin)
	}
}

func TestLoadNodeDownAfterMinZeroDefaults(t *testing.T) {
	withDSN(t)
	path := writeConfig(t, "node_down_after_min: 0\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NodeDownAfterMin != 10 {
		t.Fatalf("node_down_after_min: 0 → %d, want default 10", cfg.NodeDownAfterMin)
	}
}

func TestLoadNodeDownAfterMinExplicit(t *testing.T) {
	withDSN(t)
	path := writeConfig(t, "node_down_after_min: 5\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NodeDownAfterMin != 5 {
		t.Fatalf("node_down_after_min = %d, want 5", cfg.NodeDownAfterMin)
	}
}

func TestLoadNodeDownAfterMinRejectsBelowOne(t *testing.T) {
	withDSN(t)
	path := writeConfig(t, "node_down_after_min: -1\n")
	if _, err := Load(path); err == nil {
		t.Fatal("Load must reject node_down_after_min < 1")
	}
}

// mute → alertmanager silence mirroring (tracker #245): alertmanager_url
// defaults to the same-box AM, is read from the file, overridden by
// BIRDMAN_ALERTMANAGER_URL, and an explicit "" disables mirroring (yaml.v3
// overwrites the default with the empty string).
func TestLoadAlertmanagerURLDefault(t *testing.T) {
	withDSN(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Alerts.AlertmanagerURL != "http://127.0.0.1:9093" {
		t.Fatalf("default alertmanager_url = %q, want http://127.0.0.1:9093", cfg.Alerts.AlertmanagerURL)
	}
}

func TestLoadAlertmanagerURLFromFile(t *testing.T) {
	withDSN(t)
	path := writeConfig(t, "alerts:\n  alertmanager_url: \"http://am.internal:9093\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Alerts.AlertmanagerURL != "http://am.internal:9093" {
		t.Fatalf("alertmanager_url from file = %q", cfg.Alerts.AlertmanagerURL)
	}
}

func TestLoadAlertmanagerURLEnvOverride(t *testing.T) {
	withDSN(t)
	path := writeConfig(t, "alerts:\n  alertmanager_url: \"http://am.internal:9093\"\n")
	t.Setenv("BIRDMAN_ALERTMANAGER_URL", "http://other:9093")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Alerts.AlertmanagerURL != "http://other:9093" {
		t.Fatalf("env override alertmanager_url = %q, want http://other:9093", cfg.Alerts.AlertmanagerURL)
	}
}

// An explicit empty string in the file disables mirroring — the self-host case
// without a monitoring stack. yaml.v3 overwrites the default with "".
func TestLoadAlertmanagerURLExplicitEmptyDisables(t *testing.T) {
	withDSN(t)
	path := writeConfig(t, "alerts:\n  alertmanager_url: \"\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Alerts.AlertmanagerURL != "" {
		t.Fatalf("explicit empty alertmanager_url = %q, want \"\" (mirroring disabled)", cfg.Alerts.AlertmanagerURL)
	}
}

// listen_metrics (tracker #1003): прометеевская экспозиция уехала с API-порта
// на свой адрес. Дефолт — loopback, и это ДЕФОЛТ КОДА, а не рекомендация
// развёртывания: реестр перечисляет проекты, окружения и живые server_id.
func TestLoadListenMetricsDefaultsToLoopback(t *testing.T) {
	withDSN(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenMetrics != "127.0.0.1:9102" {
		t.Fatalf("listen_metrics = %q, want 127.0.0.1:9102", cfg.ListenMetrics)
	}
	// 9101 занят агентом на том же боксе — совпадение сделало бы дефолт
	// неработающим ровно в рекомендованной раскладке.
	if cfg.ListenMetrics == "127.0.0.1:9101" {
		t.Fatal("дефолт метрик совпал с портом агента")
	}
}

func TestLoadListenMetricsFromFileAndEnv(t *testing.T) {
	withDSN(t)
	path := writeConfig(t, "listen_metrics: \"127.0.0.1:9999\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenMetrics != "127.0.0.1:9999" {
		t.Fatalf("listen_metrics из файла = %q, want 127.0.0.1:9999", cfg.ListenMetrics)
	}
	t.Setenv("BIRDMAN_LISTEN_METRICS", "127.0.0.1:9998")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenMetrics != "127.0.0.1:9998" {
		t.Fatalf("env-override listen_metrics = %q, want 127.0.0.1:9998", cfg.ListenMetrics)
	}
}

// ЯВНЫЙ пустой listen_metrics выключает экспозицию совсем (у оператора нет
// скрейпера — незачем держать порт). Отсутствие ключа при этом даёт ДЕФОЛТ, а
// не выключение: апгрейд со старым конфигом не имеет права молча остаться без
// метрик, иначе вместе с ними молча умрут алерты.
func TestLoadListenMetricsExplicitEmptyDisables(t *testing.T) {
	withDSN(t)
	path := writeConfig(t, "listen_metrics: \"\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenMetrics != "" {
		t.Fatalf("явный пустой listen_metrics = %q, want \"\" (экспозиция выключена)", cfg.ListenMetrics)
	}
	old := writeConfig(t, "listen_grpc: \":8444\"\n")
	cfg, err = Load(old)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenMetrics != "127.0.0.1:9102" {
		t.Fatalf("конфиг без ключа: listen_metrics = %q, want дефолт 127.0.0.1:9102", cfg.ListenMetrics)
	}
}

func writeKeyFile(t *testing.T, key []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.key")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
