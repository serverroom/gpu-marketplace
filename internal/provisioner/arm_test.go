package provisioner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

func statsFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../stats/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// rk3588 is a Radxa ROCK 5B-like board on Armbian: 8 cores (4x Cortex-A55 at
// 1.8 GHz, 4x Cortex-A76 at 2.4 GHz), 15 GB, no GPU anyone rents (its Mali is
// a platform device, not on PCI), KVM, the runtime, an image without a driver,
// a passing test boot, and room on its NVMe.
func rk3588(t *testing.T) *fakehost.Host {
	t.Helper()
	h := fakehost.New()
	h.Files["/proc/cpuinfo"] = statsFixture(t, "cpuinfo-rk3588")
	h.Files["/proc/device-tree/compatible"] = statsFixture(t, "dt-compatible-rock5b")
	h.Files["/proc/device-tree/model"] = statsFixture(t, "dt-model-rock5b")
	for cpu := 0; cpu < 8; cpu++ {
		khz := "1800000"
		if cpu >= 4 {
			khz = "2400000"
		}
		h.Files[fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq/cpuinfo_max_freq", cpu)] = []byte(khz + "\n")
	}
	h.Files["/dev/kvm"] = nil
	h.Files["/etc/os-release"] = []byte("PRETTY_NAME=\"Armbian 25.8 bookworm\"\nID=debian\nVERSION_ID=\"12\"\n")
	h.Files["/usr/share/AAVMF/AAVMF_CODE.fd"] = nil
	h.Files["/usr/share/AAVMF/AAVMF_VARS.fd"] = nil
	golden, _ := json.Marshal(vmrt.GoldenInfo{Base: "resolute-server-cloudimg-arm64.img", Driver: vmrt.NoDriver, Extras: []string{"rdma"}})
	h.Files[filepath.Join(dataDir, "golden.img")] = nil
	h.Files[filepath.Join(dataDir, "golden.img")+".json"] = golden
	pass, _ := json.Marshal(vmrt.SelfTestResult{Passed: true, AgentVersion: version})
	h.Files[vmrt.SelfTestPath(dataDir)] = pass
	h.Files["/proc/meminfo"] = []byte("MemTotal:       15728640 kB\n")
	h.Files[dataDir] = nil
	h.Outputs["df --output=avail"] = " Avail\n  900G\n"
	h.Outputs["df -B1G --output=source,target,fstype,avail "+dataDir] = "Filesystem Mounted on Type Avail\n/dev/nvme0n1p1 / ext4 900G\n"
	return h
}

func detectARM(t *testing.T, h *fakehost.Host) *Provisioner {
	t.Helper()
	noRelay(t)
	return Detect(h, "linux", "arm64", dataDir, version)
}

func TestAnRK3588BoardHosts(t *testing.T) {
	p := detectARM(t, rk3588(t))
	c := p.Capability()
	if !c.Ready || c.Kind != KindQEMU || c.GPUCount == nil || *c.GPUCount != 0 {
		t.Fatalf("capability = %+v", c)
	}
	// The renter gets the four Cortex-A76 cores, pinned; the host keeps the A55s.
	if c.Guest == nil || *c.Guest != (control.Guest{VCPUs: 4, MemoryGB: 11, DiskGB: 500, CPU: "Cortex-A76"}) {
		t.Errorf("guest = %+v", c.Guest)
	}
	if got := p.Runtime().Spec().GuestCores; fmt.Sprint(got) != "[4 5 6 7]" {
		t.Errorf("pinned to %v", got)
	}
	data, _ := json.Marshal(c)
	if !strings.Contains(string(data), `"guest":{"vcpus":4,"memory_gb":11,"disk_gb":500,"cpu":"Cortex-A76"}`) {
		t.Errorf("capability JSON = %s", data)
	}
	if c.Identity == nil || c.Identity.ConfirmedDGXSpark {
		t.Errorf("identity = %+v", c.Identity)
	}
}

func TestADGXSparkRunsItsRentalsOnTheX925Cores(t *testing.T) {
	h := sparkMachine(t, "58:a2:e1:00:00:0")
	h.Files["/proc/cpuinfo"] = statsFixture(t, "cpuinfo-gb10")
	for cpu := 0; cpu < 20; cpu++ {
		khz := "2808000"
		if cpu < 10 {
			khz = "3900000"
		}
		h.Files[fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq/cpuinfo_max_freq", cpu)] = []byte(khz + "\n")
	}
	p := detectARM(t, h)
	c := p.Capability()
	if !c.Ready || c.Guest == nil || c.Guest.VCPUs != 10 || c.Guest.CPU != "Cortex-X925" || c.Kind != KindQEMUVFIO || *c.GPUCount != 1 {
		t.Fatalf("capability = %+v guest = %+v", c, c.Guest)
	}
}

func TestANeoverseServerIsNotPinned(t *testing.T) {
	h := rk3588(t)
	h.Files["/proc/cpuinfo"] = statsFixture(t, "cpuinfo-altra")
	delete(h.Files, "/proc/device-tree/compatible")
	p := detectARM(t, h)
	if s := p.Runtime().Spec(); len(s.GuestCores) != 0 || s.GuestCPUName != "Neoverse-N1" {
		t.Errorf("spec = %+v", s)
	}
}

func TestAnX86MachineNamesItsCPU(t *testing.T) {
	h := cpuHost(t)
	h.Files["/proc/cpuinfo"] = statsFixture(t, "cpuinfo-x86")
	noRelay(t)
	c := Detect(h, "linux", "amd64", dataDir, version).Capability()
	if c.Guest == nil || c.Guest.CPU != "AMD Ryzen 9 7950X 16-Core Processor" {
		t.Errorf("guest = %+v", c.Guest)
	}
}

// Every way a vendor kernel or a small board falls short is named in words a
// host can act on.
func TestAnRK3588SaysWhatItLacks(t *testing.T) {
	for name, c := range map[string]struct {
		spoil func(h *fakehost.Host)
		want  string
	}{
		"no KVM in the kernel": {func(h *fakehost.Host) { delete(h.Files, "/dev/kvm") },
			"this machine's Linux kernel has no KVM (/dev/kvm is missing), so it cannot run rental VMs: install a kernel with KVM enabled (for Rockchip boards, e.g. a mainline or 'edge' kernel)"},
		"firmware below EL2": {func(h *fakehost.Host) {
			delete(h.Files, "/dev/kvm")
			h.Outputs["dmesg"] = "[    0.000000] CPU: All CPU(s) started at EL1\n[    0.354227] kvm [1]: HYP mode not available\n"
		}, "firmware starts Linux without virtualisation"},
		"no nf_tables": {func(h *fakehost.Host) { h.Fail["modprobe -n -q nf_tables"] = errors.New("not found") },
			"has no nf_tables, the firewall that fences a rental off your network"},
		"no dm-crypt": {func(h *fakehost.Host) { h.Fail["modprobe -n -q dm_crypt"] = errors.New("not found") }, "has no dm-crypt"},
		"no tun":      {func(h *fakehost.Host) { h.Fail["modprobe -n -q tun"] = errors.New("not found") }, "has no TUN/TAP support"},
		"an SD card": {func(h *fakehost.Host) {
			h.Outputs["df -B1G --output=source,target,fstype,avail "+dataDir] = "Filesystem Mounted on Type Avail\n/dev/mmcblk1p2 / ext4 50G\n"
			h.Files["/sys/block/mmcblk1/device/type"] = []byte("SD\n")
		}, "would go on an SD card (/dev/mmcblk1p2 holds"},
		"a USB stick": {func(h *fakehost.Host) {
			h.Outputs["df -B1G --output=source,target,fstype,avail "+dataDir] = "Filesystem Mounted on Type Avail\n/dev/sda1 / ext4 50G\n"
			h.Files["/sys/block/sda/removable"] = []byte("1\n")
		}, "would go on removable storage"},
		"a small eMMC, an NVMe mounted": {func(h *fakehost.Host) {
			h.Outputs["df --output=avail"] = " Avail\n  5G\n"
			h.Outputs["df -B1G --output=source,target,fstype,avail "+dataDir] = "Filesystem Mounted on Type Avail\n/dev/mmcblk0p2 / ext4 5G\n"
			h.Outputs["df -B1G --output=source,target,fstype,avail"] = "Filesystem Mounted on Type Avail\n/dev/mmcblk0p2 / ext4 5G\n" +
				"tmpfs /run tmpfs 2G\n/dev/mmcblk0p1 /boot vfat 1G\n/dev/nvme0n1p1 /mnt/nvme ext4 900G\n/dev/sda1 /media/stick ext4 60G\n"
			h.Files["/sys/block/mmcblk0/device/type"] = []byte("MMC\n")
			h.Files["/sys/block/sda/removable"] = []byte("1\n")
		}, "not 20 GB free for a rental's disk under /var/lib/gpu-agent after the 20 GB the machine keeps for itself; /mnt/nvme has 900 GB free, so the automatic setup keeps the rentals' disks in /mnt/nvme/gpu-agent"},
		"a small eMMC and nothing else": {func(h *fakehost.Host) { h.Outputs["df --output=avail"] = " Avail\n  5G\n" },
			"add a disk (NVMe, SATA or a USB drive -- not an SD card)"},
		"a non-apt system": {func(h *fakehost.Host) {
			h.Tools = map[string]bool{"modprobe": true}
			h.Files["/etc/os-release"] = []byte("ID=fedora\n")
		}, "on this system install QEMU, UEFI firmware for it, cloud-image-utils, cryptsetup and nftables with its own package manager"},
		"2 GB": {func(h *fakehost.Host) { h.Files["/proc/meminfo"] = []byte("MemTotal:        2000000 kB\n") },
			"a rental needs at least 4 GB"},
	} {
		h := rk3588(t)
		c.spoil(h)
		got := strings.Join(detectARM(t, h).Capability().Reasons, " | ")
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: reasons %q, want %q", name, got, c.want)
		}
	}
}

func TestAptDistros(t *testing.T) {
	for release, want := range map[string]bool{
		"ID=ubuntu\nVERSION_ID=\"26.04\"\n":                   true,
		"ID=debian\nVERSION_ID=\"12\"\n":                      true,
		"ID=armbian\nID_LIKE=debian\n":                        true,
		"ID=\"ubuntu-rockchip\"\nID_LIKE=\"ubuntu debian\"\n": true,
		"ID=fedora\nID_LIKE=\"rhel centos fedora\"\n":         false,
		"ID=arch\n": false,
		"ID=\"opensuse-tumbleweed\"\nID_LIKE=\"opensuse suse\"\n": false,
	} {
		if got := isDebianLike(release); got != want {
			t.Errorf("%q: %v, want %v", release, got, want)
		}
	}
	// The package names are the same on Ubuntu and Debian (Armbian included).
	if got := strings.Join(vmrt.Packages("arm64"), " "); got != "qemu-system-arm qemu-utils qemu-efi-aarch64 cloud-image-utils cryptsetup-bin nftables iproute2 kmod" {
		t.Errorf("arm64 packages = %s", got)
	}
}

func TestStorageTarget(t *testing.T) {
	h := fakehost.New()
	h.Files["/mnt/nvme"] = nil
	h.Outputs["df -B1G --output=source,target,fstype,avail /mnt/nvme"] = "Filesystem Mounted on Type Avail\n/dev/nvme0n1p1 /mnt/nvme ext4 900G\n"
	got, err := StorageTarget(h, "/mnt/nvme")
	if err != nil || got != "/mnt/nvme/gpu-agent" {
		t.Errorf("target = %q, %v", got, err)
	}
	h.Files["/media/sd"] = nil
	h.Outputs["df -B1G --output=source,target,fstype,avail /media/sd"] = "Filesystem Mounted on Type Avail\n/dev/mmcblk1p1 /media/sd ext4 100G\n"
	h.Files["/sys/block/mmcblk1/device/type"] = []byte("SD\n")
	if _, err := StorageTarget(h, "/media/sd/gpu-agent"); err == nil || !strings.Contains(err.Error(), "an SD card") {
		t.Errorf("an SD card was accepted: %v", err)
	}
	h.Files["/small"] = nil
	h.Outputs["df -B1G --output=source,target,fstype,avail /small"] = "Filesystem Mounted on Type Avail\n/dev/sdb1 /small ext4 30G\n"
	if _, err := StorageTarget(h, "/small"); err == nil || !strings.Contains(err.Error(), "30 GB free") {
		t.Errorf("a small disk was accepted: %v", err)
	}
	if _, err := StorageTarget(h, "relative/dir"); err == nil {
		t.Errorf("a relative path was accepted")
	}
}

func TestMoveStorageTakesTheImagesAlong(t *testing.T) {
	h := fakehost.New()
	h.Files["/var/lib/gpu-agent/golden.img"] = nil
	h.Files["/var/lib/gpu-agent/golden.img.json"] = nil
	h.Files["/var/lib/gpu-agent/images/SHA256SUMS"] = nil
	if err := MoveStorage(h, "/var/lib/gpu-agent", "/mnt/nvme/gpu-agent"); err != nil {
		t.Fatal(err)
	}
	for _, item := range []string{"golden.img", "golden.img.json", "images"} {
		if !h.Ran("run mv /var/lib/gpu-agent/" + item + " /mnt/nvme/gpu-agent/" + item) {
			t.Errorf("%s not moved: %v", item, h.Calls)
		}
	}
}

// A place the host chose with `setup --data-dir` is never overruled: when it
// runs out of room the host is told, with the command, and the setup waits.
func TestStorageTheHostChoseIsNotMoved(t *testing.T) {
	h := fakehost.New()
	h.Files["/srv/gpu-agent"] = nil
	h.Outputs["df -B1G --output=source,target,fstype,avail /srv/gpu-agent"] = "Filesystem Mounted on Type Avail\n/dev/sda2 /srv ext4 5G\n"
	h.Outputs["df -B1G --output=source,target,fstype,avail"] = "Filesystem Mounted on Type Avail\n/dev/sda2 /srv ext4 5G\n/dev/nvme0n1p1 /data ext4 900G\n"
	for _, chosen := range []bool{true, false} {
		fs := storageFindings(h, "/srv/gpu-agent", 5, chosen)
		if len(fs) != 1 {
			t.Fatalf("chosen=%v: findings %+v", chosen, fs)
		}
		want := ReasonStorage
		if chosen {
			want = ReasonHuman
		}
		if fs[0].Kind != want {
			t.Errorf("chosen=%v: kind %s, want %s (%s)", chosen, fs[0].Kind, want, fs[0].Text)
		}
	}
}
