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

// socGPUNames are the GPUs whose node's compatible string says exactly which
// one it is, in the vendors' own words.
var socGPUNames = map[string]string{
	"rockchip,rk3588-mali": "Arm Mali-G610 MP4",
	"rockchip,rk3576-mali": "Arm Mali-G52 MC3",
	"rockchip,rk3568-mali": "Arm Mali-G52 2EE",
	"rockchip,rk3566-mali": "Arm Mali-G52 2EE",
	"rockchip,rk3399-mali": "Arm Mali-T860 MP4",
}

// socMaliNames are the Mali GPUs by the SoC the device tree's root names, for
// a GPU node that says only its family: Rockchip's own kernels describe an
// RK3588's Mali-G610 as plain "arm,mali-bifrost".
var socMaliNames = map[string]string{
	"rockchip,rk3588":  "Arm Mali-G610 MP4",
	"rockchip,rk3588s": "Arm Mali-G610 MP4",
	"rockchip,rk3576":  "Arm Mali-G52 MC3",
	"rockchip,rk3568":  "Arm Mali-G52 2EE",
	"rockchip,rk3566":  "Arm Mali-G52 2EE",
	"rockchip,rk3399":  "Arm Mali-T860 MP4",
}

// dtStrings are a device-tree property's NUL-separated strings.
func dtStrings(value []byte) []string {
	var out []string
	for _, v := range strings.Split(string(value), "\x00") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// socGPUName names a GPU node from its compatible strings: the exact GPU where
// the node says it, a Mali by the SoC the root names (rootCompatible), the
// family otherwise ("Arm Mali GPU"), "" for a node that is not a GPU this knows.
func socGPUName(compatible, rootCompatible []byte) string {
	values := dtStrings(compatible)
	for _, v := range values {
		if name, ok := socGPUNames[v]; ok {
			return name
		}
	}
	mali := false
	for _, v := range values {
		mali = mali || strings.HasPrefix(v, "arm,mali")
	}
	if mali {
		for _, v := range dtStrings(rootCompatible) {
			if name, ok := socMaliNames[v]; ok {
				return name
			}
		}
		return "Arm Mali GPU"
	}
	for _, v := range values {
		if strings.HasPrefix(v, "qcom,adreno") {
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
	root, _ := fs.ReadFile("/proc/device-tree/compatible")
	for _, pattern := range socGPUPatterns {
		paths, _ := fs.Glob(pattern)
		for _, path := range paths {
			node := strings.TrimSuffix(path, "/compatible")
			if seen[node] {
				continue
			}
			seen[node] = true
			if status, err := fs.ReadFile(node + "/status"); err == nil {
				if s := dtStrings(status); len(s) > 0 && s[0] != "okay" && s[0] != "ok" {
					continue
				}
			}
			compatible, err := fs.ReadFile(path)
			if err != nil {
				continue
			}
			if name := socGPUName(compatible, root); name != "" {
				out = append(out, GPUInfo{Model: name, Integrated: true, UnifiedMemory: true})
			}
		}
	}
	return out
}
