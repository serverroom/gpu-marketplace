// Package wsl is how a Windows machine hosts rentals: in a WSL 2 distribution
// the agent owns, run under a Windows account of its own, with the same
// hardened container a Linux host uses (vmrt.ContainerRuntime) inside it.
//
// Why an account of its own: WSL cannot be driven as LocalSystem (the service
// the agent runs as), but since WSL 2.0 it can from session 0 as a normal user
// (%ProgramFiles%\WSL\wsl.exe). WSL runs one utility VM per Windows user, so
// the agent's account gets its own VM -- apart from any WSL the host person
// uses -- and its own .wslconfig (memory, processors, NAT networking), which
// the agent writes without touching the person's.
package wsl

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/serverroom/gpu-marketplace/internal/provisioner"
)

const (
	// Distro is the agent's WSL distribution.
	Distro = "gpu-agent"
	// Account is the local Windows account the distribution runs under.
	Account = "gpu-agent-wsl"
	// rootfsBase is Canonical's WSL image of Ubuntu 24.04, checked against the
	// SHA256SUMS published beside it.
	rootfsBase = "https://cloud-images.ubuntu.com/wsl/releases/24.04/current/"
	// wslReleases is where the WSL 2 installer comes from on a machine that
	// has no WSL 2.x yet (Microsoft's own releases, Authenticode-signed).
	wslReleases = "https://api.github.com/repos/microsoft/WSL/releases/latest"
)

// rootfsName is the image file for arch.
func rootfsName(arch string) string {
	if arch == "arm64" {
		return "ubuntu-noble-wsl-arm64-wsl.rootfs.tar.gz"
	}
	return "ubuntu-noble-wsl-amd64-wsl.rootfs.tar.gz"
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

// wslConf is /etc/wsl.conf in the distribution: nothing of Windows reaches
// inside -- no drives mounted, no Windows programs runnable, no Windows PATH --
// and root is the default user (the agent is root in its own distribution; the
// renter never is: they are in a container whose root is an unprivileged uid).
const wslConf = `# Written by gpu-agent: this distribution hosts GPU marketplace rentals.
[boot]
systemd=false

[automount]
enabled=false
mountFsTab=false

[interop]
enabled=false
appendWindowsPath=false

[network]
hostname=gpu-agent
generateHosts=true
generateResolvConf=true

[user]
default=root

[gpu]
enabled=true
`

// containersConf is podman's configuration without systemd: cgroups managed
// directly, events to a file.
const containersConf = `# Written by gpu-agent (WSL has no systemd here).
[engine]
cgroup_manager = "cgroupfs"
events_logger = "file"
`

// WSLConfig is the agent account's %UserProfile%\.wslconfig: the utility VM
// sized for one rental and what the distribution needs beside it, behind
// Windows' own NAT (never mirrored onto the host's networks), with nothing
// forwarded to Windows' localhost and no GUI.
func WSLConfig(memMB, cpus int) string {
	var b strings.Builder
	b.WriteString("# Written by gpu-agent: the VM of its WSL distribution.\n[wsl2]\n")
	if memMB > 0 {
		fmt.Fprintf(&b, "memory=%dMB\n", memMB)
	}
	if cpus > 0 {
		fmt.Fprintf(&b, "processors=%d\n", cpus)
	}
	b.WriteString("swap=0\nnetworkingMode=nat\nlocalhostForwarding=false\ndnsTunneling=false\nguiApplications=false\nnestedVirtualization=false\n")
	b.WriteString("\n[experimental]\nautoMemoryReclaim=dropCache\n")
	return b.String()
}

// VMMemoryMB is the utility VM's memory: the rental's, plus 1 GB for the
// distribution beside it, never more than the machine less 2 GB.
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

// parseNVIDIA reads `nvidia-smi --query-gpu=pci.bus_id,name,uuid,pci.device_id,
// memory.total,driver_version --format=csv,noheader,nounits`.
func parseNVIDIA(out string) []provisioner.WinGPU {
	var gpus []provisioner.WinGPU
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 6 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		g := provisioner.WinGPU{BusID: busID(f[0]), Name: f[1], UUID: f[2], DeviceID: deviceID(f[3]), Driver: f[5]}
		g.MemoryMB, _ = strconv.Atoi(f[4])
		if g.BusID == "" || g.Name == "" {
			continue
		}
		gpus = append(gpus, g)
	}
	return gpus
}

// busID turns nvidia-smi's "00000000:01:00.0" into "0000:01:00.0".
func busID(s string) string {
	s = strings.ToLower(s)
	if i := strings.Index(s, ":"); i > 4 {
		s = s[i-4:]
	}
	return s
}

// deviceID turns nvidia-smi's "0x268410DE" (device, then vendor) into "10de:2684".
func deviceID(s string) string {
	s = strings.ToLower(strings.TrimPrefix(strings.ToLower(s), "0x"))
	if len(s) != 8 {
		return ""
	}
	return s[4:] + ":" + s[:4]
}

// parseGateway finds the default gateway in `route print -4 0.0.0.0`: the
// first route whose destination and mask are both 0.0.0.0.
func parseGateway(out string) string {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 5 && f[0] == "0.0.0.0" && f[1] == "0.0.0.0" {
			if ip := net.ParseIP(f[2]); ip != nil && ip.To4() != nil {
				return ip.String()
			}
		}
	}
	return ""
}
