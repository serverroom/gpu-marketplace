package provisioner

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// cpuHost is a Linux KVM machine with no GPU at all and no IOMMU, that has
// everything else: tools, firmware, a base image baked without the NVIDIA
// driver, and a passing test boot (with no GPU) for this version.
func cpuHost(t *testing.T) *fakehost.Host {
	t.Helper()
	h := fakehost.New()
	h.Files["/dev/kvm"] = nil
	h.Files["/usr/share/OVMF/OVMF_CODE_4M.fd"] = nil
	h.Files["/usr/share/OVMF/OVMF_VARS_4M.fd"] = nil
	h.Files[dataDir+"/golden.img"] = nil
	h.Files[filepath.Join(dataDir, "golden.img")] = nil
	golden, _ := json.Marshal(vmrt.GoldenInfo{Base: "resolute-server-cloudimg-amd64.img", Driver: vmrt.NoDriver, Extras: []string{"rdma"}})
	h.Files[dataDir+"/golden.img.json"] = golden
	h.Files[filepath.Join(dataDir, "golden.img")+".json"] = golden
	pass, _ := json.Marshal(vmrt.SelfTestResult{Passed: true, AgentVersion: version, HostGPUs: nil})
	h.Files[vmrt.SelfTestPath(dataDir)] = pass
	h.Files["/proc/meminfo"] = []byte("MemTotal:       65536000 kB\n")
	h.Files[dataDir] = nil
	h.Outputs["df --output=avail"] = " Avail\n  900G\n"
	// An onboard BMC display controller is not a GPU anyone rents.
	h.PCI("0000:03:00.0", "ast", "0x030000")
	h.Files["/sys/bus/pci/devices/0000:03:00.0/vendor"] = []byte("0x1a03\n")
	return h
}

func TestAMachineWithoutAGPUHosts(t *testing.T) {
	h := cpuHost(t)
	rep := Preflight(h, "linux", spec(), version)
	if len(rep.Reasons) != 0 || rep.Vendor != VendorNone || rep.GPUCount != 0 || len(rep.BDFs) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	noRelay(t)
	p := Detect(h, "linux", "amd64", dataDir, version)
	c := p.Capability()
	if !c.Ready || c.Kind != KindQEMU || c.GPUCount == nil || *c.GPUCount != 0 {
		t.Fatalf("capability = %+v", c)
	}
	data, _ := json.Marshal(c)
	if !strings.Contains(string(data), `"gpu_count":0`) || !strings.Contains(string(data), `"kind":"qemu"`) {
		t.Errorf("capability JSON = %s", data)
	}
	// It rents: nothing about a missing GPU stops a rental.
	m := &fakeMachine{stopRes: clean()}
	p.machine, p.async = m, false
	withFakeForward(t, nil)
	if err := p.Provision("R1", renterKey(t)); err != nil || p.Status() != StatusRented {
		t.Fatalf("Provision = %v, status %s", err, p.Status())
	}
}

func TestTheIOMMUAndTheTestBootWithoutAGPU(t *testing.T) {
	h := cpuHost(t)
	delete(h.Files, vmrt.SelfTestPath(dataDir))
	got := reasons(Preflight(h, "linux", spec(), version))
	if !strings.Contains(got, "has not yet booted a test rental; run 'sudo gpu-agent check --boot'") || strings.Contains(got, "GPU") {
		t.Errorf("reasons = %s", got)
	}
}

// A machine with a GPU the driver cannot see is refused, not rented as a
// machine without one.
func TestAnNVIDIAGPUWithoutItsDriverIsNotACPUMachine(t *testing.T) {
	h := cpuHost(t)
	h.PCI("0000:41:00.0", "", "0x030200")
	h.Files["/sys/bus/pci/devices/0000:41:00.0/vendor"] = []byte("0x10de\n")
	rep := Preflight(h, "linux", spec(), version)
	got := reasons(rep)
	if rep.Vendor != VendorNVIDIA || rep.GPUCount != 1 || !strings.Contains(got, "NVIDIA GPU (PCI 0000:41:00.0), but nvidia-smi does not see it") ||
		!strings.Contains(got, "IOMMU") {
		t.Errorf("vendor %s count %d reasons %s", rep.Vendor, rep.GPUCount, got)
	}
	if !HasNVIDIAGPU(h) {
		t.Errorf("runtime prepare would bake no driver for a machine with an NVIDIA GPU")
	}
	noRelay(t)
	c := Detect(h, "linux", "amd64", dataDir, version).Capability()
	if c.Ready || c.Kind != KindQEMUVFIO || c.GPUCount == nil || *c.GPUCount != 1 {
		t.Errorf("capability = %+v", c)
	}
}

// A machine that gains a GPU after its image was baked without the driver is
// told to rebuild -- and its test boot without a GPU no longer counts.
func TestAMachineThatGainedAGPURebuilds(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA L4, 24564\n")
	golden, _ := json.Marshal(vmrt.GoldenInfo{Base: "resolute-server-cloudimg-amd64.img", Driver: vmrt.NoDriver})
	h.Files[dataDir+"/golden.img.json"] = golden
	got := reasons(Preflight(h, "linux", spec(), version))
	if !strings.Contains(got, "built without the NVIDIA driver") || !strings.Contains(got, "runtime prepare") {
		t.Errorf("reasons = %s", got)
	}
	cpuPass := &vmrt.SelfTestResult{Passed: true, AgentVersion: version}
	if p := vmrt.SelfTestProblem(cpuPass, version, []string{gpu}); !strings.Contains(p, "has a GPU now") {
		t.Errorf("a test boot without the GPU counted: %q", p)
	}
}

func TestGPUCountIsOnlyReportedWhereTheAgentHosts(t *testing.T) {
	for goos, want := range map[string]string{"darwin": "qemu-vfio", "windows": "qemu-vfio"} {
		c := Detect(fakehost.New(), goos, "arm64", dataDir, version).Capability()
		if c.GPUCount != nil || c.Kind != want {
			t.Errorf("%s: gpu_count %v kind %s", goos, c.GPUCount, c.Kind)
		}
	}
	noRelay(t)
	h := goodHost(t, "00000000:01:00.0, NVIDIA L4, 24564\n")
	h.Files["/proc/meminfo"] = []byte("MemTotal:       65536000 kB\n")
	h.Files[dataDir] = nil
	h.Outputs["df --output=avail"] = " Avail\n  900G\n"
	c := Detect(h, "linux", "amd64", dataDir, version).Capability()
	if c.Kind != KindQEMUVFIO || c.GPUCount == nil || *c.GPUCount != 1 || !c.Ready {
		t.Errorf("GPU machine: %+v", c)
	}
}

func TestCheckBakeDriver(t *testing.T) {
	for flag, ok := range map[string]bool{"": true, "580-server": true, "none": true, "latest": false, "580; reboot": false} {
		if err := CheckBakeDriver(flag); (err == nil) != ok {
			t.Errorf("CheckBakeDriver(%q) = %v", flag, err)
		}
	}
}
