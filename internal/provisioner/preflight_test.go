package provisioner

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

const (
	gpu     = "0000:01:00.0"
	version = "v0.1.7"
	dataDir = "/var/lib/gpu-agent"
)

func spec() vmrt.Spec {
	return vmrt.Spec{Arch: "amd64", DataDir: dataDir, GoldenImage: dataDir + "/golden.img", TotalMemMB: 32768, CPUs: 12, DiskGB: 100}
}

// goodHost is a machine that passes everything: KVM, IOMMU, one discrete GPU in
// a group of its own, the tools and firmware, a base image, and a passing test
// boot for this agent version and this GPU.
func goodHost(t *testing.T, gpuLine string) *fakehost.Host {
	t.Helper()
	h := fakehost.New()
	h.Files["/dev/kvm"] = nil
	h.Files["/sys/kernel/iommu_groups/13"] = nil
	h.Outputs["nvidia-smi --query-gpu=pci.bus_id,name"] = gpuLine
	h.PCI(gpu, "nvidia", "0x030200", gpu)
	h.PCI("000f:01:00.0", "nvidia", "0x030200", "000f:01:00.0")
	h.Files["/usr/share/OVMF/OVMF_CODE_4M.fd"] = nil
	h.Files["/usr/share/OVMF/OVMF_VARS_4M.fd"] = nil
	h.Files[dataDir+"/golden.img"] = nil
	h.Files[filepath.Join(dataDir, "golden.img")] = nil // Detect joins with the OS separator
	golden, _ := json.Marshal(vmrt.GoldenInfo{Base: "resolute-server-cloudimg-amd64.img"})
	h.Files[dataDir+"/golden.img.json"] = golden
	h.Files[filepath.Join(dataDir, "golden.img")+".json"] = golden
	bdf := NormalizeBDF(strings.Split(gpuLine, ",")[0])
	pass, _ := json.Marshal(vmrt.SelfTestResult{Passed: true, AgentVersion: version, HostGPUs: []string{bdf}})
	h.Files[vmrt.SelfTestPath(dataDir)] = pass
	return h
}

func reasons(rep HostReport) string { return strings.Join(rep.Reasons, " | ") }

// A machine that baked its image under an older agent keeps that image across the
// upgrade. It must not rent out the old Ubuntu, nor an image nobody can name.
func TestPreflightRefusesABaseImageFromAnotherRelease(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA CMP 170HX, 8192\n")
	old, _ := json.Marshal(vmrt.GoldenInfo{Base: "noble-server-cloudimg-amd64.img"})
	h.Files[dataDir+"/golden.img.json"] = old
	if r := reasons(Preflight(h, "linux", spec(), version)); !strings.Contains(r, "built from noble-server-cloudimg-amd64.img") ||
		!strings.Contains(r, "runtime prepare") {
		t.Errorf("a 24.04 base image was accepted: %q", r)
	}
	delete(h.Files, dataDir+"/golden.img.json")
	if r := reasons(Preflight(h, "linux", spec(), version)); !strings.Contains(r, "does not say which Ubuntu image") {
		t.Errorf("a base image with no record was accepted: %q", r)
	}
}

func TestPreflightReadyHost(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA CMP 170HX, 8192\n")
	rep := Preflight(h, "linux", spec(), version)
	if len(rep.Reasons) != 0 {
		t.Fatalf("a good host is not ready: %s", reasons(rep))
	}
	if rep.Vendor != VendorNVIDIA || strings.Join(rep.BDFs, " ") != gpu || rep.Firmware.Code == "" {
		t.Errorf("report = %+v", rep)
	}
}

// A DGX Spark: nvidia-smi says [N/A] for a GB10, and that means the machine's
// pool is its memory. It is a host like any other.
func TestPreflightAcceptsGB10UnifiedMemory(t *testing.T) {
	h := goodHost(t, "0000000F:01:00.0, NVIDIA GB10, [N/A]\n")
	rep := Preflight(h, "linux", spec(), version)
	if len(rep.Reasons) != 0 || !rep.Unified {
		t.Fatalf("GB10 not accepted as unified: unified=%v %s", rep.Unified, reasons(rep))
	}
}

func TestPreflightRefusesUnknownGPUWithNoMemory(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA Mystery, [N/A]\n")
	rep := Preflight(h, "linux", spec(), version)
	if !strings.Contains(reasons(rep), "NVIDIA Mystery") || rep.Unified {
		t.Fatalf("reasons = %s", reasons(rep))
	}
}

func TestPreflightNamesWhatToRunNext(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA L4, 24564\n")
	h.Tools = map[string]bool{"qemu-img": true}
	delete(h.Files, dataDir+"/golden.img")
	all := reasons(Preflight(h, "linux", spec(), version))
	for _, want := range []string{"qemu-system-x86_64", "runtime prepare --install-deps", "base image has not been built"} {
		if !strings.Contains(all, want) {
			t.Errorf("reasons missing %q: %s", want, all)
		}
	}
	if strings.Contains(all, "check --boot") {
		t.Errorf("asked for a test boot before the runtime exists: %s", all)
	}
}

func TestPreflightWantsATestBootForThisVersion(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA L4, 24564\n")
	if all := reasons(Preflight(h, "linux", spec(), "v0.1.8")); !strings.Contains(all, "check --boot") {
		t.Errorf("a test boot from another version counted: %s", all)
	}
	delete(h.Files, vmrt.SelfTestPath(dataDir))
	if all := reasons(Preflight(h, "linux", spec(), version)); !strings.Contains(all, "has not yet booted a test rental") {
		t.Errorf("no test boot counted as ready: %s", all)
	}
}

func TestPreflightRefusesAGPUThatSharesItsGroup(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA L4, 24564\n")
	h.PCI(gpu, "nvidia", "0x030200", gpu, "0000:02:00.0")
	h.PCI("0000:02:00.0", "ixgbe", "0x020000")
	if all := reasons(Preflight(h, "linux", spec(), version)); !strings.Contains(all, "0000:02:00.0") {
		t.Errorf("a shared IOMMU group was not reported: %s", all)
	}
}

func TestPreflightNoKVMNoIOMMUNoGPU(t *testing.T) {
	h := fakehost.New()
	all := reasons(Preflight(h, "linux", spec(), version))
	for _, want := range []string{"/dev/kvm", "IOMMU", "no NVIDIA or AMD GPU"} {
		if !strings.Contains(all, want) {
			t.Errorf("reasons missing %q: %s", want, all)
		}
	}
}

func TestPreflightNonLinux(t *testing.T) {
	for goos, want := range map[string]string{"windows": "runs windows", "darwin": "Apple Silicon"} {
		rep := Preflight(fakehost.New(), goos, spec(), version)
		if len(rep.Reasons) != 1 || !strings.Contains(rep.Reasons[0], want) {
			t.Errorf("%s: reasons = %v", goos, rep.Reasons)
		}
	}
}

func TestPreflightSmallMachine(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA L4, 24564\n")
	s := spec()
	s.TotalMemMB = 4096
	s.DiskGB = 5
	all := reasons(Preflight(h, "linux", s, version))
	if !strings.Contains(all, "4096 MB") || !strings.Contains(all, "20 GB free") {
		t.Errorf("reasons = %s", all)
	}
}

func TestDetectBuildsACapability(t *testing.T) {
	h := goodHost(t, "0000000F:01:00.0, NVIDIA GB10, [N/A]\n")
	h.Files["/proc/meminfo"] = []byte("MemTotal:       128000000 kB\n")
	h.Files[dataDir] = nil
	h.Outputs["df --output=avail"] = " Avail\n  900G\n"
	p := Detect(h, "linux", "amd64", dataDir, version)
	c := p.Capability()
	if !c.Ready || !c.UnifiedMemory || c.Kind != KindQEMUVFIO || c.AgentVersion != version || c.VMUser != "root" {
		t.Fatalf("capability = %+v", c)
	}
	if p.Runtime() == nil || p.Runtime().Spec().DiskGB != 500 || p.Runtime().Spec().TotalMemMB != 125000 {
		t.Errorf("runtime spec = %+v", p.Runtime().Spec())
	}
}
