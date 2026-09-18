package register

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/speedtest"
	"github.com/serverroom/gpu-marketplace/internal/stats"
)

// ErrNotRegistered is returned by calls that need a registration on disk.
var ErrNotRegistered = errors.New("this machine is not registered")

// EndpointError is a non-200 reply from a control-plane endpoint. A 4xx will
// not heal on retry; a 5xx might.
type EndpointError struct {
	Op   string
	Code int
	Body string
}

func (e *EndpointError) Error() string {
	return fmt.Sprintf("%s failed (%d): %s", e.Op, e.Code, strings.TrimSpace(e.Body))
}

// endpointURL is where to send a lifecycle call. Registrations made by this
// release carry the URL the control plane handed back; older ones only kept the
// select-relay URL, whose sibling path is the same API.
func endpointURL(explicit, selectURL, name string) string {
	if explicit != "" {
		return explicit
	}
	const suffix = "/select-relay"
	if strings.HasSuffix(selectURL, suffix) {
		return strings.TrimSuffix(selectURL, suffix) + "/" + name
	}
	return ""
}

func postBearer(url, token string, payload interface{}) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody, nil
}

func lifecycleTarget(explicit func(*Registration) string, name string) (*Registration, string, string, error) {
	reg, err := LoadRegistration()
	if err != nil {
		return nil, "", "", ErrNotRegistered
	}
	url := endpointURL(explicit(reg), reg.SelectURL, name)
	if url == "" {
		return nil, "", "", fmt.Errorf("this registration has no %s endpoint; register again with a new code", name)
	}
	token := LoadControlToken()
	if token == "" {
		return nil, "", "", errors.New("no control token on disk")
	}
	return reg, url, token, nil
}

// CapabilityResponse is the control plane's answer to a capability report.
type CapabilityResponse struct {
	// Speedtest names the server to measure against while the listing still
	// needs its initial network measurement; nil once it has one.
	Speedtest *speedtest.Target `json:"speedtest,omitempty"`
	// MeasureURL is where a measurement is posted.
	MeasureURL string `json:"measure_url,omitempty"`
}

// ReportCapability tells the control plane whether this machine can host a
// rental. The marketplace only offers a machine to renters while its latest
// report says ready, so this runs at register and on every start.
//
// The answer can ask for the listing's network measurement. Its target is
// saved, so `gpu-agent speedtest` can measure again once the marketplace has
// stopped asking.
func ReportCapability(c control.Capability) (*CapabilityResponse, error) {
	reg, url, token, err := lifecycleTarget(func(r *Registration) string { return r.CapabilityURL }, "capability")
	if err != nil {
		return nil, err
	}
	payload := map[string]interface{}{
		"listing_id": reg.ListingID,
		"capability": c,
	}
	// The machine's specs as they are now (an ARM board's CPU and board name,
	// say, which agents before v0.2.0 reported empty at register). A control
	// plane that does not read them ignores them.
	if st, err := collectSpecs(); err == nil {
		payload["specs"] = st
	}
	code, body, err := postBearer(url, token, payload)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, &EndpointError{Op: "capability report", Code: code, Body: string(body)}
	}
	resp := parseCapabilityResponse(body)
	// Best effort: losing it costs only the command's fallback target.
	_ = rememberSpeedtest(resp)
	return resp, nil
}

// collectSpecs is stats.Collect; a variable so tests need not read the machine.
var collectSpecs = func() (*stats.SystemStats, error) { return stats.Collect() }

// parseCapabilityResponse reads the answer to a capability report. A control
// plane older than the speed test answers with no body, or a body without
// these fields; the report still succeeded, so that is an empty answer, not an
// error.
func parseCapabilityResponse(body []byte) *CapabilityResponse {
	var r CapabilityResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return &CapabilityResponse{}
	}
	return &r
}

// SpeedtestPath is where the last speed test target and measure URL the
// control plane named are kept.
func SpeedtestPath() string {
	return filepath.Join(config.ConfigDir(), "speedtest.json")
}

// SavedSpeedtest is the last speed test target and measure URL the control
// plane named, or neither.
func SavedSpeedtest() CapabilityResponse {
	data, err := os.ReadFile(SpeedtestPath())
	if err != nil {
		return CapabilityResponse{}
	}
	var r CapabilityResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return CapabilityResponse{}
	}
	return r
}

// rememberSpeedtest saves the newest target and measure URL. An answer that
// names neither -- the listing is already measured -- keeps the old ones.
func rememberSpeedtest(resp *CapabilityResponse) error {
	if resp == nil || (resp.Speedtest == nil && resp.MeasureURL == "") {
		return nil
	}
	saved := SavedSpeedtest()
	if resp.Speedtest != nil {
		saved.Speedtest = resp.Speedtest
	}
	if resp.MeasureURL != "" {
		saved.MeasureURL = resp.MeasureURL
	}
	if err := os.MkdirAll(config.ConfigDir(), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(SpeedtestPath(), data, 0600)
}

// Measurement is what a speed test posts to the listing.
type Measurement struct {
	ListingID string  `json:"listing_id"`
	DownMbps  float64 `json:"down_mbps"`
	UpMbps    float64 `json:"up_mbps"`
	LatencyMs float64 `json:"latency_ms"`
	Server    string  `json:"server"`
}

// PostMeasurement posts a speed test result to this machine's listing, at
// measureURL when the control plane named one.
func PostMeasurement(measureURL string, r speedtest.Result) error {
	reg, err := LoadRegistration()
	if err != nil {
		return ErrNotRegistered
	}
	url := measureEndpoint(measureURL, SavedSpeedtest().MeasureURL, reg)
	if url == "" {
		return errors.New("this registration has no measure endpoint; register again with a new code")
	}
	token := LoadControlToken()
	if token == "" {
		return errors.New("no control token on disk")
	}
	code, body, err := postBearer(url, token, Measurement{
		ListingID: reg.ListingID,
		DownMbps:  r.DownMbps,
		UpMbps:    r.UpMbps,
		LatencyMs: r.LatencyMs,
		Server:    r.Server,
	})
	if err != nil {
		return err
	}
	// Values the control plane will not store come back as 219; that, like
	// anything but a 200, is a failure.
	if code != http.StatusOK {
		return &EndpointError{Op: "speed test report", Code: code, Body: string(body)}
	}
	return nil
}

// measureEndpoint is where a measurement goes: the URL the control plane just
// named, else the last one it named, else the capability endpoint's sibling on
// the same API.
func measureEndpoint(explicit, saved string, reg *Registration) string {
	if explicit != "" {
		return explicit
	}
	if saved != "" {
		return saved
	}
	const suffix = "/capability"
	capabilityURL := endpointURL(reg.CapabilityURL, reg.SelectURL, "capability")
	if strings.HasSuffix(capabilityURL, suffix) {
		return strings.TrimSuffix(capabilityURL, suffix) + "/measure"
	}
	return ""
}

// DeregisterOutcome is what the marketplace said when told this agent is going.
type DeregisterOutcome struct {
	ListingID    string
	Withdrawn    bool
	RelayRevoked bool
	Note         string
}

// Deregister withdraws this machine's listing and revokes its relay access. It
// changes nothing on this machine; `gpu-agent remove` does the local half.
func Deregister() (DeregisterOutcome, error) {
	reg, url, token, err := lifecycleTarget(func(r *Registration) string { return r.DeregisterURL }, "deregister")
	if err != nil {
		return DeregisterOutcome{}, err
	}
	out := DeregisterOutcome{ListingID: reg.ListingID}
	code, body, err := postBearer(url, token, map[string]string{"listing_id": reg.ListingID})
	if err != nil {
		return out, err
	}
	switch code {
	case http.StatusOK:
		var r struct {
			Status       string `json:"status"`
			RelayRevoked bool   `json:"relay_revoked"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return out, fmt.Errorf("withdrawal: unreadable reply: %w", err)
		}
		out.Withdrawn = r.Status == "withdrawn"
		out.RelayRevoked = r.RelayRevoked
		return out, nil
	case http.StatusUnauthorized:
		// The control token is cleared on withdrawal, so a second withdrawal
		// lands here -- as does a token replaced by a newer registration.
		out.Note = "the marketplace no longer recognises this agent's token: the listing was already withdrawn, or replaced by a newer registration"
		return out, nil
	default:
		return out, &EndpointError{Op: "withdrawal", Code: code, Body: string(body)}
	}
}
