package pcidev

import (
	"fmt"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// A server with a BMC display, a discrete card with its audio function, an
// integrated GPU, and a NIC: only the two real GPUs are GPUs.
func serverWithGPUs() *fakehost.Host {
	h := fakehost.New()
	h.PCI("0000:03:00.0", "ast", "0x030000")
	h.PCIID("0000:03:00.0", "1a03", "2000")
	h.PCI("0000:01:00.0", "nvidia", "0x030000")
	h.PCIID("0000:01:00.0", "10de", "2684")
	h.PCI("0000:01:00.1", "snd_hda_intel", "0x040300")
	h.PCIID("0000:01:00.1", "10de", "22ba")
	h.PCI("0000:00:02.0", "i915", "0x030000")
	h.PCIID("0000:00:02.0", "8086", "46a6")
	h.PCI("0000:02:00.0", "ixgbe", "0x020000")
	h.PCIID("0000:02:00.0", "8086", "10fb")
	return h
}

func TestDisplayFindsGPUsAndSkipsManagementDisplays(t *testing.T) {
	got := Display(serverWithGPUs())
	if len(got) != 2 {
		t.Fatalf("Display = %+v, want the NVIDIA card and the Intel iGPU", got)
	}
	if got[0].BDF != "0000:00:02.0" || got[0].ID() != "8086:46a6" || got[0].Driver != "i915" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].BDF != "0000:01:00.0" || got[1].ID() != "10de:2684" || got[1].Driver != "nvidia" || got[1].Class != "030000" {
		t.Errorf("second = %+v", got[1])
	}
}

func TestDisplaySkipsVirtualDisplays(t *testing.T) {
	h := fakehost.New()
	for i, vendor := range []string{"1234", "1af4", "1b36", "15ad", "80ee", "1414", "1013", "102b"} {
		bdf := "0000:00:0" + string(rune('1'+i)) + ".0"
		h.PCI(bdf, "", "0x030000")
		h.PCIID(bdf, vendor, "0001")
	}
	if got := Display(h); len(got) != 0 {
		t.Errorf("virtual or management displays counted as GPUs: %+v", got)
	}
}

// AMD's Instinct MI300 family is a "processing accelerator" (class 0x12), not a
// display device, and it is a GPU. The same class also holds Intel's NPUs and
// Huawei's Ascend, which are not; only NVIDIA's and AMD's count.
func TestDisplayFindsAcceleratorClassGPUs(t *testing.T) {
	h := fakehost.New()
	h.PCI("0000:41:00.0", "amdgpu", "0x120000")
	h.PCIID("0000:41:00.0", "1002", "74a1")
	h.PCI("0000:00:0b.0", "intel_vpu", "0x120000")
	h.PCIID("0000:00:0b.0", "8086", "7d1d")
	h.PCI("0000:81:00.0", "", "0x120000")
	h.PCIID("0000:81:00.0", "19e5", "d801")
	got := Display(h)
	if len(got) != 1 || got[0].BDF != "0000:41:00.0" {
		t.Errorf("Display = %+v, want only the MI300X", got)
	}
}

// More management displays: Huawei's iBMC, the Pilot and Silicon Motion
// chips some boards carry, XGI on older servers.
func TestDisplaySkipsMoreManagementDisplays(t *testing.T) {
	h := fakehost.New()
	for i, vendor := range []string{"19e5", "19a2", "126f", "18ca"} {
		bdf := "0000:0" + string(rune('1'+i)) + ":00.0"
		h.PCI(bdf, "", "0x030000")
		h.PCIID(bdf, vendor, "1711")
	}
	if got := Display(h); len(got) != 0 {
		t.Errorf("management displays counted as GPUs: %+v", got)
	}
}

// An SR-IOV virtual function carries its GPU's class and IDs. It is a slice of
// a GPU that is listed already, not a GPU of its own.
func TestDisplaySkipsVirtualFunctions(t *testing.T) {
	h := fakehost.New()
	h.PCI("0000:41:00.0", "amdgpu", "0x030000")
	h.PCIID("0000:41:00.0", "1002", "7460")
	h.PCI("0000:41:02.0", "", "0x030000")
	h.PCIID("0000:41:02.0", "1002", "7461")
	h.Links["/sys/bus/pci/devices/0000:41:02.0/physfn"] = "../0000:41:00.0"
	if got := Display(h); len(got) != 1 || got[0].BDF != "0000:41:00.0" {
		t.Errorf("Display = %+v, want the physical GPU only", got)
	}
}

// A processor's own GPU is told from a card by where it sits: Intel's at
// 00:02.0 on the root bus, an AMD APU's behind the processor's internal GPU
// bridge 00:08.1. An Arc card and a Radeon card are cards.
func TestIntegrated(t *testing.T) {
	h := fakehost.New()
	h.Links["/sys/bus/pci/devices/0000:c4:00.0"] = "../../../devices/pci0000:00/0000:00:08.1/0000:c4:00.0"
	h.Links["/sys/bus/pci/devices/0000:03:00.0"] = "../../../devices/pci0000:00/0000:00:01.1/0000:03:00.0"
	// An APU whose iGPU sits behind some other bridge still carries the PSP in
	// its slot; a Radeon card's slot holds its audio (and USB-C), never a PSP.
	h.PCI("0000:e5:00.0", "amdgpu", "0x030000")
	h.PCI("0000:e5:00.1", "snd_hda_intel", "0x040300")
	h.PCI("0000:e5:00.2", "ccp", "0x108000")
	h.PCI("0000:05:00.0", "amdgpu", "0x030000")
	h.PCI("0000:05:00.1", "snd_hda_intel", "0x040300")
	h.PCI("0000:05:00.2", "xhci_hcd", "0x0c0330")
	for d, want := range map[Device]bool{
		{BDF: "0000:00:02.0", Vendor: Intel}:  true,
		{BDF: "0000:03:00.0", Vendor: Intel}:  false,
		{BDF: "0000:c4:00.0", Vendor: AMD}:    true,
		{BDF: "0000:03:00.0", Vendor: AMD}:    false,
		{BDF: "0000:e5:00.0", Vendor: AMD}:    true,
		{BDF: "0000:05:00.0", Vendor: AMD}:    false,
		{BDF: "0000:00:02.0", Vendor: NVIDIA}: false,
	} {
		if got := Integrated(h, d); got != want {
			t.Errorf("Integrated(%+v) = %v, want %v", d, got, want)
		}
	}
}

// A pre-Zen APU (Kaveri, Carrizo, the Moonshot m700) puts its GPU straight on
// the root bus; a card is always behind a root port.
func TestIntegratedOnTheRootBus(t *testing.T) {
	h := fakehost.New()
	h.Links["/sys/bus/pci/devices/0000:00:01.0"] = "../../../devices/pci0000:00/0000:00:01.0"
	h.Links["/sys/bus/pci/devices/0000:01:00.0"] = "../../../devices/pci0000:00/0000:00:03.1/0000:01:00.0"
	if !Integrated(h, Device{BDF: "0000:00:01.0", Vendor: AMD}) {
		t.Error("a GPU on the root bus is not the processor's")
	}
	if Integrated(h, Device{BDF: "0000:01:00.0", Vendor: AMD}) {
		t.Error("a card behind a root port counted as integrated")
	}
}

// window returns a sysfs resource file with one memory BAR of size bytes.
func window(size uint64) []byte {
	return []byte(fmt.Sprintf("0x00000000e0000000 0x%016x 0x0000000000042208\n0x0000000000000000 0x0000000000000000 0x0000000000000000\n", 0xe0000000+size-1))
}

// Onboard display chips from vendors that also make GPUs: by device where known,
// and by their small memory window whatever they are. NVIDIA and 3D-class
// devices are not judged by window.
func TestOnboardDisplaysAreNotGPUs(t *testing.T) {
	h := fakehost.New()
	add := func(bdf, class, vendor, device string, size uint64) {
		h.PCI(bdf, "", class)
		h.PCIID(bdf, vendor, device)
		if size > 0 {
			h.Files["/sys/bus/pci/devices/"+bdf+"/resource"] = window(size)
		}
	}
	add("0000:01:03.0", "0x030000", "1002", "515e", 0)       // ES1000, by device
	add("0000:02:00.0", "0x030000", "1b21", "1234", 32<<20)  // an unknown onboard chip, by window
	add("0000:03:00.0", "0x030000", "1002", "744c", 256<<20) // a Radeon card without resizable BAR
	add("0000:04:00.0", "0x030200", "1002", "74a1", 64<<20)  // a 3D controller is not judged by window
	add("000f:01:00.0", "0x030000", "10de", "2e12", 32<<20)  // nor is NVIDIA (a GB10)
	var got []string
	for _, d := range Display(h) {
		got = append(got, d.BDF)
	}
	if strings.Join(got, " ") != "0000:03:00.0 0000:04:00.0 000f:01:00.0" {
		t.Errorf("Display = %v", got)
	}
}

func TestReadAnUnboundDevice(t *testing.T) {
	h := fakehost.New()
	h.PCI("0000:41:00.0", "", "0x030200")
	h.PCIID("0000:41:00.0", "1002", "74a1")
	d := Read(h, "0000:41:00.0")
	if d.Driver != "" || d.ID() != "1002:74a1" || d.Class != "030200" {
		t.Errorf("Read = %+v", d)
	}
}

const pciIDs = `# comment
10de  NVIDIA Corporation
	2684  AD102 [GeForce RTX 4090]
		10de 167c  Subsystem line that must not match
	2330  GH100 [H100 SXM5 80GB]
	1eb8  TU104GL [Tesla T4]
	9999  Plain Name Without Brackets
1002  Advanced Micro Devices, Inc. [AMD/ATI]
	74a1  Aqua Vanjaram [Instinct MI300X]
8086  Intel Corporation
	46a6  Alder Lake-P GT2 [Iris Xe Graphics]
`

func TestNameFromThePCIIDDatabase(t *testing.T) {
	h := fakehost.New()
	h.Files["/usr/share/misc/pci.ids"] = []byte(pciIDs)
	for id, want := range map[[2]string]string{
		{"10de", "2684"}: "NVIDIA GeForce RTX 4090",
		{"10de", "1eb8"}: "NVIDIA Tesla T4",
		{"10de", "9999"}: "NVIDIA Plain Name Without Brackets",
		{"1002", "74a1"}: "AMD Instinct MI300X",
		{"8086", "46a6"}: "Intel Iris Xe Graphics",
		{"10de", "0000"}: "NVIDIA GPU 10de:0000",
		{"1ed5", "0100"}: "PCI vendor 1ed5 GPU 1ed5:0100",
	} {
		if got := Name(h, Device{Vendor: id[0], Device: id[1]}); got != want {
			t.Errorf("Name(%s:%s) = %q, want %q", id[0], id[1], got, want)
		}
	}
}

func TestNameWithoutADatabase(t *testing.T) {
	if got := Name(fakehost.New(), Device{Vendor: "1002", Device: "744c"}); got != "AMD GPU 1002:744c" {
		t.Errorf("Name = %q", got)
	}
}

func TestNameFallsBackToHwdata(t *testing.T) {
	h := fakehost.New()
	h.Files["/usr/share/hwdata/pci.ids"] = []byte(pciIDs)
	if got := Name(h, Device{Vendor: "10de", Device: "2330"}); got != "NVIDIA H100 SXM5 80GB" {
		t.Errorf("Name = %q", got)
	}
}

func TestNormalizeBDF(t *testing.T) {
	for in, want := range map[string]string{
		"00000000:0F:01.0":    "0000:0f:01.0",
		"0000:41:00.0":        "0000:41:00.0",
		" 0000000F:01:00.0\n": "000f:01:00.0",
		"not a bdf":           "",
		"0000:41:00":          "",
	} {
		if got := NormalizeBDF(in); got != want {
			t.Errorf("NormalizeBDF(%q) = %q, want %q", in, got, want)
		}
	}
}
