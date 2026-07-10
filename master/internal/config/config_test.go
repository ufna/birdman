package config

import (
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

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
