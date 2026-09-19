package provisioner

import (
	"fmt"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/netguard"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// hasDirectRMR reports whether a device's IOMMU group has a 1:1 (RMR) reserved
// region -- a "direct" or "direct-relaxable" line in reserved_regions. That
// requirement is exactly why the kernel's generic vfio-pci refuses the device
// ("Firmware has requested this device have a 1:1 IOMMU mapping"), as on the
// DGX Spark's GB10.
func hasDirectRMR(h vmrt.Host, bdf string) bool {
	data, err := h.ReadFile("/sys/bus/pci/devices/" + bdf + "/iommu_group/reserved_regions")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && (f[2] == "direct" || f[2] == "direct-relaxable") {
			return true
		}
	}
	return false
}

// pciField reads a lowercased 0x-stripped sysfs id (vendor or device).
func pciField(h vmrt.Host, bdf, field string) string {
	data, err := h.ReadFile("/sys/bus/pci/devices/" + bdf + "/" + field)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(string(data)), "0x"))
}

// nvgraceHandles reports whether the installed nvgrace-gpu-vfio-pci module lists
// this device in its id_table (its modinfo alias). When it does, the device HAS
// a VFIO path (through nvgrace) and container mode is not needed; when it does
// not -- and the device needs a 1:1 mapping -- there is no VFIO path at all.
func nvgraceHandles(h vmrt.Host, bdf string) bool {
	device := pciField(h, bdf, "device")
	if device == "" {
		return false
	}
	out, err := h.Output("modinfo", "-F", "alias", "nvgrace-gpu-vfio-pci")
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(out), "d0000"+device)
}

// vfioImpossible reports whether an NVIDIA GPU cannot be passed through over
// VFIO at all: it needs a 1:1 IOMMU mapping (so generic vfio-pci refuses it)
// and the installed nvgrace variant does not carry its id either.
func vfioImpossible(h vmrt.Host, bdf string) bool {
	if pciField(h, bdf, "vendor") != "10de" {
		return false
	}
	return hasDirectRMR(h, bdf) && !nvgraceHandles(h, bdf)
}

// containerFallback reports whether this machine's rentable GPUs should be
// hosted in container mode: it has some, they are all NVIDIA, and none can be
// passed through over VFIO.
func containerFallback(h vmrt.Host, bdfs []string) bool {
	if len(bdfs) == 0 {
		return false
	}
	for _, bdf := range bdfs {
		if !vfioImpossible(h, bdf) {
			return false
		}
	}
	return true
}

// containerReasons is the container-mode preflight: the reasons this machine
// cannot host a rental as a container. It drops the microVM's KVM/IOMMU/VFIO/
// firmware/golden checks and adds the container stack's, keeping the shared
// memory, CPU and disk floors and gating on a passing container test boot.
func containerReasons(h vmrt.Host, spec vmrt.Spec, version string) []Finding {
	var findings []Finding
	add := func(kind ReasonKind, format string, a ...interface{}) {
		findings = append(findings, Finding{Kind: kind, Text: fmt.Sprintf(format, a...)})
	}
	apt := AptDistro(h)
	// The container runtime and the tools the rental needs.
	tools := []string{"podman", "nsenter", "cryptsetup", "mkfs.ext4", "mount", "ip", "nft"}
	var missing []string
	for _, t := range tools {
		if _, err := h.LookPath(t); err != nil {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		if apt {
			add(ReasonTools, "the container rental tools are missing (%s); run 'sudo gpu-agent runtime prepare --install-deps'", strings.Join(missing, ", "))
		} else {
			add(ReasonTools, "the container rental tools are missing (%s): install podman, util-linux, cryptsetup, e2fsprogs and nftables with this system's package manager", strings.Join(missing, ", "))
		}
	}
	// userns remap needs subordinate id ranges for root (--userns=auto).
	if data, err := h.ReadFile("/etc/subuid"); err != nil || len(strings.TrimSpace(string(data))) == 0 {
		add(ReasonTools, "user-namespace ranges are not configured (/etc/subuid is empty), so the container cannot remap root; add a subuid/subgid range")
	}
	// The NVIDIA container stack and a GPU it can see.
	if _, err := h.LookPath("nvidia-ctk"); err != nil && !h.Exists("/etc/cdi/nvidia.yaml") && !h.Exists("/var/run/cdi/nvidia.yaml") {
		add(ReasonTools, "the NVIDIA Container Toolkit (nvidia-ctk) and its CDI spec are missing, so the GPU cannot be shared into a container; install nvidia-container-toolkit and run 'nvidia-ctk cdi generate'")
	}
	if len(vmrt.CDIDeviceRefs(h)) == 0 {
		add(ReasonHuman, "nvidia-smi does not list a usable GPU, so none can be shared into a container")
	}
	if problem := vmrt.ContainerImageProblem(h); problem != "" {
		add(ReasonImage, "%s", problem)
	}
	// The same memory, CPU and disk floors a rental needs either way.
	if spec.GuestMemoryMB() == 0 {
		add(ReasonHuman, "this machine has %d MB of memory, and a rental needs at least 4 GB: 2 GB for the rental, the rest kept for the machine", spec.TotalMemMB)
	}
	if n := spec.GuestCPUs(); n < 2 {
		add(ReasonHuman, "a rental on this machine would get %d CPU, and a rental needs at least 2", n)
	}
	for _, problem := range storageProblems(h, spec.Storage(), spec.DiskGB) {
		add(ReasonHuman, "%s", problem)
	}
	// The container test boot proves the GPU works in the container and the
	// fence holds -- only once the checks above pass.
	if len(findings) == 0 {
		t, err := vmrt.LoadContainerTest(h, spec.DataDir)
		if err != nil {
			add(ReasonTestBoot, "the last container test boot could not be read (%v); run 'sudo gpu-agent check --boot'", err)
		} else if problem := vmrt.ContainerSelfTestProblem(t, vmrt.ContainerFingerprint(spec, version)); problem != "" {
			add(ReasonTestBoot, "%s", problem)
		}
	}
	return findings
}

// detectContainer builds a provisioner backed by the container runtime, for a
// machine whose GPU cannot be passed through over VFIO. Called from Detect.
func detectContainer(h vmrt.Host, goos, arch, dataDir, version string, spec vmrt.Spec, rep HostReport) *Provisioner {
	fence := netguard.New(h, netguard.Bridge, vmrt.GuestSubnet, netguard.HostNetworks)
	cdi := vmrt.CDIDeviceRefs(h)
	crt := vmrt.NewContainer(h, spec, fence, vmrt.ContainerImageRef, cdi, gpuVerifier(h))

	findings := containerReasons(h, spec, version)
	var reasons []string
	for _, f := range findings {
		reasons = append(reasons, f.Text)
	}

	p := New(crt, VendorContainerNV, rep.BDFs, rep.Unified)
	p.host = h
	p.dataDir = dataDir
	p.version = version
	p.findings = findings
	p.now = time.Now
	p.redetect = func() *Provisioner { return Detect(h, goos, arch, dataDir, version) }
	p.midRental = crt.Present()

	count := rep.GPUCount
	guest := &control.Guest{VCPUs: spec.GuestCPUs(), MemoryGB: spec.GuestMemoryMB() / 1024, DiskGB: spec.DiskGB, CPU: spec.GuestCPUName}
	cap := control.Capability{
		Ready:         len(reasons) == 0,
		Kind:          KindContainer,
		GPUCount:      &count,
		Guest:         guest,
		Reasons:       reasons,
		AgentVersion:  version,
		UnifiedMemory: rep.Unified,
		VMUser:        "renter",
		Excluded:      rep.Excluded,
		Identity:      rep.Identity,
	}
	for _, model := range rep.Models {
		cap.GPUs = append(cap.GPUs, control.GPU{Model: model, Unified: rep.Unified})
	}
	if t, err := vmrt.LoadContainerTest(h, dataDir); err == nil && t != nil {
		cap.SelfTest = &control.SelfTestSummary{Passed: t.Passed, GPUVerified: t.GPUVerified, At: t.At, AgentVersion: t.AgentVersion}
	}
	p.capability = cap
	return p
}
