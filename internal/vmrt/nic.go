package vmrt

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

// Host-side facts about network cards, shared by the pair preflight
// (internal/interconnect) and the pair runtime: what netdev a PCI function has,
// its MAC and firmware, and a fingerprint of its persistent configuration.

// DriverOf is the driver a PCI function is bound to now, or "".
func DriverOf(h Host, bdf string) string { return driverOf(h, bdf) }

// DevPath is a PCI function's sysfs directory.
func DevPath(bdf string) string { return devPath(bdf) }

// SlotOf is a PCI address without its function number.
func SlotOf(bdf string) string { return slotOf(bdf) }

// IsBridge reports whether a PCI function is a PCI bridge.
func IsBridge(h Host, bdf string) bool { return isBridge(h, bdf) }

// NetdevsOf lists the network interfaces the kernel has for a PCI function. A
// function on vfio-pci has none.
func NetdevsOf(h Host, bdf string) []string {
	matches, _ := h.Glob(devPath(bdf) + "/net/*")
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, path.Base(m))
	}
	sort.Strings(names)
	return names
}

// NetAttr reads one /sys/class/net attribute, trimmed, or "".
func NetAttr(h Host, netdev, attr string) string {
	data, err := h.ReadFile("/sys/class/net/" + netdev + "/" + attr)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// NetdevMAC is an interface's MAC address in lower case.
func NetdevMAC(h Host, netdev string) string {
	return strings.ToLower(NetAttr(h, netdev, "address"))
}

// FunctionMACs are the MACs of every netdev the given PCI functions have now,
// keyed by MAC with the netdev as value.
func FunctionMACs(h Host, bdfs []string) map[string]string {
	macs := map[string]string{}
	for _, bdf := range bdfs {
		for _, nd := range NetdevsOf(h, bdf) {
			if mac := NetdevMAC(h, nd); mac != "" {
				macs[mac] = nd
			}
		}
	}
	return macs
}

// NICFirmware is the firmware version `ethtool -i` reports for an interface.
func NICFirmware(h Host, netdev string) (string, error) {
	out, err := h.Output("ethtool", "-i", netdev)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "firmware-version" {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("ethtool -i %s reports no firmware version", netdev)
}

// NVConfigHash fingerprints a ConnectX function's persistent (non-volatile)
// configuration as `mstconfig q` shows it. A guest that owns the whole card can
// change those settings, and they would survive into the next rental and the
// host; comparing the fingerprint before and after a rental catches it.
func NVConfigHash(h Host, bdf string) (string, error) {
	out, err := h.Output("mstconfig", "-d", bdf, "q")
	if err != nil {
		return "", err
	}
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimRight(line, " \t\r"); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return "", errors.New("mstconfig -d " + bdf + " q printed nothing")
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

// DeviceHolders are the host processes with a file under prefix open (for
// example /dev/infiniband/), as "name (pid N)", except those skip says to
// leave out. Processes are named like ClassifyGPUHolders names them.
func DeviceHolders(h Host, prefix string, skip func(name string) bool) []string {
	fds, _ := h.Glob("/proc/[0-9]*/fd/*")
	seen := map[string]bool{}
	var holders []string
	for _, fd := range fds {
		target, err := h.Readlink(fd)
		if err != nil || !strings.HasPrefix(target, prefix) {
			continue
		}
		parts := strings.Split(fd, "/")
		if len(parts) < 3 {
			continue
		}
		pid := parts[2]
		name := processName(h, pid)
		if skip != nil && skip(name) {
			continue
		}
		who := fmt.Sprintf("%s (pid %s)", name, pid)
		if !seen[who] {
			seen[who] = true
			holders = append(holders, who)
		}
	}
	sort.Strings(holders)
	return holders
}

// GroupMembers are the non-bridge PCI functions in a function's IOMMU group.
func GroupMembers(h Host, bdf string) ([]string, error) {
	members, err := h.Glob(devPath(bdf) + "/iommu_group/devices/*")
	if err != nil || len(members) == 0 {
		return nil, fmt.Errorf("PCI device %s is in no IOMMU group (is the IOMMU on?)", bdf)
	}
	var out []string
	for _, m := range members {
		if b := path.Base(m); !isBridge(h, b) {
			out = append(out, b)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ForeignGroupMembers are the functions sharing an IOMMU group with any of
// devices that allowed does not accept. Passing devices through would take
// those from the host too.
func ForeignGroupMembers(h Host, devices []string, allowed func(bdf string) bool) ([]string, error) {
	seen := map[string]bool{}
	var foreign []string
	for _, d := range devices {
		members, err := GroupMembers(h, d)
		if err != nil {
			return foreign, err
		}
		for _, m := range members {
			if seen[m] || allowed(m) {
				continue
			}
			seen[m] = true
			foreign = append(foreign, m)
		}
	}
	sort.Strings(foreign)
	return foreign, nil
}
