package vmrt

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/stats"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// holdGPU makes pid hold /dev/nvidia0 under name.
func holdGPU(h *fakehost.Host, pid, name string) {
	h.SetLink("/proc/"+pid+"/fd/3", "/dev/nvidia0")
	h.SetFile("/proc/"+pid+"/comm", []byte(name+"\n"))
}

// The customer Spark's refusal: "the GPU is in use on this machine by
// nvidia-smi (pid 66061)". An nvidia-smi that exits a moment later must not
// fail the start.
func TestATransientNvidiaSmiDoesNotBlockAStart(t *testing.T) {
	h := newHost()
	holdGPU(h, "66061", "nvidia-smi")
	h.OnSleep = func(h *fakehost.Host) { h.DeleteLink("/proc/66061/fd/3") } // gone by the second look
	rt, _ := newRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start = %v; a short-lived nvidia-smi must be waited for", err)
	}
	if got := h.SleptFor(); got < TransientPoll || got >= TransientWait {
		t.Errorf("waited %v; want one look after %v", got, TransientPoll)
	}
}

func TestANvidiaSmiThatStaysIsRefused(t *testing.T) {
	h := newHost()
	holdGPU(h, "66061", "nvidia-smi")
	rt, _ := newRuntime(h, nil)
	err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)})
	if err == nil || !strings.Contains(err.Error(), "the GPU is in use on this machine by nvidia-smi (pid 66061)") {
		t.Fatalf("Start = %v, want the usual refusal", err)
	}
	if h.SleptFor() < TransientWait {
		t.Errorf("gave up after %v, want %v", h.SleptFor(), TransientWait)
	}
	if h.Driver(testGPU) != "nvidia" || h.Ran("run systemd-run") {
		t.Error("the GPU was taken or a VM booted")
	}
}

// Whatever this agent itself started (its own query, whatever its name) is
// short-lived too.
func TestAChildOfTheAgentIsWaitedFor(t *testing.T) {
	h := newHost()
	holdGPU(h, "5001", "rocm-smi")
	h.SetFile("/proc/5001/status", []byte(fmt.Sprintf("Name:\trocm-smi\nPPid:\t%d\n", os.Getpid())))
	h.OnSleep = func(h *fakehost.Host) { h.DeleteLink("/proc/5001/fd/3") }
	rt, _ := newRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start = %v", err)
	}
}

// Anything else is refused on sight, exactly as before: no waiting.
func TestANonTransientHolderIsRefusedAtOnce(t *testing.T) {
	h := newHost()
	holdGPU(h, "4242", "python3")
	holdGPU(h, "66061", "nvidia-smi")
	rt, _ := newRuntime(h, nil)
	err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)})
	if err == nil || !strings.Contains(err.Error(), "python3 (pid 4242)") {
		t.Fatalf("Start = %v", err)
	}
	if h.SleptFor() != 0 {
		t.Errorf("waited %v for a holder that is not short-lived", h.SleptFor())
	}
}

func TestIsTransientTool(t *testing.T) {
	for _, name := range []string{"nvidia-smi", "nvidia-debugdump", "nvidia-bug-report.sh", "nvidia-bug-repo", "dcgmi"} {
		if !IsTransientTool(name) {
			t.Errorf("%q not taken for a short-lived tool", name)
		}
	}
	for _, name := range []string{"python3", "nvidia-smi-exporter", "dcgm-exporter", "Xorg"} {
		if IsTransientTool(name) {
			t.Errorf("%q wrongly taken for a short-lived tool", name)
		}
	}
}

// The agent's own GPU queries are paused while the GPU is taken, and resumed
// afterwards -- whether the start succeeds or fails.
func TestStatsArePausedWhileTheGPUIsTaken(t *testing.T) {
	for _, fail := range []bool{false, true} {
		h := newHost()
		pausedDuringTake := false
		h.OnRun["modprobe vfio-pci"] = func(*fakehost.Host, string) { pausedDuringTake = stats.GPUQueriesPaused() }
		if fail {
			h.Fail["systemd-run"] = errors.New("qemu: could not open vfio device")
		}
		rt, _ := newRuntime(h, nil)
		err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)})
		if (err != nil) != fail {
			t.Fatalf("fail=%v: Start = %v", fail, err)
		}
		if !pausedDuringTake {
			t.Errorf("fail=%v: GPU queries ran while the GPU was being taken", fail)
		}
		if stats.GPUQueriesPaused() {
			t.Errorf("fail=%v: GPU queries still paused after the start", fail)
		}
		rt.Stop()
	}
}

func TestStatsAreResumedAfterARefusedStart(t *testing.T) {
	h := newHost()
	holdGPU(h, "4242", "python3")
	rt, _ := newRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err == nil {
		t.Fatal("Start succeeded")
	}
	if stats.GPUQueriesPaused() {
		t.Error("GPU queries still paused after a refused start")
	}
}
