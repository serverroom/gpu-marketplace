package interconnect

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// Everything that decides whether a machine is an NVIDIA DGX Spark is in this
// block. The exact DMI strings a Spark carries are NOT verified on hardware yet
// (DESIGN.md assumption A6): other GB10 machines (ASUS Ascent GX10, HP ZGX
// Nano) share the GPU but are not Sparks, and pairs are only offered between
// two confirmed Sparks. The raw values are always reported so the patterns can
// be corrected from what real machines say.
var (
	sparkOS              = "linux"
	sparkArch            = "arm64"
	sparkVendorPattern   = regexp.MustCompile(`(?i)nvidia`)
	sparkProductPattern  = regexp.MustCompile(`(?i)dgx[\s_-]*spark`)
	sparkGPUPart         = "GB10"
	dmiDir               = "/sys/class/dmi/id/"
	identityReasonPrefix = "linked pairs need two NVIDIA DGX Sparks: "
)

// DMIMaxLen caps each DMI string the agent reports.
const DMIMaxLen = 80

// ReadIdentity reads the machine's DMI strings and matches them. A missing
// file reads as "".
func ReadIdentity(h vmrt.Host, goos, arch string, gpuModels []string) control.Identity {
	return MatchIdentity(control.Identity{
		SysVendor:     dmiValue(h, "sys_vendor"),
		ProductName:   dmiValue(h, "product_name"),
		BoardName:     dmiValue(h, "board_name"),
		ProductFamily: dmiValue(h, "product_family"),
	}, goos, arch, gpuModels)
}

func dmiValue(h vmrt.Host, name string) string {
	data, err := h.ReadFile(dmiDir + name)
	if err != nil {
		return ""
	}
	return cleanDMI(string(data))
}

// cleanDMI trims a DMI string, replaces anything but printable ASCII, and caps
// its length: it is shown to people and stored by the control plane.
func cleanDMI(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= DMIMaxLen {
			break
		}
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	return strings.TrimSpace(b.String())
}

// MatchIdentity decides whether raw DMI values, the platform this binary runs
// on and the detected GPU models are an NVIDIA DGX Spark: linux/arm64, an
// NVIDIA system vendor, a product name or family naming a DGX Spark, and a
// GB10 GPU. Reason says which of those failed first. It is the one matcher:
// the pair checks and the desktop-on-demand rule both use it.
func MatchIdentity(id control.Identity, goos, arch string, gpuModels []string) control.Identity {
	id.SysVendor = cleanDMI(id.SysVendor)
	id.ProductName = cleanDMI(id.ProductName)
	id.BoardName = cleanDMI(id.BoardName)
	id.ProductFamily = cleanDMI(id.ProductFamily)
	id.ConfirmedDGXSpark = false
	id.Reason = ""

	hasGPU := false
	for _, m := range gpuModels {
		if strings.Contains(strings.ToUpper(m), sparkGPUPart) {
			hasGPU = true
		}
	}
	switch {
	case goos != sparkOS:
		id.Reason = fmt.Sprintf("this machine runs %s, and an NVIDIA DGX Spark runs Linux", goos)
	case arch != sparkArch:
		id.Reason = fmt.Sprintf("this machine is %s, and an NVIDIA DGX Spark is %s", arch, sparkArch)
	case id.SysVendor == "":
		id.Reason = "this machine does not report its system vendor (DMI sys_vendor), so it cannot be confirmed as an NVIDIA DGX Spark"
	case !sparkVendorPattern.MatchString(id.SysVendor):
		id.Reason = fmt.Sprintf("system vendor '%s' is not NVIDIA", id.SysVendor)
	case !sparkProductPattern.MatchString(id.ProductName) && !sparkProductPattern.MatchString(id.ProductFamily):
		if id.ProductName == "" && id.ProductFamily == "" {
			id.Reason = "this machine does not report its product name (DMI product_name, product_family), so it cannot be confirmed as an NVIDIA DGX Spark"
		} else {
			name := id.ProductName
			if name == "" {
				name = id.ProductFamily
			}
			id.Reason = fmt.Sprintf("product name '%s' is not an NVIDIA DGX Spark", name)
		}
	case !hasGPU:
		id.Reason = "no NVIDIA " + sparkGPUPart + " GPU was detected"
	default:
		id.ConfirmedDGXSpark = true
	}
	return id
}
