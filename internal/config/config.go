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

// Config is the agent configuration.
type Config struct {
	PeerID    string    `yaml:"peer_id"`
	HubName   string    `yaml:"hub_name"`
	ListenPort int      `yaml:"listen_port"`
	Hubs      []Hub     `yaml:"hubs"`
}

// DefaultHubs returns the built-in list of relay POPs. Port 443 is the latency
// probe target (always reachable); the actual tunnel endpoint and port are
// assigned by the control plane at register time.
func DefaultHubs() []Hub {
	return []Hub{
		{Name: "nyc", Host: "162.244.81.236", Port: 443},
		{Name: "bucharest", Host: "89.39.149.246", Port: 443},
		{Name: "miami", Host: "38.126.208.235", Port: 443},
		{Name: "amsterdam", Host: "209.127.202.254", Port: 443},
		{Name: "sf", Host: "198.145.121.234", Port: 443},
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
