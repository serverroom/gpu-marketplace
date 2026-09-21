package provisioner

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// What a machine's kernel and disks must offer, checked in words a host can
// act on. Vendor kernels (Rockchip BSP, Armbian vendor/edge, ubuntu-rockchip,
// Debian) often lack a piece; each missing one is named.

// kvmMissing is the reason when /dev/kvm is absent. On Arm the kernel says why
// in its log: a firmware that starts Linux below EL2 leaves "HYP mode not
// available" there, and no kernel can fix that.
func kvmMissing(h vmrt.Host, arch string) string {
	if arch != "arm64" {
		return "KVM is not available (/dev/kvm is missing): enable virtualisation in the firmware and load the kvm module"
	}
	if out, err := h.Output("dmesg"); err == nil && strings.Contains(strings.ToLower(out), "hyp mode not available") {
		return "this machine's firmware starts Linux without virtualisation (the kernel says HYP mode is not available, and /dev/kvm is missing), " +
			"so it cannot run rental VMs: use a bootloader and firmware that start Linux at EL2 (the board maker's or Armbian's current images do), then start the machine again"
	}
	return "this machine's Linux kernel has no KVM (/dev/kvm is missing), so it cannot run rental VMs: install a kernel with KVM enabled " +
		"(for Rockchip boards, e.g. a mainline or 'edge' kernel) and start the machine again"
}

// kernelFeature is a piece of the kernel the runtime needs.
type kernelFeature struct {
	module string   // what modprobe knows it as
	paths  []string // any of them present: available
	reason string
}

var kernelFeatures = []kernelFeature{
	{"tun", []string{"/dev/net/tun"}, "the Linux kernel has no TUN/TAP support (/dev/net/tun), which a rental's network needs: load the 'tun' module, or install a kernel that has it"},
	{"bridge", []string{"/sys/module/bridge"}, "the Linux kernel has no network bridge support (the 'bridge' module), which a rental's network needs: install a kernel that has it"},
	{"nf_tables", []string{"/sys/module/nf_tables"}, "the Linux kernel has no nf_tables, the firewall that fences a rental off your network: install a kernel that has it"},
	{"dm_crypt", []string{"/sys/module/dm_crypt"}, "the Linux kernel has no dm-crypt, which encrypts a rental's disk: install a kernel that has it"},
	{"loop", []string{"/dev/loop-control", "/sys/module/loop"}, "the Linux kernel has no loop devices, which hold a rental's disk: install a kernel that has them"},
}

// missingKernelFeatures names what the kernel lacks. A feature is there when
// its device or module shows, or modprobe knows it (built in or loadable);
// without modprobe (a missing tool, reported on its own) nothing is judged.
func missingKernelFeatures(h vmrt.Host) []string {
	if _, err := h.LookPath("modprobe"); err != nil {
		return nil
	}
	var out []string
	for _, f := range kernelFeatures {
		present := false
		for _, p := range f.paths {
			if h.Exists(p) {
				present = true
			}
		}
		if !present && h.Run("modprobe", "-n", "-q", f.module) != nil {
			out = append(out, f.reason)
		}
	}
	return out
}

// mount is one mounted filesystem as df reports it.
type mount struct {
	source, target, fstype string
	availGB                int
}

// mounts lists the mounted filesystems with space, from df.
func mounts(h vmrt.Host, paths ...string) []mount {
	args := append([]string{"-B1G", "--output=source,target,fstype,avail"}, paths...)
	out, err := h.Output("df", args...)
	if err != nil && out == "" {
		return nil
	}
	var ms []mount
	for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) != 4 {
			continue
		}
		avail, err := strconv.Atoi(strings.TrimSuffix(f[3], "G"))
		if err != nil {
			continue
		}
		ms = append(ms, mount{source: f[0], target: f[1], fstype: f[2], availGB: avail})
	}
	return ms
}

var partitionSuffix = regexp.MustCompile(`^((?:mmcblk|nvme\d+n)\d+)p\d+$|^([a-z]+)\d+$`)

// diskOf is the whole disk a block device belongs to: mmcblk1p2 -> mmcblk1,
// nvme0n1p1 -> nvme0n1, sda1 -> sda.
func diskOf(source string) string {
	dev := strings.TrimPrefix(source, "/dev/")
	if m := partitionSuffix.FindStringSubmatch(dev); m != nil {
		if m[1] != "" {
			return m[1]
		}
		return m[2]
	}
	return dev
}

// removableMedia says what kind of removable medium holds source ("an SD
// card", "removable storage"), or "" for fixed storage. The kernel marks USB
// sticks removable; an SD card in a board's slot often is not, but its MMC
// type says SD (eMMC says MMC).
func removableMedia(h vmrt.Host, source string) string {
	if !strings.HasPrefix(source, "/dev/") {
		return ""
	}
	disk := diskOf(source)
	if t, err := h.ReadFile("/sys/block/" + disk + "/device/type"); err == nil && strings.TrimSpace(string(t)) == "SD" {
		return "an SD card"
	}
	if r, err := h.ReadFile("/sys/block/" + disk + "/removable"); err == nil && strings.TrimSpace(string(r)) == "1" {
		return "removable storage"
	}
	return ""
}

// usbDisk reports whether source is on a disk attached over USB: a drive the
// host plugged in (a backup disk, say), which the agent never picks by itself.
func usbDisk(h vmrt.Host, source string) bool {
	if !strings.HasPrefix(source, "/dev/") {
		return false
	}
	link, err := h.Readlink("/sys/block/" + diskOf(source))
	return err == nil && strings.Contains(link, "/usb")
}

// storageFindings are the reasons the rentals' disks cannot go where they
// would: on removable media, or where there is not room. When there is not
// room where the agent keeps them by default (chosen false: no `setup
// --data-dir`) and a fixed internal disk -- NVMe, SATA, eMMC, never USB --
// has room, the finding is the setup's to fix (ReasonStorage, with the
// directory it moves them to): a board with a small system partition and a
// big data one hosts with nothing typed. A place the host chose is never
// overruled, and a USB drive is only ever named, with the command.
func storageFindings(h vmrt.Host, storage string, diskGB int, chosen bool) []Finding {
	var findings []Finding
	here := mounts(h, existingParent(h, storage))
	if len(here) == 1 {
		if media := removableMedia(h, here[0].source); media != "" {
			findings = append(findings, Finding{Kind: ReasonHuman, Text: fmt.Sprintf("the rentals' disks would go on %s (%s holds %s): a rental's disk never goes on removable media; "+
				"put the agent's data on eMMC, NVMe or SATA storage with 'sudo gpu-agent setup --data-dir <directory on that disk>'", media, here[0].source, storage)})
		}
	}
	if diskGB >= 20 {
		return findings
	}
	msg := fmt.Sprintf("there is not 20 GB free for a rental's disk under %s after the 20 GB the machine keeps for itself", storage)
	best, ok := biggestFit(h, here)
	switch {
	case ok && !chosen && !usbDisk(h, best.source):
		dir := path.Join(best.target, "gpu-agent")
		return append(findings, Finding{Kind: ReasonStorage, Dir: dir, Text: msg + fmt.Sprintf(
			"; %s has %d GB free, so the automatic setup keeps the rentals' disks in %s ('sudo gpu-agent setup' does it now; 'sudo gpu-agent setup --data-dir <directory>' puts them elsewhere)",
			best.target, best.availGB, dir)})
	case ok:
		msg += fmt.Sprintf("; %s has %d GB free: 'sudo gpu-agent setup --data-dir %s' puts the rentals' disks there",
			best.target, best.availGB, path.Join(best.target, "gpu-agent"))
	default:
		msg += "; add a disk (NVMe, SATA or a USB drive -- not an SD card) with at least 40 GB free and run 'sudo gpu-agent setup --data-dir <its mount point>/gpu-agent'"
	}
	return append(findings, Finding{Kind: ReasonHuman, Text: msg})
}

// biggestFit is the mounted fixed filesystem with the most room, if it has
// room for a rental's disk (40 GB: 20 for the disk, 20 kept for the machine)
// and is not where the storage is now.
func biggestFit(h vmrt.Host, here []mount) (mount, bool) {
	skip := map[string]bool{"tmpfs": true, "devtmpfs": true, "squashfs": true, "overlay": true, "efivarfs": true, "vfat": true, "ramfs": true}
	var fits []mount
	for _, m := range mounts(h) {
		if skip[m.fstype] || !strings.HasPrefix(m.source, "/dev/") || strings.HasPrefix(m.target, "/boot") || m.availGB < 40 {
			continue
		}
		if len(here) == 1 && m.target == here[0].target {
			continue
		}
		if removableMedia(h, m.source) != "" {
			continue
		}
		fits = append(fits, m)
	}
	if len(fits) == 0 {
		return mount{}, false
	}
	sort.SliceStable(fits, func(i, j int) bool { return fits[i].availGB > fits[j].availGB })
	return fits[0], true
}

// existingParent is dir, or its nearest parent that exists.
func existingParent(h vmrt.Host, dir string) string {
	for dir != "" && !h.Exists(dir) {
		parent := path.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return dir
}

// AptDistro reports whether this machine installs packages the way the agent
// does: apt-get on Ubuntu, Debian or a system built on them (Armbian,
// ubuntu-rockchip). The package names are the same on all of them
// (vmrt.Packages).
func AptDistro(h vmrt.Host) bool {
	if _, err := h.LookPath("apt-get"); err != nil {
		return false
	}
	data, err := h.ReadFile("/etc/os-release")
	if err != nil {
		return true // apt-get is there; nothing says otherwise
	}
	return isDebianLike(string(data))
}

func isDebianLike(osRelease string) bool {
	for _, line := range strings.Split(osRelease, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || (k != "ID" && k != "ID_LIKE") {
			continue
		}
		for _, id := range strings.Fields(strings.Trim(v, "\"'")) {
			if id == "debian" || id == "ubuntu" {
				return true
			}
		}
	}
	return false
}

// StorageTarget checks a directory named with `gpu-agent setup --data-dir`
// and returns where the agent's big files go: dir itself when it is called
// gpu-agent, else a gpu-agent directory inside it -- so `gpu-agent remove`
// only ever deletes what the agent made. It must be an absolute path on fixed
// storage with room for a rental's disk (40 GB free: 20 for the disk, 20 kept).
func StorageTarget(h vmrt.Host, dir string) (string, error) {
	if !path.IsAbs(dir) {
		return "", fmt.Errorf("%s is not an absolute path", dir)
	}
	target := path.Clean(dir)
	if path.Base(target) != "gpu-agent" {
		target = path.Join(target, "gpu-agent")
	}
	ms := mounts(h, existingParent(h, target))
	if len(ms) != 1 {
		return "", fmt.Errorf("the disk holding %s cannot be read (df)", target)
	}
	if media := removableMedia(h, ms[0].source); media != "" {
		return "", fmt.Errorf("%s is on %s (%s): a rental's disk never goes on removable media; name a directory on eMMC, NVMe or SATA storage", target, media, ms[0].source)
	}
	if ms[0].availGB < 40 {
		return "", fmt.Errorf("%s has %d GB free, and a rental needs 40 GB there (20 GB for its disk, 20 GB kept free)", ms[0].target, ms[0].availGB)
	}
	return target, nil
}

// storageItems are what lives in the storage directory between rentals.
var storageItems = []string{"golden.img", "golden.img.json", "images"}

// MoveStorage moves the base image (and the downloaded cloud image) from one
// storage directory to another, across disks if need be, so the machine does
// not rebuild them.
func MoveStorage(h vmrt.Host, from, to string) error {
	if path.Clean(from) == path.Clean(to) {
		return nil
	}
	if err := h.MkdirAll(to, 0700); err != nil {
		return err
	}
	for _, item := range storageItems {
		src := path.Join(from, item)
		if !h.Exists(src) {
			continue
		}
		dst := path.Join(to, item)
		if h.Exists(dst) {
			_ = h.RemoveAll(dst)
		}
		if err := h.Run("mv", src, dst); err != nil {
			return fmt.Errorf("move %s to %s: %w", src, dst, err)
		}
	}
	return nil
}
