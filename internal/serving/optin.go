package serving

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Whether this machine serves is the host's choice, made in two places (owner's
// decision, 2026-10-05):
//
//   - the control panel, which opts a listing in and picks the model: the
//     marketplace says so as "serving" in its answer to every capability
//     report, {"enabled": bool, "model": "<id>"}, and the agent keeps the last
//     answer in the data directory so a restart remembers it;
//   - the machine itself, where `sudo gpu-agent serve off` overrides the panel
//     until `serve on` lifts it, as `update --auto off` does for updates.
//
// Downloading the weights (65 GB for gpt-oss-120b) and serving both need the
// panel's yes and no veto on the machine.

// Offer is the control panel's choice for this machine.
type Offer struct {
	Enabled bool   `json:"enabled"`
	Model   string `json:"model,omitempty"`
}

// ParseOffer reads the marketplace's "serving" strictly: an object with a
// boolean enabled and, when enabled, a model id; any other field, or anything
// else, is no offer at all (nil), which leaves the last one standing.
func ParseOffer(raw json.RawMessage) *Offer {
	if len(raw) == 0 || len(raw) > 512 {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	var o Offer
	sawEnabled := false
	for k, v := range fields {
		switch k {
		case "enabled":
			if json.Unmarshal(v, &o.Enabled) != nil {
				return nil
			}
			sawEnabled = true
		case "model":
			if json.Unmarshal(v, &o.Model) != nil {
				return nil
			}
		default:
			return nil
		}
	}
	if !sawEnabled {
		return nil
	}
	if o.Enabled && !ValidModelID(o.Model) {
		return nil
	}
	if !o.Enabled {
		o.Model = ""
	}
	return &o
}

// OfferPath is where the last offer is kept.
func OfferPath(dataDir string) string { return filepath.Join(dataDir, "serving-offer.json") }

// SaveOffer keeps the panel's choice.
func SaveOffer(dataDir string, o Offer) error {
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return err
	}
	path := OfferPath(dataDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadOffer is the last choice the panel made, or nil when it never made one
// (or the file is unreadable: no offer, never a guess).
func LoadOffer(dataDir string) *Offer {
	data, err := os.ReadFile(OfferPath(dataDir))
	if err != nil {
		return nil
	}
	return ParseOffer(data)
}

// VetoPath is the host's switch on the machine, in the config directory: "off"
// stops serving whatever the panel says; missing (or anything else) leaves the
// choice to the panel.
func VetoPath(configDir string) string { return filepath.Join(configDir, "serving") }

// Vetoed reports whether the host switched serving off on this machine.
func Vetoed(configDir string) bool {
	data, err := os.ReadFile(VetoPath(configDir))
	if err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(string(data)), "off")
}

// SetVeto switches serving off on this machine (true), or lifts that (false).
func SetVeto(configDir string, off bool) error {
	if !off {
		if err := os.Remove(VetoPath(configDir)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return err
	}
	return os.WriteFile(VetoPath(configDir), []byte("off\n"), 0644)
}

// Wanted is the model this machine should download and serve, or, when none,
// why not, in a line a host can read.
func Wanted(offer *Offer, vetoed bool) (Model, string) {
	switch {
	case vetoed:
		return Model{}, "serving is switched off on this machine ('sudo gpu-agent serve on' leaves it to the control panel)"
	case offer == nil || !offer.Enabled:
		return Model{}, "serving is not turned on for this machine in the control panel"
	}
	m, ok := Lookup(offer.Model)
	if !ok {
		return Model{}, "the control panel chose " + offer.Model + ", which this agent version does not serve"
	}
	return m, ""
}
