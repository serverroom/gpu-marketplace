package provisioner

import (
	"encoding/json"
	"errors"
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

// A DGX Spark straight out of the box draws its desktop on the GB10. The check
// must say so, and name the command that fixes it, before a test boot finds out.
func TestPreflightNamesADesktopOnTheGPU(t *testing.T) {
	h := goodHost(t, "00000000:0F:01.0, NVIDIA GB10, [N/A]")
	h.Links["/proc/2558/fd/7"] = "/dev/nvidia0"
	h.Files["/proc/2558/comm"] = []byte("Xorg\n")
	h.Links["/proc/2411/fd/7"] = "/dev/nvidia0"
	h.Files["/proc/2411/comm"] = []byte("nvidia-persiste\n")
	rep := Preflight(h, "linux", spec(), version)
	got := reasons(rep)
	if !strings.Contains(got, "desktop is running on the GPU (Xorg (pid 2558))") ||
		!strings.Contains(got, "sudo gpu-agent runtime prepare --headless") {
		t.Errorf("reasons = %q", got)
	}
	if strings.Contains(got, "persiste") {
		t.Errorf("nvidia-persistenced was reported: %q", got)
	}
}

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

// sparkGoodHost is a DGX Spark that passes everything, with its desktop up on
// the GB10 under gdm (display-manager.service).
func sparkGoodHost(t *testing.T) *fakehost.Host {
	t.Helper()
	h := goodHost(t, "0000000F:01:00.0, NVIDIA GB10, [N/A]\n")
	for name, v := range map[string]string{"sys_vendor": "NVIDIA", "product_name": "NVIDIA DGX Spark", "product_family": "DGX Spark"} {
		h.Files["/sys/class/dmi/id/"+name] = []byte(v + "\n")
	}
	h.Files["/usr/share/AAVMF/AAVMF_CODE.fd"] = nil
	h.Files["/usr/share/AAVMF/AAVMF_VARS.fd"] = nil
	golden, _ := json.Marshal(vmrt.GoldenInfo{Base: "resolute-server-cloudimg-arm64.img"})
	h.Files[dataDir+"/golden.img.json"] = golden
	h.Files[filepath.Join(dataDir, "golden.img")+".json"] = golden
	h.Links["/proc/2558/fd/7"] = "/dev/nvidia0"
	h.Files["/proc/2558/comm"] = []byte("Xorg\n")
	h.Outputs["systemctl show -p LoadState --value display-manager.service"] = "loaded\n"
	return h
}

func sparkSpec() vmrt.Spec {
	s := spec()
	s.Arch = "arm64"
	return s
}

// On a confirmed DGX Spark the desktop is not a reason: it closes while the
// machine is rented or tested and comes back after.
func TestPreflightASparksDesktopIsNotAReason(t *testing.T) {
	h := sparkGoodHost(t)
	rep := Preflight(h, "linux", sparkSpec(), version)
	if len(rep.Reasons) != 0 {
		t.Fatalf("a Spark with its desktop up is not ready: %s", reasons(rep))
	}
	if !rep.DesktopOnDemand || rep.Identity == nil || !rep.Identity.ConfirmedDGXSpark {
		t.Errorf("DesktopOnDemand=%v identity=%+v", rep.DesktopOnDemand, rep.Identity)
	}
}

// ... unless the agent could not close it: no display manager runs it.
func TestPreflightASparkDesktopWithoutADisplayManagerIsAReason(t *testing.T) {
	h := sparkGoodHost(t)
	h.Fail["systemctl is-active --quiet display-manager.service"] = errors.New("inactive")
	if r := reasons(Preflight(h, "linux", sparkSpec(), version)); !strings.Contains(r, "desktop is running on the GPU") {
		t.Errorf("reasons = %q", r)
	}
}

// A Spark made headless on purpose stays that way: nothing on demand, and a
// desktop that is somehow back on the GPU is the usual reason.
func TestPreflightAHeadlessSparkIsLeftAlone(t *testing.T) {
	h := sparkGoodHost(t)
	h.Files[vmrt.HeadlessPath(dataDir)] = []byte(`{"previous_default":"graphical.target"}`)
	rep := Preflight(h, "linux", sparkSpec(), version)
	if rep.DesktopOnDemand || !strings.Contains(reasons(rep), "runtime prepare --headless") {
		t.Errorf("DesktopOnDemand=%v reasons=%q", rep.DesktopOnDemand, reasons(rep))
	}
}

// Another GB10 machine is not a Spark: its desktop is the v0.1.9 reason.
func TestPreflightAnotherGB10MachineKeepsTheDesktopReason(t *testing.T) {
	h := sparkGoodHost(t)
	h.Files["/sys/class/dmi/id/sys_vendor"] = []byte("ASUSTeK COMPUTER INC.\n")
	h.Files["/sys/class/dmi/id/product_name"] = []byte("Ascent GX10\n")
	h.Files["/sys/class/dmi/id/product_family"] = []byte("\n")
	rep := Preflight(h, "linux", sparkSpec(), version)
	if rep.DesktopOnDemand || !strings.Contains(reasons(rep), "runtime prepare --headless") {
		t.Errorf("DesktopOnDemand=%v reasons=%q", rep.DesktopOnDemand, reasons(rep))
	}
	if rep.Identity == nil || rep.Identity.SysVendor != "ASUSTeK COMPUTER INC." || rep.Identity.ConfirmedDGXSpark {
		t.Errorf("identity = %+v", rep.Identity)
	}
}

func kinds(rep HostReport) map[ReasonKind]int {
	k := map[ReasonKind]int{}
	for _, f := range rep.Findings {
		k[f.Kind]++
	}
	return k
}

// Every reason carries who can fix it, and Findings mirror Reasons.
func TestPreflightKindsTheReasons(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA L4, 24564\n")
	h.Tools = map[string]bool{"qemu-img": true}
	delete(h.Files, "/usr/share/OVMF/OVMF_CODE_4M.fd")
	delete(h.Files, dataDir+"/golden.img")
	delete(h.Files, filepath.Join(dataDir, "golden.img"))
	delete(h.Files, "/dev/kvm")
	rep := Preflight(h, "linux", spec(), version)
	if k := kinds(rep); k[ReasonHuman] != 1 || k[ReasonTools] != 2 || k[ReasonImage] != 1 || k[ReasonTestBoot] != 0 {
		t.Errorf("kinds = %v (%s)", k, reasons(rep))
	}
	if len(rep.Findings) != len(rep.Reasons) {
		t.Fatalf("%d findings for %d reasons", len(rep.Findings), len(rep.Reasons))
	}
	for i := range rep.Reasons {
		if rep.Findings[i].Text != rep.Reasons[i] {
			t.Errorf("finding %d = %q, reason %q", i, rep.Findings[i].Text, rep.Reasons[i])
		}
	}

	h = goodHost(t, "00000000:01:00.0, NVIDIA L4, 24564\n")
	if k := kinds(Preflight(h, "linux", spec(), "v0.1.10")); k[ReasonTestBoot] != 1 || len(k) != 1 {
		t.Errorf("an old test boot: kinds = %v", k)
	}
}

// A base image with another driver than the host's is rebuilt; one a person
// chose (--driver) is not second-guessed.
func TestPreflightWantsTheHostsDriverInTheImage(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA CMP 170HX, 8192\n")
	h.Outputs["nvidia-smi --query-gpu=driver_version"] = "580.82.07\n"
	h.Outputs["modinfo -F license nvidia"] = "NVIDIA\n"
	golden, _ := json.Marshal(vmrt.GoldenInfo{Base: "resolute-server-cloudimg-amd64.img", Driver: "580-server-open"})
	h.Files[filepath.Join(dataDir, "golden.img")+".json"] = golden
	h.Files[dataDir+"/golden.img.json"] = golden
	rep := Preflight(h, "linux", spec(), version)
	if k := kinds(rep); k[ReasonImage] != 1 || !strings.Contains(reasons(rep), "this machine runs 580-server") {
		t.Errorf("reasons = %q", reasons(rep))
	}
	pinned, _ := json.Marshal(vmrt.GoldenInfo{Base: "resolute-server-cloudimg-amd64.img", Driver: "580-server-open", DriverSource: vmrt.DriverFromFlag})
	h.Files[filepath.Join(dataDir, "golden.img")+".json"] = pinned
	h.Files[dataDir+"/golden.img.json"] = pinned
	if rep := Preflight(h, "linux", spec(), version); len(rep.Reasons) != 0 {
		t.Errorf("a driver chosen with --driver was flagged: %s", reasons(rep))
	}
}

func TestDetectReportsIdentityAndDesktopOnDemand(t *testing.T) {
	h := sparkGoodHost(t)
	h.Files["/proc/meminfo"] = []byte("MemTotal:       128000000 kB\n")
	h.Files[dataDir] = nil
	h.Outputs["df --output=avail"] = " Avail\n  900G\n"
	p := Detect(h, "linux", "arm64", dataDir, version)
	c := p.Capability()
	if !c.Ready || c.Identity == nil || !c.Identity.ConfirmedDGXSpark || c.Identity.ProductName != "NVIDIA DGX Spark" {
		t.Fatalf("capability = %+v identity = %+v", c, c.Identity)
	}
	if !p.Runtime().Spec().DesktopOnDemand {
		t.Error("the runtime of a Spark does not close its desktop on demand")
	}
	data, _ := json.Marshal(c)
	if !strings.Contains(string(data), `"identity":{"sys_vendor":"NVIDIA","product_name":"NVIDIA DGX Spark","product_family":"DGX Spark","confirmed_dgx_spark":true}`) {
		t.Errorf("capability JSON = %s", data)
	}
}
