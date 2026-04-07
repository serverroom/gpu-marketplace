package tunnel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"text/template"

	"github.com/serverroom/gpu-marketplace/internal/config"
)

const wgConfigTemplate = `[Interface]
PrivateKey = {{.PrivateKey}}
Address = {{.Address}}/24

[Peer]
PublicKey = {{.HubPubKey}}
Endpoint = {{.Endpoint}}
AllowedIPs = 10.99.0.0/16
PersistentKeepalive = 25
`

// RegistrationRequest is sent to the hub to register a new peer.
type RegistrationRequest struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
}

// RegistrationResponse is returned by the hub after registration.
type RegistrationResponse struct {
	PeerID     string `json:"peer_id"`
	PrivateKey string `json:"private_key"`
	Address    string `json:"address"`
	HubPubKey  string `json:"hub_public_key"`
	Endpoint   string `json:"endpoint"`
}

// Register contacts the hub to register this agent and get WireGuard config.
func Register(hub *config.Hub) (*RegistrationResponse, error) {
	hostname, _ := os.Hostname()
	reqBody := RegistrationRequest{
		Hostname: hostname,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
	}

	data, _ := json.Marshal(reqBody)
	url := fmt.Sprintf("http://%s:%d/register", hub.Host, hub.Port+1) // Registration API on port+1
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("register with hub %s: %w", hub.Name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("hub registration returned %d", resp.StatusCode)
	}

	var reg RegistrationResponse
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil {
		return nil, fmt.Errorf("decode registration response: %w", err)
	}

	return &reg, nil
}

// WriteConfig writes the WireGuard configuration file.
func WriteConfig(reg *RegistrationResponse) error {
	tmpl, err := template.New("wg").Parse(wgConfigTemplate)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, reg); err != nil {
		return err
	}

	path := wgConfigPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create wireguard config dir: %w", err)
	}

	if err := os.WriteFile(path, buf.Bytes(), 0600); err != nil {
		return fmt.Errorf("write wireguard config: %w", err)
	}

	fmt.Printf("WireGuard config written to %s\n", path)
	return nil
}

// BringUp starts the WireGuard interface.
func BringUp() error {
	switch runtime.GOOS {
	case "linux":
		return exec.Command("wg-quick", "up", "wg-gpu").Run()
	case "darwin":
		return exec.Command("wg-quick", "up", "wg-gpu").Run()
	case "windows":
		// Windows WireGuard service manages tunnels
		configPath := wgConfigPath()
		return exec.Command("wireguard.exe", "/installtunnelservice", configPath).Run()
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

// BringDown stops the WireGuard interface.
func BringDown() error {
	switch runtime.GOOS {
	case "linux", "darwin":
		return exec.Command("wg-quick", "down", "wg-gpu").Run()
	case "windows":
		return exec.Command("wireguard.exe", "/uninstalltunnelservice", "wg-gpu").Run()
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

// IsUp checks if the WireGuard tunnel is active.
func IsUp() bool {
	switch runtime.GOOS {
	case "linux", "darwin":
		err := exec.Command("wg", "show", "wg-gpu").Run()
		return err == nil
	case "windows":
		// Check if the tunnel service is running
		err := exec.Command("sc", "query", "WireGuardTunnel$wg-gpu").Run()
		return err == nil
	default:
		return false
	}
}

func wgConfigPath() string {
	switch runtime.GOOS {
	case "windows":
		return `C:\Program Files\WireGuard\Data\Configurations\wg-gpu.conf`
	case "darwin":
		return "/etc/wireguard/wg-gpu.conf"
	default:
		return "/etc/wireguard/wg-gpu.conf"
	}
}
