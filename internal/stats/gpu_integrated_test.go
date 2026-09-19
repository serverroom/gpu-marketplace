package stats

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

const strixHalo = "0000:c5:00.0"

// strixHaloHost is an AMD Ryzen AI Max+ 395 machine: its Radeon 8060S is
// the processor's own GPU, behind the APU bridge 00:08.1, revision c1.
func strixHaloHost(withLibdrm bool) *fakehost.Host {
	h := fakehost.New()
	h.Files["/usr/share/misc/pci.ids"] = []byte("1002  Advanced Micro Devices, Inc. [AMD/ATI]\n" +
		"\t1586  Strix Halo [Radeon Graphics / Radeon 8050S / Radeon 8060S]\n" +
		"10de  NVIDIA Corporation\n\t2684  AD102 [GeForce RTX 4090]\n")
	h.PCI(strixHalo, "amdgpu", "0x030000")
	h.PCIID(strixHalo, "1002", "1586")
	h.Files["/sys/bus/pci/devices/"+strixHalo+"/revision"] = []byte("0xc1\n")
	h.Links["/sys/bus/pci/devices/"+strixHalo] = "../../../devices/pci0000:00/0000:00:08.1/" + strixHalo
	if withLibdrm {
		h.Files["/usr/share/libdrm/amdgpu.ids"] = []byte("# List of AMDGPU IDs\n#\n# Syntax:\n# device_id,\trevision_id,\tproduct_name\n\n" +
			"1586,\tC1,\tAMD Radeon 8060S Graphics\n1586,\tC2,\tAMD Radeon 8050S Graphics\n")
	}
	return h
}

// The specs list the processor's own GPU, marked integrated (and sharing the
// machine's memory), by the name libdrm gives its revision.
func TestTheSpecsListAnIntegratedGPU(t *testing.T) {
	h := strixHaloHost(true)
	gpus := withIntegrated(gpusFromPCI(h), h)
	if len(gpus) != 1 || gpus[0].Model != "AMD Radeon 8060S Graphics" || !gpus[0].Integrated || !gpus[0].UnifiedMemory {
		t.Fatalf("gpus = %+v", gpus)
	}
	data, _ := json.Marshal(gpus[0])
	if !strings.Contains(string(data), `"integrated":true`) || strings.Contains(string(data), "bdf") {
		t.Errorf("json = %s", data)
	}
	// Without libdrm's table: the PCI ID database's name, all its variants.
	h = strixHaloHost(false)
	if gpus := withIntegrated(nil, h); len(gpus) != 1 || gpus[0].Model != "AMD Radeon Graphics / Radeon 8050S / Radeon 8060S" {
		t.Errorf("gpus = %+v", gpus)
	}
}

// A rentable card stays as it was; the integrated GPU is added after it. One
// a vendor tool listed (rocm-smi lists an APU) is marked, not listed twice.
func TestAnIntegratedGPUBesideACard(t *testing.T) {
	h := strixHaloHost(true)
	h.PCI("0000:01:00.0", "nvidia", "0x030000")
	h.PCIID("0000:01:00.0", "10de", "2684")
	gpus := withIntegrated(gpusFromPCI(h), h)
	if len(gpus) != 2 || gpus[0].Model != "NVIDIA GeForce RTX 4090" || gpus[0].Integrated || !gpus[1].Integrated {
		t.Fatalf("gpus = %+v", gpus)
	}
	fromROCm := []GPUInfo{{Model: "AMD GPU", VRAMTotalGB: 96, bdf: strixHalo}}
	gpus = withIntegrated(fromROCm, h)
	if len(gpus) != 1 || !gpus[0].Integrated || gpus[0].Model != "AMD Radeon 8060S Graphics" || gpus[0].VRAMTotalGB != 96 {
		t.Errorf("gpus = %+v", gpus)
	}
}
