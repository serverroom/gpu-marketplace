package register

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
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

// ReportCapability tells the control plane whether this machine can host a
// rental. The marketplace only offers a machine to renters while its latest
// report says ready, so this runs at register and on every start.
func ReportCapability(c control.Capability) error {
	reg, url, token, err := lifecycleTarget(func(r *Registration) string { return r.CapabilityURL }, "capability")
	if err != nil {
		return err
	}
	code, body, err := postBearer(url, token, map[string]interface{}{
		"listing_id": reg.ListingID,
		"capability": c,
	})
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return &EndpointError{Op: "capability report", Code: code, Body: string(body)}
	}
	return nil
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
