package vmrt

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/serverroom/gpu-marketplace/internal/stats"
)

// A rental's vCPUs run on ONE core type. On an ARM machine with big and little
// cores (an RK3588's Cortex-A76 + Cortex-A55, a GB10's Cortex-X925 +
// Cortex-A725), a KVM vCPU that the host moves between core types sees its CPU
// change under it -- QEMU may not even start ("kvm_init_vcpu failed") -- so the
// VM gets the fastest cluster, pinned, and the host keeps the others. On a
// machine with one core type nothing is pinned and the VM gets all but one or
// two CPUs, as before.

// CPUChoice is what a rental's vCPUs run on.
type CPUChoice struct {
	// Cores are the host CPUs the VM is pinned to; nil when it is not pinned.
	Cores []int
	// Name is the guest's CPU in words ("Cortex-A76", or x86's model name).
	Name string
}

// ChooseGuestCPUs reads the machine's cores and chooses the ones a rental runs
// on (see above).
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
	return CPUChoice{Cores: append([]int(nil), g.CPUs...), Name: g.Name()}
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
