package autosetup

import (
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// cpuMachine is a Linux KVM machine without a GPU and without an IOMMU,
// fresh: no runtime, no image, no test boot.
func cpuMachine(t *testing.T) *fakehost.Host {
	t.Helper()
	h := machine(t)
	delete(h.Outputs, "nvidia-smi --query-gpu=pci.bus_id,name")
	delete(h.Outputs, "nvidia-smi --query-gpu=driver_version")
	h.DeleteFile("/sys/bus/pci/devices/" + gpu + "/class") // no GPU on the PCI bus either
	h.DeleteFile("/sys/kernel/iommu_groups/13")
	noTools(h, true)
	noImage(h)
	noTestBoot(h)
	return h
}

// A machine without a GPU sets itself up like any other: the runtime, an image
// with no NVIDIA driver, and its own test boot -- no driver matching, no VFIO.
func TestAMachineWithoutAGPUSetsItselfUp(t *testing.T) {
	h := cpuMachine(t)
	r := newRig(t, h)
	r.run()
	if got := r.ran(); got != "deps,image,test-boot" {
		t.Fatalf("steps %q", got)
	}
	if len(r.drivers) != 1 || r.drivers[0] != vmrt.NoDriver {
		t.Errorf("image built with %v, want no driver", r.drivers)
	}
	a := r.last()
	if a == nil || !a.Passed || a.Driver != vmrt.NoDriver || a.DriverSource != vmrt.DriverNoGPU || a.HostDriver != "no NVIDIA GPU" {
		t.Errorf("attempt = %+v", a)
	}
	c := r.d.Prov.Capability()
	if !c.Ready || c.Kind != provisioner.KindQEMU || c.GPUCount == nil || *c.GPUCount != 0 {
		t.Errorf("capability after the setup = %+v", c)
	}
	for _, line := range r.reasons() {
		if strings.Contains(line, "IOMMU") || strings.Contains(line, "GPU") {
			t.Errorf("a machine without a GPU was told about a GPU: %q", line)
		}
	}
}

// Every report during and after the setup still carries what the machine is:
// its identity and its pair half, beside the progress line.
func TestSetupReportsCarryTheIdentityAndThePairHalf(t *testing.T) {
	h := machine(t)
	noImage(h)
	r := newRig(t, h)
	r.run()
	if len(r.reports) < 2 {
		t.Fatalf("reports = %v", r.reasons())
	}
	for i, c := range r.reports {
		if c.Identity == nil || c.Interconnect == nil || !c.Interconnect.Supported {
			t.Errorf("report %d (%q) has no identity or pair half: %+v / %+v", i, strings.Join(c.Reasons, "; "), c.Identity, c.Interconnect)
		}
	}
}
