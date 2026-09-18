package vmrt

import (
	"reflect"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// A desktop: an Intel iGPU drives the screen, an NVIDIA card and an AMD card do
// compute. Each holder must be pinned on the GPU it really holds.
func desktop() *fakehost.Host {
	h := fakehost.New()
	h.PCI("0000:00:02.0", "i915", "0x030000")
	h.PCIID("0000:00:02.0", "8086", "46a6")
	h.Files["/sys/bus/pci/devices/0000:00:02.0/drm/card0"] = nil
	h.Files["/sys/bus/pci/devices/0000:00:02.0/drm/renderD128"] = nil
	h.PCI("0000:01:00.0", "nvidia", "0x030000")
	h.PCIID("0000:01:00.0", "10de", "2684")
	h.Files["/sys/bus/pci/devices/0000:01:00.0/drm/card1"] = nil
	h.PCI("0000:02:00.0", "amdgpu", "0x030000")
	h.PCIID("0000:02:00.0", "1002", "744c")
	h.Files["/sys/bus/pci/devices/0000:02:00.0/drm/card2"] = nil
	h.Files["/sys/bus/pci/devices/0000:02:00.0/drm/renderD130"] = nil
	return h
}

func holds(h *fakehost.Host, pid, comm, dev string) {
	h.Links["/proc/"+pid+"/fd/9"] = dev
	h.Files["/proc/"+pid+"/comm"] = []byte(comm + "\n")
}

func TestHoldersArePinnedOnTheGPUTheyHold(t *testing.T) {
	h := desktop()
	holds(h, "1001", "Xorg", "/dev/dri/card0")
	holds(h, "1002", "gnome-shell", "/dev/dri/renderD128")
	holds(h, "2001", "python3", "/dev/nvidia0")
	holds(h, "2002", "nvidia-persistenced", "/dev/nvidiactl")
	holds(h, "3001", "ollama", "/dev/kfd")
	holds(h, "4001", "bash", "/dev/pts/0")

	for gpu, want := range map[string][]string{
		"0000:00:02.0": {"Xorg (pid 1001)", "gnome-shell (pid 1002)"},
		"0000:01:00.0": {"python3 (pid 2001)"},
		"0000:02:00.0": {"ollama (pid 3001)"},
	} {
		if got := GPUHolders(h, []string{gpu}); !reflect.DeepEqual(got, want) {
			t.Errorf("GPUHolders(%s) = %v, want %v", gpu, got, want)
		}
	}
	desktop, other := ClassifyGPUHolders(h, []string{"0000:00:02.0"})
	if len(desktop) != 2 || len(other) != 0 {
		t.Errorf("the iGPU's desktop was not classed as the desktop: desktop %v, other %v", desktop, other)
	}
}

// A GPU passed through to the provider's own VM is held through its VFIO group
// node, whatever its make.
func TestAVFIOGroupHolderHoldsTheGPU(t *testing.T) {
	h := desktop()
	h.Links["/sys/bus/pci/devices/0000:02:00.0/iommu_group"] = "../../../kernel/iommu_groups/27"
	holds(h, "5001", "qemu-system-x86", "/dev/vfio/27")
	holds(h, "5002", "qemu-system-x86", "/dev/vfio/vfio")
	if got := GPUHolders(h, []string{"0000:01:00.0"}); len(got) != 0 {
		t.Errorf("the card nobody holds: %v", got)
	}
	if got := GPUHolders(h, []string{"0000:02:00.0"}); !reflect.DeepEqual(got, []string{"qemu-system-x86 (pid 5001)"}) {
		t.Errorf("GPUHolders(AMD) = %v", got)
	}
}

// A discrete card's other functions go with it: its audio, and on Turing its
// USB-C controller and UCSI. An APU's slot also holds the host's USB and its
// security processor (PSP); those are not the GPU's to give away.
func TestGroupProblemsLetOnlyAGraphicsCardsOwnFunctionsGo(t *testing.T) {
	h := fakehost.New()
	card := []string{"0000:01:00.0", "0000:01:00.1", "0000:01:00.2", "0000:01:00.3"}
	for i, class := range []string{"0x030000", "0x040300", "0x0c0330", "0x0c8000"} {
		h.PCI(card[i], "", class, card...)
	}
	if p := GroupProblems(h, []string{card[0]}); len(p) != 0 {
		t.Errorf("a Turing card's own functions were a problem: %v", p)
	}

	apu := []string{"0000:c4:00.0", "0000:c4:00.1", "0000:c4:00.2", "0000:c4:00.3"}
	for i, class := range []string{"0x030000", "0x040300", "0x108000", "0x0c0330"} {
		h.PCI(apu[i], "", class, apu...)
	}
	p := GroupProblems(h, []string{apu[0]})
	if len(p) != 1 || !strings.Contains(p[0], "0000:c4:00.2") {
		t.Errorf("the APU's security processor would go to a tenant: %v", p)
	}
}

// systemd-logind hands a desktop its DRM node and keeps it, and a copy sits in
// systemd's descriptor store: neither uses the GPU. And an NVIDIA GPU is its
// driver's /dev/nvidia* only, as before other makes were rented -- a DGX
// Spark's DRM node is held by logind whenever its desktop is up.
func TestSessionPlumbingIsNoHolder(t *testing.T) {
	h := desktop()
	h.Files["/sys/bus/pci/devices/0000:01:00.0/drm/card1"] = nil
	holds(h, "1", "systemd", "/dev/dri/card2")
	holds(h, "812", "systemd-logind", "/dev/dri/card2")
	holds(h, "813", "systemd-logind", "/dev/dri/card1")
	holds(h, "2558", "Xorg", "/dev/dri/card1")
	if got := GPUHolders(h, []string{"0000:02:00.0"}); len(got) != 0 {
		t.Errorf("GPUHolders(AMD) = %v; logind and systemd are nobody's workload", got)
	}
	if got := GPUHolders(h, []string{"0000:01:00.0"}); len(got) != 0 {
		t.Errorf("GPUHolders(NVIDIA) = %v; only /dev/nvidia* holds an NVIDIA GPU", got)
	}
}

func TestAnIdleGPUHasNoHolders(t *testing.T) {
	h := desktop()
	holds(h, "1001", "Xorg", "/dev/dri/card0")
	if got := GPUHolders(h, []string{"0000:01:00.0", "0000:02:00.0"}); len(got) != 0 {
		t.Errorf("idle GPUs reported held by %v", got)
	}
}
