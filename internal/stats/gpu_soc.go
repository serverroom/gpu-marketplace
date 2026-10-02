package stats

import (
	"strings"

	"github.com/serverroom/gpu-marketplace/internal/pcidev"
)

// An ARM board's GPU is part of its SoC and sits on no PCI bus: an RK3588's
// Mali-G610 is a device-tree node (gpu@fb000000), so the PCI scan never saw it
// and the marketplace listed such a machine with no GPU at all (DifraTech's
// Radxa ZAKU2 nodes, 2026-10-02). Like an x86 processor's own GPU it is never
// rented -- a microVM cannot be handed a SoC's GPU -- so it is listed marked
// integrated, and the marketplace says it is not included.

// socGPUPatterns are where a SoC's GPU node sits in the device tree: at its
// root (Rockchip) or under its soc bus (most others).
var socGPUPatterns = []string{
	"/proc/device-tree/gpu@*/compatible",
	"/proc/device-tree/soc/gpu@*/compatible",
	"/proc/device-tree/soc@*/gpu@*/compatible",
}

// socGPUNames are the GPUs whose SoC's compatible string says exactly which
// one it is, in the vendors' own words.
var socGPUNames = map[string]string{
	"rockchip,rk3588-mali": "Arm Mali-G610 MP4",
	"rockchip,rk3576-mali": "Arm Mali-G52 MC3",
	"rockchip,rk3568-mali": "Arm Mali-G52 2EE",
	"rockchip,rk3566-mali": "Arm Mali-G52 2EE",
	"rockchip,rk3399-mali": "Arm Mali-T860 MP4",
}

// socGPUName names a GPU node from its compatible strings: the exact GPU where
// the SoC says it, the family otherwise ("Arm Mali GPU"), "" for a node that
// is not a GPU this knows.
func socGPUName(compatible []byte) string {
	values := strings.Split(strings.TrimRight(string(compatible), "\x00"), "\x00")
	for _, v := range values {
		if name, ok := socGPUNames[strings.TrimSpace(v)]; ok {
			return name
		}
	}
	for _, v := range values {
		switch v = strings.TrimSpace(v); {
		case strings.HasPrefix(v, "arm,mali"):
			return "Arm Mali GPU"
		case strings.HasPrefix(v, "qcom,adreno"):
			return "Qualcomm Adreno GPU"
		}
	}
	return ""
}

// socGPUs are the SoC's own GPUs from the device tree, marked integrated; a
// node the board's device tree switched off (status "disabled") is left out.
func socGPUs(fs pcidev.FS) []GPUInfo {
	var out []GPUInfo
	seen := map[string]bool{}
	for _, pattern := range socGPUPatterns {
		paths, _ := fs.Glob(pattern)
		for _, path := range paths {
			node := strings.TrimSuffix(path, "/compatible")
			if seen[node] {
				continue
			}
			seen[node] = true
			if status, err := fs.ReadFile(node + "/status"); err == nil {
				if s := strings.TrimRight(strings.TrimSpace(string(status)), "\x00"); s != "okay" && s != "ok" {
					continue
				}
			}
			compatible, err := fs.ReadFile(path)
			if err != nil {
				continue
			}
			if name := socGPUName(compatible); name != "" {
				out = append(out, GPUInfo{Model: name, Integrated: true, UnifiedMemory: true})
			}
		}
	}
	return out
}
