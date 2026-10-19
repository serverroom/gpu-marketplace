package register

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/serverroom/gpu-marketplace/internal/direct"
)

// DirectAnswer is the control plane's answer to a report of this machine's
// public addresses: the route to use now (Connection: relay, ipv4, ipv6 or
// dual), the host's choice, the addresses when the route is direct, a relay
// tunnel to open, or the locations to pick one from when there is none to
// give; why a change waits (a rental runs), and why an address could not be
// used.
type DirectAnswer struct {
	Connection string            `json:"connection"`
	Choice     string            `json:"choice"`
	Direct     *direct.Route     `json:"direct,omitempty"`
	Tunnel     *TunnelInfo       `json:"tunnel,omitempty"`
	Relays     []RelayOption     `json:"relays,omitempty"`
	SelectURL  string            `json:"select_url,omitempty"`
	Pending    string            `json:"pending,omitempty"`
	Refused    map[string]string `json:"refused,omitempty"`
	Reason     string            `json:"reason,omitempty"`
}

// directURL is where the service reports its addresses. Registrations from
// before v0.3.14 kept no such URL; it is the capability URL's sibling.
func directURL(r *Registration) string {
	if r.DirectURL != "" {
		return r.DirectURL
	}
	if strings.HasSuffix(r.CapabilityURL, "/capability") {
		return strings.TrimSuffix(r.CapabilityURL, "/capability") + "/direct"
	}
	return ""
}

// MarketplaceHost is the marketplace's host name: the addresses the machine
// leaves by for it are the ones that count.
func MarketplaceHost() string {
	reg, err := LoadRegistration()
	if err != nil {
		return ""
	}
	for _, u := range []string{directURL(reg), reg.CapabilityURL, reg.SelectURL} {
		if host := direct.HostOf(u); host != "" {
			return host
		}
	}
	return ""
}

// ReportDirect tells the marketplace this machine's public addresses (nil:
// none) and returns the route to use now. The marketplace reaches /health on
// each address before it uses it, so the control channel must already be
// listening there.
func ReportDirect(report *direct.Report) (*DirectAnswer, error) {
	reg, url, token, err := lifecycleTarget(directURL, "direct")
	if err != nil {
		return nil, err
	}
	code, body, err := postBearer(url, token, map[string]interface{}{"listing_id": reg.ListingID, "direct": report})
	if err != nil {
		return nil, err
	}
	var answer DirectAnswer
	_ = json.Unmarshal(body, &answer)
	if code != http.StatusOK {
		text := string(body)
		if answer.Reason != "" {
			text = answer.Reason
		}
		return &answer, &EndpointError{Op: "direct address report", Code: code, Body: text}
	}
	if answer.Direct != nil {
		answer.Direct.Connection = answer.Connection
	}
	return &answer, nil
}

// JoinRelay gets a tunnel for an answer that sends the machine back to a
// relay without one to give: the closest location, chosen without asking,
// from select_url.
func JoinRelay(answer *DirectAnswer) (*TunnelInfo, error) {
	if answer.SelectURL == "" || len(answer.Relays) == 0 {
		return nil, errors.New("the marketplace gave no tunnel and no location to pick")
	}
	reg, err := LoadRegistration()
	if err != nil {
		return nil, ErrNotRegistered
	}
	hub := closestHub(hubsFromRelayOptions(answer.Relays))
	if hub == nil {
		return nil, errors.New("no location answered")
	}
	sresp, err := selectRelayWithRetry(answer.SelectURL, LoadControlToken(), reg.ListingID, hub.Name)
	if err != nil {
		return nil, err
	}
	if sresp.Tunnel == nil {
		return nil, fmt.Errorf("the marketplace gave no tunnel at %s", hub.Name)
	}
	reg.Hub = hub.Name
	if err := saveRegistration(*reg); err != nil {
		return nil, err
	}
	return sresp.Tunnel, nil
}

// SaveTunnel writes the tunnel config for t with this registration's key.
func SaveTunnel(t *TunnelInfo) error {
	reg, err := LoadRegistration()
	if err != nil {
		return ErrNotRegistered
	}
	return saveTunnelConfig(t, reg.KeyPath)
}

// DropTunnel forgets the tunnel config: the machine is direct now.
func DropTunnel() error {
	err := os.Remove(TunnelConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
