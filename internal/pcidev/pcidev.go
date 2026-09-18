// Package pcidev reads the machine's GPUs straight from sysfs, whatever their
// make. A GPU is a PCI display function; nothing here asks a vendor tool, so a
// machine qualifies with no NVIDIA or AMD software installed at all -- the
// rental VM, not the host, is where a GPU's driver has to work.
package pcidev

import (
	"bufio"
	"bytes"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// FS is the part of the filesystem this package reads. vmrt.Host satisfies it,
// and so does the fake host tests use.
type FS interface {
	ReadFile(path string) ([]byte, error)
	Readlink(path string) (string, error)
	Glob(pattern string) ([]string, error)
}

// OS reads the real filesystem.
type OS struct{}

func (OS) ReadFile(p string) ([]byte, error)     { return os.ReadFile(p) }
func (OS) Readlink(p string) (string, error)     { return os.Readlink(p) }
func (OS) Glob(pattern string) ([]string, error) { return filepath.Glob(pattern) }

const devices = "/sys/bus/pci/devices/"

// Device is one PCI function. IDs are lowercase hex without the 0x sysfs
// prints; Driver is "" when nothing is bound.
type Device struct {
	BDF    string `json:"bdf"`
	Vendor string `json:"vendor"`
	Device string `json:"device"`
	Class  string `json:"class"`
	Driver string `json:"driver,omitempty"`
}

// ID is the device's vendor:device pair, the same inside a VM as on the host,
// which is what makes it the way to recognise a GPU on both sides of VFIO.
func (d Device) ID() string { return d.Vendor + ":" + d.Device }

// Vendor IDs.
const (
	NVIDIA = "10de"
	AMD    = "1002"
	Intel  = "8086"
)

// notGPU are display functions that are not GPUs anyone could rent: the
// management display every server's BMC puts on the bus (ASPEED on most boards,
// Matrox behind HPE iLO, Dell iDRAC and Lenovo XCC) and the virtual displays of
// a machine that is itself a VM. Without this list every server would offer its
// console chip.
var notGPU = map[string]string{
	"1a03": "ASPEED BMC display",
	"102b": "Matrox BMC display",
	"19e5": "Huawei iBMC display",
	"19a2": "Emulex Pilot BMC display",
	"126f": "Silicon Motion onboard display",
	"18ca": "XGI onboard display",
	"1013": "Cirrus Logic virtual display",
	"1234": "QEMU virtual display",
	"1af4": "virtio virtual display",
	"1b36": "QXL virtual display",
	"15ad": "VMware virtual display",
	"80ee": "VirtualBox virtual display",
	"1414": "Hyper-V virtual display",
	"1d0f": "AWS Nitro display",
	"1912": "Renesas BMC display",
}

// notGPUDevice are onboard display chips from vendors that also make GPUs: the
// ATI ES1000 on HP ProLiant G5-G7, Dell PowerEdge 9G/10G, IBM and Supermicro X7/X8
// boards, and the Rage XL and Radeon 7000 on older ones.
var notGPUDevice = map[string]string{
	"1002:515e": "ATI ES1000 onboard display",
	"1002:4752": "ATI Rage XL onboard display",
	"1002:5159": "ATI Radeon 7000 onboard display",
}

// minGPUWindow is the smallest memory window (BAR) a VGA-class GPU card worth
// renting has: every one maps 256 MB or more, and a server's onboard display
// chip 128 MB at most, so a VGA function below this is one, whoever made it.
const minGPUWindow = 256 << 20

// largestMemoryBAR is the size of a function's largest memory window, from its
// standard BARs in sysfs; ok is false when sysfs does not say.
func largestMemoryBAR(fs FS, bdf string) (size uint64, ok bool) {
	data, err := fs.ReadFile(devices + bdf + "/resource")
	if err != nil {
		return 0, false
	}
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if i >= 6 {
			break
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		start, err1 := strconv.ParseUint(strings.TrimPrefix(f[0], "0x"), 16, 64)
		end, err2 := strconv.ParseUint(strings.TrimPrefix(f[1], "0x"), 16, 64)
		flags, err3 := strconv.ParseUint(strings.TrimPrefix(f[2], "0x"), 16, 64)
		if err1 != nil || err2 != nil || err3 != nil || flags&0x200 == 0 || end <= start {
			continue
		}
		ok = true
		if n := end - start + 1; n > size {
			size = n
		}
	}
	return size, ok
}

// NotGPU reports whether a display function from this vendor is a management
// or virtual display rather than a GPU.
func NotGPU(vendor string) bool {
	_, ok := notGPU[vendor]
	return ok
}

func hex(fs FS, p string) string {
	data, err := fs.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(string(data))), "0x")
}

// Read is what sysfs says about one PCI function.
func Read(fs FS, bdf string) Device {
	d := Device{
		BDF:    bdf,
		Vendor: hex(fs, devices+bdf+"/vendor"),
		Device: hex(fs, devices+bdf+"/device"),
		Class:  hex(fs, devices+bdf+"/class"),
	}
	if link, err := fs.Readlink(devices + bdf + "/driver"); err == nil {
		d.Driver = path.Base(link)
	}
	return d
}

// IsGPU reports whether a PCI function is a GPU: the display class (0x03: VGA,
// XGA, 3D controller, other) less management, onboard and virtual displays, or
// a processing accelerator (0x12) from NVIDIA or AMD -- AMD's Instinct MI300
// family reports that class. Other 0x12 devices (Intel's NPUs, Huawei's Ascend)
// are not GPUs.
func IsGPU(d Device) bool {
	switch {
	case strings.HasPrefix(d.Class, "03"):
		_, onboard := notGPUDevice[d.ID()]
		return !NotGPU(d.Vendor) && !onboard
	case strings.HasPrefix(d.Class, "12"):
		return d.Vendor == NVIDIA || d.Vendor == AMD
	}
	return false
}

// Display is every GPU on the machine, in PCI address order. SR-IOV virtual
// functions are left out: each is a slice of a GPU that is listed already.
func Display(fs FS) []Device {
	classes, _ := fs.Glob(devices + "*/class")
	var out []Device
	for _, c := range classes {
		bdf := path.Base(path.Dir(filepath.ToSlash(c)))
		if _, err := fs.Readlink(devices + bdf + "/physfn"); err == nil {
			continue
		}
		d := Read(fs, bdf)
		if !IsGPU(d) {
			continue
		}
		// A VGA chip too small to be a GPU card is a server's onboard one,
		// whatever vendor list it is missing from. NVIDIA makes no onboard
		// chips, and its SoC GPUs (a GB10) are not judged by their windows.
		if d.Vendor != NVIDIA && strings.HasPrefix(d.Class, "0300") {
			if size, ok := largestMemoryBAR(fs, bdf); ok && size < minGPUWindow {
				continue
			}
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BDF < out[j].BDF })
	return out
}

// Integrated reports whether a GPU is the processor's own rather than a card:
// Intel's integrated graphics always sit at 00:02.0 on the root bus; an AMD
// APU's either sit on the root bus themselves (pre-Zen: Kabini, Kaveri,
// Carrizo, the Moonshot m700) or behind the processor's internal GPU bridge,
// 00:08.1 (Zen) -- or, whatever bridge a generation uses, next to the
// processor's security processor (PSP, an encryption controller, class 0x1080)
// in their own slot, which no graphics card carries. A card always sits behind
// a root port, never on the root bus. Such a GPU draws the machine's own screen
// from the machine's own memory, so a rental never takes it -- and a machine
// with no other GPU hosts CPU-only rentals.
func Integrated(fs FS, d Device) bool {
	if d.Vendor != Intel && d.Vendor != AMD {
		return false
	}
	if strings.HasSuffix(d.BDF, ":00:02.0") && d.Vendor == Intel {
		return true
	}
	link, err := fs.Readlink(devices + d.BDF)
	if err == nil {
		// ../../../devices/pci0000:00/0000:00:01.0: the parent is the root bus.
		parent := path.Base(path.Dir(filepath.ToSlash(link)))
		if strings.HasPrefix(parent, "pci") {
			return true
		}
	}
	switch d.Vendor {
	case AMD:
		if err == nil && strings.Contains(filepath.ToSlash(link), "/0000:00:08.1/") {
			return true
		}
		slot := d.BDF
		if i := strings.LastIndex(slot, "."); i > 0 {
			slot = slot[:i]
		}
		siblings, _ := fs.Glob(devices + slot + ".*/class")
		for _, c := range siblings {
			if strings.HasPrefix(hex(fs, c), "1080") {
				return true
			}
		}
	}
	return false
}

var shortVendor = map[string]string{NVIDIA: "NVIDIA", AMD: "AMD", Intel: "Intel"}

// idDatabases are where distributions keep the PCI ID database.
var idDatabases = []string{"/usr/share/misc/pci.ids", "/usr/share/hwdata/pci.ids"}

// lookup finds a vendor's and a device's names in pci.ids. Vendor lines start
// at column 0, device lines with one tab, subsystem lines with two.
func lookup(db []byte, vendor, device string) (vendorName, deviceName string) {
	in := false
	sc := bufio.NewScanner(bytes.NewReader(db))
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		if line[0] != '\t' {
			if in {
				return vendorName, ""
			}
			if id, name, ok := strings.Cut(line, "  "); ok && strings.ToLower(id) == vendor {
				in, vendorName = true, strings.TrimSpace(name)
			}
			continue
		}
		if in && !strings.HasPrefix(line, "\t\t") {
			if id, name, ok := strings.Cut(line[1:], "  "); ok && strings.ToLower(id) == device {
				return vendorName, strings.TrimSpace(name)
			}
		}
	}
	return vendorName, ""
}

// marketing picks the name people know from a pci.ids entry: "AD102 [GeForce
// RTX 4090]" is the RTX 4090, so the last bracketed part wins when there is one.
func marketing(name string) string {
	open, end := strings.LastIndex(name, "["), strings.LastIndex(name, "]")
	if open >= 0 && end > open+1 {
		return name[open+1 : end]
	}
	return name
}

// Name is how a GPU is shown to people: "NVIDIA GeForce RTX 4090" from the PCI
// ID database, or the vendor and the raw IDs when the database does not know it.
func Name(fs FS, d Device) string {
	var vendorName, deviceName string
	for _, p := range idDatabases {
		if db, err := fs.ReadFile(p); err == nil {
			vendorName, deviceName = lookup(db, d.Vendor, d.Device)
			break
		}
	}
	vendor := shortVendor[d.Vendor]
	if vendor == "" {
		vendor = vendorName
	}
	if vendor == "" {
		vendor = "PCI vendor " + d.Vendor
	}
	if deviceName == "" {
		return vendor + " GPU " + d.ID()
	}
	return vendor + " " + marketing(deviceName)
}

// VendorName is a vendor's short name, or its ID when it has none here.
func VendorName(vendor string) string {
	if name := shortVendor[vendor]; name != "" {
		return name
	}
	return "PCI vendor " + vendor
}

// NormalizeBDF turns nvidia-smi's 8-digit PCI domain ("00000000:0F:01.0") into
// the 4-digit form sysfs and VFIO use ("0000:0f:01.0"). Anything that does not
// look like a PCI address is dropped.
func NormalizeBDF(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	parts := strings.Split(s, ":")
	if len(parts) != 3 || !strings.Contains(parts[2], ".") {
		return ""
	}
	if len(parts[0]) == 8 {
		parts[0] = parts[0][4:]
	}
	return strings.Join(parts, ":")
}
