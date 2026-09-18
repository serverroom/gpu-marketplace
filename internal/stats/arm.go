package stats

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ARM machines have no "model name" in /proc/cpuinfo, so every ARM host used to
// report an empty CPU model. This names them: the system-on-chip from the
// device tree when it is one the table knows (a Rockchip RK3588 board), the
// NVIDIA GB10 when that is the GPU, else the core types from cpuinfo's
// implementer/part ("ARM Cortex-A76 + Cortex-A55"). Pure functions over the
// files' contents, so every board is a test fixture.

// ArmCore is one CPU as /proc/cpuinfo describes it on arm64.
type ArmCore struct {
	CPU         int
	Implementer string // "0x41"
	Part        string // "0xd0b"
}

type coreKind struct {
	vendor, name string
	// rank orders core types by speed, for naming the big cores first and for
	// choosing which cluster a rental runs on when frequencies are unknown.
	rank int
}

// armCores are the core types the agent can name, by implementer and part.
var armCores = map[[2]string]coreKind{
	{"0x41", "0xd05"}: {"ARM", "Cortex-A55", 10},
	{"0x41", "0xd03"}: {"ARM", "Cortex-A53", 5},
	{"0x41", "0xd08"}: {"ARM", "Cortex-A72", 30},
	{"0x41", "0xd0b"}: {"ARM", "Cortex-A76", 40},
	{"0x41", "0xd0d"}: {"ARM", "Cortex-A77", 45},
	{"0x41", "0xd41"}: {"ARM", "Cortex-A78", 50},
	{"0x41", "0xd4b"}: {"ARM", "Cortex-A78C", 52},
	{"0x41", "0xd44"}: {"ARM", "Cortex-X1", 55},
	{"0x41", "0xd87"}: {"ARM", "Cortex-A725", 60},
	{"0x41", "0xd85"}: {"ARM", "Cortex-X925", 80},
	{"0x41", "0xd0c"}: {"ARM", "Neoverse-N1", 45},
	{"0x41", "0xd40"}: {"ARM", "Neoverse-V1", 55},
	{"0x41", "0xd49"}: {"ARM", "Neoverse-N2", 55},
	{"0x41", "0xd4f"}: {"ARM", "Neoverse-V2", 70},
	{"0xc0", "0xac3"}: {"Ampere", "AmpereOne", 60},
}

// armVendors name implementers whose parts the table does not know.
var armVendors = map[string]string{"0x41": "ARM", "0xc0": "Ampere", "0x61": "Apple", "0x4e": "NVIDIA", "0x51": "Qualcomm", "0x48": "HiSilicon"}

// socNames are the systems-on-chip named by their device-tree "compatible"
// entry. A board lists its own name first and the SoC after, so every entry is
// looked up.
var socNames = map[string]string{
	"rockchip,rk3588":  "Rockchip RK3588",
	"rockchip,rk3588s": "Rockchip RK3588S",
	"rockchip,rk3582":  "Rockchip RK3582",
	"rockchip,rk3576":  "Rockchip RK3576",
	"rockchip,rk3399":  "Rockchip RK3399",
}

// CoreName is a core type's name ("Cortex-A76"), or "core <impl>/<part>" for
// one the table does not know.
func CoreName(implementer, part string) string {
	if k, ok := armCores[[2]string{implementer, part}]; ok {
		return k.name
	}
	return "core " + implementer + "/" + part
}

// CoreRank orders core types by speed (0 for an unknown one).
func CoreRank(implementer, part string) int { return armCores[[2]string{implementer, part}].rank }

func coreVendor(implementer, part string) string {
	if k, ok := armCores[[2]string{implementer, part}]; ok {
		return k.vendor
	}
	return armVendors[implementer]
}

// ParseArmCores reads the cores out of an arm64 /proc/cpuinfo: one block per
// CPU with "processor", "CPU implementer" and "CPU part".
func ParseArmCores(cpuinfo string) []ArmCore {
	var cores []ArmCore
	cur := ArmCore{CPU: -1}
	flush := func() {
		if cur.CPU >= 0 && cur.Implementer != "" && cur.Part != "" {
			cores = append(cores, cur)
		}
		cur = ArmCore{CPU: -1}
	}
	for _, line := range strings.Split(cpuinfo, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			if strings.TrimSpace(line) == "" {
				flush()
			}
			continue
		}
		k, v = strings.TrimSpace(k), strings.ToLower(strings.TrimSpace(v))
		switch k {
		case "processor":
			flush()
			if n, err := strconv.Atoi(v); err == nil {
				cur.CPU = n
			}
		case "CPU implementer":
			cur.Implementer = v
		case "CPU part":
			cur.Part = v
		}
	}
	flush()
	return cores
}

// CoreGroup is every CPU of one core type.
type CoreGroup struct {
	Implementer, Part string
	CPUs              []int
}

// Name is the group's core name.
func (g CoreGroup) Name() string { return CoreName(g.Implementer, g.Part) }

// GroupCores groups cores by type, fastest-ranked first.
func GroupCores(cores []ArmCore) []CoreGroup {
	by := map[[2]string]*CoreGroup{}
	var keys [][2]string
	for _, c := range cores {
		k := [2]string{c.Implementer, c.Part}
		if by[k] == nil {
			by[k] = &CoreGroup{Implementer: c.Implementer, Part: c.Part}
			keys = append(keys, k)
		}
		by[k].CPUs = append(by[k].CPUs, c.CPU)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		ri, rj := CoreRank(keys[i][0], keys[i][1]), CoreRank(keys[j][0], keys[j][1])
		if ri != rj {
			return ri > rj
		}
		return keys[i][1] > keys[j][1]
	})
	out := make([]CoreGroup, 0, len(keys))
	for _, k := range keys {
		g := *by[k]
		sort.Ints(g.CPUs)
		out = append(out, g)
	}
	return out
}

// CoresDetail is the core mix in words: "4× Cortex-A76 + 4× Cortex-A55".
func CoresDetail(cores []ArmCore) string {
	var parts []string
	for _, g := range GroupCores(cores) {
		parts = append(parts, fmt.Sprintf("%d× %s", len(g.CPUs), g.Name()))
	}
	return strings.Join(parts, " + ")
}

// SoCName is the system-on-chip a device-tree "compatible" (NUL-separated)
// names, or "" when the table does not know it.
func SoCName(compatible []byte) string {
	for _, c := range strings.Split(string(compatible), "\x00") {
		if name, ok := socNames[strings.ToLower(strings.TrimSpace(c))]; ok {
			return name
		}
	}
	return ""
}

// BoardName is a device tree's "model" ("Radxa ROCK 5B"): NULs trimmed,
// printable ASCII only, at most 80 characters.
func BoardName(model []byte) string {
	var b strings.Builder
	for _, r := range strings.TrimRight(string(model), "\x00") {
		if b.Len() >= 80 {
			break
		}
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// ArmCPUModel is an arm64 machine's CPU model: its SoC when the device tree
// names one the table knows; "NVIDIA GB10" when that is its GPU; else its core
// types, vendor first ("ARM Cortex-A76 + Cortex-A55"); "" when nothing is known.
func ArmCPUModel(compatible []byte, cores []ArmCore, gpuModels []string) string {
	if soc := SoCName(compatible); soc != "" {
		return soc
	}
	for _, m := range gpuModels {
		if strings.Contains(strings.ToUpper(m), "GB10") {
			return "NVIDIA GB10"
		}
	}
	groups := GroupCores(cores)
	if len(groups) == 0 {
		return ""
	}
	vendor := coreVendor(groups[0].Implementer, groups[0].Part)
	var names []string
	for _, g := range groups {
		names = append(names, g.Name())
	}
	return strings.TrimSpace(vendor + " " + strings.Join(names, " + "))
}
