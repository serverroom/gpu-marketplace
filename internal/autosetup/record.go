package autosetup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// RetryAfter is how long after a failed attempt the agent tries again on its
// own: long enough not to hammer anything, short enough that a transient apt
// or network failure heals the same day.
var RetryAfter = 6 * time.Hour

// Attempt is the last setup attempt, kept on disk (autosetup.json in the data
// directory, which `gpu-agent remove` deletes).
type Attempt struct {
	// Key is what the attempt was for: agent version, host driver, base
	// release, architecture and GPUs. One attempt per key; a new one of any of
	// them is a new attempt.
	Key          string   `json:"key"`
	AgentVersion string   `json:"agent_version"`
	Driver       string   `json:"driver"`        // baked into the image
	DriverSource string   `json:"driver_source"` // vmrt.DriverFromHost or DriverDefault
	HostDriver   string   `json:"host_driver"`
	Steps        []string `json:"steps"`
	// Step is the step running, or the last one run (1-based).
	Step          int   `json:"step"`
	StartedAt     int64 `json:"started_at"`
	StepStartedAt int64 `json:"step_started_at,omitempty"`
	// FinishedAt is 0 while running -- or when the agent died mid-attempt
	// without a word (killed, the machine lost power).
	FinishedAt int64  `json:"finished_at,omitempty"`
	Passed     bool   `json:"passed"`
	Error      string `json:"error,omitempty"`
	// By is who ran it: "agent" (automatically) or "setup command".
	By string `json:"by"`
}

const (
	ByAgent   = "agent"
	ByCommand = "setup command"
)

// CurrentStep is the step running, or the last one run.
func (a Attempt) CurrentStep() Step {
	if a.Step < 1 || a.Step > len(a.Steps) {
		return ""
	}
	return Step(a.Steps[a.Step-1])
}

// Finished reports whether the attempt ended with a verdict.
func (a Attempt) Finished() bool { return a.FinishedAt != 0 }

// Path is where the last attempt is kept.
func Path(dataDir string) string { return filepath.Join(dataDir, "autosetup.json") }

// Load reads the last attempt, or nil when the setup never ran here.
func Load(h vmrt.Host, dataDir string) (*Attempt, error) {
	data, err := h.ReadFile(Path(dataDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var a Attempt
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// Save records the attempt, readable only by root.
func Save(h vmrt.Host, dataDir string, a Attempt) error {
	if err := h.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	tmp := Path(dataDir) + ".tmp"
	if err := h.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return h.Rename(tmp, Path(dataDir))
}

// Key names what an attempt is for.
func Key(version string, drv vmrt.DriverChoice, arch string, gpus []string) string {
	g := append([]string(nil), gpus...)
	sort.Strings(g)
	host := drv.HostVersion
	if host == "" {
		host = "unknown"
	} else if drv.Open {
		host += "-open"
	}
	return strings.Join([]string{version, host, vmrt.UbuntuRelease, arch, strings.Join(g, "+")}, "|")
}

// Due says whether an automatic attempt may start now, and when not, when it
// may (the zero time: not by itself -- it passed for this key).
//
//   - no attempt yet for this key: now.
//   - it passed: never again for this key.
//   - it failed: at the agent's next start (justStarted), or RetryAfter after
//     it finished.
//   - it never finished (the agent was killed, the machine lost power): at
//     RetryAfter after it started. Not at once, so an agent that keeps dying
//     does not keep restarting a half-hour build.
func Due(last *Attempt, key string, now time.Time, justStarted bool) (bool, time.Time) {
	switch {
	case last == nil || last.Key != key:
		return true, now
	case last.Passed:
		return false, time.Time{}
	case !last.Finished():
		at := time.Unix(last.StartedAt, 0).Add(RetryAfter)
		return !now.Before(at), at
	case justStarted:
		return true, now
	}
	at := time.Unix(last.FinishedAt, 0).Add(RetryAfter)
	return !now.Before(at), at
}
