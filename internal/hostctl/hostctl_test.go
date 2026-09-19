package hostctl

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/update"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

type fakeMachine struct {
	mu        sync.Mutex
	status    string
	settingUp bool
	present   bool
	withdrawn string
}

func (m *fakeMachine) Status() string      { m.mu.Lock(); defer m.mu.Unlock(); return m.status }
func (m *fakeMachine) SettingUp() bool     { return m.settingUp }
func (m *fakeMachine) RentalPresent() bool { return m.present }
func (m *fakeMachine) Withdraw(r string)   { m.mu.Lock(); m.withdrawn = r; m.mu.Unlock() }
func (m *fakeMachine) Withdrawn() bool     { m.mu.Lock(); defer m.mu.Unlock(); return m.withdrawn != "" }

var newBinary = []byte("gpu-agent v0.1.11")

// agent is a v0.1.10 agent on a free machine, whose updater talks to a fake
// GitHub (held until release is closed) and a fake systemd.
type agent struct {
	*Agent
	m        *fakeMachine
	h        *fakehost.Host
	release  chan struct{}
	mu       sync.Mutex
	commands []string
	marked   []string
	stops    int
}

func newAgent(t *testing.T) *agent {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "gpu-agent")
	if err := os.WriteFile(bin, []byte("gpu-agent v0.1.10"), 0755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(newBinary)
	files := map[string][]byte{
		"https://github.com/serverroom/gpu-marketplace/releases/download/v0.1.11/checksums.txt":         []byte(hex.EncodeToString(sum[:]) + "  gpu-agent-linux-amd64\n"),
		"https://github.com/serverroom/gpu-marketplace/releases/download/v0.1.11/gpu-agent-linux-amd64": newBinary,
	}
	a := &agent{m: &fakeMachine{status: provisioner.StatusFree}, h: fakehost.New(), release: make(chan struct{})}
	a.Agent = &Agent{
		Version: "v0.1.10", DataDir: filepath.Join(dir, "data"), PID: 4242, Host: a.h, Machine: a.m,
		WithdrawnMessage: "This machine was removed from the marketplace in the control panel.",
		MarkWithdrawn: func(by string) error {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.marked = append(a.marked, by)
			return nil
		},
		StopHosting: func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.stops++
		},
		Later: func(f func()) { f() },
		Updater: &update.Updater{
			GOOS: "linux", GOARCH: "amd64", Current: "v0.1.10", Binary: bin, DataDir: filepath.Join(dir, "data"),
			Get: func(url string) ([]byte, error) {
				<-a.release
				if b, ok := files[url]; ok {
					return b, nil
				}
				return nil, errors.New("HTTP 404")
			},
			Exec: func(name string, args ...string) (string, error) {
				a.mu.Lock()
				a.commands = append(a.commands, filepath.Base(name)+" "+strings.Join(args, " "))
				a.mu.Unlock()
				if strings.HasSuffix(name, ".new") {
					return "gpu-agent v0.1.11\n", nil
				}
				return "", nil
			},
		},
	}
	t.Cleanup(func() {
		select {
		case <-a.release:
		default:
			close(a.release)
		}
	})
	return a
}

func codeOf(err error) int {
	var se *control.StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	var coded interface{ HTTPStatus() int }
	if errors.As(err, &coded) {
		return coded.HTTPStatus()
	}
	return 0
}

func TestUpdateIsRefusedWhileTheMachineIsBusy(t *testing.T) {
	for name, busy := range map[string]func(a *agent){
		"rented":          func(a *agent) { a.m.status = provisioner.StatusRented },
		"provisioning":    func(a *agent) { a.m.status = provisioner.StatusProvisioning },
		"automatic setup": func(a *agent) { a.m.settingUp = true },
		"a leftover":      func(a *agent) { a.m.present = true },
		"removed":         func(a *agent) { a.m.withdrawn = "removed" },
		"a person's check --boot": func(a *agent) {
			a.h.Files["/proc/9999"] = nil
			a.h.Files[filepath.Join(a.DataDir, "busy.json")] = []byte(`{"pid":9999,"what":"running a test boot"}`)
		},
	} {
		a := newAgent(t)
		busy(a)
		if _, _, err := a.Update("v0.1.11"); codeOf(err) != http.StatusConflict {
			t.Errorf("%s: Update = %v", name, err)
		}
		if update.Load(a.DataDir) != nil {
			t.Errorf("%s: an update was started", name)
		}
	}
}

func TestUpdateRefusesWhatCanNeverBeUpdated(t *testing.T) {
	for name, c := range map[string]struct {
		mod  func(a *agent)
		to   string
		code int
	}{
		"a dev build":   {func(a *agent) { a.Updater.Current = "dev" }, "v0.1.11", http.StatusConflict},
		"an older one":  {func(*agent) {}, "v0.1.9", http.StatusConflict},
		"the same one":  {func(*agent) {}, "v0.1.10", http.StatusConflict},
		"not linux":     {func(a *agent) { a.Updater.GOOS = "windows" }, "v0.1.11", http.StatusNotImplemented},
		"not a version": {func(*agent) {}, "latest", http.StatusBadRequest},
	} {
		a := newAgent(t)
		c.mod(a)
		if _, _, err := a.Update(c.to); codeOf(err) != c.code {
			t.Errorf("%s: Update = %v, want %d", name, err, c.code)
		}
	}
}

// Through the control channel: 202 at once, while the download has not even
// started; the restart comes later, scheduled, never run in the handler.
func TestUpdateAnswers202BeforeAnythingRestarts(t *testing.T) {
	a := newAgent(t)
	srv := control.New("127.0.0.1:0", "secret", noRentals{})
	srv.SetHost(a.Agent)
	h := srv.Handler()
	req := httptest.NewRequest("POST", "/update", strings.NewReader(`{"version":"v0.1.11"}`))
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"from":"v0.1.10"`) || !strings.Contains(w.Body.String(), `"to":"v0.1.11"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if r := update.Load(a.DataDir); r == nil || r.Status != update.StatusDownloading {
		t.Fatalf("update.json at the answer = %+v", r)
	}
	a.mu.Lock()
	n := len(a.commands)
	a.mu.Unlock()
	if n != 0 {
		t.Errorf("commands ran before the answer: %v", a.commands)
	}

	close(a.release)
	for i := 0; i < 300; i++ {
		if r := update.Load(a.DataDir); r != nil && r.Status == update.StatusRestarting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r := update.Load(a.DataDir); r == nil || r.Status != update.StatusRestarting {
		t.Fatalf("update.json = %+v", r)
	}
	a.mu.Lock()
	all := strings.Join(a.commands, "\n")
	a.mu.Unlock()
	if !strings.Contains(all, "systemd-run --on-active=2 --timer-property=AccuracySec=1s --collect --unit=gpu-agent-update-restart-") ||
		!strings.Contains(all, "systemd-run --on-active=180 --timer-property=AccuracySec=1s --collect --unit=gpu-agent-update-check-") {
		t.Errorf("restart and check not scheduled: %s", all)
	}
	// A second request while it runs is refused.
	if _, _, err := a.Update("v0.1.11"); codeOf(err) != http.StatusConflict {
		t.Errorf("a second update = %v", err)
	}
	if rec, ok := a.UpdateRecord().(*update.Record); !ok || rec.To != "v0.1.11" {
		t.Errorf("UpdateRecord = %#v", a.UpdateRecord())
	}
}

func TestWithdrawStopsHosting(t *testing.T) {
	a := newAgent(t)
	if err := a.Withdraw(); err != nil {
		t.Fatal(err)
	}
	if !a.m.Withdrawn() || !strings.Contains(a.m.withdrawn, "removed from the marketplace") {
		t.Errorf("machine not withdrawn: %q", a.m.withdrawn)
	}
	if strings.Join(a.marked, ",") != "control panel" || a.stops != 1 {
		t.Errorf("marked %v, stops %d", a.marked, a.stops)
	}
	// Told twice (the panel retries, or a 410 follows): stopped once.
	if err := a.Withdraw(); err != nil {
		t.Fatal(err)
	}
	a.Gone()
	if a.stops != 1 {
		t.Errorf("stopped %d times", a.stops)
	}
}

func TestWithdrawIsRefusedWhileRented(t *testing.T) {
	a := newAgent(t)
	a.m.status = provisioner.StatusRented
	if err := a.Withdraw(); codeOf(err) != http.StatusConflict {
		t.Errorf("Withdraw = %v", err)
	}
	if a.m.Withdrawn() || len(a.marked) != 0 || a.stops != 0 {
		t.Error("a rented machine stopped hosting")
	}
	// A rental waiting for the host to free the machine holds it too.
	a.m.status = provisioner.StatusWaiting
	if err := a.Withdraw(); codeOf(err) != http.StatusConflict || !strings.Contains(err.Error(), "waits for it to be free") {
		t.Errorf("Withdraw while a rental waits = %v", err)
	}
	// A dirty leftover is not a rental: the host may still remove it.
	a.m.status = provisioner.StatusDirty
	if err := a.Withdraw(); err != nil {
		t.Errorf("Withdraw of a quarantined machine = %v", err)
	}
}

// A rental that waits for the host holds an update back.
func TestAWaitingRentalHoldsAnUpdateBack(t *testing.T) {
	a := newAgent(t)
	a.m.status = provisioner.StatusWaiting
	if _, _, err := a.Update("v0.1.11"); codeOf(err) != http.StatusConflict || !strings.Contains(err.Error(), "waiting for this machine") {
		t.Errorf("Update = %v", err)
	}
}

// A capability report answered 410 leads to the same state.
func TestGoneIsTheSameState(t *testing.T) {
	a := newAgent(t)
	a.Later = nil
	done := make(chan struct{})
	a.StopHosting = func() { close(done) }
	a.Gone()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hosting never stopped")
	}
	if !a.m.Withdrawn() || strings.Join(a.marked, ",") != "capability report" {
		t.Errorf("withdrawn=%v marked=%v", a.m.Withdrawn(), a.marked)
	}
}

func TestAWithdrawalThatCannotBeRecordedSaysSo(t *testing.T) {
	a := newAgent(t)
	a.MarkWithdrawn = func(string) error { return errors.New("read-only file system") }
	err := a.Withdraw()
	if codeOf(err) != http.StatusInternalServerError || !strings.Contains(err.Error(), "read-only file system") {
		t.Errorf("Withdraw = %v", err)
	}
	if !a.m.Withdrawn() || a.stops != 1 {
		t.Error("the machine kept hosting")
	}
}

type noRentals struct{}

func (noRentals) Provision(string, string) error { return errors.New("no") }
func (noRentals) Teardown(string) error          { return errors.New("no") }
func (noRentals) Status() string                 { return provisioner.StatusFree }
func (noRentals) Capability() control.Capability { return control.Capability{} }
