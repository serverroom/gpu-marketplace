package vmrt

import (
	"errors"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

const dm = "display-manager.service"

// sparkHost is a DGX Spark with its desktop up on the GB10: Xorg, GNOME's
// shell and mutter hold the GPU, gdm runs as display-manager.service, and
// stopping it closes them (the fake's stand-in for the desktop exiting).
func sparkHost() *fakehost.Host {
	h := sparkDesktopHost()
	h.Outputs["systemctl show -p LoadState --value "+dm] = "loaded\n"
	h.SetFail("systemctl is-active --quiet "+dm, nil)
	h.OnRun["systemctl stop "+dm] = func(h *fakehost.Host, _ string) {
		for _, pid := range []string{"2558", "2872", "2909"} {
			h.DeleteLink("/proc/" + pid + "/fd/7")
		}
		h.SetFail("systemctl is-active --quiet "+dm, errors.New("inactive"))
	}
	h.OnRun["systemctl start "+dm] = func(h *fakehost.Host, _ string) {
		h.SetFail("systemctl is-active --quiet "+dm, nil)
	}
	return h
}

func sparkRuntime(h *fakehost.Host, verify func() bool) (*Runtime, *fakeFence) {
	spec := testSpec()
	spec.DesktopOnDemand = true
	fence := &fakeFence{h: h}
	return New(h, spec, fence, verify), fence
}

func TestSparkDesktopClosesForTheRentalAndComesBackAfter(t *testing.T) {
	h := sparkHost()
	rt, _ := sparkRuntime(h, func() bool { return true })
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start = %v; a Spark's desktop must close for the rental", err)
	}
	before(t, h, "run systemctl stop "+dm, "write /sys/bus/pci/drivers_probe")
	if h.Ran("run systemctl start " + dm) {
		t.Error("the desktop came back while the rental runs")
	}
	st, _ := LoadState(h, dataDir)
	if st == nil || st.StoppedDisplayManager != dm {
		t.Fatalf("stopped_display_manager not recorded: %+v", st)
	}

	if res := rt.Stop(); !res.Clean() {
		t.Fatalf("Stop = %+v", res)
	}
	// Back only once the GPU is back with its driver.
	before(t, h, "write /sys/bus/pci/drivers/vfio-pci/unbind", "run systemctl start "+dm)
	if h.Driver(testGPU) != "nvidia" {
		t.Error("GPU not given back")
	}
}

// A start that fails after the desktop closed -- here the GPU's audio
// function will not move to vfio-pci -- still brings the desktop back: the
// record is on disk before the display manager is stopped.
func TestSparkDesktopComesBackWhenTheStartFails(t *testing.T) {
	h := sparkHost()
	h.Fail["write /sys/bus/pci/devices/"+testAudio+"/driver_override"] = errors.New("permission denied")
	rt, _ := sparkRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err == nil {
		t.Fatal("Start succeeded although the GPU never moved")
	}
	before(t, h, "run systemctl stop "+dm, "run systemctl start "+dm)
	if h.Driver(testGPU) != "nvidia" || h.Driver(testAudio) != "snd_hda_intel" {
		t.Errorf("GPU group not given back: gpu=%s audio=%s", h.Driver(testGPU), h.Driver(testAudio))
	}
}

// An agent that died mid-rental finds the state on its next start; the
// teardown of a VM that is gone brings the desktop back too.
func TestSparkResumeTeardownStartsTheDesktop(t *testing.T) {
	h := sparkHost()
	rt, _ := sparkRuntime(h, func() bool { return true })
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatal(err)
	}
	h.SetFail("systemctl is-active --quiet gpu-rental-R1", errors.New("inactive")) // the machine rebooted
	again, _ := sparkRuntime(h, func() bool { return true })
	if again.Alive() {
		t.Fatal("VM still alive")
	}
	if res := again.Stop(); !res.Clean() {
		t.Fatalf("resume teardown = %+v", res)
	}
	if !h.Ran("run systemctl start " + dm) {
		t.Error("the desktop did not come back after the resume teardown")
	}
}

func TestSparkDesktopThatWillNotLetGoIsRefusedAndRestarted(t *testing.T) {
	h := sparkHost()
	delete(h.OnRun, "systemctl stop "+dm) // the desktop keeps the GPU open
	rt, _ := sparkRuntime(h, nil)
	err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)})
	if err == nil || !strings.Contains(err.Error(), "desktop is running on the GPU") || !strings.Contains(err.Error(), "Xorg (pid 2558)") {
		t.Fatalf("Start = %v, want the usual desktop refusal", err)
	}
	before(t, h, "run systemctl stop "+dm, "run systemctl start "+dm)
	if h.SleptFor() < DesktopReleaseTimeout {
		t.Errorf("gave up after %v, want %v", h.SleptFor(), DesktopReleaseTimeout)
	}
	if h.Driver(testGPU) != "nvidia" || h.Ran("run systemd-run") {
		t.Error("the GPU was taken or a VM booted")
	}
	if st, _ := LoadState(h, dataDir); st != nil {
		t.Errorf("state left behind: %+v", st)
	}
	if n := h.Count("run systemctl start " + dm); n != 1 {
		t.Errorf("display manager started %d times, want once", n)
	}
}

// Something besides the desktop holds the GPU: refused as on any machine, and
// the desktop is not touched for a rental that cannot start anyway.
func TestSparkDesktopWithAnotherHolderIsRefusedUntouched(t *testing.T) {
	h := sparkHost()
	h.Links["/proc/4242/fd/9"] = "/dev/nvidia0"
	h.Files["/proc/4242/comm"] = []byte("python3\n")
	rt, _ := sparkRuntime(h, nil)
	err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)})
	if err == nil || !strings.Contains(err.Error(), "python3 (pid 4242)") {
		t.Fatalf("Start = %v", err)
	}
	if h.Ran("run systemctl stop " + dm) {
		t.Error("the desktop was closed for a rental that could not start")
	}
}

// A desktop nobody's display manager runs (started by hand) cannot be closed
// by the agent: the usual refusal, nothing stopped.
func TestSparkDesktopWithoutADisplayManagerIsRefused(t *testing.T) {
	h := sparkHost()
	h.SetFail("systemctl is-active --quiet "+dm, errors.New("inactive"))
	rt, _ := sparkRuntime(h, nil)
	err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)})
	if err == nil || !strings.Contains(err.Error(), "runtime prepare --headless") {
		t.Fatalf("Start = %v", err)
	}
	if h.Ran("run systemctl stop " + dm) {
		t.Error("a display manager that was not running was stopped")
	}
}

// display-manager.service is an alias; when it is missing, gdm3 runs the
// desktop on Ubuntu and DGX OS.
func TestDisplayManagerFallsBackToGdm3(t *testing.T) {
	h := fakehost.New()
	h.Outputs["systemctl show -p LoadState --value "+dm] = "not-found\n"
	h.Outputs["systemctl show -p LoadState --value gdm3.service"] = "loaded\n"
	if got := DisplayManager(h); got != "gdm3.service" {
		t.Errorf("DisplayManager = %q", got)
	}
	h.Outputs["systemctl show -p LoadState --value "+dm] = "loaded\n"
	if got := DisplayManager(h); got != dm {
		t.Errorf("DisplayManager = %q", got)
	}
	if got := DisplayManager(fakehost.New()); got != "" {
		t.Errorf("DisplayManager with none installed = %q", got)
	}
}

// Everywhere else the v0.1.9 rule stands: refused with the headless fix, and
// the display manager is never touched.
func TestNonSparkDesktopIsStillRefused(t *testing.T) {
	h := sparkHost()
	rt, _ := newRuntime(h, nil)
	err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)})
	if err == nil || !strings.Contains(err.Error(), "runtime prepare --headless") {
		t.Fatalf("Start = %v", err)
	}
	if h.Ran("run systemctl stop "+dm) || h.Ran("run systemctl start "+dm) {
		t.Error("a non-Spark's display manager was touched")
	}
}
