package update

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/update/updatetest"
)

// fakeService is the Windows service manager for the tests.
type fakeService struct {
	running bool
	pid     int
	image   string
	calls   []string
	spawn   error
	spawned []string
}

func (f *fakeService) State() (bool, int, error) {
	f.calls = append(f.calls, "state")
	return f.running, f.pid, nil
}
func (f *fakeService) Stop() error               { f.calls = append(f.calls, "stop"); f.running = false; return nil }
func (f *fakeService) Start() error              { f.calls = append(f.calls, "start"); f.running = true; return nil }
func (f *fakeService) Image(int) (string, error) { return f.image, nil }
func (f *fakeService) Spawn(name string, args ...string) error {
	f.spawned = append([]string{name}, args...)
	return f.spawn
}

func windowsRig(t *testing.T) (*rig, *fakeService) {
	r := newRig(t)
	r.u.GOOS = "windows"
	r.files["https://github.com/serverroom/gpu-marketplace/releases/download/v0.1.11/gpu-agent-windows-amd64.exe"] = newBinary
	r.files["https://github.com/serverroom/gpu-marketplace/releases/download/v0.1.11/release.json"] = updatetest.Release("v0.1.11", map[string][]byte{"gpu-agent-windows-amd64.exe": newBinary})
	f := &fakeService{running: true, pid: 77, image: r.u.Binary}
	r.u.Service = f
	r.u.Sleep = func(time.Duration) {}
	return r, f
}

func TestWindowsUpdateRenamesAndSpawnsTheRestarter(t *testing.T) {
	r, f := windowsRig(t)
	rec, err := r.u.Begin("v0.1.11")
	if err != nil {
		t.Fatal(err)
	}
	if rec = r.u.Run(rec); rec.Status != StatusRestarting {
		t.Fatalf("Run = %+v", rec)
	}
	if read(t, r.u.Binary) != string(newBinary) || read(t, r.u.Binary+".prev") != "the running v0.1.10" {
		t.Error("the binaries were not swapped")
	}
	want := []string{r.u.Binary + ".prev", "update-restart", "--to", "v0.1.11", "--binary", r.u.Binary}
	if strings.Join(f.spawned, " ") != strings.Join(want, " ") {
		t.Errorf("spawned %v, want %v", f.spawned, want)
	}
	if r.ran("systemd-run") || r.ran("systemctl") {
		t.Errorf("systemd used on Windows: %v", r.commands)
	}
}

func TestWindowsAFailedSpawnPutsTheOldBinaryBack(t *testing.T) {
	r, f := windowsRig(t)
	f.spawn = errors.New("access denied")
	rec, _ := r.u.Begin("v0.1.11")
	if rec = r.u.Run(rec); rec.Status != StatusFailed {
		t.Fatalf("Run = %+v", rec)
	}
	if read(t, r.u.Binary) != "the running v0.1.10" {
		t.Error("the previous binary is not back")
	}
}

func TestWindowsRestartAndVerifyKeepsAGoodUpdate(t *testing.T) {
	r, f := windowsRig(t)
	rec, _ := r.u.Begin("v0.1.11")
	r.u.Run(rec)
	got := r.u.RestartAndVerify("v0.1.11")
	if got.Status != StatusOK {
		t.Errorf("RestartAndVerify = %+v", got)
	}
	if strings.Join(f.calls, ",") != "stop,start,state" {
		t.Errorf("service calls = %v", f.calls)
	}
}

func TestWindowsVerifyRollsBackAnAgentNotOnTheNewBinary(t *testing.T) {
	r, f := windowsRig(t)
	rec, _ := r.u.Begin("v0.1.11")
	r.u.Run(rec)
	f.image = r.u.Binary + ".prev" // still the old process
	got := r.u.Verify("v0.1.11")
	if got.Status != StatusRolledBack || !strings.Contains(got.Error, "not restarted") {
		t.Errorf("Verify = %+v", got)
	}
	if read(t, r.u.Binary) != "the running v0.1.10" {
		t.Error("the previous binary is not back")
	}
	if _, err := os.Stat(r.u.Binary + ".rejected"); err != nil {
		t.Error("the rejected binary was not kept aside")
	}
	if !f.running {
		t.Error("the service was not started again on the previous binary")
	}
}

func TestWindowsVerifyRollsBackAStoppedAgent(t *testing.T) {
	r, f := windowsRig(t)
	rec, _ := r.u.Begin("v0.1.11")
	r.u.Run(rec)
	f.running = false
	if got := r.u.Verify("v0.1.11"); got.Status != StatusRolledBack || !strings.Contains(got.Error, "not running") {
		t.Errorf("Verify = %+v", got)
	}
}
