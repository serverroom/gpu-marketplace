package provisioner

import (
	"regexp"
	"strings"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// Everything that decides whether a machine is an NVIDIA DGX Spark is in this
// block, and table-tested in identity_test.go. The exact DMI strings a Spark
// carries are NOT verified on hardware yet; other GB10 machines (ASUS Ascent
// GX10, HP ZGX Nano) share the GPU but are not Sparks, so the vendor and the
// product must both say so. The raw values always travel in the capability,
// so the patterns can be corrected from what real machines report.
var (
	sparkOS             = "linux"
	sparkArch           = "arm64"
	sparkVendorPattern  = regexp.MustCompile(`(?i)nvidia`)
	sparkProductPattern = regexp.MustCompile(`(?i)dgx[\s_-]*spark`)
	sparkGPUPart        = "GB10"
	dmiDir              = "/sys/class/dmi/id/"
	dmiMaxLen           = 80
)

// ReadIdentity reads the machine's DMI strings and matches them. A missing
// file reads as "".
func ReadIdentity(h vmrt.Host, goos, arch string, gpuNames []string) control.Identity {
	return MatchIdentity(control.Identity{
		SysVendor:     dmiValue(h, "sys_vendor"),
		ProductName:   dmiValue(h, "product_name"),
		ProductFamily: dmiValue(h, "product_family"),
	}, goos, arch, gpuNames)
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
		if b.Len() >= dmiMaxLen {
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
// on and the detected GPU names are an NVIDIA DGX Spark: linux/arm64, an
// NVIDIA system vendor, a product name or family naming a DGX Spark, and a
// GB10 GPU.
func MatchIdentity(id control.Identity, goos, arch string, gpuNames []string) control.Identity {
	id.SysVendor = cleanDMI(id.SysVendor)
	id.ProductName = cleanDMI(id.ProductName)
	id.ProductFamily = cleanDMI(id.ProductFamily)
	hasGPU := false
	for _, n := range gpuNames {
		if strings.Contains(strings.ToUpper(n), sparkGPUPart) {
			hasGPU = true
		}
	}
	id.ConfirmedDGXSpark = goos == sparkOS && arch == sparkArch &&
		sparkVendorPattern.MatchString(id.SysVendor) &&
		(sparkProductPattern.MatchString(id.ProductName) || sparkProductPattern.MatchString(id.ProductFamily)) &&
		hasGPU
	return id
}
