package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

// Hub represents a hub server endpoint.
type Hub struct {
	Name string `yaml:"name"`
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// WireGuard holds the local WireGuard tunnel configuration.
type WireGuard struct {
	PrivateKey string `yaml:"private_key"`
	Address    string `yaml:"address"`
	HubPubKey  string `yaml:"hub_public_key"`
	Endpoint   string `yaml:"endpoint"`
}

// Config is the agent configuration.
type Config struct {
	PeerID    string    `yaml:"peer_id"`
	HubName   string    `yaml:"hub_name"`
	ListenPort int      `yaml:"listen_port"`
	WireGuard WireGuard `yaml:"wireguard"`
	Hubs      []Hub     `yaml:"hubs"`
}

// DefaultHubs returns the built-in list of hub servers.
func DefaultHubs() []Hub {
	return []Hub{
		{Name: "US-36", Host: "hub36.serverroom.net", Port: 51820},
		{Name: "US-50", Host: "hub50.serverroom.net", Port: 51820},
		{Name: "US-60", Host: "hub60.serverroom.net", Port: 51820},
		{Name: "US-61", Host: "hub61.serverroom.net", Port: 51820},
		{Name: "EU-72", Host: "hub72.serverroom.net", Port: 51820},
	}
}

// DefaultConfig returns a config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		ListenPort: 9100,
		Hubs:       DefaultHubs(),
	}
}

// ConfigDir returns the platform-specific config directory.
func ConfigDir() string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("ProgramData"), "gpu-agent")
	case "darwin":
		return "/Library/Application Support/gpu-agent"
	default:
		return "/etc/gpu-agent"
	}
}

// ConfigPath returns the full path to the config file.
func ConfigPath() string {
	return filepath.Join(ConfigDir(), "config.yaml")
}

// Load reads the config from disk.
func Load() (*Config, error) {
	path := ConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// Save writes the config to disk.
func Save(cfg *Config) error {
	dir := ConfigDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	path := ConfigPath()
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
