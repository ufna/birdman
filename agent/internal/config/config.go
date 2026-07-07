// Package config loads the agent YAML config (docs/specs/agent.md §10).
//
// v0 parses the run-once subset. Unknown keys (master_addr, node_token, …)
// are ignored on purpose: the full spec config from ansible must stay
// loadable by a v0 agent.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Limits are per-server cgroup limits.
type Limits struct {
	CPUMillis int `yaml:"cpu_millis"`
	MemMB     int `yaml:"mem_mb"`
}

// RegistryAuth configures pulls from a private registry (GHCR).
// The token itself lives only in TokenFile — never in the config or code.
type RegistryAuth struct {
	Username  string `yaml:"username"`
	TokenFile string `yaml:"token_file"`
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
type Config struct {
	Region        string        `yaml:"region"`
	CapacitySlots int           `yaml:"capacity_slots"`
	PortRange     []int         `yaml:"port_range"`
	LimitsDefault Limits        `yaml:"limits_default"`
	LogDir        string        `yaml:"log_dir"`
	DataDir       string        `yaml:"data_dir"`
	RegistryAuth  *RegistryAuth `yaml:"registry_auth"`
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
	}
	return nil
}
