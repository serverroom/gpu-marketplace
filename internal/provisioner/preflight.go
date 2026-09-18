package provisioner

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/netguard"
	"github.com/serverroom/gpu-marketplace/internal/stats"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// KindQEMUVFIO names the only way this agent hosts a rental: a QEMU/KVM microVM
// with the GPUs handed to it over VFIO, behind the netguard fence.
const KindQEMUVFIO = "qemu-vfio"

// HostReport is what preflight found: the GPUs a rental would get, and every
// reason this machine cannot host one. No reasons means ready.
type HostReport struct {
	Vendor   GPUVendor
	BDFs     []string
	Unified  bool
	Firmware vmrt.Firmware
	Reasons  []string
}

// Preflight checks, without changing anything, whether this machine can host a
// rental: a Linux KVM host with the IOMMU on, GPUs that can be passed through
// on their own, the runtime's tools, firmware and base image installed, enough
// memory and disk -- and, once all of that holds, a passing test boot on this
// very machine for this agent version. Every failing check is reported in words
// a provider can act on, not just the first.
func Preflight(h vmrt.Host, goos string, spec vmrt.Spec, version string) HostReport {
	var rep HostReport
	add := func(format string, a ...interface{}) {
		rep.Reasons = append(rep.Reasons, fmt.Sprintf(format, a...))
	}

	switch goos {
	case "linux":
	case "darwin":
		rep.Vendor = VendorApple
		add("%v: Apple Silicon has no way to pass its GPU through to a microVM", ErrVendorCannotIsolate)
		return rep
	default:
		add("rentals run inside a Linux KVM microVM, and this machine runs %s", goos)
		return rep
	}

	if !h.Exists("/dev/kvm") {
		add("KVM is not available (/dev/kvm is missing): enable virtualisation in the firmware and load the kvm module")
	}
	if groups, _ := h.Glob("/sys/kernel/iommu_groups/*"); len(groups) == 0 {
		add("the IOMMU is off, so no GPU can be handed to a microVM: enable VT-d / AMD-Vi (or the SMMU on Arm) in the firmware and on the kernel command line")
	}

	if nv, ok := detectNVIDIA(h); ok {
		rep.Vendor = VendorNVIDIA
		rep.BDFs = nv.bdfs
		rep.Unified = nv.unified
		for _, g := range nv.noMemory {
			add("GPU %s (%s) reports no memory and is not a known unified-memory part, so the agent cannot tell what a tenant would get or prove it clean afterwards", g.bdf, g.name)
		}
	} else if bdfs, ok := detectAMD(h); ok {
		rep.Vendor = VendorAMD
		rep.BDFs = bdfs
	} else {
		add("no NVIDIA or AMD GPU was detected")
	}
	if len(rep.BDFs) > 0 && len(rep.Reasons) == 0 {
		rep.Reasons = append(rep.Reasons, vmrt.GroupProblems(h, rep.BDFs)...)
	}
	// A desktop drawn on the GPU holds it for as long as it runs. Desktop
	// machines such as the DGX Spark ship that way; say so before a test boot
	// finds out, with the one command that fixes it.
	if rep.Vendor == VendorNVIDIA {
		if desktop, _ := vmrt.ClassifyGPUHolders(h); len(desktop) > 0 {
			add("%s", vmrt.DesktopOnGPUProblem(desktop))
		}
	}

	if missing := vmrt.MissingTools(h, spec.Arch); len(missing) > 0 {
		add("the rental runtime's tools are missing (%s); run 'sudo gpu-agent runtime prepare --install-deps'", strings.Join(missing, ", "))
	}
	if fw, ok := vmrt.FindFirmware(h, spec.Arch); ok {
		rep.Firmware = fw
	} else {
		add("no UEFI firmware for microVMs is installed; run 'sudo gpu-agent runtime prepare --install-deps'")
	}
	if !h.Exists(spec.GoldenImage) {
		add("the rental base image has not been built; run 'sudo gpu-agent runtime prepare'")
	} else if problem := vmrt.GoldenProblem(h, spec); problem != "" {
		add("%s", problem)
	}
	if spec.GuestMemoryMB() == 0 {
		add("this machine has %d MB of memory, and a rental needs at least 6 GB", spec.TotalMemMB)
	}
	if spec.DiskGB < 20 {
		add("there is not 20 GB free under %s for a rental's disk", spec.DataDir)
	}

	// A test boot proves what the checks above cannot: that this GPU really
	// works inside a VM on this hardware. It only means anything once they pass.
	if len(rep.Reasons) == 0 {
		res, err := vmrt.LoadSelfTest(h, spec.DataDir)
		if err != nil {
			add("its last test boot could not be read (%v); run 'sudo gpu-agent check --boot'", err)
		} else if problem := vmrt.SelfTestProblem(res, version, rep.BDFs); problem != "" {
			add("%s", problem)
		}
	}
	return rep
}

// Detect runs preflight and returns a provisioner, backed by the real microVM
// runtime, whose Capability says what it found. It is the only constructor
// production code should use.
func Detect(h vmrt.Host, goos, arch, dataDir, version string) *Provisioner {
	spec := vmrt.Spec{
		Arch:        arch,
		DataDir:     dataDir,
		GoldenImage: filepath.Join(dataDir, "golden.img"),
		TotalMemMB:  hostMemoryMB(h),
		CPUs:        runtime.NumCPU(),
		DiskGB:      rentalDiskGB(h, dataDir),
	}
	rep := Preflight(h, goos, spec, version)
	spec.GPUs = rep.BDFs
	spec.Unified = rep.Unified
	spec.Firmware = rep.Firmware

	fence := netguard.New(h, netguard.Bridge, vmrt.GuestSubnet, netguard.HostNetworks)
	rt := vmrt.New(h, spec, fence, gpuVerifier(h, rep.Vendor, rep.Unified, rep.BDFs))
	p := New(rt, rep.Vendor, rep.BDFs, rep.Unified)
	p.runtime = rt
	p.capability = control.Capability{
		Ready:         len(rep.Reasons) == 0,
		Kind:          KindQEMUVFIO,
		Reasons:       rep.Reasons,
		AgentVersion:  version,
		UnifiedMemory: rep.Unified,
		VMUser:        vmrt.VMUser,
	}
	return p
}

func hostMemoryMB(h vmrt.Host) int {
	data, err := h.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemTotal:" {
			kb, _ := strconv.Atoi(f[1])
			return kb / 1024
		}
	}
	return 0
}

// rentalDiskGB is the disk a rental gets: what is free under dataDir (or the
// nearest existing parent), less 20 GB for the host, capped at 500 GB.
func rentalDiskGB(h vmrt.Host, dataDir string) int {
	dir := dataDir
	for dir != "" && dir != "/" && !h.Exists(dir) {
		dir = filepath.Dir(dir)
	}
	out, err := h.Output("df", "--output=avail", "-B1G", dir)
	if err != nil {
		return 0
	}
	lines := strings.Fields(out)
	if len(lines) < 2 {
		return 0
	}
	avail, err := strconv.Atoi(strings.TrimSuffix(lines[len(lines)-1], "G"))
	if err != nil {
		return 0
	}
	gb := avail - 20
	if gb > 500 {
		gb = 500
	}
	if gb < 0 {
		gb = 0
	}
	return gb
}

type gpuRef struct{ bdf, name string }

type nvidiaGPUs struct {
	bdfs []string
	// unified is set when the GPUs share the machine's memory pool (a GB10).
	// A rental then gets that pool as both its system memory and its GPU memory.
	unified bool
	// noMemory are GPUs that report no memory and are NOT a known unified part.
	noMemory []gpuRef
}

// detectNVIDIA lists the NVIDIA GPUs by PCI address. nvidia-smi reports
// memory.total as [N/A] for a GPU with no memory of its own; for a known
// unified-memory part (GB10) that is expected and the machine's pool is the
// GPU's memory, for anything else it is a GPU whose memory cannot be read.
func detectNVIDIA(r Runner) (nvidiaGPUs, bool) {
	var nv nvidiaGPUs
	out, err := r.Output("nvidia-smi", "--query-gpu=pci.bus_id,name,memory.total", "--format=csv,noheader,nounits")
	if err != nil {
		return nv, false
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 3 {
			continue
		}
		bdf := NormalizeBDF(fields[0])
		if bdf == "" {
			continue
		}
		name := strings.TrimSpace(fields[1])
		nv.bdfs = append(nv.bdfs, bdf)
		if total, err := strconv.ParseFloat(strings.TrimSpace(fields[2]), 64); err == nil && total > 0 {
			continue
		}
		if stats.IsUnifiedMemoryModel(name) {
			nv.unified = true
		} else {
			nv.noMemory = append(nv.noMemory, gpuRef{bdf: bdf, name: name})
		}
	}
	return nv, len(nv.bdfs) > 0
}

// detectAMD lists the AMD GPUs by PCI address from `rocm-smi --showbus --csv`.
func detectAMD(r Runner) ([]string, bool) {
	out, err := r.Output("rocm-smi", "--showbus", "--csv")
	if err != nil {
		return nil, false
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return nil, false
	}
	col := -1
	for i, h := range strings.Split(lines[0], ",") {
		if strings.Contains(strings.ToLower(h), "bus") {
			col = i
			break
		}
	}
	if col < 0 {
		return nil, false
	}
	var bdfs []string
	for _, line := range lines[1:] {
		cols := strings.Split(line, ",")
		if col < len(cols) {
			if bdf := NormalizeBDF(cols[col]); bdf != "" {
				bdfs = append(bdfs, bdf)
			}
		}
	}
	return bdfs, len(bdfs) > 0
}

// NormalizeBDF turns nvidia-smi's 8-digit PCI domain ("00000000:0F:01.0") into
// the 4-digit form sysfs and VFIO use ("0000:0f:01.0"). Anything that does not
// look like a PCI address is dropped.
func NormalizeBDF(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	parts := strings.Split(s, ":")
	if len(parts) != 3 || !strings.Contains(parts[2], ".") {
		return ""
	}
	if len(parts[0]) == 8 {
		parts[0] = parts[0][4:]
	}
	return strings.Join(parts, ":")
}
