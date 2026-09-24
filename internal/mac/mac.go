// Package mac is how a macOS machine hosts rentals: in the same hardened
// container a DGX Spark uses, inside a Linux VM the agent owns and runs with
// QEMU on Apple's Hypervisor.framework (-accel hvf, on Intel and Apple Silicon
// alike). macOS has no built-in Linux environment like Windows' WSL, so the
// agent creates and manages the VM itself; the container runtime, the fence
// and the encrypted volumes run inside it exactly as on a native Linux host.
//
// A Mac cannot pass its GPU through to a guest, so a Mac rents its CPUs,
// memory and disk. Apple Silicon may later offer the guest Vulkan compute
// (venus/virtio-gpu); that is not built here.
package mac

import (
	"fmt"
	"strings"
)

const (
	// Dir under the agent's data directory holds the VM: its disk, the base
	// image, the seed and the agent's key to it.
	VMName = "gpu-agent-vm"
	// GuestSSHPort is the loopback port on the Mac that QEMU forwards to the
	// VM's sshd; the agent reaches the VM there.
	GuestSSHPort = 52422
	// rootfsBase is Canonical's Ubuntu 24.04 cloud image (a raw disk in a
	// tarball), checked against the SHA256SUMS published beside it.
	rootfsBase = "https://cloud-images.ubuntu.com/releases/noble/release/"
	// MinMacOS is the oldest macOS with Hypervisor.framework virtio support
	// the VM needs (Monterey); it is also Go's floor.
	MinMacOS = 12
	// KeepGB is what the Mac's disk keeps free beside a rental's disk.
	KeepGB = 20
)

// rootfsName is the Ubuntu cloud-image tarball for arch.
func rootfsName(arch string) string {
	a := "amd64"
	if arch == "arm64" {
		a = "arm64"
	}
	return "ubuntu-24.04-server-cloudimg-" + a + ".tar.gz"
}

// sumFor finds name's SHA-256 in a SHA256SUMS file.
func sumFor(sums, name string) string {
	for _, line := range strings.Split(sums, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

// VMMemoryMB is the VM's memory: the rental's, plus 1 GB for the guest system
// and the container beside it, never more than the machine less 2 GB for macOS.
func VMMemoryMB(totalMB, guestMB int) int {
	mb := guestMB + 1024
	if max := totalMB - 2048; mb > max {
		mb = max
	}
	if mb < 0 {
		return 0
	}
	return mb
}

// qemuBinary is the system emulator for arch (each Mac runs its own
// architecture under hvf; there is no cross-emulation for a rental).
func qemuBinary(arch string) string {
	if arch == "arm64" {
		return "qemu-system-aarch64"
	}
	return "qemu-system-x86_64"
}

// QEMUArgs is the command line for the agent's VM: the machine's own
// architecture on Apple's hypervisor, the rental disk image, the cloud-init
// seed, and user-mode networking with one loopback forward to the VM's sshd.
// User-mode networking gives the guest a private 10.0.2.0/24 and NATs it out;
// the guest never sits on the Mac's LAN, and the fence inside it blocks every
// private range regardless. No display, no sound, no shared folder.
func QEMUArgs(o VMConfig) []string {
	args := []string{
		"-machine", machineType(o.Arch),
		"-accel", "hvf",
		"-cpu", "host",
		"-smp", fmt.Sprintf("%d", o.CPUs),
		"-m", fmt.Sprintf("%dM", o.MemMB),
		"-nographic", "-nodefaults", "-serial", "file:" + o.SerialLog,
		"-drive", "if=virtio,format=qcow2,file=" + o.Disk,
		"-drive", "if=virtio,format=raw,file=" + o.Seed + ",readonly=on",
		"-netdev", fmt.Sprintf("user,id=net0,hostfwd=tcp:127.0.0.1:%d-:22", o.SSHPort),
		"-device", "virtio-net-pci,netdev=net0",
		"-device", "virtio-rng-pci",
	}
	if o.Arch == "arm64" {
		// Apple Silicon has no legacy firmware; the VM boots UEFI from the
		// pflash the setup laid down.
		args = append(args, "-bios", o.Firmware)
	}
	return args
}

func machineType(arch string) string {
	if arch == "arm64" {
		return "virt,highmem=on"
	}
	return "q35"
}

// VMConfig is one boot of the agent's VM.
type VMConfig struct {
	Arch      string
	CPUs      int
	MemMB     int
	Disk      string // qcow2 overlay on the base image
	Seed      string // cloud-init NoCloud seed (raw image, label cidata)
	Firmware  string // UEFI code (arm64)
	SerialLog string
	SSHPort   int
}
