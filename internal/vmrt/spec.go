package vmrt

import (
	"fmt"
	"strings"
)

// The rental network: one point-to-point /30 on the netguard bridge. The host
// end is the tenant's gateway (and NAT) and nothing else -- the fence drops
// every other packet a tenant sends to the host.
const (
	Tap         = "gpurent0t"
	HostIP      = "10.254.254.1"
	GuestIP     = "10.254.254.2"
	PrefixLen   = 30
	GuestSubnet = "10.254.254.0/30"
	GuestMAC    = "52:54:00:67:70:01"
)

// Firmware is a UEFI code image plus the template its per-VM variable store is
// copied from.
type Firmware struct {
	Code string `json:"code"`
	Vars string `json:"vars"`
	// Secure: a Secure Boot build, whose variable store has the keys enrolled
	// and Secure Boot on (secureboot.go). The VM then runs with SMM.
	Secure bool `json:"secure,omitempty"`
}

// Spec is what this machine can give a rental.
type Spec struct {
	Arch        string   // runtime.GOARCH: amd64 or arm64
	DataDir     string   // /var/lib/gpu-agent
	GoldenImage string   // the baked rental base image (qcow2)
	Firmware    Firmware // found by FindFirmware
	GPUs        []string // primary GPU functions, as PCI addresses
	// NICs are every PCI function of the machine's ConnectX cards. A linked-pair
	// rental hands all of them to its VM; a single rental never touches them.
	NICs    []string
	Unified bool // the GPUs use the machine's memory pool (GB10)
	// DesktopOnDemand: a confirmed DGX Spark that was not made headless on
	// purpose. Its desktop closes while the GPU is rented or tested and comes
	// back after; on any other machine a desktop on the GPU refuses the rental.
	DesktopOnDemand bool
	TotalMemMB      int
	CPUs            int
	DiskGB          int
	// GuestCores pins the VM to these host CPUs (one core type on a machine
	// with big and little cores, ChooseGuestCPUs, unless HomeCores says they
	// are the wider layout); nil: not pinned.
	GuestCores []int
	// GuestCPUName is the guest's CPU in words, for what the renter is told.
	GuestCPUName string
	// HomeCores, when set, makes GuestCores the wider layout of a machine with
	// two core types (layout.go): one vCPU per core of GuestCores, in order,
	// each pinned to its core for the life of the VM. HomeCores is the fastest
	// cluster, GuestCores' first cores: QEMU's own threads run there, as they
	// do in the one-core-type layout the machine goes back to if the wider one
	// fails (HomeCPUName is its CPU in words).
	HomeCores   []int
	HomeCPUName string
	// TrialCores is the wider layout this machine has not tried yet: its next
	// test boot tries it, and it is on offer only once that passed.
	TrialCores   []int
	TrialCPUName string
	// StorageDir holds the base image and the rentals' disks; "" is DataDir.
	StorageDir string
	// SharedGPU: the GPUs reach the rental through the host OS's GPU
	// paravirtualization (WSL 2's /dev/dxg on Windows) rather than a PCI
	// device of this Linux. Nothing in this Linux holds them, and there is no
	// desktop here to close: the programs using them are the host's own, read
	// on the host.
	SharedGPU bool
}

// Storage is where the base image and the rentals' disks live.
func (s Spec) Storage() string {
	if s.StorageDir != "" {
		return s.StorageDir
	}
	return s.DataDir
}

// QEMUBinary is the system emulator for this machine's architecture.
func (s Spec) QEMUBinary() string {
	if s.Arch == "arm64" {
		return "qemu-system-aarch64"
	}
	return "qemu-system-x86_64"
}

// GuestMemoryMB is the memory a rental gets: the machine's, less what the host
// keeps to run itself (a tenth, and never under 4 GB -- or half, on a machine
// of less than 8 GB such as a small ARM board). On a unified-memory machine
// this is ALSO the GPU's memory -- inside the VM, as on the bare machine, the
// GPU works in the same pool the system does. VFIO pins every page of it for
// the life of the VM. 0 means the machine is too small (under 2 GB for the VM).
func (s Spec) GuestMemoryMB() int {
	floor := 4096
	if s.TotalMemMB/2 < floor {
		floor = s.TotalMemMB / 2
	}
	reserve := s.TotalMemMB / 10
	if reserve < floor {
		reserve = floor
	}
	m := s.TotalMemMB - reserve
	if m < 2048 {
		return 0
	}
	return m
}

// GuestCPUs is the vCPUs a rental gets: one per pinned core when the VM is
// pinned (GuestCores), else all but one CPU on a small machine and two on a
// larger one.
func (s Spec) GuestCPUs() int {
	if len(s.GuestCores) > 0 {
		return len(s.GuestCores)
	}
	n := s.CPUs - 1
	if s.CPUs > 8 {
		n = s.CPUs - 2
	}
	if n < 1 {
		n = 1
	}
	return n
}

// EachOnOne reports whether every vCPU is pinned to a core of its own: the
// wider layout of a machine with two core types.
func (s Spec) EachOnOne() bool { return len(s.HomeCores) > 0 }

// widerCores names the wider layout s is in ("4,5,6,7,2,3"), "" when it is in
// any other.
func (s Spec) widerCores() string {
	if !s.EachOnOne() {
		return ""
	}
	return cpuList(s.GuestCores)
}

// LaunchCores are the host CPUs QEMU starts on, and its own threads stay on:
// the fastest cluster in the wider layout (where every register a vCPU starts
// with is read on one core type, as it always was), else GuestCores.
func (s Spec) LaunchCores() []int {
	if s.EachOnOne() {
		return s.HomeCores
	}
	return s.GuestCores
}

// OneCoreType is s as the machine rents on its fastest cores only: what the
// wider layout falls back to. A spec that is not the wider layout is returned
// without its trial.
func (s Spec) OneCoreType() Spec {
	if s.EachOnOne() {
		s.GuestCores, s.GuestCPUName = s.HomeCores, s.HomeCPUName
	}
	s.HomeCores, s.HomeCPUName, s.TrialCores, s.TrialCPUName = nil, "", nil, ""
	return s
}

// Wider is s with the wider layout it has not tried yet in place of its own.
func (s Spec) Wider() Spec {
	s.HomeCores, s.HomeCPUName = s.GuestCores, s.GuestCPUName
	s.GuestCores, s.GuestCPUName = s.TrialCores, s.TrialCPUName
	s.TrialCores, s.TrialCPUName = nil, ""
	return s
}

var firmwareCandidates = map[string][]Firmware{
	"amd64": {
		{Code: "/usr/share/OVMF/OVMF_CODE_4M.fd", Vars: "/usr/share/OVMF/OVMF_VARS_4M.fd"},
		{Code: "/usr/share/OVMF/OVMF_CODE.fd", Vars: "/usr/share/OVMF/OVMF_VARS.fd"},
		{Code: "/usr/share/edk2/ovmf/OVMF_CODE.fd", Vars: "/usr/share/edk2/ovmf/OVMF_VARS.fd"},
	},
	"arm64": {
		{Code: "/usr/share/AAVMF/AAVMF_CODE.fd", Vars: "/usr/share/AAVMF/AAVMF_VARS.fd"},
		{Code: "/usr/share/edk2/aarch64/QEMU_EFI-pflash.raw", Vars: "/usr/share/edk2/aarch64/vars-template-pflash.raw"},
	},
}

// FindFirmware returns the first UEFI firmware pair installed for arch.
func FindFirmware(h Host, arch string) (Firmware, bool) {
	for _, fw := range firmwareCandidates[arch] {
		if h.Exists(fw.Code) && h.Exists(fw.Vars) {
			return fw, true
		}
	}
	return Firmware{}, false
}

// RequiredTools are the host commands the runtime cannot work without.
func RequiredTools(arch string) []string {
	qemu := Spec{Arch: arch}.QEMUBinary()
	return []string{qemu, "qemu-img", "cloud-localds", "cryptsetup", "losetup", "truncate", "nft", "ip", "modprobe", "systemd-run", "systemctl"}
}

// Packages are the Debian/Ubuntu packages that provide RequiredTools and the
// firmware, for `gpu-agent runtime prepare --install-deps`.
func Packages(arch string) []string {
	if arch == "arm64" {
		return []string{"qemu-system-arm", "qemu-utils", "qemu-efi-aarch64", "cloud-image-utils", "cryptsetup-bin", "nftables", "iproute2", "kmod"}
	}
	return []string{"qemu-system-x86", "qemu-utils", "ovmf", "cloud-image-utils", "cryptsetup-bin", "nftables", "iproute2", "kmod"}
}

// InstallHint is the one command that installs everything on Debian/Ubuntu.
func InstallHint(arch string) string {
	return "apt-get install -y " + strings.Join(Packages(arch), " ")
}

// MissingTools lists RequiredTools that are not on PATH.
func MissingTools(h Host, arch string) []string {
	var missing []string
	for _, t := range RequiredTools(arch) {
		if _, err := h.LookPath(t); err != nil {
			missing = append(missing, t)
		}
	}
	return missing
}

// MapperName is the device-mapper name of a rental's encrypted disk.
func MapperName(id string) string { return "gpu-rental-" + id }

func mib(gb int) string { return fmt.Sprintf("%dG", gb) }
