package register

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/sshtunnel"
	"github.com/serverroom/gpu-marketplace/internal/stats"
	"github.com/serverroom/gpu-marketplace/internal/tunnel"
)

// RegisterRequest is the payload the agent sends to register its hardware. The
// one-time code is the credential; the agent's public key is its tunnel identity.
type RegisterRequest struct {
	Code           string      `json:"code"`
	AgentPubkey    string      `json:"agent_pubkey"`
	Specs          interface{} `json:"specs"`
	GPUModels      []string    `json:"gpu_models"`
	Reachability   string      `json:"reachability"`
	PreferredRelay string      `json:"preferred_relay,omitempty"`
}

// DecodeCode unpacks the registration code into the POST URL and the inner
// one-time secret. The URL travels inside the code, so the agent hardcodes no
// endpoint and this repo references no brand site.
func DecodeCode(encoded string) (postURL, secret string, err error) {
	raw, derr := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
	if derr != nil {
		return "", "", fmt.Errorf("invalid code: %w", derr)
	}
	var p struct {
		URL    string `json:"u"`
		Secret string `json:"c"`
	}
	if jerr := json.Unmarshal(raw, &p); jerr != nil {
		return "", "", fmt.Errorf("invalid code: %w", jerr)
	}
	if p.URL == "" || p.Secret == "" {
		return "", "", fmt.Errorf("invalid code: missing fields")
	}
	return p.URL, p.Secret, nil
}

// TunnelInfo is the relay connection the control plane assigns at register time.
type TunnelInfo struct {
	RelayHost    string `json:"relay_host"`
	RelayPort    int    `json:"relay_port"`
	RelayUser    string `json:"relay_user"`
	ControlSlot  int    `json:"control_slot"`
	SSHSlot      int    `json:"ssh_slot"`
	RelayHostkey string `json:"relay_hostkey"`
}

// RegisterResponse is what the control plane returns on a successful register.
type RegisterResponse struct {
	ListingID    string      `json:"listing_id"`
	Status       string      `json:"status"`
	Tunnel       *TunnelInfo `json:"tunnel,omitempty"`
	ControlToken string      `json:"control_token,omitempty"`
}

// Local ports the relay forwards expose back over the tunnel: the agent control
// channel and the active rental's microVM SSH (set up by the provisioner).
const (
	AgentControlPort = 9101
	MicroVMSSHPort   = 2222
)

// GenerateKey creates an ed25519 SSH keypair at keyPath via ssh-keygen and
// returns the public-key line. The private key is the agent's tunnel identity
// and never leaves the box.
func GenerateKey(keyPath string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		return "", err
	}
	os.Remove(keyPath)
	os.Remove(keyPath + ".pub")
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-f", keyPath, "-N", "", "-C", "gpu-agent", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("ssh-keygen: %w: %s", err, out)
	}
	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(pub)), nil
}

// Submit POSTs the registration to the control plane (via the relay in
// production) and returns the listing it created.
func Submit(postURL string, req RegisterRequest) (*RegisterResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequest("POST", postURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("register failed (%d): %s", resp.StatusCode, respBody)
	}
	var rr RegisterResponse
	if err := json.Unmarshal(respBody, &rr); err != nil {
		return nil, err
	}
	return &rr, nil
}

// Run executes the full registration flow: let the user pick a relay (closest
// preselected), generate the agent keypair, collect specs, register, and
// persist the result.
func Run(code string) error {
	fmt.Println("GPU Marketplace Agent - register")
	fmt.Println("================================")

	postURL, secret, err := DecodeCode(code)
	if err != nil {
		return err
	}

	// Pick the relay before registering, so a probe hiccup never strands a
	// server-side registration. The choice is a preference; the control plane
	// has the final word via the tunnel info in its response.
	hubs := loadHubs()
	hub, err := chooseHub(hubs)
	if err != nil {
		return err
	}

	keyPath := filepath.Join(config.ConfigDir(), "agent_ed25519")
	fmt.Println("Generating agent SSH key...")
	pub, err := GenerateKey(keyPath)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}

	st, err := stats.Collect()
	if err != nil {
		return fmt.Errorf("collect specs: %w", err)
	}

	req := RegisterRequest{
		Code:           secret,
		AgentPubkey:    pub,
		Specs:          st,
		GPUModels:      gpuModels(st),
		Reachability:   "unknown",
		PreferredRelay: hub.Name,
	}

	// The POST endpoint is decoded from the code (which also carries the owner's
	// account, resolved server-side), so the agent hardcodes no URL.
	fmt.Println("Registering ...")
	resp, err := Submit(postURL, req)
	if err != nil {
		return err
	}

	if err := saveRegistration(resp.ListingID, hub.Name, keyPath); err != nil {
		return fmt.Errorf("save registration: %w", err)
	}
	if resp.Tunnel != nil {
		if err := saveTunnelConfig(resp.Tunnel, keyPath); err != nil {
			return fmt.Errorf("save tunnel config: %w", err)
		}
		fmt.Println("Tunnel config saved; the agent service keeps the reverse tunnel up.")
	}
	if resp.ControlToken != "" {
		if err := saveControlToken(resp.ControlToken); err != nil {
			return fmt.Errorf("save control token: %w", err)
		}
	}

	fmt.Printf("\nRegistered! listing_id=%s status=%s relay=%s\n", resp.ListingID, resp.Status, hub.Name)
	fmt.Printf("Agent key: %s\n", keyPath)
	return nil
}

// loadHubs returns the relay list from config.yaml when present, otherwise the
// built-in defaults.
func loadHubs() []config.Hub {
	if cfg, err := config.Load(); err == nil && len(cfg.Hubs) > 0 {
		return cfg.Hubs
	}
	return config.DefaultConfig().Hubs
}

// chooseHub probes all relays, shows them closest-first, and lets the user pick
// one. Enter accepts the preselected closest relay. An unreachable probe is not
// fatal: the probe port can be filtered locally while the tunnel still works.
func chooseHub(hubs []config.Hub) (*config.Hub, error) {
	if len(hubs) == 0 {
		return nil, fmt.Errorf("no relays configured")
	}

	fmt.Println("Testing latency to relays...")
	results := tunnel.TestAllHubs(hubs)

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Success != results[j].Success {
			return results[i].Success
		}
		return results[i].AvgMs < results[j].AvgMs
	})

	fmt.Println()
	fmt.Println("Available relays (closest first):")
	for i, r := range results {
		if r.Success {
			fmt.Printf("  %d) %-12s %.1f ms\n", i+1, r.Hub.Name, r.AvgMs)
		} else {
			fmt.Printf("  %d) %-12s unreachable\n", i+1, r.Hub.Name)
		}
	}
	if !results[0].Success {
		fmt.Println("Warning: no relay responded to the latency probe; you can still pick one.")
	}

	fmt.Printf("Select relay [1]: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)

	choice := 1
	if line != "" {
		n, err := strconv.Atoi(line)
		if err != nil || n < 1 || n > len(results) {
			return nil, fmt.Errorf("invalid relay choice %q", line)
		}
		choice = n
	}

	sel := results[choice-1]
	if !sel.Success {
		fmt.Printf("Note: relay %s did not respond to the probe; continuing anyway.\n", sel.Hub.Name)
	}
	fmt.Printf("Using relay %s (%s)\n", sel.Hub.Name, sel.Hub.Host)
	return &sel.Hub, nil
}

func gpuModels(st *stats.SystemStats) []string {
	models := make([]string, 0, len(st.GPUs))
	for _, g := range st.GPUs {
		models = append(models, g.Model)
	}
	return models
}

func saveRegistration(listingID, hub, keyPath string) error {
	dir := config.ConfigDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(map[string]string{
		"listing_id": listingID,
		"hub":        hub,
		"key_path":   keyPath,
	}, "", "  ")
	return os.WriteFile(filepath.Join(dir, "registration.json"), data, 0600)
}

// TunnelConfigPath is where the reverse-tunnel config is persisted.
func TunnelConfigPath() string {
	return filepath.Join(config.ConfigDir(), "tunnel.json")
}

func saveTunnelConfig(t *TunnelInfo, keyPath string) error {
	dir := config.ConfigDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	knownHosts := ""
	if t.RelayHostkey != "" {
		knownHosts = filepath.Join(dir, "relay_known_hosts")
		line := fmt.Sprintf("[%s]:%d %s\n", t.RelayHost, t.RelayPort, t.RelayHostkey)
		if err := os.WriteFile(knownHosts, []byte(line), 0644); err != nil {
			return err
		}
	}

	cfg := sshtunnel.Config{
		RelayHost:      t.RelayHost,
		RelayPort:      t.RelayPort,
		RelayUser:      t.RelayUser,
		IdentityFile:   keyPath,
		KnownHostsFile: knownHosts,
		Forwards: []sshtunnel.Forward{
			{RelayBindPort: t.ControlSlot, TargetPort: AgentControlPort},
			{RelayBindPort: t.SSHSlot, TargetPort: MicroVMSSHPort},
		},
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(TunnelConfigPath(), data, 0600)
}

// LoadTunnelConfig reads the persisted reverse-tunnel config, or returns nil if
// the agent has not registered a tunnel yet.
func LoadTunnelConfig() (*sshtunnel.Config, error) {
	data, err := os.ReadFile(TunnelConfigPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg sshtunnel.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ControlTokenPath is where the control-channel bearer token is persisted.
func ControlTokenPath() string {
	return filepath.Join(config.ConfigDir(), "control.token")
}

func saveControlToken(token string) error {
	dir := config.ConfigDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(ControlTokenPath(), []byte(token), 0600)
}

// LoadControlToken returns the saved control-channel token, or "" if none.
func LoadControlToken() string {
	data, err := os.ReadFile(ControlTokenPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
