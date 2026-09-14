package provisioner

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/serverroom/gpu-marketplace/internal/control"
)

// KindKataVFIO names the only way this agent hosts a rental: a Kata microVM
// with the GPUs handed to it over VFIO, behind the netguard fence.
const KindKataVFIO = "kata-vfio"

// RuntimeHelpers are the host commands a rental cannot start without. The three
// gpu-agent-* helpers are the microVM runtime itself; they are NOT shipped with
// the agent binary, which is why a stock install reports "not ready" instead of
// pretending it could deliver.
var RuntimeHelpers = []string{"gpu-agent-kata", "gpu-agent-mkdisk", "gpu-agent-injectkey", "cryptsetup", "nft"}

// Probe reads the host facts preflight needs. Real hosts use OSProbe.
type Probe interface {
	LookPath(name string) (string, error)
	Exists(path string) bool
	CountEntries(dir string) int
}

// OSProbe reads the real host.
type OSProbe struct{}

func (OSProbe) LookPath(name string) (string, error) { return exec.LookPath(name) }

func (OSProbe) Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (OSProbe) CountEntries(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	return len(entries)
}

// HostReport is what preflight found: the GPUs a rental would get, and every
// reason this machine cannot host one. No reasons means ready.
type HostReport struct {
	Vendor  GPUVendor
	BDFs    []string
	Reasons []string
}

// Preflight checks, without changing anything, whether this machine can host a
// rental: a Linux KVM host with the IOMMU on, discrete GPUs whose memory is
// their own, and the microVM runtime installed. Every failing check is reported
// in words a provider can act on, not just the first.
func Preflight(r Runner, probe Probe, goos, goldenImage string) HostReport {
	var rep HostReport
	add := func(format string, a ...interface{}) {
		rep.Reasons = append(rep.Reasons, fmt.Sprintf(format, a...))
	}

	switch goos {
	case "linux":
	case "darwin":
		rep.Vendor = VendorApple
		add("%v: Apple Silicon cannot pass a GPU through to a microVM, and its GPU memory is the system's own memory", ErrVendorCannotIsolate)
		return rep
	default:
		add("rentals run inside a Linux KVM microVM, and this machine runs %s", goos)
		return rep
	}

	if !probe.Exists("/dev/kvm") {
		add("KVM is not available (/dev/kvm is missing): enable virtualisation in the firmware and load the kvm module")
	}
	if probe.CountEntries("/sys/kernel/iommu_groups") == 0 {
		add("the IOMMU is off, so no GPU can be handed to a microVM: enable VT-d / AMD-Vi (or the SMMU on Arm) in the firmware and on the kernel command line")
	}

	if bdfs, unified, ok := detectNVIDIA(r); ok {
		rep.Vendor = VendorNVIDIA
		rep.BDFs = bdfs
		for _, bdf := range unified {
			add("GPU %s shares its memory with the host (unified memory, as on the GB10 in a DGX Spark): there is no separate GPU memory to hand to a tenant, and none that can be proven clean after a rental", bdf)
		}
	} else if bdfs, ok := detectAMD(r); ok {
		rep.Vendor = VendorAMD
		rep.BDFs = bdfs
	} else {
		add("no NVIDIA or AMD GPU was detected")
	}

	var missing []string
	for _, h := range RuntimeHelpers {
		if _, err := probe.LookPath(h); err != nil {
			missing = append(missing, h)
		}
	}
	if len(missing) > 0 {
		add("the rental runtime is not installed (missing: %s); it does not ship with this agent release, so no rental can start on this machine yet", strings.Join(missing, ", "))
	} else if !probe.Exists(goldenImage) {
		add("the rental base image is missing (%s)", goldenImage)
	}
	return rep
}

// Detect runs preflight and returns a provisioner whose Capability says what it
// found. It is the only constructor production code should use.
func Detect(r Runner, probe Probe, goos, goldenImage, diskDir, agentVersion string) *Provisioner {
	rep := Preflight(r, probe, goos, goldenImage)
	p := New(r, goldenImage, diskDir, rep.BDFs, rep.Vendor)
	p.capability = control.Capability{
		Ready:        len(rep.Reasons) == 0,
		Kind:         KindKataVFIO,
		Reasons:      rep.Reasons,
		AgentVersion: agentVersion,
	}
	return p
}

// detectNVIDIA lists the NVIDIA GPUs by PCI address. unified holds the ones
// with no memory of their own: nvidia-smi reports memory.total as [N/A] (or 0)
// for a GPU that shares the host's memory pool, which is exactly what a GB10
// does.
func detectNVIDIA(r Runner) (bdfs, unified []string, ok bool) {
	out, err := r.Output("nvidia-smi", "--query-gpu=pci.bus_id,memory.total", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, nil, false
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 2 {
			continue
		}
		bdf := NormalizeBDF(fields[0])
		if bdf == "" {
			continue
		}
		bdfs = append(bdfs, bdf)
		if total, err := strconv.ParseFloat(strings.TrimSpace(fields[1]), 64); err != nil || total <= 0 {
			unified = append(unified, bdf)
		}
	}
	return bdfs, unified, len(bdfs) > 0
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
