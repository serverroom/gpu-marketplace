package update

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var newBinary = []byte("#!gpu-agent v0.1.11 for linux/amd64")

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// rig is an updater on a temporary directory, with GitHub, the new binary
// and systemd faked.
type rig struct {
	u        *Updater
	mu       sync.Mutex
	urls     []string
	commands []string
	files    map[string][]byte // what "GitHub" serves
	reports  string            // what the new binary prints for -version
	fail     map[string]error  // command prefix -> error
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "gpu-agent")
	if err := os.WriteFile(bin, []byte("the running v0.1.10"), 0755); err != nil {
		t.Fatal(err)
	}
	r := &rig{
		files: map[string][]byte{
			"https://github.com/serverroom/gpu-marketplace/releases/download/v0.1.11/gpu-agent-linux-amd64": newBinary,
			"https://github.com/serverroom/gpu-marketplace/releases/download/v0.1.11/checksums.txt": []byte(
				sum([]byte("other")) + "  gpu-agent-linux-arm64\n" + sum(newBinary) + "  gpu-agent-linux-amd64\n"),
		},
		reports: "gpu-agent v0.1.11\n",
		fail:    map[string]error{},
	}
	r.u = &Updater{
		GOOS: "linux", GOARCH: "amd64", Current: "v0.1.10", Binary: bin, DataDir: filepath.Join(dir, "data"),
		Get: func(url string) ([]byte, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.urls = append(r.urls, url)
			if b, ok := r.files[url]; ok {
				return b, nil
			}
			return nil, fmt.Errorf("GET %s: HTTP 404", url)
		},
		Exec: func(name string, args ...string) (string, error) {
			cmd := strings.TrimSpace(filepath.Base(name) + " " + strings.Join(args, " "))
			r.mu.Lock()
			r.commands = append(r.commands, cmd)
			r.mu.Unlock()
			for prefix, err := range r.fail {
				if strings.HasPrefix(cmd, prefix) {
					return "", err
				}
			}
			if strings.HasSuffix(name, ".new") && len(args) == 1 && args[0] == "-version" {
				return r.reports, nil
			}
			if name == bin && len(args) == 1 && args[0] == "-version" {
				data, _ := os.ReadFile(bin)
				if string(data) == string(newBinary) {
					return "gpu-agent v0.1.11\n", nil
				}
				return "gpu-agent v0.1.10\n", nil
			}
			return "", nil
		},
		Readlink: func(string) (string, error) { return bin, nil },
	}
	return r
}

func (r *rig) ran(prefix string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func code(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

func TestAnUpdateSwapsTheBinaryAndSchedulesRestartAndCheck(t *testing.T) {
	r := newRig(t)
	rec, err := r.u.Begin("v0.1.11")
	if err != nil || rec.Status != StatusDownloading || rec.From != "v0.1.10" {
		t.Fatalf("Begin = %+v, %v", rec, err)
	}
	if got := Load(r.u.DataDir); got == nil || got.Status != StatusDownloading {
		t.Fatalf("update.json after Begin = %+v", got)
	}
	rec = r.u.Run(rec)
	if rec.Status != StatusRestarting || rec.Error != "" {
		t.Fatalf("Run = %+v", rec)
	}
	if got := read(t, r.u.Binary); got != string(newBinary) {
		t.Errorf("binary = %q", got)
	}
	if got := read(t, r.u.Binary+".prev"); got != "the running v0.1.10" {
		t.Errorf(".prev = %q", got)
	}
	if _, err := os.Stat(r.u.Binary + ".new"); !os.IsNotExist(err) {
		t.Error("the .new file was left behind")
	}
	// The restart and the check are scheduled, never run here: the answer
	// to /update has long gone out when the agent restarts.
	if r.ran("systemctl restart") {
		t.Error("the agent was restarted directly, not scheduled")
	}
	if !r.ran("systemd-run --on-active=2 --timer-property=AccuracySec=1s --collect --unit=gpu-agent-update-restart-") ||
		!strings.Contains(strings.Join(r.commands, "\n"), "systemctl restart gpu-agent") {
		t.Errorf("no delayed restart scheduled: %v", r.commands)
	}
	if !r.ran("systemd-run --on-active=180 --timer-property=AccuracySec=1s --collect --unit=gpu-agent-update-check-") ||
		!strings.Contains(strings.Join(r.commands, "\n"), "gpu-agent.prev update-check --to v0.1.11") {
		t.Errorf("no rollback check scheduled with the old binary: %v", r.commands)
	}
	if got := Load(r.u.DataDir); got == nil || got.Status != StatusRestarting {
		t.Errorf("update.json = %+v", got)
	}
}

// The URL is the constant's, whatever the request: only a plain version goes
// into it.
func TestTheURLIsBuiltOnlyFromTheConstant(t *testing.T) {
	r := newRig(t)
	rec, _ := r.u.Begin("v0.1.11")
	r.u.Run(rec)
	want := []string{
		"https://github.com/serverroom/gpu-marketplace/releases/download/v0.1.11/checksums.txt",
		"https://github.com/serverroom/gpu-marketplace/releases/download/v0.1.11/gpu-agent-linux-amd64",
	}
	if strings.Join(r.urls, " ") != strings.Join(want, " ") {
		t.Errorf("fetched %v", r.urls)
	}
	for _, bad := range []string{"v0.1.11/../../evil", "https://evil.example/gpu-agent", "v0.1.11?x=1", "0.1.11", "v0.1", "v1.2.3-rc1", ""} {
		r := newRig(t)
		if _, err := r.u.Begin(bad); code(err) != http.StatusBadRequest {
			t.Errorf("%q: Begin = %v", bad, err)
		}
		if len(r.urls) != 0 {
			t.Errorf("%q: fetched %v", bad, r.urls)
		}
	}
}

func TestAChecksumMismatchIsRefusedAndNothingChanges(t *testing.T) {
	r := newRig(t)
	r.files["https://github.com/serverroom/gpu-marketplace/releases/download/v0.1.11/gpu-agent-linux-amd64"] = []byte("tampered")
	rec, _ := r.u.Begin("v0.1.11")
	rec = r.u.Run(rec)
	if rec.Status != StatusFailed || !strings.Contains(rec.Error, "does not match the published checksum") {
		t.Fatalf("Run = %+v", rec)
	}
	if read(t, r.u.Binary) != "the running v0.1.10" {
		t.Error("the running binary was replaced")
	}
	if _, err := os.Stat(r.u.Binary + ".prev"); !os.IsNotExist(err) {
		t.Error(".prev made for a refused update")
	}
	if r.ran("systemd-run") {
		t.Error("a restart was scheduled for a refused update")
	}
	if got := Load(r.u.DataDir); got == nil || got.Status != StatusFailed || got.FinishedAt == 0 {
		t.Errorf("update.json = %+v", got)
	}
}

func TestAnAssetMissingFromTheChecksumsIsRefused(t *testing.T) {
	r := newRig(t)
	r.u.GOARCH = "riscv64"
	rec, _ := r.u.Begin("v0.1.11")
	if rec = r.u.Run(rec); rec.Status != StatusFailed || !strings.Contains(rec.Error, "lists no gpu-agent-linux-riscv64") {
		t.Errorf("Run = %+v", rec)
	}
}

// The new binary must say it is the version asked for.
func TestABinaryReportingAnotherVersionIsRefused(t *testing.T) {
	r := newRig(t)
	r.reports = "gpu-agent v0.1.9\n"
	rec, _ := r.u.Begin("v0.1.11")
	rec = r.u.Run(rec)
	if rec.Status != StatusFailed || !strings.Contains(rec.Error, `reports "gpu-agent v0.1.9"`) {
		t.Fatalf("Run = %+v", rec)
	}
	if read(t, r.u.Binary) != "the running v0.1.10" || r.ran("systemd-run") {
		t.Error("an unverified binary was installed")
	}
}

func TestOnlyNewerReleasesOfAReleaseBuild(t *testing.T) {
	for name, c := range map[string]struct {
		current, to string
		code        int
	}{
		"older":           {"v0.1.10", "v0.1.9", http.StatusConflict},
		"equal":           {"v0.1.10", "v0.1.10", http.StatusConflict},
		"dev build":       {"dev", "v0.1.11", http.StatusConflict},
		"-dev build":      {"v0.1.10-dev", "v0.1.11", http.StatusConflict},
		"newer patch":     {"v0.1.10", "v0.1.11", 0},
		"newer minor":     {"v0.1.10", "v0.2.0", 0},
		"not numerically": {"v0.1.10", "v0.1.2", http.StatusConflict},
	} {
		r := newRig(t)
		r.u.Current = c.current
		if err := r.u.Check(c.to); code(err) != c.code {
			t.Errorf("%s: Check(%s) from %s = %v", name, c.to, c.current, err)
		}
	}
	r := newRig(t)
	r.u.Current = "dev"
	if err := r.u.Check("v0.1.11"); !strings.Contains(fmt.Sprint(err), "development build") {
		t.Errorf("dev: %v", err)
	}
}

func TestNotOffLinux(t *testing.T) {
	r := newRig(t)
	r.u.GOOS = "darwin"
	if _, err := r.u.Begin("v0.1.11"); code(err) != http.StatusNotImplemented {
		t.Errorf("Begin = %v", err)
	}
}

func TestOneUpdateAtATime(t *testing.T) {
	r := newRig(t)
	if _, err := r.u.Begin("v0.1.11"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.u.Begin("v0.1.11"); code(err) != http.StatusConflict {
		t.Errorf("second Begin in the same agent = %v", err)
	}
	// Another agent process (the CLI) sees update.json.
	other := &Updater{GOOS: r.u.GOOS, GOARCH: r.u.GOARCH, Current: r.u.Current, Binary: r.u.Binary, DataDir: r.u.DataDir, Get: r.u.Get, Exec: r.u.Exec}
	if _, err := other.Begin("v0.1.11"); code(err) != http.StatusConflict {
		t.Errorf("Begin beside a recorded update = %v", err)
	}
	// An update that never finished stops blocking after a while.
	other.Now = func() time.Time { return time.Now().Add(Stale + time.Minute) }
	if err := other.Check("v0.1.11"); err != nil {
		t.Errorf("a stale update still blocks: %v", err)
	}
}

// Start answers at once and does the work in the background.
func TestStartIsAsynchronous(t *testing.T) {
	r := newRig(t)
	gate := make(chan struct{})
	get := r.u.Get
	r.u.Get = func(url string) ([]byte, error) { <-gate; return get(url) }
	rec, err := r.u.Start("v0.1.11")
	if err != nil || rec.Status != StatusDownloading {
		t.Fatalf("Start = %+v, %v", rec, err)
	}
	close(gate)
	for i := 0; i < 200; i++ {
		if got := Load(r.u.DataDir); got != nil && got.Status == StatusRestarting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the background update never got to restarting: %+v", Load(r.u.DataDir))
}

func TestAFailedScheduleKeepsTheRunningVersion(t *testing.T) {
	r := newRig(t)
	r.fail["systemd-run --on-active=2"] = errors.New("Failed to start transient timer unit")
	rec, _ := r.u.Begin("v0.1.11")
	rec = r.u.Run(rec)
	if rec.Status != StatusFailed || read(t, r.u.Binary) != "the running v0.1.10" {
		t.Errorf("Run = %+v, binary %q", rec, read(t, r.u.Binary))
	}
}

// Three minutes later the old binary checks the new one.
func TestVerifyKeepsAGoodUpdate(t *testing.T) {
	r := newRig(t)
	rec, _ := r.u.Begin("v0.1.11")
	r.u.Run(rec)
	got := r.u.Verify("v0.1.11")
	if got.Status != StatusOK || got.From != "v0.1.10" || got.FinishedAt == 0 {
		t.Errorf("Verify = %+v", got)
	}
	if read(t, r.u.Binary) != string(newBinary) || r.ran("systemctl restart") {
		t.Error("a good update was undone")
	}
}

func TestVerifyRollsBackAnAgentThatIsNotRunning(t *testing.T) {
	r := newRig(t)
	rec, _ := r.u.Begin("v0.1.11")
	r.u.Run(rec)
	r.fail["systemctl is-active"] = errors.New("exit status 3")
	got := r.u.Verify("v0.1.11")
	if got.Status != StatusRolledBack || !strings.Contains(got.Error, "not running") {
		t.Errorf("Verify = %+v", got)
	}
	if read(t, r.u.Binary) != "the running v0.1.10" {
		t.Error("the previous binary is not back")
	}
	if !r.ran("systemctl restart gpu-agent") {
		t.Error("the agent was not restarted on the previous binary")
	}
	if l := Load(r.u.DataDir); l == nil || l.Status != StatusRolledBack {
		t.Errorf("update.json = %+v", l)
	}
}

func TestVerifyRollsBackAnAgentThatNeverRestarted(t *testing.T) {
	r := newRig(t)
	rec, _ := r.u.Begin("v0.1.11")
	r.u.Run(rec)
	r.u.Exec = func(name string, args ...string) (string, error) {
		cmd := filepath.Base(name) + " " + strings.Join(args, " ")
		r.commands = append(r.commands, cmd)
		switch {
		case strings.HasPrefix(cmd, "systemctl show -p MainPID"):
			return "4242\n", nil
		case name == r.u.Binary:
			return "gpu-agent v0.1.11\n", nil
		}
		return "", nil
	}
	r.u.Readlink = func(string) (string, error) { return r.u.Binary + " (deleted)", nil }
	if got := r.u.Verify("v0.1.11"); got.Status != StatusRolledBack || !strings.Contains(got.Error, "not restarted") {
		t.Errorf("Verify = %+v", got)
	}
}

func TestLatest(t *testing.T) {
	r := newRig(t)
	r.files[LatestURL] = []byte(`{"tag_name":"v0.1.12","name":"v0.1.12"}`)
	if v, err := r.u.Latest(); err != nil || v != "v0.1.12" {
		t.Errorf("Latest = %q, %v", v, err)
	}
	if LatestURL != "https://api.github.com/repos/serverroom/gpu-marketplace/releases/latest" {
		t.Errorf("LatestURL = %s", LatestURL)
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.1.11", "v0.1.10", true}, {"v0.1.10", "v0.1.9", true}, {"v1.0.0", "v0.99.99", true},
		{"v0.1.10", "v0.1.10", false}, {"v0.1.9", "v0.1.10", false}, {"dev", "v0.1.0", false}, {"v0.1.11", "dev", false},
	} {
		if Newer(c.a, c.b) != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.a, c.b, !c.want)
		}
	}
}
