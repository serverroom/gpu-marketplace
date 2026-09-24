package provisioner

import (
	"fmt"
	"net"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/netguard"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// A macOS machine hosts rentals as the same hardened container a DGX Spark
// uses, inside a Linux VM the agent owns and runs with QEMU on Apple's
// Hypervisor.framework (internal/mac). macOS has no built-in Linux like
// Windows' WSL, so the agent creates and manages the VM itself. A Mac cannot
// pass its GPU through to a guest, so a Mac rents its CPUs, memory and disk.

const (
	// MacVMDataDir is the agent's data directory inside its VM.
	MacVMDataDir = "/var/lib/gpu-agent"
	// KindContainerVM is the hardened container inside the agent's own Linux VM:
	// how a macOS machine hosts.
	KindContainerVM = "container-vm"
	// MinMacOS is the oldest macOS the VM needs (Monterey), also Go's floor.
	MinMacOS = 12
	// MacKeepGB is what the Mac's disk keeps free beside a rental's disk.
	MacKeepGB = 20
)

// MacFacts is what the agent reads of a Mac and of the agent's VM on it.
// internal/mac fills it (ReadMac); tests build it by hand.
type MacFacts struct {
	MacOS      int    // major version (15)
	Name       string // "macOS 15.6.1"
	Arch       string // runtime.GOARCH
	TotalMemMB int
	CPUs       int
	HVF        bool // Hypervisor.framework is available (kern.hv_support)
	QEMU       bool // QEMU is installed
	Brew       bool // Homebrew is present (so the setup can install QEMU)
	// Ready: the agent's VM key and disk exist (it may still be booting).
	Ready         bool
	Machine       vmrt.Host
	Dial          func(addr string) (net.Conn, error)
	StorageFreeGB int
	Problem       string
}

// ReadMac reads the Mac; cmd sets it to mac.Facts. nil in a build that cannot.
var ReadMac func() MacFacts

// MacMemAvailableMB is the memory macOS has free now; cmd sets it.
var MacMemAvailableMB func() int

// macFindings are the reasons a Mac cannot host, before its VM is looked into.
func macFindings(f MacFacts, spec vmrt.Spec) (findings []Finding, done bool) {
	add := func(kind ReasonKind, format string, a ...interface{}) {
		findings = append(findings, Finding{Kind: kind, Text: fmt.Sprintf(format, a...)})
	}
	if f.Problem != "" {
		add(ReasonHuman, "%s", f.Problem)
		return findings, true
	}
	if f.MacOS < MinMacOS {
		add(ReasonHuman, "rentals on a Mac run in a Linux VM, which needs macOS 12 (Monterey) or later, and this machine runs macOS %d", f.MacOS)
		return findings, true
	}
	if !f.HVF {
		add(ReasonHuman, "this Mac's hypervisor is not available (kern.hv_support is 0): a Mac in a VM (nested) cannot host, only a physical Mac can")
		return findings, true
	}
	if !f.QEMU && !f.Brew {
		add(ReasonHuman, "QEMU runs the rental's Linux VM and is not installed, and Homebrew is not present to install it; install Homebrew from https://brew.sh, then the setup installs QEMU itself")
		return findings, true
	}
	if !f.QEMU {
		add(ReasonTools, "QEMU is not installed; the automatic setup installs it with Homebrew (or 'gpu-agent setup' in Terminal)")
		return findings, true
	}
	if spec.DiskGB < 20 {
		add(ReasonHuman, "there is not %d GB free for a rental's disk (20 GB for the disk and %d GB kept for macOS); free up space", 20+MacKeepGB, MacKeepGB)
	}
	if !f.Ready {
		add(ReasonTools, "the agent's Linux VM is not set up yet; the automatic setup creates it (or 'gpu-agent setup' in Terminal)")
		return findings, true
	}
	return findings, false
}

// detectMacOS is Detect on macOS.
func detectMacOS(osHost vmrt.Host, arch, dataDir, version string) *Provisioner {
	f := MacFacts{Problem: "this build of the agent cannot read macOS"}
	if ReadMac != nil {
		f = ReadMac()
	}
	if f.Arch == "" {
		f.Arch = arch
	}
	disk := f.StorageFreeGB - MacKeepGB
	if disk < 0 {
		disk = 0
	}
	spec := vmrt.Spec{Arch: arch, DataDir: MacVMDataDir, TotalMemMB: f.TotalMemMB, CPUs: f.CPUs, DiskGB: disk}

	machine := f.Machine
	if machine == nil {
		machine = vmrt.ExecHost{}
	}
	findings, done := macFindings(f, spec)
	if !done {
		findings = containerReasons(machine, spec, version, findings, false)
	}
	var reasons []string
	for i, fd := range findings {
		fd.Text = sudoHint.ReplaceAllString(fd.Text, "run '$1' in Terminal")
		findings[i] = fd
		reasons = append(reasons, fd.Text)
	}

	fence := netguard.New(machine, netguard.Bridge, vmrt.GuestSubnet, netguard.HostNetworks)
	crt := vmrt.NewContainer(machine, spec, fence, vmrt.ContainerImageRef, nil, nil)

	p := New(crt, VendorNone, nil, false)
	p.host = osHost
	p.dataDir = dataDir
	p.version = version
	p.findings = findings
	p.now = time.Now
	p.dialGuest = f.Dial
	p.redetect = func() *Provisioner { return detectMacOS(osHost, arch, dataDir, version) }
	p.midRental = f.Ready && crt.Present()
	p.readHostUse = func() vmrt.HostUse {
		var u vmrt.HostUse
		if MacMemAvailableMB != nil {
			if need, free := spec.GuestMemoryMB(), MacMemAvailableMB(); need > 0 && free >= 0 && free < need {
				u.MemoryShortMB = need - free
			}
		}
		return u
	}

	zero := 0
	gpuCount := &zero
	if f.Problem != "" {
		gpuCount = nil
	}
	cap := control.Capability{
		Ready:        len(reasons) == 0,
		Kind:         KindContainerVM,
		GPUCount:     gpuCount,
		Guest:        &control.Guest{VCPUs: spec.GuestCPUs(), MemoryGB: spec.GuestMemoryMB() / 1024, DiskGB: spec.DiskGB},
		Reasons:      reasons,
		AgentVersion: version,
		VMUser:       "renter",
	}
	if f.Ready {
		if t, err := vmrt.LoadContainerTest(machine, spec.DataDir); err == nil && t != nil {
			cap.SelfTest = &control.SelfTestSummary{Passed: t.Passed, GPUVerified: t.GPUVerified, At: t.At, AgentVersion: t.AgentVersion}
		}
	}
	p.capability = cap
	return p
}
