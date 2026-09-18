package vmrt

import (
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// inCgroup puts pid in a cgroup (cgroup v2's unified line).
func inCgroup(h *fakehost.Host, pid, path string) {
	h.SetFile("/proc/"+pid+"/cgroup", []byte("0::"+path+"\n"))
}

// waylandLogin is user 1000's GNOME-on-Wayland login, session 2, and gdm's
// own login screen, session c1.
func waylandLogin(h *fakehost.Host) {
	h.Outputs["loginctl show-session 2 -p Type --value"] = "wayland\n"
	h.Outputs["loginctl show-session 2 -p Class --value"] = "user\n"
	h.Outputs["loginctl show-user 1000 -p Sessions --value"] = "2\n"
	h.Outputs["loginctl show-session c1 -p Type --value"] = "unspecified\n"
	h.Outputs["loginctl show-session c1 -p Class --value"] = "greeter\n"
	h.Outputs["loginctl show-session 7 -p Type --value"] = "tty\n" // an SSH login
	h.Outputs["loginctl show-session 7 -p Class --value"] = "user\n"
}

// browsers are a desktop's everyday GPU users: Chrome's GPU process started
// by GNOME as an app unit of the user's own systemd, and Firefox in the login
// session's scope.
func browsers(h *fakehost.Host) {
	holdGPU(h, "3100", "chrome")
	inCgroup(h, "3100", "/user.slice/user-1000.slice/user@1000.service/app.slice/app-gnome-google\\x2dchrome-3050.scope")
	holdGPU(h, "3200", "firefox")
	inCgroup(h, "3200", "/user.slice/user-1000.slice/session-2.scope")
}

// sparkInUse is a DGX Spark with its desktop up and a person at it: GNOME on
// Wayland, Chrome and Firefox open. Stopping the display manager ends the
// login, and with it every one of them.
func sparkInUse() *fakehost.Host {
	h := sparkHost()
	waylandLogin(h)
	browsers(h)
	stopDesktop := h.OnRun["systemctl stop "+dm]
	h.OnRun["systemctl stop "+dm] = func(h *fakehost.Host, cmd string) {
		stopDesktop(h, cmd)
		h.DeleteLink("/proc/3100/fd/3")
		h.DeleteLink("/proc/3200/fd/3")
	}
	return h
}

func TestBrowsersInAGraphicalLoginAreTheDesktop(t *testing.T) {
	h := newHost()
	waylandLogin(h)
	browsers(h)
	desktop, other := ClassifyGPUHolders(h)
	if strings.Join(desktop, ", ") != "chrome (pid 3100), firefox (pid 3200)" || len(other) != 0 {
		t.Errorf("desktop = %v, other = %v", desktop, other)
	}
}

func TestWhatIsAndIsNotTheDesktop(t *testing.T) {
	for name, c := range map[string]struct {
		cgroup  string
		parent  string
		desktop bool
	}{
		"a wayland login's scope":            {"/user.slice/user-1000.slice/session-2.scope", "", true},
		"an app unit of a graphical user":    {"/user.slice/user-1000.slice/user@1000.service/app.slice/app-org.gnome.Totem-4000.scope", "", true},
		"the login screen (greeter)":         {"/user.slice/user-120.slice/session-c1.scope", "", true},
		"an SSH login":                       {"/user.slice/user-1000.slice/session-7.scope", "", false},
		"a user with no graphical login":     {"/user.slice/user-1001.slice/user@1001.service/app.slice/jupyter.service", "", false},
		"a system service":                   {"/system.slice/ollama.service", "", false},
		"a container":                        {"/system.slice/docker-4f1c2a.scope", "", false},
		"started by the display manager":     {"/system.slice/lightdm.service", "850", true},
		"no cgroup, not the display manager": {"", "", false},
	} {
		h := newHost()
		waylandLogin(h)
		h.Outputs["systemctl show -p LoadState --value "+dm] = "loaded\n"
		h.Outputs["systemctl show -p MainPID --value "+dm] = "850\n"
		h.SetFile("/proc/901/status", []byte("PPid:\t850\n")) // a child of the display manager
		holdGPU(h, "4242", "worker")
		if c.cgroup != "" {
			inCgroup(h, "4242", c.cgroup)
		}
		if c.parent != "" {
			h.SetFile("/proc/4242/status", []byte("PPid:\t901\n"))
		}
		desktop, _ := ClassifyGPUHolders(h)
		if (len(desktop) == 1) != c.desktop {
			t.Errorf("%s: desktop = %v, want desktop=%v", name, desktop, c.desktop)
		}
	}
}

func TestCgroupV1PathIsRead(t *testing.T) {
	h := fakehost.New()
	h.SetFile("/proc/5/cgroup", []byte("12:memory:/user.slice\n1:name=systemd:/user.slice/user-1000.slice/session-2.scope\n"))
	if got := cgroupPath(h, "5"); got != "/user.slice/user-1000.slice/session-2.scope" {
		t.Errorf("cgroupPath = %q", got)
	}
	h.SetFile("/proc/6/cgroup", []byte("0::/system.slice/gpu-agent.service\n"))
	if got := cgroupPath(h, "6"); got != "/system.slice/gpu-agent.service" {
		t.Errorf("cgroupPath = %q", got)
	}
}

// The owner's case: a Spark used as a desktop, browsers open. It is rented
// (or test-booted) with no step by anybody: the desktop closes, taking the
// browsers with it, and comes back after.
func TestASparkWithBrowsersOpenIsRentedWithoutASingleStep(t *testing.T) {
	h := sparkInUse()
	rt, _ := sparkRuntime(h, func() bool { return true })
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start = %v; a Spark with browsers open must close its desktop for the rental", err)
	}
	before(t, h, "run systemctl stop "+dm, "write /sys/bus/pci/drivers_probe")
	if res := rt.Stop(); !res.Clean() {
		t.Fatalf("Stop = %+v", res)
	}
	before(t, h, "write /sys/bus/pci/drivers/vfio-pci/unbind", "run systemctl start "+dm)
}

// An nvidia-smi someone runs in a terminal on the Spark's desktop (a `watch
// nvidia-smi`) goes with the desktop: no refusal before the desktop closes.
func TestASparkWithNvidiaSmiRunningOnItsDesktop(t *testing.T) {
	h := sparkInUse()
	holdGPU(h, "3300", "nvidia-smi")
	inCgroup(h, "3300", "/user.slice/user-1000.slice/session-2.scope")
	stop := h.OnRun["systemctl stop "+dm]
	h.OnRun["systemctl stop "+dm] = func(h *fakehost.Host, cmd string) { stop(h, cmd); h.DeleteLink("/proc/3300/fd/3") }
	rt, _ := sparkRuntime(h, func() bool { return true })
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start = %v", err)
	}
}

// A holder outside any login -- a container's job -- is refused before the
// desktop is touched: the display manager keeps running.
func TestASparkWithAStrayHolderIsRefusedUntouched(t *testing.T) {
	h := sparkInUse()
	holdGPU(h, "5000", "python3")
	inCgroup(h, "5000", "/system.slice/docker-4f1c2a.scope")
	rt, _ := sparkRuntime(h, nil)
	err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)})
	if err == nil || !strings.Contains(err.Error(), "python3 (pid 5000)") {
		t.Fatalf("Start = %v", err)
	}
	if h.Ran("run systemctl stop " + dm) {
		t.Error("the desktop was closed for a rental that could not start")
	}
	if h.Run("systemctl", "is-active", "--quiet", dm) != nil {
		t.Error("the display manager is not running")
	}
}

// Something still holds the GPU after the desktop closed -- here a job that
// started outside any login while the desktop was closing: refused, the
// display manager started again, and the holder named.
func TestASparkRefusesWhatOutlivesTheDesktopAndRestoresIt(t *testing.T) {
	h := sparkInUse()
	stop := h.OnRun["systemctl stop "+dm]
	h.OnRun["systemctl stop "+dm] = func(h *fakehost.Host, cmd string) {
		stop(h, cmd)
		holdGPU(h, "6000", "python3")
		inCgroup(h, "6000", "/system.slice/batch-job.service")
	}
	rt, _ := sparkRuntime(h, nil)
	err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)})
	if err == nil || !strings.Contains(err.Error(), "still in use on this machine by python3 (pid 6000) after its desktop was closed") {
		t.Fatalf("Start = %v", err)
	}
	before(t, h, "run systemctl stop "+dm, "run systemctl start "+dm)
	if h.Run("systemctl", "is-active", "--quiet", dm) != nil {
		t.Error("the display manager was not restored")
	}
	if h.Driver(testGPU) != "nvidia" || h.Ran("run systemd-run") {
		t.Error("the GPU was taken or a VM booted")
	}
	if st, _ := LoadState(h, dataDir); st != nil {
		t.Errorf("state left behind: %+v", st)
	}
}

// A program of the login that does not exit with it (started with nohup,
// say) is named too.
func TestASparkNamesALoginProgramThatSurvivesTheLogout(t *testing.T) {
	h := sparkInUse()
	holdGPU(h, "3400", "jupyter-lab")
	inCgroup(h, "3400", "/user.slice/user-1000.slice/session-2.scope")
	rt, _ := sparkRuntime(h, nil)
	err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)})
	if err == nil || !strings.Contains(err.Error(), "jupyter-lab (pid 3400)") || strings.Contains(err.Error(), "chrome") {
		t.Fatalf("Start = %v", err)
	}
	if n := h.Count("run systemctl start " + dm); n != 1 {
		t.Errorf("display manager started %d times", n)
	}
}

// Anywhere else a desktop still refuses, with the headless fix -- and the
// message now names the desktop's programs, browsers included, as the desktop.
func TestANonSparkNamesTheBrowsersAsTheDesktop(t *testing.T) {
	h := sparkInUse()
	rt, _ := newRuntime(h, nil)
	err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)})
	if err == nil || !strings.Contains(err.Error(), "desktop is running on the GPU") ||
		!strings.Contains(err.Error(), "chrome (pid 3100)") || !strings.Contains(err.Error(), "runtime prepare --headless") {
		t.Fatalf("Start = %v", err)
	}
	if h.Ran("run systemctl stop " + dm) {
		t.Error("a non-Spark's display manager was touched")
	}
}

// An nvidia-smi in a terminal on a workstation whose desktop is not on this
// GPU is still just a short-lived tool, waited for -- not a desktop refusal.
func TestANvidiaSmiInALoginIsStillShortLived(t *testing.T) {
	h := newHost()
	waylandLogin(h)
	holdGPU(h, "3300", "nvidia-smi")
	inCgroup(h, "3300", "/user.slice/user-1000.slice/session-2.scope")
	h.OnSleep = func(h *fakehost.Host) { h.DeleteLink("/proc/3300/fd/3") }
	rt, _ := newRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start = %v", err)
	}
}
