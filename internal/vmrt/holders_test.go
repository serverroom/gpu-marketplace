package vmrt

import (
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// The exact report from a DGX Spark with its desktop on: Xorg, the GNOME shell,
// mutter and nvidia-persistenced (comm cuts it to "nvidia-persiste").
func sparkDesktopHost() *fakehost.Host {
	h := newHost()
	for _, p := range []struct{ pid, comm string }{
		{"2558", "Xorg"}, {"2872", "gnome-shell"}, {"2909", "mutter-x11-fram"}, {"2411", "nvidia-persiste"},
	} {
		h.Links["/proc/"+p.pid+"/fd/7"] = "/dev/nvidia0"
		h.Files["/proc/"+p.pid+"/comm"] = []byte(p.comm + "\n")
	}
	return h
}

func TestClassifyGPUHoldersOnADesktopSpark(t *testing.T) {
	desktop, other := ClassifyGPUHolders(sparkDesktopHost(), []string{testGPU})
	want := "Xorg (pid 2558), gnome-shell (pid 2872), mutter-x11-fram (pid 2909)"
	if strings.Join(desktop, ", ") != want {
		t.Errorf("desktop = %v, want %s", desktop, want)
	}
	if len(other) != 0 {
		t.Errorf("other = %v; nvidia-persistenced (comm cut to 15 characters) must not count", other)
	}
}

func TestProcessNamePrefersTheExecutable(t *testing.T) {
	h := newHost()
	h.Links["/proc/77/fd/3"] = "/dev/nvidia0"
	h.Files["/proc/77/comm"] = []byte("nvidia-persiste\n")
	h.Links["/proc/77/exe"] = "/usr/bin/nvidia-persistenced"
	h.Links["/proc/78/fd/3"] = "/dev/nvidiactl"
	h.Files["/proc/78/comm"] = []byte("python3\n")
	h.Links["/proc/78/exe"] = "/usr/bin/python3.12"
	desktop, other := ClassifyGPUHolders(h, []string{testGPU})
	if len(desktop) != 0 || strings.Join(other, ",") != "python3.12 (pid 78)" {
		t.Errorf("desktop = %v, other = %v", desktop, other)
	}
}

func TestIsNVIDIAServiceAllowsForCommTruncation(t *testing.T) {
	for _, name := range []string{"nvidia-persistenced", "nvidia-persiste", "nvidia-powerd", "nv-hostengine"} {
		if !IsNVIDIAService(name) {
			t.Errorf("%q not recognised as an NVIDIA service", name)
		}
	}
	for _, name := range []string{"nvidia-persist", "nvidia", "python3", "nvidia-persistencedX"} {
		if IsNVIDIAService(name) {
			t.Errorf("%q wrongly taken for an NVIDIA service", name)
		}
	}
}

func TestADesktopOnTheGPUIsRefusedWithTheFix(t *testing.T) {
	h := sparkDesktopHost()
	rt, _ := newRuntime(h, nil)
	err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)})
	if err == nil {
		t.Fatal("Start succeeded with a desktop on the GPU")
	}
	for _, want := range []string{"desktop is running on the GPU", "Xorg (pid 2558)", "runtime prepare --headless"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "persiste") {
		t.Errorf("refusal %q names nvidia-persistenced, which the runtime stops itself", err)
	}
	if h.Driver(testGPU) != "nvidia" || h.Ran("run systemd-run") {
		t.Errorf("the GPU was taken or a VM booted")
	}
}

func TestPersistencedHoldingTheGPUDoesNotBlockAStart(t *testing.T) {
	h := newHost()
	h.Links["/proc/2411/fd/7"] = "/dev/nvidia0"
	h.Files["/proc/2411/comm"] = []byte("nvidia-persiste\n")
	h.SetFail("systemctl is-active --quiet nvidia-persistenced", nil)
	rt, _ := newRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start = %v; nvidia-persistenced must be stopped, not refused", err)
	}
	if !h.Ran("run systemctl stop nvidia-persistenced") {
		t.Error("nvidia-persistenced was not stopped")
	}
}

func TestEveryRunningNVIDIAServiceIsStoppedAndRestarted(t *testing.T) {
	h := newHost()
	h.SetFail("systemctl is-active --quiet nvidia-powerd", nil)
	h.SetFail("systemctl is-active --quiet nvidia-dcgm", nil)
	rt, _ := newRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatal(err)
	}
	for _, unit := range []string{"nvidia-powerd", "nvidia-dcgm"} {
		if !h.Ran("run systemctl stop " + unit) {
			t.Errorf("%s was not stopped", unit)
		}
	}
	if h.Ran("run systemctl stop nvidia-persistenced") {
		t.Error("an inactive nvidia-persistenced was stopped")
	}
	rt.Stop()
	for _, unit := range []string{"nvidia-powerd", "nvidia-dcgm"} {
		if !h.Ran("run systemctl start " + unit) {
			t.Errorf("%s was not restarted", unit)
		}
	}
	if h.Ran("run systemctl start nvidia-persistenced") {
		t.Error("nvidia-persistenced was started although the rental never stopped it")
	}
}

func TestServicesToRestartReadsWhatAnOlderAgentRecorded(t *testing.T) {
	st := State{StoppedPersistenced: true}
	if got := strings.Join(st.ServicesToRestart(), ","); got != "nvidia-persistenced" {
		t.Errorf("legacy state restarts %q", got)
	}
	st = State{StoppedPersistenced: true, StoppedServices: []string{"nvidia-persistenced", "nvidia-powerd"}}
	if got := strings.Join(st.ServicesToRestart(), ","); got != "nvidia-persistenced,nvidia-powerd" {
		t.Errorf("state restarts %q", got)
	}
}
