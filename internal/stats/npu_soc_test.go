package stats

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// platformDevice binds a device-tree node's platform device to a driver, the
// way sysfs shows it.
func platformDevice(h *fakehost.Host, name, node, driver string) {
	h.Links["/sys/bus/platform/devices/"+name+"/of_node"] = "../../../firmware/devicetree/base" + node
	if driver != "" {
		h.Links["/sys/bus/platform/devices/"+name+"/driver"] = "../../../bus/platform/drivers/" + driver
	}
}

// A Rockchip kernel describes an RK3588's NPU as one device-tree node: the
// specs list it by its SoC's name, with its maker's rating, its three cores
// and the driver the host runs it with.
func TestTheSpecsListAnRK3588sNPU(t *testing.T) {
	h := fakehost.New()
	h.Files["/proc/device-tree/compatible"] = []byte("radxa,zaku2\x00rockchip,rk3588\x00")
	h.Files["/proc/device-tree/npu@fdab0000/compatible"] = []byte("rockchip,rk3588-rknpu\x00")
	h.Files["/proc/device-tree/npu@fdab0000/status"] = []byte("okay\x00")
	platformDevice(h, "fdab0000.npu", "/npu@fdab0000", "RKNPU")
	got := socAccelerators(h)
	want := Accelerator{Kind: "npu", Model: "Rockchip RK3588 NPU", TOPS: 6, Cores: 3, Driver: "RKNPU"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("accelerators = %+v", got)
	}
	if label := got[0].Label(); label != "Rockchip RK3588 NPU (6 TOPS)" {
		t.Errorf("label = %q", label)
	}
	data, _ := json.Marshal(got[0])
	if string(data) != `{"kind":"npu","model":"Rockchip RK3588 NPU","tops":6,"cores":3,"driver":"RKNPU"}` {
		t.Errorf("JSON = %s", data)
	}
}

// A mainline kernel describes the same NPU as one node per core: it is still
// one NPU, with the cores the device tree switched on. With one switched off
// it is not given the whole NPU's rating.
func TestAMainlineKernelsNPUIsOneNPU(t *testing.T) {
	h := fakehost.New()
	h.Files["/proc/device-tree/compatible"] = []byte("radxa,rock-5b\x00rockchip,rk3588\x00")
	for _, addr := range []string{"fdab0000", "fdac0000", "fdad0000"} {
		h.Files["/proc/device-tree/npu@"+addr+"/compatible"] = []byte("rockchip,rk3588-rknn-core\x00")
		platformDevice(h, addr+".npu", "/npu@"+addr, "rocket")
	}
	got := socAccelerators(h)
	want := Accelerator{Kind: "npu", Model: "Rockchip RK3588 NPU", TOPS: 6, Cores: 3, Driver: "rocket"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("accelerators = %+v", got)
	}
	h.Files["/proc/device-tree/npu@fdad0000/status"] = []byte("disabled\x00")
	got = socAccelerators(h)
	if len(got) != 1 || got[0].Cores != 2 || got[0].TOPS != 0 || got[0].Label() != "Rockchip RK3588 NPU" {
		t.Fatalf("two cores on: %+v", got)
	}
}

// An NPU the board's device tree switched off is left out, as its GPU is; one
// with no driver bound is listed without one.
func TestASwitchedOffNPUIsLeftOut(t *testing.T) {
	h := fakehost.New()
	h.Files["/proc/device-tree/compatible"] = []byte("radxa,rock-5b\x00rockchip,rk3588\x00")
	h.Files["/proc/device-tree/npu@fdab0000/compatible"] = []byte("rockchip,rk3588-rknpu\x00")
	h.Files["/proc/device-tree/npu@fdab0000/status"] = []byte("disabled\x00")
	if got := socAccelerators(h); len(got) != 0 {
		t.Fatalf("switched off: %+v", got)
	}
	h.Files["/proc/device-tree/npu@fdab0000/status"] = []byte("okay\x00")
	platformDevice(h, "fdab0000.npu", "/npu@fdab0000", "")
	// Another node's platform device is not this one's.
	platformDevice(h, "fdb50000.npu", "/soc/npu@fdb50000", "other")
	if got := socAccelerators(h); len(got) != 1 || got[0].Driver != "" {
		t.Fatalf("no driver bound: %+v", got)
	}
}

// NPUs by what the device tree says: a Rockchip one on a board whose root the
// table does not know is named by its own node, a Vivante one by its maker,
// any other node called npu plainly. Only one whose SoC the table rates
// carries a figure.
func TestNPUsAreNamedByWhatTheDeviceTreeSays(t *testing.T) {
	cases := []struct{ root, path, compatible, want string }{
		{"vendor,board\x00", "/proc/device-tree/npu@fde40000", "rockchip,rk3568-rknpu\x00rockchip,rknpu\x00", "Rockchip RK3568 NPU"},
		{"vendor,board\x00", "/proc/device-tree/npu@fde40000", "rockchip,rk9999-rknpu\x00", "Rockchip NPU"},
		{"vendor,board\x00rockchip,rk3576\x00", "/proc/device-tree/npu@27700000", "rockchip,rk3576-rknpu\x00", "Rockchip RK3576 NPU"},
		{"vendor,board\x00", "/proc/device-tree/soc@0/npu@38500000", "vivante,gc\x00", "VeriSilicon Vivante NPU"},
		{"vendor,board\x00", "/proc/device-tree/soc/npu@ff100000", "vendor,npu\x00", "NPU"},
		// An NPU that is not the SoC's own is not given the SoC's rating.
		{"vendor,board\x00rockchip,rk3588\x00", "/proc/device-tree/soc/npu@ff100000", "vivante,gc\x00", "VeriSilicon Vivante NPU"},
	}
	for _, c := range cases {
		h := fakehost.New()
		h.Files["/proc/device-tree/compatible"] = []byte(c.root)
		h.Files[c.path+"/compatible"] = []byte(c.compatible)
		got := socAccelerators(h)
		if len(got) != 1 || got[0].Model != c.want || got[0].Kind != "npu" {
			t.Errorf("%s: %+v, want %s", c.compatible, got, c.want)
			continue
		}
		if rated := strings.Contains(c.root, "rk3576"); (got[0].TOPS > 0) != rated {
			t.Errorf("%s: TOPS %v", c.compatible, got[0].TOPS)
		}
	}
}

// No device tree (an x86 machine, a GB10 on ACPI): no accelerator, and the
// specs carry no "accelerators" at all.
func TestNoDeviceTreeNoAccelerators(t *testing.T) {
	if got := socAccelerators(fakehost.New()); len(got) != 0 {
		t.Fatalf("accelerators = %+v", got)
	}
	data, _ := json.Marshal(SystemStats{})
	if strings.Contains(string(data), "accelerators") {
		t.Errorf("specs = %s", data)
	}
}

// The SoC's GPU says which kernel driver the host runs it with: a board
// maker's kernel binds "mali", mainline "panthor".
func TestASoCGPUNamesItsDriver(t *testing.T) {
	h := fakehost.New()
	h.Files["/proc/device-tree/compatible"] = []byte("radxa,zaku2\x00rockchip,rk3588\x00")
	h.Files["/proc/device-tree/gpu@fb000000/compatible"] = []byte("arm,mali-bifrost\x00")
	platformDevice(h, "fb000000.gpu", "/gpu@fb000000", "mali")
	gpus := socGPUs(h)
	if len(gpus) != 1 || gpus[0].Driver != "mali" {
		t.Fatalf("gpus = %+v", gpus)
	}
	data, _ := json.Marshal(gpus[0])
	if !strings.Contains(string(data), `"driver":"mali"`) {
		t.Errorf("JSON = %s", data)
	}
}

// What a SoC carries that no rental is given, by name: its GPU and its NPU.
func TestWhatASoCCarriesThatRentalsLeaveOut(t *testing.T) {
	h := fakehost.New()
	if got := SoCLeftOut(h); len(got) != 0 {
		t.Fatalf("no device tree: %v", got)
	}
	h.Files["/proc/device-tree/compatible"] = []byte("radxa,zaku2\x00rockchip,rk3588\x00")
	h.Files["/proc/device-tree/gpu@fb000000/compatible"] = []byte("arm,mali-bifrost\x00")
	h.Files["/proc/device-tree/npu@fdab0000/compatible"] = []byte("rockchip,rk3588-rknpu\x00")
	got := SoCLeftOut(h)
	if len(got) != 2 || got[0] != "Arm Mali-G610 MP4" || got[1] != "Rockchip RK3588 NPU (6 TOPS)" {
		t.Fatalf("left out = %v", got)
	}
}
