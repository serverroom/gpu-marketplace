package vmrt

import (
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
)

// The wider layout of a machine with two core types.
//
// An RK3588 has four fast cores and four slow ones. A rental there used to get
// the four fast ones and nothing else, and the host kept four cores it has
// little work for. The wider layout gives the rental the fast cores and the
// slow cores the host does not need (ChooseGuestCPUs), as what they are: the
// guest's CPU is named "4× Cortex-A76 + 2× Cortex-A55", never as six of one
// kind.
//
// What it takes, and why a machine may not have it:
//
//   - Every vCPU stays on ONE physical core for the life of the VM. KVM shows
//     a guest the MIDR of the core its vCPU is on, and a guest works around a
//     core's errata by the MIDR it read when that CPU came up (a Cortex-A55's
//     broken hardware dirty-bit management, for one): a vCPU that moved to the
//     other core type would run without them. Affining vCPUs is what KVM's
//     maintainers ask of whoever runs a VM on such a machine. So QEMU starts
//     paused (-S), the agent pins each vCPU thread to its core and reads the
//     pin back, and only then lets the guest run (pinVCPUs).
//   - Linux 6.3 or later on the host. Before it KVM handed out the cache
//     registers (CCSIDR) of whichever physical core the asking thread was on,
//     and refused a write that did not match the core the writing thread was
//     on. QEMU reads them on its main thread at every reset and writes them
//     back on each vCPU's thread, so with vCPUs on two core types it fails
//     with "Invalid argument", at the start or at the guest's first reboot,
//     and no pinning helps. 6.3 made those registers the vCPU's own ("KVM:
//     arm64: Normalize cache configuration"). Rockchip's 5.10 and 6.1 vendor
//     kernels do not have it; its 6.6 and 6.12 ones and mainline do.
//   - Proof on this very machine. The agent has no way to know a board's
//     kernel, firmware and QEMU agree on any of this, so the wider layout is
//     tried in a test boot first: the test VM is reset once before it runs (the
//     write that fails on a kernel that cannot do it), and has to come up and
//     say, from inside, that it has every CPU and that each is the core type it
//     was pinned to. Only a pass there puts the wider layout on offer
//     (LayoutRecord), for this agent version, this kernel and these cores.
//
// A machine where any of that fails rents exactly as it did before, on its
// fastest cores, and says why in `gpu-agent check`. It is never left unable to
// rent: the wider layout is only ever an attempt beside the old one.
//
// What it is not. The guest's scheduler does not know which of its CPUs are
// the slow ones: QEMU's virt machine passes no CPU capacities, so a job that
// spreads evenly over every CPU is held back by the slow ones. A renter sees
// the two core types in /proc/cpuinfo and lscpu, fastest first, and can keep a
// job on the fast ones (taskset). Where the host's kernel has a PMU for each
// core type, the guest's performance counters count on the fast cores only:
// KVM gives a VM the PMU of the cores it started on.
//
// Whom it is for: a machine that rents its CPUs, memory and disk, such as an
// RK3588 board. A machine with a GPU on its PCI bus (a GB10) keeps the one core
// type it always had, whatever it rents as (provisioner.Detect); so does every
// rental in a container (container.go), whose processes run under the host's
// own kernel.

// mixedCoresKernel is the first Linux that can run one VM on two core types
// under QEMU: 6.3, where a vCPU's cache registers stopped being the physical
// core's.
var mixedCoresKernel = [2]int{6, 3}

// LayoutRecord is what this machine found when a test boot tried the wider
// layout.
type LayoutRecord struct {
	// Usable: a test rental came up on both core types and reported every CPU
	// as the core it was pinned to.
	Usable bool `json:"usable"`
	// Why is the failure that showed it cannot, in the agent's own words.
	Why string `json:"why,omitempty"`
	// What the verdict holds for: the layout tried, the kernel it was tried
	// on, and the agent version that tried it. Any of them changed, and the
	// machine is one that has not tried yet.
	Cores        string `json:"cores"`
	Kernel       string `json:"kernel"`
	AgentVersion string `json:"agent_version"`
	At           int64  `json:"at"`
}

// LayoutPath is where the record is kept.
func LayoutPath(dataDir string) string { return path.Join(dataDir, "cpu-layout.json") }

// LoadLayout returns the record, or nil when there is none.
func LoadLayout(h Host, dataDir string) *LayoutRecord {
	data, err := h.ReadFile(LayoutPath(dataDir))
	if err != nil {
		return nil
	}
	var rec LayoutRecord
	if json.Unmarshal(data, &rec) != nil {
		return nil
	}
	return &rec
}

// MarkLayout records what a test boot found of the wider layout on cores:
// usable, or not and why.
func MarkLayout(h Host, dataDir, version string, cores []int, usable bool, why string) error {
	data, err := json.MarshalIndent(LayoutRecord{Usable: usable, Why: why, Cores: cpuList(cores),
		Kernel: KernelRelease(h), AgentVersion: version, At: time.Now().Unix()}, "", "  ")
	if err != nil {
		return err
	}
	if err := h.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	tmp := LayoutPath(dataDir) + ".tmp"
	if err := h.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return h.Rename(tmp, LayoutPath(dataDir))
}

// layoutVerdict is the record when it holds for this layout, kernel and agent
// version; nil when the machine has not tried, or tried as something else.
func layoutVerdict(h Host, dataDir, version string, cores []int) *LayoutRecord {
	rec := LoadLayout(h, dataDir)
	if rec == nil || rec.AgentVersion != version || rec.Cores != cpuList(cores) || rec.Kernel != KernelRelease(h) {
		return nil
	}
	return rec
}

// KernelRelease is the running kernel's release ("6.1.84-vendor-rk35xx"), or
// "" when it cannot be read.
func KernelRelease(h Host) string {
	data, err := h.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// kernelAtLeast reports whether a kernel release is version major.minor or
// later. A release it cannot read is not.
func kernelAtLeast(release string, want [2]int) bool {
	var got [2]int
	parts := strings.SplitN(release, ".", 3)
	if len(parts) < 2 {
		return false
	}
	for i := 0; i < 2; i++ {
		digits := parts[i]
		for j, r := range digits {
			if r < '0' || r > '9' {
				digits = digits[:j]
				break
			}
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			return false
		}
		got[i] = n
	}
	return got[0] > want[0] || (got[0] == want[0] && got[1] >= want[1])
}

// Layout is the cores this machine's rentals run on now, and the wider layout
// its next test boot tries, if any.
type Layout struct {
	// Cores, Name: what a rental runs on (CPUChoice's, or the wider layout).
	Cores []int
	Name  string
	// Home is the fastest cluster when Cores is the wider layout, each vCPU on
	// one core (Spec.HomeCores); nil otherwise.
	Home     []int
	HomeName string
	// Trial is the wider layout this machine has not tried yet with this
	// agent version, kernel and cores: its next test boot tries it.
	Trial     []int
	TrialName string
	// Note says, for the host of a machine with two core types, where the
	// wider layout stands: in use, to be tried, or why not. "" on a machine
	// with one core type.
	Note string
}

// ChooseLayout is ChooseGuestCPUs with what this machine has found of the
// wider layout: in use once a test boot proved it, on trial before that, and
// left alone where it cannot work or did not.
func ChooseLayout(h Host, arch, dataDir, version string) Layout {
	c := ChooseGuestCPUs(h, arch)
	l := Layout{Cores: c.Cores, Name: c.Name}
	if len(c.WideCores) == 0 {
		return l
	}
	only := fmt.Sprintf("a rental runs on the %d %s cores only", len(c.Cores), c.Name)
	kernel := KernelRelease(h)
	if !kernelAtLeast(kernel, mixedCoresKernel) {
		l.Note = fmt.Sprintf("%s: this machine's kernel (Linux %s) cannot run one VM on two types of core, which takes Linux %d.%d or later",
			only, orUnknown(kernel), mixedCoresKernel[0], mixedCoresKernel[1])
		return l
	}
	if _, ok := h.(Monitor); !ok {
		return l
	}
	if _, err := h.LookPath("taskset"); err != nil {
		l.Note = only + ": the taskset tool (util-linux) is missing, and the agent pins each vCPU to its core with it"
		return l
	}
	switch rec := layoutVerdict(h, dataDir, version, c.WideCores); {
	case rec == nil:
		l.Trial, l.TrialName = c.WideCores, c.WideName
		l.Note = fmt.Sprintf("%s for now; this machine's next test boot tries %s, each vCPU on a core of its own", only, c.WideName)
	case rec.Usable:
		l.Home, l.HomeName = c.Cores, c.Name
		l.Cores, l.Name = c.WideCores, c.WideName
		l.Note = "each vCPU runs on one core of its own, as this machine's test boot proved"
	default:
		why := rec.Why
		if why == "" {
			why = "a test rental did not come up that way"
		}
		l.Note = fmt.Sprintf("%s: a test rental on %s did not pass on this machine (%s)", only, c.WideName, why)
	}
	return l
}

// OneCoreType is the layout without the wider one, in use or on trial, and
// with nothing to say about it: the machine as it rented before there was one.
func (l Layout) OneCoreType() Layout {
	if len(l.Home) > 0 {
		l.Cores, l.Name = l.Home, l.HomeName
	}
	l.Home, l.HomeName, l.Trial, l.TrialName, l.Note = nil, "", nil, "", ""
	return l
}

// Apply puts the layout into a spec.
func (l Layout) Apply(s *Spec) {
	s.GuestCores, s.GuestCPUName = l.Cores, l.Name
	s.HomeCores, s.HomeCPUName = l.Home, l.HomeName
	s.TrialCores, s.TrialCPUName = l.Trial, l.TrialName
}
