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

// baseHost is a machine with everything but GPUs: KVM, the IOMMU on, the tools
// and firmware, and a base image baked for these makes.
func baseHost(vendors ...string) *fakehost.Host {
	h := fakehost.New()
	h.Files["/dev/kvm"] = nil
	h.Files["/sys/kernel/iommu_groups/13"] = nil
	h.Files["/usr/share/OVMF/OVMF_CODE_4M.fd"] = nil
	h.Files["/usr/share/OVMF/OVMF_VARS_4M.fd"] = nil
	h.Files[dataDir+"/golden.img"] = nil
	h.Files[filepath.Join(dataDir, "golden.img")] = nil // Detect joins with the OS separator
	golden, _ := json.Marshal(vmrt.GoldenInfo{Base: "resolute-server-cloudimg-amd64.img", Vendors: vendors})
	h.Files[dataDir+"/golden.img.json"] = golden
	h.Files[filepath.Join(dataDir, "golden.img")+".json"] = golden
	return h
}

// addGPU puts a GPU in an IOMMU group of its own.
func addGPU(h *fakehost.Host, bdf, driver, vendor, device string) {
	h.PCI(bdf, driver, "0x030200", bdf)
	h.PCIID(bdf, vendor, device)
}

// passedTestBoot records a passing test boot for this agent version and these GPUs.
func passedTestBoot(h *fakehost.Host, gpus ...vmrt.GuestGPU) {
	res := vmrt.SelfTestResult{Passed: true, AgentVersion: version, GPUs: gpus}
	for _, g := range gpus {
		res.HostGPUs = append(res.HostGPUs, g.HostBDF)
	}
	pass, _ := json.Marshal(res)
	h.Files[vmrt.SelfTestPath(dataDir)] = pass
}

// goodHost is a machine that passes everything: one NVIDIA GPU in a group of its
// own, as nvidia-smi on the host describes it, and a passing test boot.
func goodHost(t *testing.T, gpuLine string) *fakehost.Host {
	t.Helper()
	h := baseHost("10de")
	h.Outputs["nvidia-smi --query-gpu=pci.bus_id,name"] = gpuLine
	bdf := NormalizeBDF(strings.Split(gpuLine, ",")[0])
	addGPU(h, bdf, "nvidia", "10de", "27b8")
	passedTestBoot(h, vmrt.GuestGPU{HostBDF: bdf, ID: "10de:27b8", Model: "NVIDIA L4", Driver: "nvidia", MemoryMB: 23034})
	return h
}

func holds(h *fakehost.Host, pid, comm, dev string) {
	h.Links["/proc/"+pid+"/fd/9"] = dev
	h.Files["/proc/"+pid+"/comm"] = []byte(comm + "\n")
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
	if rep.Vendor != VendorPCI || strings.Join(rep.BDFs, " ") != gpu || rep.Firmware.Code == "" || len(rep.Excluded) != 0 {
		t.Errorf("report = %+v", rep)
	}
	if len(rep.GPUs) != 1 || rep.GPUs[0].Model != "NVIDIA L4" || rep.GPUs[0].MemoryMB != 23034 {
		t.Errorf("the test boot's GPUs were not carried: %+v", rep.GPUs)
	}
}

// Nothing on the host has to know an AMD GPU: no rocm-smi, no ROCm at all. The
// GPU is on the PCI bus, and the test boot proved the VM gets it.
func TestPreflightAMDHostWithoutROCmIsReady(t *testing.T) {
	h := baseHost("1002")
	addGPU(h, "0000:41:00.0", "amdgpu", "1002", "74a1")
	passedTestBoot(h, vmrt.GuestGPU{HostBDF: "0000:41:00.0", ID: "1002:74a1", Model: "AMD Instinct MI300X", Driver: "amdgpu", MemoryMB: 196608})
	h.Tools = map[string]bool{"qemu-system-x86_64": true, "qemu-img": true, "cloud-localds": true, "cryptsetup": true, "losetup": true,
		"truncate": true, "nft": true, "ip": true, "modprobe": true, "systemd-run": true, "systemctl": true}
	rep := Preflight(h, "linux", spec(), version)
	if len(rep.Reasons) != 0 || strings.Join(rep.BDFs, " ") != "0000:41:00.0" {
		t.Fatalf("an AMD host was refused: %s %+v", reasons(rep), rep)
	}
}

// A GPU nobody can read the memory of is still a GPU: the VM, not the host, says
// what a renter gets.
func TestPreflightAcceptsAGPUWhoseMemoryItCannotRead(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA Mystery, [N/A]\n")
	rep := Preflight(h, "linux", spec(), version)
	if len(rep.Reasons) != 0 || rep.Unified {
		t.Fatalf("reasons = %s, unified = %v", reasons(rep), rep.Unified)
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

// The same GB10 seen only by its test boot: the host has no NVIDIA software.
func TestPreflightLearnsUnifiedMemoryFromTheTestBoot(t *testing.T) {
	h := baseHost("10de")
	addGPU(h, "000f:01:00.0", "", "10de", "2e12")
	passedTestBoot(h, vmrt.GuestGPU{HostBDF: "000f:01:00.0", ID: "10de:2e12", Model: "NVIDIA GB10", Driver: "nvidia", Unified: true})
	if rep := Preflight(h, "linux", spec(), version); len(rep.Reasons) != 0 || !rep.Unified {
		t.Fatalf("unified=%v %s", rep.Unified, reasons(rep))
	}
}

// A workstation: an Arc card runs the screen, the NVIDIA card is free. The
// NVIDIA card rents; the Arc is left out and named, and is no reason to refuse
// the machine. The processor's own GPU is left out too, for being that.
func TestPreflightLeavesOutAGPUTheHostIsUsing(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA L4, 23034\n")
	h.PCI("0000:03:00.0", "xe", "0x030000", "0000:03:00.0")
	h.PCIID("0000:03:00.0", "8086", "56a0")
	h.Files["/sys/bus/pci/devices/0000:03:00.0/drm/card0"] = nil
	holds(h, "1001", "Xorg", "/dev/dri/card0")
	h.PCI("0000:00:02.0", "i915", "0x030000", "0000:00:02.0")
	h.PCIID("0000:00:02.0", "8086", "46a6")
	rep := Preflight(h, "linux", spec(), version)
	if len(rep.Reasons) != 0 || strings.Join(rep.BDFs, " ") != gpu {
		t.Fatalf("the free card did not rent: %s %+v", reasons(rep), rep)
	}
	all := strings.Join(rep.Excluded, " | ")
	if len(rep.Excluded) != 2 || !strings.Contains(all, "0000:03:00.0") || !strings.Contains(all, "Xorg (pid 1001)") ||
		!strings.Contains(all, "0000:00:02.0 (Intel GPU 8086:46a6) is the processor's integrated GPU") {
		t.Errorf("excluded = %v", rep.Excluded)
	}
}

// A mini PC whose only GPU is its processor's own hosts CPU-only rentals, as a
// machine without a GPU does: the iGPU draws its screen from its own memory
// and is never rented.
func TestPreflightAnIntegratedGPUAloneIsACPUMachine(t *testing.T) {
	h := baseHost()
	h.PCI("0000:00:02.0", "i915", "0x030000", "0000:00:02.0")
	h.PCIID("0000:00:02.0", "8086", "46a6")
	rep := Preflight(h, "linux", spec(), version)
	if rep.Vendor != VendorNone || len(rep.BDFs) != 0 || rep.GPUCount != 0 {
		t.Errorf("an iGPU-only machine: vendor %s bdfs %v count %d", rep.Vendor, rep.BDFs, rep.GPUCount)
	}
	if len(rep.Excluded) != 1 || !strings.Contains(rep.Excluded[0], "integrated") {
		t.Errorf("excluded = %v", rep.Excluded)
	}
}

// A GPU already on vfio-pci belongs to a VM of the provider's own. Taking it
// would take it from that VM, and nothing would reset it between renters.
func TestPreflightLeavesOutAGPUAlreadyOnVFIO(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA L4, 23034\n")
	addGPU(h, "0000:41:00.0", "vfio-pci", "10de", "27b8")
	rep := Preflight(h, "linux", spec(), version)
	if len(rep.Reasons) != 0 || strings.Join(rep.BDFs, " ") != gpu {
		t.Fatalf("%s %+v", reasons(rep), rep)
	}
	if len(rep.Excluded) != 1 || !strings.Contains(rep.Excluded[0], "0000:41:00.0") || !strings.Contains(rep.Excluded[0], "vfio-pci") {
		t.Errorf("excluded = %v", rep.Excluded)
	}
}

// The agent restarted while its own rental runs: that rental's GPUs are on
// vfio-pci and its QEMU holds them, and they are still this machine's to rent.
func TestPreflightKeepsItsOwnRentalsGPUs(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA L4, 23034\n")
	h.PCI(gpu, "vfio-pci", "0x030200", gpu)
	h.Links["/sys/bus/pci/devices/"+gpu+"/iommu_group"] = "../../../kernel/iommu_groups/13"
	holds(h, "9001", "qemu-system-x86", "/dev/vfio/13")
	st := &vmrt.State{RentalID: "R1", Devices: []vmrt.BoundDevice{{BDF: gpu, Driver: "nvidia"}}}
	if err := vmrt.SaveState(h, dataDir, st); err != nil {
		t.Fatal(err)
	}
	rep := Preflight(h, "linux", spec(), version)
	if len(rep.Reasons) != 0 || strings.Join(rep.BDFs, " ") != gpu || len(rep.Excluded) != 0 {
		t.Errorf("the running rental's GPU was left out: %s %+v", reasons(rep), rep)
	}
}

// A laptop or desktop whose firmware console is on the iGPU: the card rents,
// the host keeps its screen. A machine whose only GPU is its boot display (a
// DGX Spark) still rents that GPU.
func TestPreflightLeavesOutTheBootDisplayWhenACardIsFree(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA L4, 23034\n")
	h.PCI("0000:02:00.0", "radeon", "0x030000", "0000:02:00.0")
	h.PCIID("0000:02:00.0", "1002", "6779")
	h.Files["/sys/bus/pci/devices/0000:02:00.0/boot_vga"] = []byte("1\n")
	h.Files["/sys/bus/pci/devices/"+gpu+"/boot_vga"] = []byte("0\n")
	rep := Preflight(h, "linux", spec(), version)
	if len(rep.Reasons) != 0 || strings.Join(rep.BDFs, " ") != gpu {
		t.Fatalf("%s %+v", reasons(rep), rep)
	}
	if len(rep.Excluded) != 1 || !strings.Contains(rep.Excluded[0], "boot display") {
		t.Errorf("excluded = %v", rep.Excluded)
	}

	only := goodHost(t, "00000000:01:00.0, NVIDIA GB10, [N/A]\n")
	only.Files["/sys/bus/pci/devices/"+gpu+"/boot_vga"] = []byte("1\n")
	if rep := Preflight(only, "linux", spec(), version); len(rep.Reasons) != 0 || strings.Join(rep.BDFs, " ") != gpu {
		t.Errorf("the only GPU was left out for being the boot display: %s %+v", reasons(rep), rep)
	}
}

// A GB10 that is not a confirmed DGX Spark draws its desktop: its only GPU is
// left out, so the desktop IS the reason, with the command that fixes it.
func TestPreflightRefusesWhenTheOnlyGPUDrawsTheDesktop(t *testing.T) {
	h := goodHost(t, "0000000F:01:00.0, NVIDIA GB10, [N/A]\n")
	holds(h, "5591", "Xorg", "/dev/nvidia0")
	all := reasons(Preflight(h, "linux", spec(), version))
	for _, want := range []string{"000f:01:00.0", "desktop is running on the GPU", "Xorg (pid 5591)", "--headless"} {
		if !strings.Contains(all, want) {
			t.Errorf("reasons missing %q: %s", want, all)
		}
	}
}

// Anything else holding a GPU is judged when a rental starts, as it always was:
// a process that happens to have it open when the agent starts is no reason to
// take the GPU off the market until the next restart.
func TestPreflightLeavesBusyGPUsToTheRentalStart(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA L4, 23034\n")
	holds(h, "11435", "llama-server", "/dev/nvidia0")
	if rep := Preflight(h, "linux", spec(), version); len(rep.Reasons) != 0 || strings.Join(rep.BDFs, " ") != gpu || len(rep.Excluded) != 0 {
		t.Errorf("a busy GPU was judged at preflight: %s %+v", reasons(rep), rep)
	}
}

// The boot display is left out only for a card that can really be rented: when
// the other card's group holds a NIC, the boot display is the one that rents.
func TestPreflightJudgesTheBootDisplayAfterTheGroups(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA L4, 23034\n")
	h.PCI(gpu, "nvidia", "0x030200", gpu, "0000:05:00.0")
	h.PCI("0000:05:00.0", "ixgbe", "0x020000")
	h.PCI("0000:02:00.0", "nvidia", "0x030000", "0000:02:00.0")
	h.PCIID("0000:02:00.0", "10de", "2204")
	h.Files["/sys/bus/pci/devices/0000:02:00.0/boot_vga"] = []byte("1\n")
	passedTestBoot(h, vmrt.GuestGPU{HostBDF: "0000:02:00.0", ID: "10de:2204", Model: "NVIDIA RTX 3090", Driver: "nvidia"})
	rep := Preflight(h, "linux", spec(), version)
	if strings.Join(rep.BDFs, " ") != "0000:02:00.0" {
		t.Fatalf("bdfs = %v, reasons %s, excluded %v", rep.BDFs, reasons(rep), rep.Excluded)
	}
	if all := strings.Join(rep.Excluded, " | "); strings.Contains(all, "boot display") || !strings.Contains(all, "0000:05:00.0") {
		t.Errorf("excluded = %v", rep.Excluded)
	}
}

// Every server's BMC shows up as a display function. It is not a GPU.
func TestPreflightIgnoresTheBMCDisplay(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA L4, 23034\n")
	h.PCI("0000:03:00.0", "ast", "0x030000", "0000:03:00.0")
	h.PCIID("0000:03:00.0", "1a03", "2000")
	rep := Preflight(h, "linux", spec(), version)
	if len(rep.Reasons) != 0 || strings.Join(rep.BDFs, " ") != gpu || len(rep.Excluded) != 0 {
		t.Errorf("the BMC display counted: %s %+v", reasons(rep), rep)
	}
}

func TestPreflightWantsARebuildForANewMake(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA L4, 23034\n")
	addGPU(h, "0000:41:00.0", "amdgpu", "1002", "744c")
	if all := reasons(Preflight(h, "linux", spec(), version)); !strings.Contains(all, "not built for this machine's AMD GPU") {
		t.Errorf("an image with nothing for AMD was accepted: %s", all)
	}
}

func TestPreflightNamesWhatToRunNext(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA L4, 24564\n")
	h.Tools = map[string]bool{"qemu-img": true, "apt-get": true} // an Ubuntu/Debian machine
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

// A passing test boot sells across agent versions while the machine is the
// one it was run on (a record from before v0.2.3 knows only its image's age);
// the running version owes its own full test, which it runs when the GPU is
// free and before a rental.
func TestPreflightKeepsATestBootAcrossVersions(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA L4, 24564\n")
	rep := Preflight(h, "linux", spec(), "v0.1.8")
	if len(rep.Reasons) != 0 || !rep.RetestPending || len(rep.GPUs) != 1 {
		t.Errorf("a pass by v0.1.7 did not sell under v0.1.8: %s retest=%v gpus=%+v", reasons(rep), rep.RetestPending, rep.GPUs)
	}
	if rep := Preflight(h, "linux", spec(), version); len(rep.Reasons) != 0 || rep.RetestPending {
		t.Errorf("the version's own pass: %s retest=%v", reasons(rep), rep.RetestPending)
	}
	rebuiltImage(h)
	if all := reasons(Preflight(h, "linux", spec(), "v0.1.8")); !strings.Contains(all, "base image was rebuilt after its last test boot") {
		t.Errorf("a test boot from before the image was rebuilt counted for another version: %s", all)
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

// Two GPUs behind one PCIe switch without ACS share an IOMMU group. They can go
// to the VM together -- each is the other's only group-mate -- unless the host
// is using one of them, which takes the other down with it.
func TestPreflightGPUsSharingAGroupGoTogether(t *testing.T) {
	a, b := "0000:41:00.0", "0000:42:00.0"
	h := baseHost("10de")
	for _, g := range []string{a, b} {
		h.PCI(g, "nvidia", "0x030200", a, b)
		h.PCIID(g, "10de", "2330")
	}
	passedTestBoot(h,
		vmrt.GuestGPU{HostBDF: a, ID: "10de:2330", Model: "NVIDIA H100", Driver: "nvidia"},
		vmrt.GuestGPU{HostBDF: b, ID: "10de:2330", Model: "NVIDIA H100", Driver: "nvidia"})
	rep := Preflight(h, "linux", spec(), version)
	if len(rep.Reasons) != 0 || strings.Join(rep.BDFs, " ") != a+" "+b {
		t.Fatalf("two GPUs sharing a group did not rent together: %s %+v", reasons(rep), rep)
	}

	// An NVIDIA holder would count against both anyway; a group of an AMD and
	// an Intel GPU lets the DRM node pin the holder on one of them.
	h2 := baseHost("1002", "8086")
	h2.PCI(a, "amdgpu", "0x030000", a, b)
	h2.PCIID(a, "1002", "744c")
	h2.PCI(b, "i915", "0x030000", a, b)
	h2.PCIID(b, "8086", "56a0")
	h2.Files["/sys/bus/pci/devices/"+b+"/drm/card1"] = nil
	holds(h2, "7001", "Xorg", "/dev/dri/card1")
	all := reasons(Preflight(h2, "linux", spec(), version))
	for _, want := range []string{a + " (AMD GPU 1002:744c) cannot be passed through on its own", b + " (Intel GPU 8086:56a0): this machine's desktop is running on the GPU (Xorg (pid 7001))"} {
		if !strings.Contains(all, want) {
			t.Errorf("reasons missing %q: %s", want, all)
		}
	}
}

// A bare machine lacks KVM; since v0.2.0 it is not refused for having no GPU
// (CONTRACT-v2 s1), and without a GPU it needs no IOMMU. A machine WITH a GPU
// still needs the IOMMU.
func TestPreflightNoKVMNoIOMMUNoGPU(t *testing.T) {
	h := fakehost.New()
	all := reasons(Preflight(h, "linux", spec(), version))
	if !strings.Contains(all, "/dev/kvm") {
		t.Errorf("reasons missing /dev/kvm: %s", all)
	}
	for _, unwanted := range []string{"IOMMU", "no NVIDIA or AMD GPU"} {
		if strings.Contains(all, unwanted) {
			t.Errorf("a machine without a GPU was refused for %q: %s", unwanted, all)
		}
	}
	gpu := goodHost(t, "00000000:01:00.0, NVIDIA L4, 24564\n")
	delete(gpu.Files, "/sys/kernel/iommu_groups/13")
	if all := reasons(Preflight(gpu, "linux", spec(), version)); !strings.Contains(all, "the IOMMU is off") {
		t.Errorf("a GPU machine without the IOMMU was not refused: %s", all)
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
	s.TotalMemMB = 3500 // since v0.2.0 a 4 GB machine can host (2 GB for the VM)
	s.DiskGB = 5
	all := reasons(Preflight(h, "linux", s, version))
	if !strings.Contains(all, "3500 MB") || !strings.Contains(all, "20 GB free") {
		t.Errorf("reasons = %s", all)
	}
}

func TestDetectBuildsACapability(t *testing.T) {
	h := goodHost(t, "0000000F:01:00.0, NVIDIA GB10, [N/A]\n")
	h.PCI("0000:00:02.0", "i915", "0x030000", "0000:00:02.0")
	h.PCIID("0000:00:02.0", "8086", "46a6")
	h.Files["/sys/bus/pci/devices/0000:00:02.0/drm/card0"] = nil
	holds(h, "1001", "Xorg", "/dev/dri/card0")
	h.Files["/proc/meminfo"] = []byte("MemTotal:       128000000 kB\n")
	h.Files[dataDir] = nil
	h.Outputs["df --output=avail"] = " Avail\n  900G\n"
	p := Detect(h, "linux", "amd64", dataDir, version)
	c := p.Capability()
	if !c.Ready || !c.UnifiedMemory || c.Kind != KindQEMUVFIO || c.AgentVersion != version || c.VMUser != "root" {
		t.Fatalf("capability = %+v", c)
	}
	if len(c.GPUs) != 1 || c.GPUs[0].Model != "NVIDIA L4" || c.GPUs[0].PCIID != "10de:27b8" || c.GPUs[0].MemoryMB != 23034 {
		t.Errorf("capability GPUs = %+v", c.GPUs)
	}
	if len(c.Excluded) != 1 || !strings.Contains(c.Excluded[0], "integrated") {
		t.Errorf("capability excluded = %v", c.Excluded)
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
	rebuiltImage(h)
	if k := kinds(Preflight(h, "linux", spec(), "v0.1.10")); k[ReasonTestBoot] != 1 || len(k) != 1 {
		t.Errorf("an old test boot on a rebuilt image: kinds = %v", k)
	}
}

// rebuiltImage records the base image as built after the machine's test boot.
func rebuiltImage(h *fakehost.Host) {
	var info vmrt.GoldenInfo
	_ = json.Unmarshal(h.Files[dataDir+"/golden.img.json"], &info)
	info.CreatedAt = 1758200000
	golden, _ := json.Marshal(info)
	h.Files[dataDir+"/golden.img.json"] = golden
	h.Files[filepath.Join(dataDir, "golden.img")+".json"] = golden
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
	if !strings.Contains(string(data), `"identity":{"sys_vendor":"NVIDIA","product_name":"NVIDIA DGX Spark","board_name":"","product_family":"DGX Spark","confirmed_dgx_spark":true,"reason":""}`) {
		t.Errorf("capability JSON = %s", data)
	}
}

// The pair checks ride along with every capability report, and never change
// whether the machine can host a single rental.
func TestDetectReportsThePairHalfWithoutTouchingTheSingleOne(t *testing.T) {
	h := goodHost(t, "0000000F:01:00.0, NVIDIA GB10, [N/A]\n")
	h.Files["/proc/meminfo"] = []byte("MemTotal:       128000000 kB\n")
	h.Files[dataDir] = nil
	h.Outputs["df --output=avail"] = " Avail\n  900G\n"
	h.Files["/sys/class/dmi/id/sys_vendor"] = []byte("NVIDIA\n")
	h.Files["/sys/class/dmi/id/product_name"] = []byte("DGX Spark\n")
	h.Files["/usr/share/AAVMF/AAVMF_CODE.fd"] = nil
	h.Files["/usr/share/AAVMF/AAVMF_VARS.fd"] = nil
	arm, _ := json.Marshal(vmrt.GoldenInfo{Base: "resolute-server-cloudimg-arm64.img"})
	h.Files[filepath.Join(dataDir, "golden.img")+".json"] = arm
	h.NIC("0000:01:00.0", "enp1s0f0np0", "58:a2:e1:00:00:01", "MT2412X00001", "0000:01:00.0")
	h.NIC("0002:01:00.0", "enP2p1s0f0np0", "58:a2:e1:00:00:03", "MT2412X00001", "0002:01:00.0")
	orig := RelayAddrs
	RelayAddrs = func() []string { return nil }
	t.Cleanup(func() { RelayAddrs = orig })

	p := Detect(h, "linux", "arm64", dataDir, version)
	c := p.Capability()
	if !c.Ready {
		t.Fatalf("the single half changed: %v", c.Reasons)
	}
	if c.Identity == nil || !c.Identity.ConfirmedDGXSpark || c.Interconnect == nil || !c.Interconnect.Supported {
		t.Fatalf("identity=%+v interconnect=%+v", c.Identity, c.Interconnect)
	}
	if c.Interconnect.Ready || len(c.Interconnect.Ports) != 2 {
		t.Errorf("interconnect = %+v (no RDMA image, no pair test: not ready)", c.Interconnect)
	}
	if got := strings.Join(p.Runtime().Spec().NICs, " "); got != "0000:01:00.0 0002:01:00.0" {
		t.Errorf("spec NICs = %s", got)
	}
}
