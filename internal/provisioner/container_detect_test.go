package provisioner

import (
	"os"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

func TestVFIOImpossibleAndContainerFallback(t *testing.T) {
	h := fakehost.New()
	bdf := "0000:0f:00.0"
	h.PCIID(bdf, "10de", "2e12") // a GB10
	rr := "/sys/bus/pci/devices/" + bdf + "/iommu_group/reserved_regions"
	h.SetFile(rr, []byte("0x00000000 0x000fffff direct\n"))
	h.SetOutput("modinfo -F alias nvgrace-gpu-vfio-pci", "pci:v000010DEd00002941sv*sd*bc*sc*i*\n")

	if !vfioImpossible(h, bdf) {
		t.Error("a GB10 with a 1:1 (direct) RMR and no nvgrace id must be VFIO-impossible")
	}
	if !containerFallback(h, []string{bdf}) {
		t.Error("such a GPU must fall back to container mode")
	}

	// nvgrace carries the id: VFIO works through nvgrace, so no fallback.
	h.SetOutput("modinfo -F alias nvgrace-gpu-vfio-pci", "pci:v000010DEd00002E12sv*sd*bc*sc*i*\n")
	if vfioImpossible(h, bdf) {
		t.Error("with nvgrace carrying 0x2E12, VFIO is possible")
	}
	if containerFallback(h, []string{bdf}) {
		t.Error("a VFIO-capable GPU must not fall back to container mode")
	}

	// No RMR at all: generic vfio-pci works.
	h.SetOutput("modinfo -F alias nvgrace-gpu-vfio-pci", "pci:v000010DEd00002941sv*sd*bc*sc*i*\n")
	h.SetFile(rr, []byte("0x00000000 0x000fffff msi\n"))
	if vfioImpossible(h, bdf) {
		t.Error("without an RMR, generic vfio-pci works, so not impossible")
	}

	// A non-NVIDIA GPU never uses container mode.
	amd := "0000:03:00.0"
	h.PCIID(amd, "1002", "744c")
	if containerFallback(h, []string{amd}) {
		t.Error("a non-NVIDIA GPU must not use container mode")
	}

	// The force override enables it for any NVIDIA GPU (for testing on
	// VFIO-capable hardware), but never for a non-NVIDIA one.
	os.Setenv("GPU_AGENT_FORCE_CONTAINER", "1")
	defer os.Unsetenv("GPU_AGENT_FORCE_CONTAINER")
	if !containerFallback(h, []string{bdf}) {
		t.Error("the force override must enable container mode for an NVIDIA GPU")
	}
	if containerFallback(h, []string{amd}) {
		t.Error("the force override must not enable container mode for a non-NVIDIA GPU")
	}
}

// A Spark in container mode is a Spark all the same: a rental waits for the
// host's own programs on the GPU, and a logged-in desktop is warned before a
// rental closes it -- though no microVM runtime backs the machine.
func TestContainerSparkSeesHostUseAndItsDesktop(t *testing.T) {
	h := fakehost.New()
	bdf := "000f:01:00.0"
	h.PCI(bdf, "nvidia", "0x030200", bdf)
	h.PCIID(bdf, "10de", "2e12")
	for _, pr := range []struct{ pid, comm string }{{"2558", "Xorg"}, {"11435", "llama-server"}} {
		h.Links["/proc/"+pr.pid+"/fd/5"] = "/dev/nvidia0"
		h.Files["/proc/"+pr.pid+"/comm"] = []byte(pr.comm + "\n")
	}
	h.Outputs["loginctl list-sessions --no-legend"] = "     3 1000 ana  seat0 tty2\n"
	h.Outputs["loginctl show-session 3 -p Type"] = "Type=x11\nClass=user\nName=ana\nUser=1000\nState=active\n"
	spec := vmrt.Spec{Arch: "arm64", DataDir: "/var/lib/gpu-agent", GPUs: []string{bdf},
		TotalMemMB: 131072, CPUs: 20, DiskGB: 200, DesktopOnDemand: true}
	p := New(vmrt.NewContainer(h, spec, nil, vmrt.ContainerImageRef, nil, nil), VendorContainerNV, []string{bdf}, true)
	p.host = h

	if p.Runtime() != nil {
		t.Fatal("a container machine has no microVM runtime")
	}
	if u := p.hostUseNow(); !u.Busy() || strings.Join(u.Holders, ", ") != "llama-server (pid 11435)" {
		t.Errorf("the host's own GPU program must hold a container rental back: %+v", u)
	}
	if got := p.loggedInDesktop(); strings.Join(got, ",") != "ana" {
		t.Errorf("the logged-in desktop must be warned before a rental closes it: %v", got)
	}
	if !p.desktopOnDemandLocked() {
		t.Error("the rented notice must carry the Spark's desktop line")
	}
}
