package vmrt

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/serverroom/gpu-marketplace/internal/stats"
)

// A rental's vCPUs and the machine's core types.
//
// On an ARM machine with big and little cores (an RK3588's Cortex-A76 +
// Cortex-A55, a GB10's Cortex-X925 + Cortex-A725), a KVM vCPU that the host
// moves between core types sees its CPU change under it: its MIDR, its caches,
// the errata its guest worked around when the vCPU came up. Before Linux 6.3
// QEMU may not even start that way ("Failed to put registers after init:
// Invalid argument" on an RK3588; "kvm_init_vcpu failed" on a board whose two
// core types are different KVM targets). So the VM gets the fastest cluster,
// pinned, and the host keeps the others: that is the layout every such machine
// starts with, and the one it always goes back to.
//
// The wider layout (layout.go) adds some of the slower cores, with every vCPU
// held on one physical core for the life of the VM, so none ever changes type.
// A machine uses it only once its own test boot has proven it there.
//
// On a machine with one core type nothing is pinned and the VM gets all but
// one or two CPUs, as before.

// hostKeepsCores is how many of its slowest cores a machine with two core
// types keeps for itself in the wider layout: for the agent, the renter's SSH
// tunnel, and the kernel's own work for the rental (its disk's encryption, its
// network), which QEMU's threads on the VM's cores do not do.
const hostKeepsCores = 2

// CPUChoice is what a rental's vCPUs run on.
type CPUChoice struct {
	// Cores are the host CPUs the VM is pinned to; nil when it is not pinned.
	Cores []int
	// Name is the guest's CPU in words ("Cortex-A76", or x86's model name).
	Name string
	// WideCores is the wider layout a machine with two core types can try, in
	// vCPU order: Cores first, then the slower cores the host does not keep.
	// nil when the machine has one core type, or nothing to add.
	WideCores []int
	// WideName is the wider layout's CPUs in words, never as one kind of
	// core: "4× Cortex-A76 + 2× Cortex-A55".
	WideName string
}

// ChooseGuestCPUs reads the machine's cores and chooses the ones a rental runs
// on (see above), and the wider layout it could try.
func ChooseGuestCPUs(h Host, arch string) CPUChoice {
	data, _ := h.ReadFile("/proc/cpuinfo")
	if arch != "arm64" {
		return CPUChoice{Name: x86Model(string(data))}
	}
	groups := stats.GroupCores(stats.ParseArmCores(string(data)))
	switch len(groups) {
	case 0:
		return CPUChoice{}
	case 1:
		return CPUChoice{Name: groups[0].Name()}
	}
	// The fastest cluster: the highest top frequency, else the core type the
	// table ranks first (groups are already in that order).
	best, bestKHz := 0, 0
	for i, g := range groups {
		for _, cpu := range g.CPUs {
			if khz := maxFreqKHz(h, cpu); khz > bestKHz {
				best, bestKHz = i, khz
			}
		}
	}
	g := groups[best]
	choice := CPUChoice{Cores: append([]int(nil), g.CPUs...), Name: g.Name()}

	// The wider layout: the other types follow, fastest first, less the cores
	// the host keeps -- the lowest-numbered of the slowest type (CPU 0 among
	// them on every board seen, where the kernel's own housekeeping lands).
	others := append(append([]stats.CoreGroup{}, groups[:best]...), groups[best+1:]...)
	keep := hostKeepsCores
	for i := len(others) - 1; i >= 0 && keep > 0; i-- {
		n := len(others[i].CPUs)
		if n > keep {
			n = keep
		}
		others[i].CPUs = others[i].CPUs[n:]
		keep -= n
	}
	wide := append([]int(nil), choice.Cores...)
	names := []string{fmt.Sprintf("%d× %s", len(g.CPUs), g.Name())}
	for _, o := range others {
		if len(o.CPUs) == 0 {
			continue
		}
		wide = append(wide, o.CPUs...)
		names = append(names, fmt.Sprintf("%d× %s", len(o.CPUs), o.Name()))
	}
	if len(wide) > len(choice.Cores) {
		choice.WideCores, choice.WideName = wide, strings.Join(names, " + ")
	}
	return choice
}

// coreTypes is each of these host CPUs' core type as /proc/cpuinfo names it,
// "<implementer>/<part>" ("0x41/0xd0b"), in order; "" for a CPU it does not
// list.
func coreTypes(h Host, cpus []int) []string {
	data, _ := h.ReadFile("/proc/cpuinfo")
	by := map[int]string{}
	for _, c := range stats.ParseArmCores(string(data)) {
		by[c.CPU] = c.Implementer + "/" + c.Part
	}
	out := make([]string, len(cpus))
	for i, cpu := range cpus {
		out[i] = by[cpu]
	}
	return out
}

func maxFreqKHz(h Host, cpu int) int {
	data, err := h.ReadFile(fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq/cpuinfo_max_freq", cpu))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return n
}

func x86Model(cpuinfo string) string {
	for _, line := range strings.Split(cpuinfo, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
			return strings.Join(strings.Fields(v), " ")
		}
	}
	return ""
}

// cpuList is a list of CPUs as systemd's CPUAffinity= takes it: "4,5,6,7".
func cpuList(cpus []int) string {
	parts := make([]string, len(cpus))
	for i, c := range cpus {
		parts[i] = strconv.Itoa(c)
	}
	return strings.Join(parts, ",")
}
