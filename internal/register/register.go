package register

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
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
// No relay preference here: the code resolves to an account (and with it the
// brand and its relay set) only server-side, so relay choice happens after.
type RegisterRequest struct {
	Code         string      `json:"code"`
	AgentPubkey  string      `json:"agent_pubkey"`
	Specs        interface{} `json:"specs"`
	GPUModels    []string    `json:"gpu_models"`
	Reachability string      `json:"reachability"`
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

// RelayOption is one relay the control plane offers for this account's brand.
type RelayOption struct {
	Name string `json:"name"`
	Host string `json:"host"`
	Port int    `json:"port"`
}

// RegisterResponse is what the control plane returns on a successful register.
// Either it pre-assigns the tunnel directly, or it returns the brand's relay
// list plus a select_url for the agent to report the user's choice to.
type RegisterResponse struct {
	ListingID    string        `json:"listing_id"`
	Status       string        `json:"status"`
	Relays       []RelayOption `json:"relays,omitempty"`
	SelectURL    string        `json:"select_url,omitempty"`
	Tunnel       *TunnelInfo   `json:"tunnel,omitempty"`
	ControlToken string        `json:"control_token,omitempty"`
}

// SelectRelayRequest tells the control plane which relay the provider picked.
type SelectRelayRequest struct {
	ListingID string `json:"listing_id"`
	Relay     string `json:"relay"`
}

// SelectRelayResponse returns the tunnel allocation on the chosen relay.
type SelectRelayResponse struct {
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

// Run executes the full registration flow: generate the agent keypair, collect
// specs, register, then pick a relay from the brand's list the control plane
// returned (closest preselected) and report the choice back for slot allocation.
func Run(code string) error {
	fmt.Println("GPU Marketplace Agent - register")
	fmt.Println("================================")

	postURL, secret, err := DecodeCode(code)
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
		Code:         secret,
		AgentPubkey:  pub,
		Specs:        st,
		GPUModels:    gpuModels(st),
		Reachability: "unknown",
	}

	// The POST endpoint is decoded from the code (which also carries the owner's
	// account, resolved server-side), so the agent hardcodes no URL.
	fmt.Println("Registering ...")
	resp, err := Submit(postURL, req)
	if err != nil {
		return err
	}

	// The one-time code is consumed now — persist everything needed to finish
	// (or later retry) the remaining steps before anything else can fail.
	reg := Registration{
		ListingID: resp.ListingID,
		KeyPath:   keyPath,
		SelectURL: resp.SelectURL,
		Relays:    resp.Relays,
	}
	if err := saveRegistration(reg); err != nil {
		return fmt.Errorf("save registration: %w", err)
	}
	if resp.ControlToken != "" {
		if err := saveControlToken(resp.ControlToken); err != nil {
			return fmt.Errorf("save control token: %w", err)
		}
	}

	tun := resp.Tunnel
	switch {
	case tun != nil:
		reg.Hub = tun.RelayHost
	case resp.SelectURL == "":
		// Nothing to choose and nowhere to send a choice: don't prompt (the
		// answer would go nowhere) and don't pretend the agent is connected.
		fmt.Println()
		fmt.Println("Warning: the marketplace assigned no tunnel and offered no location choice.")
		fmt.Println("The listing is registered, but this agent stays offline until it gets one.")
	default:
		// The account's brand — and with it the relay set — was resolved
		// server-side from the code, so the location choice happens only now.
		hub, herr := chooseHub(relayOptions(resp))
		if herr != nil {
			return fmt.Errorf("registered (listing %s) but location selection failed: %w\nRun 'gpu-agent select-location' to retry without a new code", resp.ListingID, herr)
		}
		reg.Hub = hub.Name

		sresp, serr := selectRelayWithRetry(resp.SelectURL, resp.ControlToken, resp.ListingID, hub.Name)
		if serr != nil {
			saveRegistration(reg) // keep the choice for the retry path
			return fmt.Errorf("registered (listing %s) but location assignment failed: %w\nRun 'gpu-agent select-location' to retry without a new code", resp.ListingID, serr)
		}
		if sresp.ControlToken != "" {
			if err := saveControlToken(sresp.ControlToken); err != nil {
				return fmt.Errorf("save control token: %w", err)
			}
		}
		if sresp.Tunnel == nil {
			saveRegistration(reg)
			return fmt.Errorf("registered (listing %s) but the marketplace returned no tunnel for location %s\nRun 'gpu-agent select-location' to retry without a new code", resp.ListingID, hub.Name)
		}
		tun = sresp.Tunnel
	}

	if err := saveRegistration(reg); err != nil {
		return fmt.Errorf("save registration: %w", err)
	}
	if tun != nil {
		if err := saveTunnelConfig(tun, keyPath); err != nil {
			return fmt.Errorf("save tunnel config: %w", err)
		}
		fmt.Println("Tunnel config saved; the agent service keeps the reverse tunnel up.")
	}

	location := reg.Hub
	if location == "" {
		location = "unassigned"
	}
	fmt.Printf("\nRegistered! listing_id=%s status=%s location=%s\n", resp.ListingID, resp.Status, location)
	fmt.Printf("Agent key: %s\n", keyPath)
	fmt.Println()
	if tun != nil {
		fmt.Println("Link established. Continue the setup on your dashboard.")
	} else {
		fmt.Println("Continue the setup on your dashboard.")
	}
	return nil
}

// RetrySelection re-runs the location choice and assignment from the persisted
// registration state — the recovery path when register was interrupted after
// the one-time code was already consumed.
func RetrySelection() error {
	reg, err := LoadRegistration()
	if err != nil {
		return fmt.Errorf("no registration found (run 'gpu-agent register' first): %w", err)
	}
	if reg.SelectURL == "" {
		return fmt.Errorf("this registration has no location-selection endpoint; generate a new code and re-register")
	}

	hubs := hubsFromRelayOptions(reg.Relays)
	if len(hubs) == 0 {
		hubs = loadHubs()
	}
	hub, err := chooseHub(hubs)
	if err != nil {
		return err
	}
	reg.Hub = hub.Name

	sresp, err := selectRelayWithRetry(reg.SelectURL, LoadControlToken(), reg.ListingID, hub.Name)
	if err != nil {
		return fmt.Errorf("location assignment failed: %w", err)
	}
	if sresp.ControlToken != "" {
		if err := saveControlToken(sresp.ControlToken); err != nil {
			return fmt.Errorf("save control token: %w", err)
		}
	}
	if sresp.Tunnel == nil {
		return fmt.Errorf("the marketplace returned no tunnel for location %s", hub.Name)
	}
	if err := saveRegistration(*reg); err != nil {
		return fmt.Errorf("save registration: %w", err)
	}
	if err := saveTunnelConfig(sresp.Tunnel, reg.KeyPath); err != nil {
		return fmt.Errorf("save tunnel config: %w", err)
	}
	fmt.Printf("Location %s assigned; tunnel config saved. Restart the agent service to connect.\n", hub.Name)
	return nil
}

// SelectRelay reports the provider's relay choice to the control plane, which
// allocates tunnel slots on that relay and returns the connection info.
func SelectRelay(selectURL, bearer, listingID, relayName string) (*SelectRelayResponse, error) {
	body, err := json.Marshal(SelectRelayRequest{ListingID: listingID, Relay: relayName})
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequest("POST", selectURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		httpReq.Header.Set("Authorization", "Bearer "+bearer)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, &StatusError{Code: resp.StatusCode, Body: string(respBody)}
	}
	var sr SelectRelayResponse
	if err := json.Unmarshal(respBody, &sr); err != nil {
		return nil, err
	}
	return &sr, nil
}

// StatusError is a non-200 control-plane reply; 4xx will not heal on retry.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("location select failed (%d): %s", e.Code, e.Body)
}

// selectRelayWithRetry retries transient failures (network errors, 5xx) a few
// times before giving up; 4xx replies fail immediately.
func selectRelayWithRetry(selectURL, bearer, listingID, relayName string) (*SelectRelayResponse, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		resp, err := SelectRelay(selectURL, bearer, listingID, relayName)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		var se *StatusError
		if errors.As(err, &se) && se.Code >= 400 && se.Code < 500 {
			break
		}
		if attempt < 3 {
			fmt.Printf("Location assignment attempt %d failed (%v); retrying...\n", attempt, err)
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
	}
	return nil, lastErr
}

// stdinIsTerminal reports whether stdin is an interactive console (vs a pipe),
// so invalid piped input falls back to the default instead of re-prompting.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func hubsFromRelayOptions(relays []RelayOption) []config.Hub {
	hubs := make([]config.Hub, 0, len(relays))
	for _, r := range relays {
		hubs = append(hubs, config.Hub{Name: r.Name, Host: r.Host, Port: r.Port})
	}
	return hubs
}

// relayOptions converts the control plane's relay list for probing, falling
// back to config.yaml / built-in defaults when the response carries none.
func relayOptions(resp *RegisterResponse) []config.Hub {
	if len(resp.Relays) > 0 {
		return hubsFromRelayOptions(resp.Relays)
	}
	return loadHubs()
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
// one. Enter accepts the preselected closest one. An unreachable probe is not
// fatal: the probe port can be filtered locally while the tunnel still works.
// User-facing wording says "location" — relays are an infrastructure detail.
func chooseHub(hubs []config.Hub) (*config.Hub, error) {
	if len(hubs) == 0 {
		return nil, fmt.Errorf("no locations available")
	}

	fmt.Println("Testing latency to available locations...")
	results := tunnel.TestAllHubs(hubs)

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Success != results[j].Success {
			return results[i].Success
		}
		return results[i].AvgMs < results[j].AvgMs
	})

	fmt.Println()
	fmt.Println("Available locations (closest first):")
	for i, r := range results {
		if r.Success {
			fmt.Printf("  %d) %-12s %.1f ms\n", i+1, r.Hub.Name, r.AvgMs)
		} else {
			fmt.Printf("  %d) %-12s unreachable\n", i+1, r.Hub.Name)
		}
	}
	if !results[0].Success {
		fmt.Println("Warning: no location responded to the latency probe; you can still pick one.")
	}

	// Never fatal on bad input: by the time this prompt runs, the one-time
	// code has already been consumed, so aborting would strand the register.
	reader := bufio.NewReader(os.Stdin)
	interactive := stdinIsTerminal()
	choice := 1
	for {
		fmt.Printf("Select location [1]: ")
		line, rerr := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			break // Enter (or EOF): accept the closest
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(results) {
			choice = n
			break
		}
		if !interactive || rerr != nil {
			fmt.Printf("Invalid location choice %q; using %s.\n", line, results[0].Hub.Name)
			break
		}
		fmt.Printf("Enter a number between 1 and %d, or press Enter for the closest.\n", len(results))
	}

	sel := results[choice-1]
	if !sel.Success {
		fmt.Printf("Note: %s did not respond to the probe; continuing anyway.\n", sel.Hub.Name)
	}
	fmt.Printf("Using location %s\n", sel.Hub.Name)
	return &sel.Hub, nil
}

func gpuModels(st *stats.SystemStats) []string {
	models := make([]string, 0, len(st.GPUs))
	for _, g := range st.GPUs {
		models = append(models, g.Model)
	}
	return models
}

// Registration is the persisted register-time state. SelectURL and Relays are
// kept so location selection can be retried (`gpu-agent select-location`) after
// an interrupted register — the one-time code is consumed at Submit and cannot
// be replayed.
type Registration struct {
	ListingID string        `json:"listing_id"`
	Hub       string        `json:"hub,omitempty"`
	KeyPath   string        `json:"key_path"`
	SelectURL string        `json:"select_url,omitempty"`
	Relays    []RelayOption `json:"relays,omitempty"`
}

// RegistrationPath is where the register-time state is persisted.
func RegistrationPath() string {
	return filepath.Join(config.ConfigDir(), "registration.json")
}

func saveRegistration(reg Registration) error {
	if err := os.MkdirAll(config.ConfigDir(), 0755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(reg, "", "  ")
	return os.WriteFile(RegistrationPath(), data, 0600)
}

// LoadRegistration reads the persisted register-time state, or an error if the
// agent has not registered yet.
func LoadRegistration() (*Registration, error) {
	data, err := os.ReadFile(RegistrationPath())
	if err != nil {
		return nil, err
	}
	var reg Registration
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, err
	}
	return &reg, nil
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
