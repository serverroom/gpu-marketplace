package vmrt

import (
	"fmt"
	"strconv"
	"strings"
)

// The host keeps using its machine until it is rented. What the host uses of
// what a rental would take -- its own programs holding the GPUs a rental gets,
// and the memory the rental's VM needs -- does not take the machine off the
// market: a rental that arrives meanwhile waits for the host to free it, and
// a test boot leaves the GPU alone. The agent never stops, kills or signals
// a host program; it only looks.

// HostUse is what the host itself uses, now, of what a rental would take.
type HostUse struct {
	// Holders are the host's programs holding the GPUs a rental gets, as
	// "name (pid N)". Not the desktop (a DGX Spark's closes for a rental; any
	// other machine's GPU with a desktop is left out of rentals), not NVIDIA's
	// own services (a rental stops and restarts them), not short-lived tools.
	Holders []string
	// MemoryShortMB is how much more memory must be free before the rental's
	// VM fits: 0 when it does, or when the free memory cannot be read.
	MemoryShortMB int
}

// Busy reports whether the host is using what a rental would take.
func (u HostUse) Busy() bool { return len(u.Holders) > 0 || u.MemoryShortMB > 0 }

// MemoryShortGB is MemoryShortMB in whole GB, rounded up.
func (u HostUse) MemoryShortGB() int { return (u.MemoryShortMB + 1023) / 1024 }

// Programs are the holders' names, each once, in order: "llama-server".
func (u HostUse) Programs() []string {
	var out []string
	seen := map[string]bool{}
	for _, h := range u.Holders {
		name := h
		if i := strings.LastIndex(h, " (pid "); i > 0 {
			name = h[:i]
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// Describe is the host's use in a few words: "llama-server, and 12 GB of the
// memory a rental needs".
func (u HostUse) Describe() string {
	var parts []string
	if progs := u.Programs(); len(progs) > 0 {
		parts = append(parts, strings.Join(progs, ", "))
	}
	if u.MemoryShortMB > 0 {
		parts = append(parts, fmt.Sprintf("%d GB of the memory a rental needs", u.MemoryShortGB()))
	}
	return strings.Join(parts, ", and ")
}

// MemAvailableMB is the memory the kernel says is available for new work
// (MemAvailable in /proc/meminfo), or -1 when it cannot be read.
func MemAvailableMB(h Host) int {
	data, err := h.ReadFile("/proc/meminfo")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemAvailable:" {
			kb, err := strconv.Atoi(f[1])
			if err != nil {
				return -1
			}
			return kb / 1024
		}
	}
	return -1
}

// ReadHostUse looks at what the host uses, now, of what a rental on spec
// would take. It waits for nothing: a short-lived tool is simply not counted.
func ReadHostUse(h Host, spec Spec) HostUse {
	var u HostUse
	if len(spec.GPUs) > 0 {
		u.Holders = readGPUHolders(h, spec.GPUs).other
	}
	if need, free := spec.GuestMemoryMB(), MemAvailableMB(h); need > 0 && free >= 0 && free < need {
		u.MemoryShortMB = need - free
	}
	return u
}

// TestOptions shape one test boot.
type TestOptions struct {
	// NoGPU leaves the GPUs with the host, which is using them.
	NoGPU bool
	// MemoryMB sizes the test VM below a rental's (0: a rental's size).
	MemoryMB int
}

// TestPlan is what a test boot can do now without interrupting the host.
type TestPlan struct {
	TestOptions
	// InUse says what keeps the GPU from the test, when NoGPU.
	InUse []string
	// Wait, when not "", is why no test boot can run now at all.
	Wait string
}

const (
	// testHeadroomMB is left free beside a test VM sized down to what is free.
	testHeadroomMB = 1024
	// minTestMB is the smallest test VM: a rental's smallest.
	minTestMB = 2048
)

// PlanTest decides how a test boot can run now without stopping anything of
// the host's: with the GPU when nothing of the host's holds it, else without
// it; at a rental's size when that much memory is free, else smaller (never
// under 2 GB, with 1 GB left free beside it). closeDesktop says whether a DGX
// Spark's desktop may close for the test even with a person logged in to it
// -- a person asked for the test, or a rental starts right after it; the
// agent's own tests never close a desktop someone is using.
func PlanTest(h Host, spec Spec, closeDesktop bool) TestPlan {
	var plan TestPlan
	if want, free := spec.GuestMemoryMB(), MemAvailableMB(h); want > 0 && free >= 0 && free-testHeadroomMB < want {
		room := (free - testHeadroomMB) / 256 * 256
		if room < minTestMB {
			plan.Wait = fmt.Sprintf("only %.1f GB of memory is free on this machine, and a test rental needs %d GB free; it runs once that much is free",
				float64(free)/1024, (minTestMB+testHeadroomMB)/1024)
			return plan
		}
		plan.MemoryMB = room
	}
	if len(spec.GPUs) == 0 {
		return plan
	}
	g := settleGPUHolders(h, spec.GPUs)
	// The host's programs; short-lived tools only when they did not finish.
	plan.InUse = append([]string{}, g.other...)
	if len(plan.InUse) == 0 {
		plan.InUse = append(plan.InUse, g.transient...)
	}
	if len(g.desktop) > 0 && !closeDesktop {
		if !spec.DesktopOnDemand {
			plan.InUse = append(plan.InUse, g.desktop...)
		} else if logins := GraphicalLogins(h); len(logins) > 0 {
			var users []string
			for _, l := range logins {
				users = append(users, l.User)
			}
			plan.InUse = append(plan.InUse, "the desktop, where "+strings.Join(users, ", ")+" is logged in")
		}
	}
	plan.NoGPU = len(plan.InUse) > 0
	return plan
}
