package stats

import (
	"strings"

	"github.com/serverroom/gpu-marketplace/internal/pcidev"
)

// A GPU built into the processor (an Intel iGPU, an AMD APU's Radeon) is never
// rented: it draws the machine's own screen (pcidev.Integrated). The specs
// still list it, marked integrated, so the marketplace can say what the
// machine has and that it is not included ("Radeon 8060S graphics, not
// included"). It shares the machine's memory.

// amdgpuIDs is libdrm's table of AMD marketing names by device and revision:
// the one place that tells a Radeon 8060S from an 8050S (both 1002:1586).
const amdgpuIDs = "/usr/share/libdrm/amdgpu.ids"

// integratedName is an integrated GPU's name: libdrm's for an AMD one where it
// has it ("AMD Radeon 8060S Graphics"), else the PCI ID database's.
func integratedName(fs pcidev.FS, d pcidev.Device) string {
	if d.Vendor == pcidev.AMD {
		rev := ""
		if data, err := fs.ReadFile("/sys/bus/pci/devices/" + d.BDF + "/revision"); err == nil {
			rev = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(string(data))), "0x")
		}
		if db, err := fs.ReadFile(amdgpuIDs); err == nil && rev != "" {
			for _, line := range strings.Split(string(db), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "#") {
					continue
				}
				f := strings.SplitN(line, ",", 3)
				if len(f) != 3 || !strings.EqualFold(strings.TrimSpace(f[0]), d.Device) || !strings.EqualFold(strings.TrimSpace(f[1]), rev) {
					continue
				}
				name := strings.TrimSpace(f[2])
				if name == "" {
					break
				}
				if !strings.HasPrefix(name, "AMD ") {
					name = "AMD " + name
				}
				return name
			}
		}
	}
	return pcidev.Name(fs, d)
}

// integratedGPUs are the processor's own GPUs, marked.
func integratedGPUs(fs pcidev.FS) []GPUInfo {
	var out []GPUInfo
	for _, d := range pcidev.Display(fs) {
		if pcidev.Integrated(fs, d) {
			out = append(out, GPUInfo{Model: integratedName(fs, d), Integrated: true, UnifiedMemory: true, bdf: d.BDF})
		}
	}
	return out
}

// withIntegrated marks the processor's own GPU where a vendor tool listed it
// (rocm-smi lists an APU) and adds it where none did: a rentable card keeps
// its entry as it is.
func withIntegrated(gpus []GPUInfo, fs pcidev.FS) []GPUInfo {
	for _, ig := range integratedGPUs(fs) {
		found := false
		for i := range gpus {
			if gpus[i].bdf != "" && gpus[i].bdf == ig.bdf {
				gpus[i].Integrated, gpus[i].UnifiedMemory = true, true
				if gpus[i].Model == "" || gpus[i].Model == "AMD GPU" {
					gpus[i].Model = ig.Model
				}
				found = true
			}
		}
		if !found {
			gpus = append(gpus, ig)
		}
	}
	return gpus
}
