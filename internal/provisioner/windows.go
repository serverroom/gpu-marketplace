package provisioner

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/netguard"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// A Windows machine hosts rentals as the same hardened container a DGX Spark
// uses, inside a WSL 2 distribution the agent owns and runs under a Windows
// account of its own (internal/wsl). WSL 2 is a Hyper-V utility VM, so the
// renter is in a container inside a VM; the GPU reaches it through WSL's GPU
// paravirtualization, served by the host's own NVIDIA driver (CUDA on WSL).

const (
	// WSLDataDir is the agent's data directory inside its distribution.
	WSLDataDir = "/var/lib/gpu-agent"
	// MinWindowsClientBuild is Windows 10 21H2: the oldest client Windows
	// that runs WSL 2.x -- the WSL a service can drive (session 0) -- and CUDA
	// on WSL.
	MinWindowsClientBuild = 19044
	// MinWindowsServerBuild is Windows Server 2022, the first Server with WSL 2.
	MinWindowsServerBuild = 20348
	// MinWSLNVIDIADriver is the oldest NVIDIA driver branch with CUDA on WSL.
	MinWSLNVIDIADriver = 470
	// WindowsKeepGB is what the Windows drive keeps free beside a rental's disk.
	WindowsKeepGB = 20
)

// WinGPU is an NVIDIA GPU as the Windows driver reports it (nvidia-smi.exe).
type WinGPU struct {
	BusID    string // 0000:01:00.0
	Name     string
	UUID     string
	DeviceID string // 10de:2684
	MemoryMB int
	Driver   string // 581.29
}

// WinFacts is what the agent reads of a Windows machine and of its WSL 2
// environment. internal/wsl fills it (ReadWindows); tests build it by hand.
type WinFacts struct {
	Build      int
	Server     bool
	Name       string // "Windows 11 Pro"
	TotalMemMB int
	CPUs       int
	// WSLInstalled: WSL 2.x is installed (%ProgramFiles%\WSL\wsl.exe).
	WSLInstalled bool
	// RestartPending: the setup turned on Windows features that take effect
	// at the next restart.
	RestartPending bool
	// Ready: the agent's Windows account and its distribution both exist.
	Ready bool
	// Machine is the distribution (a vmrt.WSLHost), which has nothing in it
	// until Ready; Dial reaches a rental's container inside it.
	Machine vmrt.Host
	Dial    func(addr string) (net.Conn, error)
	// Storage is the drive the distribution's disk lives (or will live) on.
	StorageDrive  string
	StorageFreeGB int
	GPUs          []WinGPU
	// LANIP and Gateway are the Windows machine's own address and default
	// gateway, which the test boot must find blocked.
	LANIP, Gateway string
	// Problem: the facts could not be read.
	Problem string
}

// ReadWindows reads the Windows machine; cmd sets it to wsl.Facts. nil in a
// build that cannot read Windows.
var ReadWindows func() WinFacts

// WindowsMemAvailableMB is the memory Windows has free now; cmd sets it.
var WindowsMemAvailableMB func() int

var sudoHint = regexp.MustCompile(`run 'sudo (gpu-agent [^']*)'`)

// driverBranch is the major version of an NVIDIA driver ("581.29" -> 581).
func driverBranch(v string) int {
	n, _ := strconv.Atoi(strings.SplitN(strings.TrimSpace(v), ".", 2)[0])
	return n
}

// windowsFindings are the reasons a Windows machine cannot host, before its
// distribution is looked into. done says nothing further is worth checking.
func windowsFindings(f WinFacts, spec vmrt.Spec) (findings []Finding, done bool) {
	add := func(kind ReasonKind, format string, a ...interface{}) {
		findings = append(findings, Finding{Kind: kind, Text: fmt.Sprintf(format, a...)})
	}
	if f.Problem != "" {
		add(ReasonHuman, "%s", f.Problem)
		return findings, true
	}
	switch {
	case f.Server && f.Build < MinWindowsServerBuild:
		add(ReasonHuman, "rentals on Windows run in WSL 2, which needs Windows Server 2022 or later, and this machine runs an older Windows Server (build %d)", f.Build)
		return findings, true
	case !f.Server && f.Build < MinWindowsClientBuild:
		add(ReasonHuman, "rentals on Windows run in WSL 2, which needs Windows 10 version 21H2 or later, and this machine runs build %d: update it with Windows Update (Windows 10 22H2 and Windows 11 are free updates)", f.Build)
		return findings, true
	}
	if spec.Arch != "amd64" && spec.Arch != "arm64" {
		add(ReasonHuman, "rentals on Windows need a 64-bit x86 or Arm processor, and this machine is %s", spec.Arch)
		return findings, true
	}
	for _, g := range f.GPUs {
		if b := driverBranch(g.Driver); b < MinWSLNVIDIADriver {
			add(ReasonHuman, "the NVIDIA driver on this machine (%s) is too old to give %s to a rental in WSL 2: install NVIDIA driver %d or later", g.Driver, g.Name, MinWSLNVIDIADriver)
		}
	}
	if f.RestartPending {
		add(ReasonHuman, "Windows must restart to finish installing WSL 2 (the setup turned on the Virtual Machine Platform); restart it and the setup carries on by itself")
		return findings, true
	}
	if spec.DiskGB < 20 {
		add(ReasonHuman, "there is not %d GB free on drive %s for a rental's disk (20 GB for the disk and %d GB kept free for Windows); free up space there", 20+WindowsKeepGB, f.StorageDrive, WindowsKeepGB)
	}
	if !f.WSLInstalled {
		add(ReasonTools, "WSL 2 is not installed; the automatic setup installs it (or run 'gpu-agent setup' in an Administrator PowerShell)")
		return findings, true
	}
	if !f.Ready {
		add(ReasonTools, "the agent's Linux environment in WSL 2 is not set up yet; the automatic setup creates it (or run 'gpu-agent setup' in an Administrator PowerShell)")
		return findings, true
	}
	return findings, false
}

// dxgVerifier checks, after a rental's container is gone, that nothing in the
// distribution still holds WSL's GPU device.
func dxgVerifier(h vmrt.Host) func([]vmrt.BoundDevice) bool {
	return func([]vmrt.BoundDevice) bool {
		out, err := h.Output("/bin/sh", "-c", `for p in /proc/[0-9]*; do ls -l "$p/fd" 2>/dev/null | grep -q /dev/dxg && echo "${p#/proc/}"; done; true`)
		return err == nil && strings.TrimSpace(out) == ""
	}
}

// detectWindows is Detect on Windows.
func detectWindows(osHost vmrt.Host, arch, dataDir, version string) *Provisioner {
	f := WinFacts{Problem: "this build of the agent cannot read Windows"}
	if ReadWindows != nil {
		f = ReadWindows()
	}
	disk := f.StorageFreeGB - WindowsKeepGB
	if disk < 0 {
		disk = 0
	}
	spec := vmrt.Spec{
		Arch: arch, DataDir: WSLDataDir, TotalMemMB: f.TotalMemMB, CPUs: f.CPUs, DiskGB: disk,
		SharedGPU: true,
	}
	for _, g := range f.GPUs {
		spec.GPUs = append(spec.GPUs, g.BusID)
	}

	machine := f.Machine
	if machine == nil {
		machine = vmrt.WSLHost{}
	}
	findings, done := windowsFindings(f, spec)
	if !done {
		findings = containerReasons(machine, spec, version, findings, false)
	}
	var reasons []string
	for i, fd := range findings {
		// The shared checks name Linux's way to run a command as root.
		fd.Text = sudoHint.ReplaceAllString(fd.Text, "run '$1' in an Administrator PowerShell")
		findings[i] = fd
		reasons = append(reasons, fd.Text)
	}

	var cdi []string
	vendor := VendorNone
	if len(spec.GPUs) > 0 {
		// WSL's CDI spec has one device, all of WSL's GPUs: they are one
		// paravirtualized device (/dev/dxg), never split per GPU.
		cdi = []string{"nvidia.com/gpu=all"}
		vendor = VendorContainerNV
	}
	fence := netguard.New(machine, netguard.Bridge, vmrt.GuestSubnet, netguard.HostNetworks)
	crt := vmrt.NewContainer(machine, spec, fence, vmrt.ContainerImageRef, cdi, dxgVerifier(machine))
	if f.Gateway != "" {
		crt.AddProbes(net.JoinHostPort(f.Gateway, "80"))
	}
	if f.LANIP != "" {
		crt.AddProbes(net.JoinHostPort(f.LANIP, "445"))
	}

	p := New(crt, vendor, spec.GPUs, false)
	p.host = osHost
	p.dataDir = dataDir
	p.version = version
	p.findings = findings
	p.now = time.Now
	p.dialGuest = f.Dial
	p.redetect = func() *Provisioner { return detectWindows(osHost, arch, dataDir, version) }
	p.midRental = f.Ready && crt.Present()
	p.readHostUse = func() vmrt.HostUse {
		var u vmrt.HostUse
		if WindowsMemAvailableMB != nil {
			if need, free := spec.GuestMemoryMB(), WindowsMemAvailableMB(); need > 0 && free >= 0 && free < need {
				u.MemoryShortMB = need - free
			}
		}
		return u
	}

	// The GPU count is left out when Windows could not be read: unknown is
	// not none.
	var gpuCount *int
	if f.Problem == "" {
		count := len(f.GPUs)
		gpuCount = &count
	}
	cap := control.Capability{
		Ready:        len(reasons) == 0,
		Kind:         KindContainerWSL,
		GPUCount:     gpuCount,
		Guest:        &control.Guest{VCPUs: spec.GuestCPUs(), MemoryGB: spec.GuestMemoryMB() / 1024, DiskGB: spec.DiskGB},
		Reasons:      reasons,
		AgentVersion: version,
		VMUser:       "renter",
	}
	for _, g := range f.GPUs {
		cap.GPUs = append(cap.GPUs, control.GPU{Model: g.Name, PCIID: g.DeviceID, MemoryMB: g.MemoryMB, Driver: "nvidia"})
	}
	if f.Ready {
		if t, err := vmrt.LoadContainerTest(machine, spec.DataDir); err == nil && t != nil {
			cap.SelfTest = &control.SelfTestSummary{Passed: t.Passed, GPUVerified: t.GPUVerified, At: t.At, AgentVersion: t.AgentVersion}
		}
	}
	p.capability = cap
	return p
}
