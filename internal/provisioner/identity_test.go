package provisioner

import (
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

func TestMatchIdentity(t *testing.T) {
	spark := control.Identity{SysVendor: "NVIDIA", ProductName: "NVIDIA DGX Spark", ProductFamily: "DGX Spark"}
	gb10 := []string{"NVIDIA GB10"}
	for name, c := range map[string]struct {
		id         control.Identity
		goos, arch string
		gpus       []string
		want       bool
	}{
		"a DGX Spark":                   {spark, "linux", "arm64", gb10, true},
		"product name only":             {control.Identity{SysVendor: "NVIDIA Corporation", ProductName: "DGX_Spark"}, "linux", "arm64", gb10, true},
		"family only":                   {control.Identity{SysVendor: "nvidia", ProductName: "P4242", ProductFamily: "DGX-Spark"}, "linux", "arm64", gb10, true},
		"spaced and cased":              {control.Identity{SysVendor: "NVIDIA", ProductName: "dgx   spark"}, "linux", "arm64", []string{"gb10"}, true},
		"on amd64":                      {spark, "linux", "amd64", gb10, false},
		"not linux":                     {spark, "darwin", "arm64", gb10, false},
		"another vendor's GB10 machine": {control.Identity{SysVendor: "ASUSTeK COMPUTER INC.", ProductName: "Ascent GX10"}, "linux", "arm64", gb10, false},
		"HP's GB10 machine":             {control.Identity{SysVendor: "HP", ProductName: "HP ZGX Nano G1n AI Station"}, "linux", "arm64", gb10, false},
		"NVIDIA, but another product":   {control.Identity{SysVendor: "NVIDIA", ProductName: "Jetson AGX Thor"}, "linux", "arm64", gb10, false},
		"no GB10":                       {spark, "linux", "arm64", []string{"NVIDIA RTX PRO 6000"}, false},
		"no GPU at all":                 {spark, "linux", "arm64", nil, false},
		"no DMI":                        {control.Identity{}, "linux", "arm64", gb10, false},
		"spark named by another vendor": {control.Identity{SysVendor: "Acme", ProductName: "DGX Spark"}, "linux", "arm64", gb10, false},
	} {
		if got := MatchIdentity(c.id, c.goos, c.arch, c.gpus); got.ConfirmedDGXSpark != c.want {
			t.Errorf("%s: confirmed=%v, want %v (%+v)", name, got.ConfirmedDGXSpark, c.want, got)
		}
	}
}

func TestReadIdentityReportsTheRawStrings(t *testing.T) {
	h := fakehost.New()
	h.Files["/sys/class/dmi/id/sys_vendor"] = []byte("NVIDIA\n")
	h.Files["/sys/class/dmi/id/product_name"] = []byte("  NVIDIA DGX Spark \n")
	h.Files["/sys/class/dmi/id/product_family"] = []byte("DGX\x00Spark\x07\n")
	id := ReadIdentity(h, "linux", "arm64", []string{"NVIDIA GB10"})
	if id.SysVendor != "NVIDIA" || id.ProductName != "NVIDIA DGX Spark" || id.ProductFamily != "DGX?Spark?" || !id.ConfirmedDGXSpark {
		t.Errorf("identity = %+v", id)
	}
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'x'
	}
	h.Files["/sys/class/dmi/id/product_name"] = long
	if got := ReadIdentity(h, "linux", "arm64", nil).ProductName; len(got) != dmiMaxLen {
		t.Errorf("an overlong DMI string was not capped: %d bytes", len(got))
	}
}
