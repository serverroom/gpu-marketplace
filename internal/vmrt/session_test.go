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

// sparkLoggedIn is a DGX Spark with its desktop up and a person logged in to
// it, nothing of theirs on the GPU: only the desktop's own programs hold it.
func sparkLoggedIn() *fakehost.Host {
	h := sparkHost()
	waylandLogin(h)
	return h
}

// sparkInUse is sparkLoggedIn with Chrome and Firefox open: the host's own
// programs, which a rental waits for.
func sparkInUse() *fakehost.Host {
	h := sparkLoggedIn()
	browsers(h)
	return h
}

// The owner's rule (v0.2.3): what a person started is theirs, in a graphical
// login or not -- a browser's GPU process is the host's use, not the desktop.
func TestBrowsersInAGraphicalLoginAreTheHostsUse(t *testing.T) {
	h := newHost()
	waylandLogin(h)
	browsers(h)
	desktop, other := ClassifyGPUHolders(h, []string{testGPU})
	if len(desktop) != 0 || strings.Join(other, ", ") != "chrome (pid 3100), firefox (pid 3200)" {
		t.Errorf("desktop = %v, other = %v", desktop, other)
	}
	if u := ReadHostUse(h, testSpec()); strings.Join(u.Holders, ", ") != "chrome (pid 3100), firefox (pid 3200)" {
		t.Errorf("host use = %+v", u)
	}
}

func TestWhatIsAndIsNotTheDesktop(t *testing.T) {
	for name, c := range map[string]struct {
		cgroup  string
		parent  string
		desktop bool
	}{
		"a wayland login's scope":            {"/user.slice/user-1000.slice/session-2.scope", "", false},
		"an app unit of a graphical user":    {"/user.slice/user-1000.slice/user@1000.service/app.slice/app-org.gnome.Totem-4000.scope", "", false},
		"a person's login started by the DM": {"/user.slice/user-1000.slice/session-2.scope", "850", false},
		"the login screen (greeter)":         {"/user.slice/user-120.slice/session-c1.scope", "", true},
		"a unit of the login screen's user":  {"/user.slice/user-120.slice/user@120.service/app.slice/xdg-desktop-portal.service", "", true},
		"an SSH login":                       {"/user.slice/user-1000.slice/session-7.scope", "", false},
		"a user with no graphical login":     {"/user.slice/user-1001.slice/user@1001.service/app.slice/jupyter.service", "", false},
		"a system service":                   {"/system.slice/ollama.service", "", false},
		"a container":                        {"/system.slice/docker-4f1c2a.scope", "", false},
		"started by the display manager":     {"/system.slice/lightdm.service", "850", true},
		"no cgroup, not the display manager": {"", "", false},
	} {
		h := newHost()
		waylandLogin(h)
		h.Outputs["loginctl show-user 120 -p Sessions --value"] = "c1\n"
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
		desktop, _ := ClassifyGPUHolders(h, []string{testGPU})
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

// A Spark someone is logged in to, with nothing of theirs on the GPU, is
// rented by closing the desktop (the agent warns before -- the provisioner's
// part), which comes back after.
func TestASparkWithOnlyItsDesktopIsRentedByClosingIt(t *testing.T) {
	h := sparkLoggedIn()
	rt, _ := sparkRuntime(h, func([]BoundDevice) bool { return true })
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start = %v", err)
	}
	before(t, h, "run systemctl stop "+dm, "write /sys/bus/pci/drivers_probe")
	if res := rt.Stop(); !res.Clean() {
		t.Fatalf("Stop = %+v", res)
	}
	before(t, h, "write /sys/bus/pci/drivers/vfio-pci/unbind", "run systemctl start "+dm)
}

// Browsers open on a Spark are the host's use: the start is refused (a
// rental waits for them) before the desktop is touched.
func TestASparksBrowsersKeepItsDesktopUp(t *testing.T) {
	h := sparkInUse()
	rt, _ := sparkRuntime(h, nil)
	err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)})
	if err == nil || !strings.Contains(err.Error(), "in use on this machine by chrome (pid 3100), firefox (pid 3200)") {
		t.Fatalf("Start = %v", err)
	}
	if h.Ran("run systemctl stop " + dm) {
		t.Error("the desktop was closed over the host's browsers")
	}
}

// An nvidia-smi someone runs in a terminal on the Spark's desktop (a `watch
// nvidia-smi`) is short-lived: it does not keep the desktop up, and goes when
// the desktop closes.
func TestASparkWithNvidiaSmiRunningOnItsDesktop(t *testing.T) {
	h := sparkLoggedIn()
	holdGPU(h, "3300", "nvidia-smi")
	inCgroup(h, "3300", "/user.slice/user-1000.slice/session-2.scope")
	stop := h.OnRun["systemctl stop "+dm]
	h.OnRun["systemctl stop "+dm] = func(h *fakehost.Host, cmd string) { stop(h, cmd); h.DeleteLink("/proc/3300/fd/3") }
	rt, _ := sparkRuntime(h, func([]BoundDevice) bool { return true })
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
	h := sparkLoggedIn()
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

// A program typed in a terminal on the Spark's desktop (a jupyter-lab, a
// llama-server) is the host's own use: named, and the desktop left alone.
func TestASparkNamesALoginProgramAsTheHostsUse(t *testing.T) {
	h := sparkLoggedIn()
	holdGPU(h, "3400", "jupyter-lab")
	inCgroup(h, "3400", "/user.slice/user-1000.slice/session-2.scope")
	rt, _ := sparkRuntime(h, nil)
	err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)})
	if err == nil || !strings.Contains(err.Error(), "in use on this machine by jupyter-lab (pid 3400)") {
		t.Fatalf("Start = %v", err)
	}
	if h.Ran("run systemctl stop " + dm) {
		t.Error("the desktop was closed")
	}
}

// Anywhere else a desktop still refuses, with the headless fix, naming only
// the desktop's own programs -- the host's browsers are its own use.
func TestANonSparkNamesTheDesktopNotTheBrowsers(t *testing.T) {
	h := sparkInUse()
	delete(h.Links, "/proc/3100/fd/3")
	delete(h.Links, "/proc/3200/fd/3")
	rt, _ := newRuntime(h, nil)
	err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)})
	if err == nil || !strings.Contains(err.Error(), "desktop is running on the GPU (Xorg (pid 2558), gnome-shell (pid 2872), mutter-x11-fram (pid 2909))") ||
		!strings.Contains(err.Error(), "runtime prepare --headless") {
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
