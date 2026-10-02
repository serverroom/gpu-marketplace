package stats

import (
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// An RK3588's Mali-G610 is a device-tree node, not a PCI device: the specs list
// it by name, marked integrated, so the marketplace says it is not included.
func TestTheSpecsListAnRK3588sMali(t *testing.T) {
	h := fakehost.New()
	h.Files["/proc/device-tree/gpu@fb000000/compatible"] = []byte("rockchip,rk3588-mali\x00arm,mali-valhall-csf\x00")
	h.Files["/proc/device-tree/gpu@fb000000/status"] = []byte("okay\x00")
	gpus := withIntegrated(nil, h)
	if len(gpus) != 1 || gpus[0].Model != "Arm Mali-G610 MP4" || !gpus[0].Integrated || !gpus[0].UnifiedMemory {
		t.Fatalf("gpus = %+v", gpus)
	}
}

// A Mali the table does not know by SoC is named by its family; a GPU node the
// board switched off, and a node that is no GPU this knows, are left out.
func TestSoCGPUsByFamilyAndStatus(t *testing.T) {
	h := fakehost.New()
	h.Files["/proc/device-tree/soc/gpu@1800000/compatible"] = []byte("vendor,newsoc-mali\x00arm,mali-bifrost\x00")
	h.Files["/proc/device-tree/gpu@fde60000/compatible"] = []byte("rockchip,rk3568-mali\x00arm,mali-bifrost\x00")
	h.Files["/proc/device-tree/gpu@fde60000/status"] = []byte("disabled\x00")
	h.Files["/proc/device-tree/gpu@2000000/compatible"] = []byte("vendor,display-controller\x00")
	gpus := socGPUs(h)
	if len(gpus) != 1 || gpus[0].Model != "Arm Mali GPU" {
		t.Fatalf("gpus = %+v", gpus)
	}
}

// No device tree (an x86 machine, a GB10 on ACPI): nothing added.
func TestNoDeviceTreeNoSoCGPU(t *testing.T) {
	if gpus := socGPUs(fakehost.New()); len(gpus) != 0 {
		t.Fatalf("gpus = %+v", gpus)
	}
}

// The disk is read where the rentals' disks live, or the nearest parent of it
// that exists on a machine not yet set up.
func TestTheDiskIsReadWhereTheRentalsLive(t *testing.T) {
	exists := func(have ...string) func(string) bool {
		return func(dir string) bool {
			for _, d := range have {
				if d == dir {
					return true
				}
			}
			return false
		}
	}
	if got := existingDir("/mnt/nvme/gpu-agent", exists("/mnt/nvme/gpu-agent", "/mnt/nvme")); got != "/mnt/nvme/gpu-agent" {
		t.Errorf("set up: %s", got)
	}
	if got := existingDir("/mnt/nvme/gpu-agent", exists("/mnt/nvme")); got != "/mnt/nvme" {
		t.Errorf("not yet made: %s", got)
	}
	if got := existingDir("/var/lib/gpu-agent", exists()); got != "/" {
		t.Errorf("nothing: %s", got)
	}
	if got := existingDir("", exists()); got != "/" {
		t.Errorf("empty: %s", got)
	}
}
