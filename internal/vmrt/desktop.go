package vmrt

import (
	"errors"
	"strings"
	"time"
)

// A DGX Spark is a desktop machine: out of the box its desktop is drawn on the
// GB10, the only GPU it has. On a confirmed Spark (Spec.DesktopOnDemand) the
// runtime closes the desktop for as long as the GPU is rented or tested, by
// stopping the display manager, and starts it again once the GPU is back with
// its driver. Every other machine keeps the v0.1.9 rule: a desktop on the GPU
// is a reason not to host until the machine runs headless.

const (
	// displayManagerAlias is the unit every display manager installs itself as.
	displayManagerAlias = "display-manager.service"
	// fallbackDisplayManager is the Ubuntu (and DGX OS) one, for a machine whose
	// alias is missing.
	fallbackDisplayManager = "gdm3.service"
)

var (
	// DesktopReleaseTimeout bounds how long the runtime waits, after stopping
	// the display manager, for the desktop to let go of the GPU.
	DesktopReleaseTimeout = 30 * time.Second
	desktopPoll           = time.Second
)

// DisplayManager is the systemd unit that runs this machine's desktop:
// display-manager.service, or gdm3 when that alias is missing. "" when neither
// is installed.
func DisplayManager(h Host) string {
	for _, unit := range []string{displayManagerAlias, fallbackDisplayManager} {
		out, err := h.Output("systemctl", "show", "-p", "LoadState", "--value", unit)
		if err == nil && strings.TrimSpace(out) == "loaded" {
			return unit
		}
	}
	return ""
}

// ActiveDisplayManager is DisplayManager while it runs, else "".
func ActiveDisplayManager(h Host) string {
	unit := DisplayManager(h)
	if unit == "" || h.Run("systemctl", "is-active", "--quiet", unit) != nil {
		return ""
	}
	return unit
}

// releaseDesktop closes the desktop so it lets go of the GPU: it records the
// display manager in the rental state first (so a crash at any later point
// still starts it again), stops it, and waits up to DesktopReleaseTimeout for
// the desktop's processes to close their GPU devices. If they do not, it
// starts the display manager again and refuses with the usual message.
func (rt *Runtime) releaseDesktop(st *State, desktop []string, save func() error) error {
	unit := ActiveDisplayManager(rt.h)
	if unit == "" {
		// Nothing the agent may stop is drawing it: someone started it by hand.
		return errors.New(DesktopOnGPUProblem(desktop))
	}
	st.StoppedDisplayManager = unit
	if err := save(); err != nil {
		st.StoppedDisplayManager = ""
		return err
	}
	if err := rt.h.Run("systemctl", "stop", unit); err != nil {
		rt.restoreDesktop(st, save)
		return errors.New(DesktopOnGPUProblem(desktop))
	}
	for waited := time.Duration(0); ; waited += desktopPoll {
		left, _ := ClassifyGPUHolders(rt.h)
		if len(left) == 0 {
			return nil
		}
		if waited >= DesktopReleaseTimeout {
			rt.restoreDesktop(st, save)
			return errors.New(DesktopOnGPUProblem(left))
		}
		rt.h.Sleep(desktopPoll)
	}
}

// restoreDesktop starts the display manager the rental stopped, and forgets it.
func (rt *Runtime) restoreDesktop(st *State, save func() error) {
	if st.StoppedDisplayManager == "" {
		return
	}
	_ = rt.h.Run("systemctl", "start", st.StoppedDisplayManager)
	st.StoppedDisplayManager = ""
	_ = save()
}
