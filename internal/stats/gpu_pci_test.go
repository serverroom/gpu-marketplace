package stats

import (
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// A machine with no vendor tool still registers with its GPUs named, and never
// with its BMC's display.
func TestGPUsFromPCI(t *testing.T) {
	h := fakehost.New()
	h.Files["/usr/share/misc/pci.ids"] = []byte("8086  Intel Corporation\n\t56a0  DG2 [Arc A770]\n")
	h.PCI("0000:03:00.0", "i915", "0x030000")
	h.PCIID("0000:03:00.0", "8086", "56a0")
	h.PCI("0000:04:00.0", "", "0x038000")
	h.PCIID("0000:04:00.0", "1002", "74a1")
	h.PCI("0000:05:00.0", "ast", "0x030000")
	h.PCIID("0000:05:00.0", "1a03", "2000")
	h.PCI("0000:00:02.0", "i915", "0x030000")
	h.PCIID("0000:00:02.0", "8086", "46a6")
	gpus := gpusFromPCI(h)
	if len(gpus) != 2 || gpus[0].Model != "Intel Arc A770" || gpus[1].Model != "AMD GPU 1002:74a1" {
		t.Errorf("gpus = %+v", gpus)
	}
	if gpus[0].VRAMTotalGB != 0 {
		t.Errorf("memory was invented: %+v", gpus[0])
	}
}
