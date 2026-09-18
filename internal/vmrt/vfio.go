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

// NVIDIAService is one of NVIDIA's own background services. They open the
// GPU's device files on most driver installs, but they are not anybody's
// workload: the runtime stops the ones that are running for a rental and
// starts them again when it ends.
type NVIDIAService struct {
	Unit string // systemd unit
	Exe  string // the executable it runs
}

// NVIDIAServices are the services the runtime stops and restarts itself.
var NVIDIAServices = []NVIDIAService{
	{Unit: "nvidia-persistenced", Exe: "nvidia-persistenced"},
	{Unit: "nvidia-powerd", Exe: "nvidia-powerd"},
	{Unit: "nvidia-dcgm", Exe: "nv-hostengine"},
}

// desktopPrefixes name display servers, desktop shells and login screens. A
// GPU that draws this machine's desktop cannot be handed to a microVM: the
// machine has to run without a desktop first (runtime prepare --headless).
// Prefixes, because /proc/<pid>/comm is cut to 15 characters
// ("mutter-x11-frames" reads "mutter-x11-fram").
var desktopPrefixes = []string{
	"Xorg", "Xwayland", "gnome-shell", "gnome-session", "gnome-remote-de", "mutter", "gdm", "sddm",
	"lightdm", "kwin", "plasmashell", "xfwm4", "xfce4-session", "cinnamon", "mate-session", "budgie-wm",
}

// commLen is how much of a process name /proc/<pid>/comm keeps.
const commLen = 15

// processName is the name of a process: its executable's file name when that
// can be read, which is never truncated, else /proc/<pid>/comm.
func processName(h Host, pid string) string {
	if exe, err := h.Readlink("/proc/" + pid + "/exe"); err == nil {
		exe = strings.TrimSuffix(exe, " (deleted)")
		if i := strings.LastIndex(exe, "/"); i >= 0 {
			exe = exe[i+1:]
		}
		if exe != "" {
			return exe
		}
	}
	comm, _ := h.ReadFile("/proc/" + pid + "/comm")
	return strings.TrimSpace(string(comm))
}

// sameProgram reports whether a process name is the executable exe, allowing
// for comm's truncation.
func sameProgram(name, exe string) bool {
	if name == exe {
		return true
	}
	return len(exe) > commLen && len(name) == commLen && strings.HasPrefix(exe, name)
}

// IsNVIDIAService reports whether a process name is one of NVIDIAServices.
func IsNVIDIAService(name string) bool {
	for _, s := range NVIDIAServices {
		if sameProgram(name, s.Exe) {
			return true
		}
	}
	return false
}

// IsDesktopProcess reports whether a process name is a display server,
// desktop shell or login screen.
func IsDesktopProcess(name string) bool {
	if name == "X" {
		return true
	}
	for _, p := range desktopPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ClassifyGPUHolders lists the host processes with an NVIDIA device open, as
// "name (pid N)", split into the machine's desktop and everything else.
// NVIDIA's own services are left out: the runtime stops them itself. A GPU
// that something holds cannot be unbound -- the kernel waits for the holder --
// so a rental is refused while either list is non-empty.
func ClassifyGPUHolders(h Host) (desktop, other []string) {
	fds, _ := h.Glob("/proc/[0-9]*/fd/*")
	seen := map[string]bool{}
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
		name := processName(h, pid)
		if IsNVIDIAService(name) {
			continue
		}
		who := fmt.Sprintf("%s (pid %s)", name, pid)
		if seen[who] {
			continue
		}
		seen[who] = true
		if IsDesktopProcess(name) {
			desktop = append(desktop, who)
		} else {
			other = append(other, who)
		}
	}
	sort.Strings(desktop)
	sort.Strings(other)
	return desktop, other
}

// GPUHolders are the host processes with an NVIDIA device open, other than
// NVIDIA's own services: the desktop and everything else together.
func GPUHolders(h Host) []string {
	desktop, other := ClassifyGPUHolders(h)
	all := append(append([]string{}, desktop...), other...)
	sort.Strings(all)
	return all
}

// DesktopOnGPUProblem is the refusal for a machine whose desktop runs on the
// GPU, naming the processes and the one command that fixes it.
func DesktopOnGPUProblem(desktop []string) string {
	return fmt.Sprintf("this machine's desktop is running on the GPU (%s); a hosting machine runs without one: "+
		"run 'sudo gpu-agent runtime prepare --headless', then try again", strings.Join(desktop, ", "))
}
