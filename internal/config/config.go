package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

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
	ListenPort int   `yaml:"listen_port"`
	Hubs       []Hub `yaml:"hubs"`
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

// locationLabels maps a relay key to the city a provider recognises. The key is
// the WIRE value — the control plane matches on it in SelectRelay and it is what
// gets persisted in registration.json — so these are display only and must never
// be substituted into a request.
var locationLabels = map[string]string{
	"nyc":       "New York",
	"miami":     "Miami",
	"sf":        "San Francisco",
	"amsterdam": "Amsterdam",
	"bucharest": "Bucharest",
}

// LocationLabel returns the display name for a relay key. An unrecognised key is
// title-cased rather than dropped, so a relay the control plane adds later still
// reads properly without shipping a new agent. Anything that does not look like a
// plain key is passed through untouched — Registration.Hub holds a bare hostname
// on the pre-assigned-tunnel path, and "162.244.81.236" must not become
// "162.244.81.236" title-cased into nonsense.
func LocationLabel(key string) string {
	k := strings.ToLower(strings.TrimSpace(key))
	if label, ok := locationLabels[k]; ok {
		return label
	}
	if k == "" || strings.ContainsAny(k, ".:/") {
		return key
	}
	parts := strings.FieldsFunc(k, func(r rune) bool { return r == '-' || r == '_' || r == ' ' })
	for i, part := range parts {
		r := []rune(part)
		parts[i] = strings.ToUpper(string(r[0])) + string(r[1:])
	}
	return strings.Join(parts, " ")
}

// DefaultConfig returns a config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		ListenPort: 9100,
		Hubs:       DefaultHubs(),
	}
}

// ConfigDirEnv overrides the platform config directory. It exists so the agent
// can be pointed at a scratch directory by tests and dry runs; a real install
// never sets it, and a root daemon never inherits it from a local user.
const ConfigDirEnv = "GPU_AGENT_CONFIG_DIR"

// ConfigDir returns the platform-specific config directory.
func ConfigDir() string {
	if dir := os.Getenv(ConfigDirEnv); dir != "" {
		return dir
	}
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("ProgramData"), "gpu-agent")
	case "darwin":
		return "/Library/Application Support/gpu-agent"
	default:
		return "/etc/gpu-agent"
	}
}

// DataDir is where rental disks and the rental base image live. It is kept
// apart from ConfigDir on Linux so the agent's credentials and a tenant's disk
// never share a directory; `gpu-agent remove` deletes both.
func DataDir() string {
	if os.Getenv(ConfigDirEnv) != "" || runtime.GOOS != "linux" {
		return filepath.Join(ConfigDir(), "data")
	}
	return "/var/lib/gpu-agent"
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

// StoragePath is where a storage directory chosen with `gpu-agent setup
// --data-dir` is recorded. A file of its own, not config.yaml, whose presence
// starts the legacy stats server.
func StoragePath() string { return filepath.Join(ConfigDir(), "storage.json") }

// StorageDir is where the big files live -- the rental base image and the
// rentals' disks: the directory `gpu-agent setup --data-dir` recorded, else
// DataDir. The agent's state files stay in DataDir either way.
func StorageDir() string {
	data, err := os.ReadFile(StoragePath())
	if err != nil {
		return DataDir()
	}
	var rec struct {
		DataDir string `json:"data_dir"`
	}
	if json.Unmarshal(data, &rec) != nil || !filepath.IsAbs(rec.DataDir) {
		return DataDir()
	}
	return filepath.Clean(rec.DataDir)
}

// SetStorageDir records dir as the storage directory ("" or DataDir forgets it).
func SetStorageDir(dir string) error {
	if dir == "" || filepath.Clean(dir) == DataDir() {
		if err := os.Remove(StoragePath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(ConfigDir(), 0755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(map[string]string{"data_dir": filepath.Clean(dir)}, "", "  ")
	return os.WriteFile(StoragePath(), data, 0600)
}
