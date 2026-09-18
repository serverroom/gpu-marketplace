package vmrt

import (
	"regexp"
	"strings"
)

// The NVIDIA driver baked into the rental image is chosen to match the host's
// own: what drives this GPU on the bare machine is what drives it inside the
// microVM. Branch for branch, and the same kind of kernel module -- a CMP 170HX
// needed the proprietary 580-server, while a DGX Spark ships the open one.

// Where a driver choice came from, as recorded in GoldenInfo.DriverSource.
const (
	// DriverFromHost: matched to the host's driver.
	DriverFromHost = "host"
	// DriverDefault: the host's driver could not be read, so DefaultDriver.
	DriverDefault = "default"
	// DriverFromFlag: a person named it with --driver. Never second-guessed.
	DriverFromFlag = "flag"
	// DriverNoGPU: the machine has no NVIDIA GPU, so the image gets none.
	DriverNoGPU = "no-gpu"
)

// NVIDIAVersionFile is where the loaded NVIDIA kernel module says what it is.
const NVIDIAVersionFile = "/proc/driver/nvidia/version"

var driverBranchPattern = regexp.MustCompile(`^[0-9]{3}$`)

// DriverChoice is the driver branch a rental image gets, and why.
type DriverChoice struct {
	Driver      string // e.g. "580-server-open"
	HostVersion string // the host's driver version ("580.95.05"); "" when it could not be read
	Open        bool   // the host runs NVIDIA's open kernel module
	Source      string // DriverFromHost or DriverDefault
}

// Describe is the host driver in words, for logs and GoldenInfo.
func (c DriverChoice) Describe() string {
	if c.Source == DriverNoGPU {
		return "no NVIDIA GPU"
	}
	if c.HostVersion == "" {
		return "unknown"
	}
	if c.Open {
		return c.HostVersion + ", open kernel module"
	}
	return c.HostVersion + ", proprietary kernel module"
}

// hostDriverVersion is the host's NVIDIA driver version, from nvidia-smi.
func hostDriverVersion(h Host) string {
	out, err := h.Output("nvidia-smi", "--query-gpu=driver_version", "--format=csv,noheader")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if v := strings.TrimSpace(line); v != "" {
			return v
		}
	}
	return ""
}

// hostRunsOpenModule reports whether the loaded NVIDIA kernel module is the
// open one: its version file says so, or its licence is the open one's.
func hostRunsOpenModule(h Host) bool {
	if v, err := h.ReadFile(NVIDIAVersionFile); err == nil && strings.Contains(string(v), "Open Kernel Module") {
		return true
	}
	out, err := h.Output("modinfo", "-F", "license", "nvidia")
	return err == nil && strings.TrimSpace(out) == "Dual MIT/GPL"
}

// ChooseDriver picks the driver for the rental image to match this host:
// "<branch>-server-open" on a host running the open kernel module, else
// "<branch>-server", where <branch> is the major version nvidia-smi reports.
// When that cannot be read, DefaultDriver.
func ChooseDriver(h Host) DriverChoice {
	c := DriverChoice{HostVersion: hostDriverVersion(h)}
	branch, _, _ := strings.Cut(c.HostVersion, ".")
	if !driverBranchPattern.MatchString(branch) {
		c.HostVersion = ""
		c.Driver, c.Source = DefaultDriver, DriverDefault
		return c
	}
	c.Open = hostRunsOpenModule(h)
	c.Driver = branch + "-server"
	if c.Open {
		c.Driver += "-open"
	}
	c.Source = DriverFromHost
	return c
}

// HasNVIDIAGPU reports whether this machine has an NVIDIA GPU: one nvidia-smi
// lists, or one on the PCI bus without a working driver yet.
func HasNVIDIAGPU(h Host) bool {
	out, err := h.Output("nvidia-smi", "--query-gpu=pci.bus_id,name,memory.total", "--format=csv,noheader,nounits")
	if err == nil && strings.TrimSpace(out) != "" {
		return true
	}
	return len(NVIDIAPCIGPUs(h)) > 0
}

// ChooseImageDriver is the driver a rental image gets on this machine:
// NoDriver on a machine with no NVIDIA GPU at all (none nvidia-smi lists, none
// on the PCI bus), else ChooseDriver's match to the host's own driver.
func ChooseImageDriver(h Host) DriverChoice {
	if !HasNVIDIAGPU(h) {
		return DriverChoice{Driver: NoDriver, Source: DriverNoGPU}
	}
	return ChooseDriver(h)
}
