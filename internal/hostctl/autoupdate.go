package hostctl

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/update"
)

// Automatic updates (CONTRACT-ops A2). The marketplace says which release a
// machine should run -- in its answer to every capability report, and on a
// status request -- as agent_update {version, push}. The agent never asks
// GitHub's API for it per machine. It updates only when the machine is idle
// (as the control panel's "Update agent" does: same download, checksum rule
// and rollback), and otherwise waits for the next answer. A host can pause
// the automatic ones (`gpu-agent update --auto off`); a push -- staff, or the
// host's own "Update agent" -- still applies.

// AutoUpdatePath is the host's switch for automatic updates, in the config
// directory: "off" pauses them; missing or anything else is on.
func AutoUpdatePath(configDir string) string { return filepath.Join(configDir, "auto-update") }

// AutoUpdateEnabled reports whether automatic updates are on (the default).
func AutoUpdateEnabled(configDir string) bool {
	data, err := os.ReadFile(AutoUpdatePath(configDir))
	if err != nil {
		return true
	}
	return !strings.EqualFold(strings.TrimSpace(string(data)), "off")
}

// SetAutoUpdate turns automatic updates on or off.
func SetAutoUpdate(configDir string, on bool) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return err
	}
	word := "off\n"
	if on {
		word = "on\n"
	}
	return os.WriteFile(AutoUpdatePath(configDir), []byte(word), 0644)
}

// Retry spacing for one release that did not go in: an automatic update that
// failed or was rolled back is not tried again for a day (the release itself
// may be at fault, and each try restarts the agent); a push, after an hour.
// Any attempt waits RetryAfter before the next, so an answer every 30 seconds
// cannot start a download every 30 seconds.
var (
	RetryAfter       = 10 * time.Minute
	PushRetryAfter   = time.Hour
	FailedRetryAfter = 24 * time.Hour
)

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Agent) autoEnabled() bool {
	if a.ConfigDir == "" {
		return true
	}
	return AutoUpdateEnabled(a.ConfigDir)
}

// AutoUpdateStatus is auto_update for /status and the capability report.
func (a *Agent) AutoUpdateStatus() *control.AutoUpdate {
	a.autoMu.Lock()
	last := a.lastCheck
	a.autoMu.Unlock()
	s := &control.AutoUpdate{Enabled: a.autoEnabled(), LastCheck: last}
	if r := update.Load(a.DataDir); r != nil {
		s.Last = r
	}
	return s
}

// Heard records that the marketplace just said which release is current (an
// answer to a capability report, with or without agent_update).
func (a *Agent) Heard() {
	a.autoMu.Lock()
	a.lastCheck = a.now().Unix()
	a.autoMu.Unlock()
}

// Offer is the marketplace's agent_update. It starts the update when the
// release is newer, updates are on (or it is a push), the machine is idle and
// the release has not just been tried; started says it did, else why says
// what it waits for ("" when there is nothing to do).
func (a *Agent) Offer(u *control.AgentUpdate) (started bool, why string) {
	if u == nil {
		return false, ""
	}
	a.Heard()
	to := strings.TrimSpace(u.Version)
	if !update.Newer(to, a.Version) {
		return false, ""
	}
	if !u.Push && !a.autoEnabled() {
		return false, "automatic updates are off on this machine ('sudo gpu-agent update --auto on' turns them on)"
	}
	now := a.now()
	a.autoMu.Lock()
	if at, ok := a.tried[to]; ok && now.Sub(at) < RetryAfter {
		a.autoMu.Unlock()
		return false, "the update to " + to + " was tried a moment ago"
	}
	a.autoMu.Unlock()
	// The last record says how the last try of this release went, across the
	// restart a rollback makes.
	if r := update.Load(a.DataDir); r != nil && r.To == to && (r.Status == update.StatusFailed || r.Status == update.StatusRolledBack) {
		wait := FailedRetryAfter
		if u.Push {
			wait = PushRetryAfter
		}
		if since := now.Sub(time.Unix(r.FinishedAt, 0)); since < wait {
			return false, "the update to " + to + " did not go in (" + strings.ReplaceAll(r.Status, "_", " ") + "); it is tried again later"
		}
	}
	// Waiting for the machine to be idle is not a try: the next answer asks again.
	if a.Machine.Withdrawn() {
		return false, "this machine was removed from the marketplace"
	}
	if why := a.busy(); why != "" {
		return false, why
	}
	a.autoMu.Lock()
	if a.tried == nil {
		a.tried = map[string]time.Time{}
	}
	a.tried[to] = now
	a.autoMu.Unlock()
	if _, _, err := a.Update(to); err != nil {
		return false, err.Error()
	}
	if a.Log != nil {
		how := "automatically"
		if u.Push {
			how = "as the marketplace asked"
		}
		a.Log("Updating the agent from %s to %s %s", a.Version, to, how)
	}
	return true, ""
}
