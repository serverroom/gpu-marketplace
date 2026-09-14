package vmrt

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// BoundDevice is one PCI function the runtime took from its driver, and the
// driver to give it back to.
type BoundDevice struct {
	BDF    string `json:"bdf"`
	Driver string `json:"driver"` // "" when it had none
}

func devPath(bdf string) string { return "/sys/bus/pci/devices/" + bdf }

func driverOf(h Host, bdf string) string {
	link, err := h.Readlink(devPath(bdf) + "/driver")
	if err != nil {
		return ""
	}
	return path.Base(link)
}

func isBridge(h Host, bdf string) bool {
	class, err := h.ReadFile(devPath(bdf) + "/class")
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(class)), "0x0604")
}

// GroupFunctions is every PCI function that has to go to the VM with the GPUs:
// all non-bridge members of their IOMMU groups. The kernel will not hand a
// guest part of a group.
func GroupFunctions(h Host, gpus []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, gpu := range gpus {
		members, err := h.Glob(devPath(gpu) + "/iommu_group/devices/*")
		if err != nil || len(members) == 0 {
			return nil, fmt.Errorf("GPU %s is in no IOMMU group (is the IOMMU on?)", gpu)
		}
		for _, m := range members {
			bdf := path.Base(m)
			if seen[bdf] || isBridge(h, bdf) {
				continue
			}
			seen[bdf] = true
			out = append(out, bdf)
		}
	}
	sort.Strings(out)
	return out, nil
}

func slotOf(bdf string) string {
	if i := strings.LastIndex(bdf, "."); i > 0 {
		return bdf[:i]
	}
	return bdf
}

// GroupProblems names what passing the GPUs through would take from the host
// besides the GPUs themselves. Other functions of the GPU's own slot (its
// audio device, its USB-C controller) go with it; anything else -- a NIC, a
// disk controller in the same group -- is a reason not to host.
func GroupProblems(h Host, gpus []string) []string {
	funcs, err := GroupFunctions(h, gpus)
	if err != nil {
		return []string{err.Error()}
	}
	slots := map[string]bool{}
	for _, g := range gpus {
		slots[slotOf(g)] = true
	}
	var problems []string
	for _, f := range funcs {
		if !slots[slotOf(f)] {
			problems = append(problems, fmt.Sprintf("PCI device %s shares an IOMMU group with the GPU, so passing the GPU through would take it from this machine too; move the GPU to another slot or enable ACS", f))
		}
	}
	return problems
}

// BindVFIO moves each function to vfio-pci. It returns what it bound so far
// even on error, so the caller can give back exactly that.
func BindVFIO(h Host, functions []string) ([]BoundDevice, error) {
	if err := h.Run("modprobe", "vfio-pci"); err != nil {
		return nil, fmt.Errorf("load vfio-pci: %w", err)
	}
	var bound []BoundDevice
	for _, bdf := range functions {
		orig := driverOf(h, bdf)
		bound = append(bound, BoundDevice{BDF: bdf, Driver: orig})
		if orig == "vfio-pci" {
			continue
		}
		if err := h.WriteFile(devPath(bdf)+"/driver_override", []byte("vfio-pci"), 0200); err != nil {
			return bound, fmt.Errorf("claim %s: %w", bdf, err)
		}
		if orig != "" {
			if err := h.WriteFile("/sys/bus/pci/drivers/"+orig+"/unbind", []byte(bdf), 0200); err != nil {
				return bound, fmt.Errorf("take %s from %s: %w", bdf, orig, err)
			}
		}
		if err := h.WriteFile("/sys/bus/pci/drivers_probe", []byte(bdf), 0200); err != nil {
			return bound, fmt.Errorf("bind %s to vfio-pci: %w", bdf, err)
		}
		if got := driverOf(h, bdf); got != "vfio-pci" {
			return bound, fmt.Errorf("%s is on %q, not vfio-pci", bdf, got)
		}
	}
	return bound, nil
}

// ReleaseVFIO gives every function back: off vfio-pci, override cleared, a
// function-level reset where the device offers one, then back to the driver it
// came from. ok is false if any of that did not verify.
func ReleaseVFIO(h Host, bound []BoundDevice) (ok bool, detail []string) {
	ok = true
	for _, d := range bound {
		if d.Driver == "vfio-pci" {
			continue
		}
		if driverOf(h, d.BDF) == "vfio-pci" {
			if err := h.WriteFile("/sys/bus/pci/drivers/vfio-pci/unbind", []byte(d.BDF), 0200); err != nil {
				ok = false
				detail = append(detail, fmt.Sprintf("release %s from vfio-pci: %v", d.BDF, err))
				continue
			}
		}
		_ = h.WriteFile(devPath(d.BDF)+"/driver_override", []byte("\n"), 0200)
		if h.Exists(devPath(d.BDF) + "/reset") {
			if err := h.WriteFile(devPath(d.BDF)+"/reset", []byte("1"), 0200); err != nil {
				ok = false
				detail = append(detail, fmt.Sprintf("reset %s: %v", d.BDF, err))
			}
		}
		if d.Driver == "" {
			continue
		}
		_ = h.WriteFile("/sys/bus/pci/drivers_probe", []byte(d.BDF), 0200)
		if got := driverOf(h, d.BDF); got != d.Driver {
			ok = false
			detail = append(detail, fmt.Sprintf("%s came back on %q, not %s", d.BDF, got, d.Driver))
		}
	}
	return ok, detail
}

// GPUHolders are the host processes with an NVIDIA device open, other than
// nvidia-persistenced (which the runtime stops itself). A GPU that something
// holds cannot be unbound -- the kernel waits for the holder -- so the rental
// is refused while any exist.
func GPUHolders(h Host) []string {
	fds, _ := h.Glob("/proc/[0-9]*/fd/*")
	seen := map[string]bool{}
	var holders []string
	for _, fd := range fds {
		target, err := h.Readlink(fd)
		if err != nil || !strings.HasPrefix(target, "/dev/nvidia") {
			continue
		}
		parts := strings.Split(fd, "/")
		if len(parts) < 3 {
			continue
		}
		pid := parts[2]
		comm, _ := h.ReadFile("/proc/" + pid + "/comm")
		name := strings.TrimSpace(string(comm))
		if name == "nvidia-persistenced" {
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
