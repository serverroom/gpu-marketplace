package provisioner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// An RK3588 board whose device tree has its Mali GPU and its NPU switched on
// still rents its CPUs, memory and disk only: a rental's microVM cannot be
// handed a SoC's own device. The host is told so, each by name, in lines short
// enough for the control panel to print whole.
func TestAnRK3588sMaliAndNPUAreLeftOutOfRentals(t *testing.T) {
	h := rk3588(t)
	h.Files["/proc/device-tree/gpu@fb000000/compatible"] = []byte("arm,mali-bifrost\x00")
	h.Files["/proc/device-tree/gpu@fb000000/status"] = []byte("okay\x00")
	h.Files["/proc/device-tree/npu@fdab0000/compatible"] = []byte("rockchip,rk3588-rknpu\x00")
	h.Files["/proc/device-tree/npu@fdab0000/status"] = []byte("okay\x00")
	c := detectARM(t, h).Capability()
	if !c.Ready || c.Kind != KindQEMU || c.GPUCount == nil || *c.GPUCount != 0 || len(c.GPUs) != 0 {
		t.Fatalf("capability = %+v", c)
	}
	want := []string{
		"Arm Mali-G610 MP4 is built into the processor: a rental runs in a virtual machine, which cannot use it",
		"Rockchip RK3588 NPU (6 TOPS) is built into the processor: a rental runs in a virtual machine, which cannot use it",
	}
	if strings.Join(c.Excluded, "|") != strings.Join(want, "|") {
		t.Fatalf("left out = %q", c.Excluded)
	}
	for _, line := range c.Excluded {
		if len(line) > 120 {
			t.Errorf("%d characters: %s", len(line), line)
		}
	}
}

// A board whose device tree switched both off, and one with neither, says
// nothing is left out.
func TestAnRK3588WithoutThemLeavesNothingOut(t *testing.T) {
	h := rk3588(t)
	h.Files["/proc/device-tree/gpu@fb000000/compatible"] = []byte("arm,mali-bifrost\x00")
	h.Files["/proc/device-tree/gpu@fb000000/status"] = []byte("disabled\x00")
	h.Files["/proc/device-tree/npu@fdab0000/compatible"] = []byte("rockchip,rk3588-rknpu\x00")
	h.Files["/proc/device-tree/npu@fdab0000/status"] = []byte("disabled\x00")
	if c := detectARM(t, h).Capability(); !c.Ready || len(c.Excluded) != 0 {
		t.Fatalf("capability = %+v", c)
	}
	if c := detectARM(t, rk3588(t)).Capability(); !c.Ready || len(c.Excluded) != 0 {
		t.Fatalf("capability = %+v", c)
	}
}

// The two do not disturb each other: a board that proved it can rent on both
// core types, and has its Mali and its NPU switched on, is offered with six
// vCPUs named as both types, still without a GPU, and still tells its host
// that the Mali and the NPU are left out. Neither is a GPU on the PCI bus, so
// neither keeps the board on one core type.
func TestABoardInTheWiderLayoutStillLeavesItsMaliAndNPUOut(t *testing.T) {
	h := rk3588(t)
	h.Files["/proc/device-tree/gpu@fb000000/compatible"] = []byte("arm,mali-bifrost\x00")
	h.Files["/proc/device-tree/npu@fdab0000/compatible"] = []byte("rockchip,rk3588-rknpu\x00")
	h.Files["/proc/sys/kernel/osrelease"] = []byte(mainlineKernel + "\n")
	if err := vmrt.MarkLayout(h, dataDir, version, []int{4, 5, 6, 7, 2, 3}, true, ""); err != nil {
		t.Fatal(err)
	}
	pass, _ := json.Marshal(vmrt.SelfTestResult{Passed: true, AgentVersion: version, Cores: "4,5,6,7,2,3"})
	h.Files[vmrt.SelfTestPath(dataDir)] = pass
	c := detectARM(t, h).Capability()
	if !c.Ready || c.Kind != KindQEMU || c.GPUCount == nil || *c.GPUCount != 0 {
		t.Fatalf("capability = %+v", c)
	}
	if c.Guest == nil || c.Guest.VCPUs != 6 || c.Guest.CPU != widerCPU {
		t.Errorf("guest = %+v", c.Guest)
	}
	if len(c.Excluded) != 2 || !strings.HasPrefix(c.Excluded[0], "Arm Mali-G610 MP4 is built into the processor") ||
		!strings.HasPrefix(c.Excluded[1], "Rockchip RK3588 NPU (6 TOPS) is built into the processor") {
		t.Errorf("left out = %q", c.Excluded)
	}
}
