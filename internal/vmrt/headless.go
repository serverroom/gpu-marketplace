package vmrt

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"
)

// A machine whose desktop runs on the GPU cannot host: the desktop holds the
// GPU, and a GPU that something holds cannot be handed to a microVM. Desktop
// machines such as the DGX Spark ship that way, so the agent offers to switch
// the machine to start without a desktop -- only when the host asks for it
// (runtime prepare --headless), and in a way `gpu-agent remove` undoes.

const (
	headlessTarget = "multi-user.target"
	headlessFile   = "headless.json"
)

// HeadlessRecord is what MakeHeadless changed, kept so it can be undone.
type HeadlessRecord struct {
	PreviousDefault string `json:"previous_default"`
	At              int64  `json:"at"`
}

// HeadlessPath is where that record lives.
func HeadlessPath(dataDir string) string { return path.Join(dataDir, headlessFile) }

// DefaultTarget is the systemd target this machine starts into.
func DefaultTarget(h Host) (string, error) {
	out, err := h.Output("systemctl", "get-default")
	if err != nil {
		return "", fmt.Errorf("read the default boot target: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// MakeHeadless makes this machine start without a desktop from now on, and
// records the target it started into before -- the first time only, so running
// it twice still restores the original. It does not stop the running desktop:
// CloseDesktop does, as the last step, because it closes the session the host
// may be typing in.
func MakeHeadless(h Host, dataDir string) (previous string, err error) {
	previous, err = DefaultTarget(h)
	if err != nil {
		return "", err
	}
	if _, statErr := LoadHeadless(h, dataDir); errors.Is(statErr, os.ErrNotExist) {
		rec := HeadlessRecord{PreviousDefault: previous, At: time.Now().Unix()}
		data, _ := json.MarshalIndent(rec, "", "  ")
		if err := h.MkdirAll(dataDir, 0700); err != nil {
			return previous, err
		}
		if err := h.WriteFile(HeadlessPath(dataDir), data, 0600); err != nil {
			return previous, fmt.Errorf("record the previous boot target: %w", err)
		}
	}
	if previous != headlessTarget {
		if err := h.Run("systemctl", "set-default", headlessTarget); err != nil {
			return previous, fmt.Errorf("make the machine start without a desktop: %w", err)
		}
	}
	return previous, nil
}

// CloseDesktop stops the running desktop now by switching to the headless
// target. Anything running inside the desktop session closes with it.
//
// Isolating a target stops every unit it does not want -- NVIDIA's own
// services too: on Ubuntu nvidia-persistenced is static, wanted by the NVIDIA
// device rather than by any target, and stayed dead after an isolate on a real
// host. So the ones running before are started again after; notRestarted names
// any that would not start.
func CloseDesktop(h Host) (notRestarted []string, err error) {
	var running []string
	for _, svc := range NVIDIAServices {
		if h.Run("systemctl", "is-active", "--quiet", svc.Unit) == nil {
			running = append(running, svc.Unit)
		}
	}
	if err := h.Run("systemctl", "isolate", headlessTarget); err != nil {
		return nil, fmt.Errorf("close the desktop: %w", err)
	}
	for _, unit := range running {
		if h.Run("systemctl", "start", unit) != nil {
			notRestarted = append(notRestarted, unit)
		}
	}
	return notRestarted, nil
}

// LoadHeadless reads the record MakeHeadless left, or an error wrapping
// os.ErrNotExist when there is none.
func LoadHeadless(h Host, dataDir string) (HeadlessRecord, error) {
	var rec HeadlessRecord
	data, err := h.ReadFile(HeadlessPath(dataDir))
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, fmt.Errorf("read %s: %w", HeadlessPath(dataDir), err)
	}
	return rec, nil
}

// RestoreDesktop undoes MakeHeadless: the machine starts into the target it
// started into before, from its next boot. restored is false when the agent
// never changed it. The running session is left alone.
func RestoreDesktop(h Host, dataDir string) (restored bool, target string, err error) {
	rec, err := LoadHeadless(h, dataDir)
	if errors.Is(err, os.ErrNotExist) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if rec.PreviousDefault != "" && rec.PreviousDefault != headlessTarget {
		if err := h.Run("systemctl", "set-default", rec.PreviousDefault); err != nil {
			return false, rec.PreviousDefault, fmt.Errorf("restore the boot target %s: %w", rec.PreviousDefault, err)
		}
		restored = true
	}
	_ = h.Remove(HeadlessPath(dataDir))
	return restored, rec.PreviousDefault, nil
}
