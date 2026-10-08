package stats

import (
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/serverroom/gpu-marketplace/internal/pcidev"
)

// An ARM system-on-chip often carries a neural processing unit (NPU) beside
// its CPU cores and its GPU: an RK3588's is three cores its maker rates at 6
// TOPS together. Like the SoC's GPU (gpu_soc.go) it is a device-tree node on no
// PCI bus, so nothing listed it. A rental runs in a microVM, which cannot be
// handed a SoC's device, so the specs list it for the marketplace to name it
// and say it is not included.

// Accelerator is a processing unit a SoC carries beside its CPU cores and its
// GPU. Absent from agents up to v0.3.10.
type Accelerator struct {
	// Kind is what it is: "npu".
	Kind string `json:"kind"`
	// Model names it: "Rockchip RK3588 NPU".
	Model string `json:"model"`
	// TOPS is its maker's own rating, in trillions of operations a second (6
	// for an RK3588's); omitted when the agent does not know it.
	TOPS float64 `json:"tops,omitempty"`
	// Cores is how many cores it has, where the device tree or its maker says
	// (3 on an RK3588); omitted when unknown.
	Cores int `json:"cores,omitempty"`
	// Driver is the kernel driver the host runs it with ("RKNPU" on Rockchip's
	// own kernels, "rocket" on mainline); omitted when none is bound.
	Driver string `json:"driver,omitempty"`
}

// Label is the accelerator in words, with its maker's rating where known:
// "Rockchip RK3588 NPU (6 TOPS)".
func (a Accelerator) Label() string {
	if a.TOPS > 0 {
		return fmt.Sprintf("%s (%s TOPS)", a.Model, strconv.FormatFloat(a.TOPS, 'f', -1, 64))
	}
	return a.Model
}

// socNPUPatterns are where a SoC's NPU node sits in the device tree: at its
// root (Rockchip) or under its soc bus (most others).
var socNPUPatterns = []string{
	"/proc/device-tree/npu@*/compatible",
	"/proc/device-tree/soc/npu@*/compatible",
	"/proc/device-tree/soc@*/npu@*/compatible",
}

// socNPURatings are the NPUs whose maker publishes a rating, by the SoC the
// device tree's root names: TOPS for the whole NPU, and its cores.
var socNPURatings = map[string]struct {
	tops  float64
	cores int
}{
	"Rockchip RK3588":  {6, 3},
	"Rockchip RK3588S": {6, 3},
	"Rockchip RK3576":  {6, 0},
}

// socNPUNames name an NPU by its node's own compatible string, for a board
// whose root does not name a SoC the table knows.
var socNPUNames = map[string]string{
	"rockchip,rk3588-rknpu":     "Rockchip RK3588 NPU",
	"rockchip,rk3588-rknn-core": "Rockchip RK3588 NPU",
	"rockchip,rk3576-rknpu":     "Rockchip RK3576 NPU",
	"rockchip,rk3568-rknpu":     "Rockchip RK3568 NPU",
}

// socNPUName names an NPU node from its compatible strings and the device
// tree's root: a Rockchip one by the SoC ("Rockchip RK3588 NPU"), a Vivante one
// by its maker, any other by what the node is called, "NPU".
func socNPUName(compatible, rootCompatible []byte) string {
	values := dtStrings(compatible)
	rockchip := false
	for _, v := range values {
		rockchip = rockchip || (strings.HasPrefix(v, "rockchip,") && (strings.Contains(v, "rknpu") || strings.Contains(v, "rknn")))
	}
	if rockchip {
		if soc := SoCName(rootCompatible); strings.HasPrefix(soc, "Rockchip ") {
			return soc + " NPU"
		}
		for _, v := range values {
			if name, ok := socNPUNames[v]; ok {
				return name
			}
		}
		return "Rockchip NPU"
	}
	for _, v := range values {
		if strings.HasPrefix(v, "vivante,") {
			return "VeriSilicon Vivante NPU"
		}
	}
	return "NPU"
}

// socDriver is the kernel driver bound to a device-tree node's platform device
// ("mali", "panthor", "RKNPU", "rocket"), "" when none is: the platform device
// is found by its of_node link, whatever address its bus gave it.
func socDriver(fs pcidev.FS, node string) string {
	name := path.Base(node)
	if i := strings.Index(name, "@"); i >= 0 {
		name = name[:i]
	}
	want := "/devicetree/base" + strings.TrimPrefix(node, "/proc/device-tree")
	links, _ := fs.Glob("/sys/bus/platform/devices/*." + name + "/of_node")
	for _, link := range links {
		target, err := fs.Readlink(link)
		if err != nil || !strings.HasSuffix(target, want) {
			continue
		}
		if driver, err := fs.Readlink(strings.TrimSuffix(link, "of_node") + "driver"); err == nil {
			return path.Base(driver)
		}
	}
	return ""
}

// dtEnabled reports whether a device-tree node is switched on: no status
// property, or one that says okay.
func dtEnabled(fs pcidev.FS, node string) bool {
	status, err := fs.ReadFile(node + "/status")
	if err != nil {
		return true
	}
	s := dtStrings(status)
	return len(s) == 0 || s[0] == "okay" || s[0] == "ok"
}

// socAccelerators are the SoC's NPUs from the device tree; a node the board's
// device tree switched off (status "disabled") is left out. A mainline kernel
// describes an RK3588's NPU as one node per core, a Rockchip kernel as one node
// for all three: either way it is one NPU.
func socAccelerators(fs pcidev.FS) []Accelerator {
	var out []Accelerator
	seen := map[string]bool{}
	root, _ := fs.ReadFile("/proc/device-tree/compatible")
	for _, pattern := range socNPUPatterns {
		paths, _ := fs.Glob(pattern)
		for _, p := range paths {
			node := strings.TrimSuffix(p, "/compatible")
			if seen[node] {
				continue
			}
			seen[node] = true
			if !dtEnabled(fs, node) {
				continue
			}
			compatible, err := fs.ReadFile(p)
			if err != nil {
				continue
			}
			model := socNPUName(compatible, root)
			driver := socDriver(fs, node)
			perCore := false
			for _, v := range dtStrings(compatible) {
				perCore = perCore || strings.HasSuffix(v, "-rknn-core")
			}
			merged := false
			for i := range out {
				if out[i].Model != model {
					continue
				}
				// Another node of the same NPU: one more of its cores.
				if perCore {
					out[i].Cores++
				}
				if out[i].Driver == "" {
					out[i].Driver = driver
				}
				merged = true
			}
			if merged {
				continue
			}
			a := Accelerator{Kind: "npu", Model: model, Driver: driver}
			if perCore {
				a.Cores = 1
			}
			out = append(out, a)
		}
	}
	// The maker's rating, for an NPU the agent knows by its SoC. A device tree
	// that switched some of its cores off is not given the whole NPU's figure.
	rating, rated := socNPURatings[SoCName(root)]
	for i := range out {
		if !rated || !strings.HasPrefix(out[i].Model, SoCName(root)) {
			continue
		}
		if out[i].Cores == 0 {
			out[i].Cores = rating.cores
		}
		if rating.cores == 0 || out[i].Cores == rating.cores {
			out[i].TOPS = rating.tops
		}
	}
	return out
}

// SoCLeftOut names what a SoC carries that no rental is given, for the agent
// to say so: its GPU ("Arm Mali-G610 MP4") and its NPU ("Rockchip RK3588 NPU
// (6 TOPS)"). Empty on a machine with no device tree.
func SoCLeftOut(fs pcidev.FS) []string {
	var out []string
	for _, g := range socGPUs(fs) {
		out = append(out, g.Model)
	}
	for _, a := range socAccelerators(fs) {
		out = append(out, a.Label())
	}
	return out
}
