package vmrt

import (
	"strings"
	"testing"
)

// firstStateWith is the index of the first save of the rental state whose
// JSON contains want, or -1.
func firstStateWith(calls []string, want string) int {
	prefix := "write " + StatePath(dataDir) + ".tmp="
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) && strings.Contains(c, want) {
			return i
		}
	}
	return -1
}

// Every function is on disk, with the driver it came from, before it is moved:
// an agent killed between the two still gives it back.
func TestEachFunctionIsSavedBeforeItMoves(t *testing.T) {
	h := pairHost()
	rt, _, err := startPair(t, h, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	for _, bdf := range []string{testGPU, testAudio, nic0, nic1} {
		saved := firstStateWith(h.Calls, `"bdf": "`+bdf+`"`)
		moved := h.Index("write /sys/bus/pci/devices/" + bdf + "/driver_override=vfio-pci")
		if saved < 0 || moved < 0 || saved > moved {
			t.Errorf("%s: saved at %d, moved at %d", bdf, saved, moved)
		}
	}
	if res := rt.Stop(); !res.Clean() {
		t.Fatalf("Stop = %+v", res)
	}
}

// An NVIDIA service is on disk before it is stopped, so a start that dies
// right after the stop still starts it again.
func TestAServiceIsSavedBeforeItStops(t *testing.T) {
	h := newHost()
	h.SetFail("systemctl is-active --quiet nvidia-persistenced", nil)
	rt, _ := newRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatal(err)
	}
	saved := firstStateWith(h.Calls, "nvidia-persistenced")
	stopped := h.Index("run systemctl stop nvidia-persistenced")
	if saved < 0 || stopped < 0 || saved > stopped {
		t.Errorf("saved at %d, stopped at %d", saved, stopped)
	}
	rt.Stop()
}

// A card that cannot go (its RDMA devices are in use) is found before
// anything is taken from the host: a Spark's desktop stays up and the GPU stays.
func TestABusyCardLeavesTheDesktopAndGPUAlone(t *testing.T) {
	h := sparkHost()
	h.NIC(nic0, "enP1p1s0f0np0", nicMAC0, "MT2412X00001", nic0)
	h.NIC(nic1, "enP1p1s0f1np1", nicMAC1, "MT2412X00001", nic1)
	h.Links["/proc/777/fd/4"] = "/dev/infiniband/uverbs0"
	h.Files["/proc/777/comm"] = []byte("ib_write_bw\n")
	rt, _ := sparkRuntime(h, func() bool { return true })
	p := goodPair(t)
	p.OtherMACs = nil
	err := rt.Start(StartOptions{ID: pairID, Pubkey: key(t), Pair: p})
	if err == nil || !strings.Contains(err.Error(), "ib_write_bw") {
		t.Fatalf("Start = %v", err)
	}
	if h.Ran("run systemctl stop "+dm) || h.Ran("write /sys/bus/pci/devices/"+testGPU+"/driver_override") {
		t.Errorf("the desktop or the GPU was taken for a card that could not go: %v", h.Calls)
	}
}

// A function that is already back on its own driver -- an earlier teardown
// gave it back, or it was never moved -- is not reset under that driver.
func TestReleaseLeavesAFunctionOnItsDriverAlone(t *testing.T) {
	h := newHost()
	ok, detail := ReleaseVFIO(h, []BoundDevice{{BDF: testGPU, Driver: "nvidia"}})
	if !ok || len(detail) != 0 {
		t.Fatalf("ok=%v detail=%v", ok, detail)
	}
	if h.Ran("write /sys/bus/pci/devices/"+testGPU+"/reset") || h.Ran("write /sys/bus/pci/drivers_probe") {
		t.Errorf("a function on its own driver was reset or probed: %v", h.Calls)
	}

	// On another host driver than it came from: reported, still not reset.
	ok, detail = ReleaseVFIO(h, []BoundDevice{{BDF: testGPU, Driver: "nouveau"}})
	if ok || len(detail) != 1 || !strings.Contains(detail[0], `"nvidia", not nouveau`) {
		t.Errorf("ok=%v detail=%v", ok, detail)
	}
	if h.Ran("write /sys/bus/pci/devices/" + testGPU + "/reset") {
		t.Errorf("reset under a live driver: %v", h.Calls)
	}

	// On vfio-pci: released, reset, probed back.
	if _, err := BindVFIO(h, []string{testGPU}, nil); err != nil {
		t.Fatal(err)
	}
	if ok, detail := ReleaseVFIO(h, []BoundDevice{{BDF: testGPU, Driver: "nvidia"}}); !ok {
		t.Fatalf("release: %v", detail)
	}
	if !h.Ran("write /sys/bus/pci/devices/"+testGPU+"/reset") || h.Driver(testGPU) != "nvidia" {
		t.Errorf("not released properly: driver %s, calls %v", h.Driver(testGPU), h.Calls)
	}
}
