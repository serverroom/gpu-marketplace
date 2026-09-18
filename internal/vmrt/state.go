package vmrt

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// State is the rental on this machine, written to disk after every step of a
// start. An agent that dies halfway through -- or a machine that reboots --
// finds it on the next start and knows exactly what to tear down, instead of
// leaving a GPU on vfio-pci and an encrypted mapping nobody remembers.
type State struct {
	RentalID string        `json:"rental_id"`
	Rental   Rental        `json:"rental"`
	Fenced   bool          `json:"fenced"`
	Net      NetState      `json:"net"`
	Disk     DiskState     `json:"disk"`
	Devices  []BoundDevice `json:"devices"`
	// StoppedPersistenced is what agents up to v0.1.8 recorded; still read so a
	// rental they started is torn down properly.
	StoppedPersistenced bool `json:"stopped_persistenced,omitempty"`
	// StoppedServices are the NVIDIA services the rental stopped (NVIDIAServices).
	StoppedServices []string `json:"stopped_services,omitempty"`
	StartedAt       int64    `json:"started_at"`
	// Dirty is set when a teardown did not verify. The state stays on disk, so
	// the machine refuses the next rental until it is cleaned up.
	Dirty       bool     `json:"dirty"`
	DirtyDetail []string `json:"dirty_detail,omitempty"`
}

// ServicesToRestart are the NVIDIA services to start again when the rental
// ends: the ones it stopped, and nvidia-persistenced for a rental an older
// agent started.
func (st *State) ServicesToRestart() []string {
	units := append([]string{}, st.StoppedServices...)
	if st.StoppedPersistenced {
		for _, u := range units {
			if u == persistencedSvc {
				return units
			}
		}
		units = append(units, persistencedSvc)
	}
	return units
}

// StatePath is where the rental state lives.
func StatePath(dataDir string) string { return filepath.Join(dataDir, "rental.json") }

// SaveState writes the state, readable only by root.
func SaveState(h Host, dataDir string, st *State) error {
	if err := h.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := StatePath(dataDir) + ".tmp"
	if err := h.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return h.Rename(tmp, StatePath(dataDir))
}

// LoadState returns the rental state, or nil when no rental is on the machine.
func LoadState(h Host, dataDir string) (*State, error) {
	data, err := h.ReadFile(StatePath(dataDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// ClearState records that no rental is on the machine.
func ClearState(h Host, dataDir string) error { return h.Remove(StatePath(dataDir)) }
