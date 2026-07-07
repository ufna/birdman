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

type Config struct {
	DSN        string `yaml:"dsn"`
	ListenAPI  string `yaml:"listen_api"`
	ListenGRPC string `yaml:"listen_grpc"`
	TLS        TLS    `yaml:"tls"`
}

func defaults() Config {
	return Config{
		ListenAPI:  ":8100",
		ListenGRPC: ":8444",
		TLS:        TLS{AutoCertDir: "certs"},
	}
}

// Load reads the YAML config at path (optional — pass "" to use defaults)
// and applies environment overrides: BIRDMAN_DSN, BIRDMAN_LISTEN_API,
// BIRDMAN_LISTEN_GRPC.
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
	if cfg.ListenAPI == "" {
		cfg.ListenAPI = ":8100"
	}
	if cfg.ListenGRPC == "" {
		cfg.ListenGRPC = ":8444"
	}
	if cfg.DSN == "" {
		return cfg, fmt.Errorf("dsn is required (config `dsn` or env BIRDMAN_DSN)")
	}
	return cfg, nil
}
