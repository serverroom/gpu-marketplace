// Package update replaces the agent's own binary with a newer release, from
// the control panel ("Update agent") or `gpu-agent update`, and puts the old
// one back by itself if the new one does not come up.
//
// Where the new binary comes from is never up to the caller: the URL is built
// from Repo and the version, the version must be a plain vX.Y.Z newer than the
// running one, and the download must match the release's checksums.txt -- the
// same rule the installers use -- and report the requested version when run.
package update

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Repo is where releases come from. Compiled in; nothing a request carries
// changes it.
const Repo = "serverroom/gpu-marketplace"

// ServiceUnit is the agent's systemd unit.
const ServiceUnit = "gpu-agent"

var (
	// RestartDelay lets the answer to /update leave through the tunnel before
	// the agent restarts; CheckDelay is when the old binary checks the new one.
	RestartDelay = 2 * time.Second
	CheckDelay   = 3 * time.Minute
	// Stale is how long an update that never finished blocks the next one.
	Stale = 10 * time.Minute
)

var versionPattern = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// Parse reads a release version, vX.Y.Z.
func Parse(v string) ([3]int, bool) {
	var out [3]int
	m := versionPattern.FindStringSubmatch(v)
	if m == nil {
		return out, false
	}
	for i := range out {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// Newer reports whether release a is newer than release b.
func Newer(a, b string) bool {
	x, ok1 := Parse(a)
	y, ok2 := Parse(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

// AssetName is the release file for a platform.
func AssetName(goos, goarch string) string {
	name := "gpu-agent-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

func releaseURL(version, file string) string {
	return "https://github.com/" + Repo + "/releases/download/" + version + "/" + file
}

// LatestURL is the GitHub API answer naming the latest release.
const LatestURL = "https://api.github.com/repos/" + Repo + "/releases/latest"

// Error carries the HTTP status the control channel answers a refusal with.
type Error struct {
	Code int
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// HTTPStatus is the status the control channel answers with.
func (e *Error) HTTPStatus() int { return e.Code }

func refuse(code int, format string, a ...interface{}) error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// Record is the last update, in update.json in the data directory, which
// `GET /status` shows the control plane as "update".
type Record struct {
	From       string `json:"from"`
	To         string `json:"to"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at,omitempty"`
	// Status moves downloading -> verifying -> restarting, and ends "ok" or
	// "rolled_back" (the check three minutes after the restart), or "failed"
	// (the running agent was never touched, or could not be put back).
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

const (
	StatusDownloading = "downloading"
	StatusVerifying   = "verifying"
	StatusRestarting  = "restarting"
	StatusOK          = "ok"
	StatusRolledBack  = "rolled_back"
	StatusFailed      = "failed"
)

// InProgress reports whether the update has not ended yet.
func (r Record) InProgress() bool {
	switch r.Status {
	case StatusDownloading, StatusVerifying, StatusRestarting:
		return true
	}
	return false
}

// Updater replaces the running binary. The zero value's functions are the
// real ones; tests replace them.
type Updater struct {
	GOOS, GOARCH string
	Current      string // the running version
	Binary       string // the running binary's path
	DataDir      string
	// Get fetches a URL's body.
	Get func(url string) ([]byte, error)
	// Exec runs a program and returns what it printed.
	Exec func(name string, args ...string) (string, error)
	// Readlink reads a symlink (a process's /proc/<pid>/exe).
	Readlink func(path string) (string, error)
	Now      func() time.Time

	mu      sync.Mutex
	running bool
}

func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *Updater) get(url string) ([]byte, error) {
	if u.Get != nil {
		return u.Get(url)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

func (u *Updater) exec(name string, args ...string) (string, error) {
	if u.Exec != nil {
		return u.Exec(name, args...)
	}
	cmd := exec.Command(name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.String(), err
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		return out.String(), fmt.Errorf("%s did not finish within 30 s", name)
	}
}

func (u *Updater) readlink(p string) (string, error) {
	if u.Readlink != nil {
		return u.Readlink(p)
	}
	return os.Readlink(p)
}

// Path is where the last update is recorded.
func Path(dataDir string) string { return filepath.Join(dataDir, "update.json") }

// Load reads the last update, or nil when there was none.
func Load(dataDir string) *Record {
	data, err := os.ReadFile(Path(dataDir))
	if err != nil {
		return nil
	}
	var r Record
	if json.Unmarshal(data, &r) != nil {
		return nil
	}
	return &r
}

func (u *Updater) save(r Record) error {
	if err := os.MkdirAll(u.DataDir, 0700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(r, "", "  ")
	tmp := Path(u.DataDir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	// A reader holding the file open (GET /status) makes a rename over it fail
	// on some systems for a moment; the progress must still land.
	var err error
	for i := 0; i < 20; i++ {
		if err = os.Rename(tmp, Path(u.DataDir)); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return err
}

// Check refuses what can never be updated from here: a platform without
// systemd, a development build, a version that is not vX.Y.Z or not newer,
// and an update while another is under way.
func (u *Updater) Check(to string) error {
	if u.GOOS != "linux" {
		return refuse(http.StatusNotImplemented, "updating from the panel works on Linux hosts")
	}
	if _, ok := Parse(to); !ok {
		return refuse(http.StatusBadRequest, "%q is not a release version (vX.Y.Z)", to)
	}
	if _, ok := Parse(u.Current); !ok {
		return refuse(http.StatusConflict, "this is a development build (%s); install a release to update it", u.Current)
	}
	if !Newer(to, u.Current) {
		return refuse(http.StatusConflict, "%s is not newer than the running %s", to, u.Current)
	}
	u.mu.Lock()
	running := u.running
	u.mu.Unlock()
	if r := Load(u.DataDir); running || (r != nil && r.InProgress() && u.now().Sub(time.Unix(r.StartedAt, 0)) < Stale) {
		to := "a new version"
		if r != nil {
			to = r.To
		}
		return refuse(http.StatusConflict, "an update to %s is already running", to)
	}
	return nil
}

func sumFor(sums, name string) string {
	for _, line := range strings.Split(sums, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && (f[1] == name || f[1] == "*"+name) {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

// Begin checks and records an update to `to` (status downloading) and claims
// the updater, so a second request is refused at once. Run does the rest.
func (u *Updater) Begin(to string) (Record, error) {
	if err := u.Check(to); err != nil {
		return Record{}, err
	}
	u.mu.Lock()
	if u.running {
		u.mu.Unlock()
		return Record{}, refuse(http.StatusConflict, "an update is already running")
	}
	u.running = true
	u.mu.Unlock()
	rec := Record{From: u.Current, To: to, StartedAt: u.now().Unix(), Status: StatusDownloading}
	if err := u.save(rec); err != nil {
		u.release()
		return Record{}, refuse(http.StatusInternalServerError, "record the update: %v", err)
	}
	return rec, nil
}

func (u *Updater) release() {
	u.mu.Lock()
	u.running = false
	u.mu.Unlock()
}

// Start is the control panel's update: Begin, and Run in the background. The
// answer to /update goes out at once; update.json says how it goes.
func (u *Updater) Start(to string) (Record, error) {
	rec, err := u.Begin(to)
	if err != nil {
		return rec, err
	}
	go u.Run(rec)
	return rec, nil
}

// Run downloads release rec.To for this platform, verifies it against the
// release's checksums.txt and by running it, keeps the running binary as
// <binary>.prev, moves the new one into place in one rename, and schedules the
// restart and the check. Progress goes to update.json. A failure before the
// rename leaves the running agent exactly as it was (status failed).
func (u *Updater) Run(rec Record) Record {
	defer u.release()
	step := func(status string) {
		rec.Status = status
		_ = u.save(rec)
	}
	fail := func(format string, a ...interface{}) Record {
		rec.Status, rec.FinishedAt = StatusFailed, u.now().Unix()
		rec.Error = fmt.Sprintf(format, a...)
		_ = u.save(rec)
		return rec
	}

	step(StatusDownloading)
	asset := AssetName(u.GOOS, u.GOARCH)
	sums, err := u.get(releaseURL(rec.To, "checksums.txt"))
	if err != nil {
		return fail("download checksums.txt for %s: %v", rec.To, err)
	}
	want := sumFor(string(sums), asset)
	if want == "" {
		return fail("checksums.txt for %s lists no %s", rec.To, asset)
	}
	bin, err := u.get(releaseURL(rec.To, asset))
	if err != nil {
		return fail("download %s %s: %v", asset, rec.To, err)
	}

	step(StatusVerifying)
	sum := sha256.Sum256(bin)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fail("the downloaded %s does not match the published checksum (expected %s, got %s); refusing it", asset, want, got)
	}
	next := u.Binary + ".new"
	_ = os.Remove(next)
	if err := os.WriteFile(next, bin, 0755); err != nil {
		return fail("write %s: %v", next, err)
	}
	_ = os.Chmod(next, 0755)
	out, err := u.exec(next, "-version")
	if got := strings.TrimSpace(out); err != nil || got != "gpu-agent "+rec.To {
		_ = os.Remove(next)
		return fail("the downloaded binary reports %q, not gpu-agent %s; refusing it", got, rec.To)
	}
	prev := u.Binary + ".prev"
	_ = os.Remove(prev)
	if err := os.Link(u.Binary, prev); err != nil {
		if err := copyFile(u.Binary, prev); err != nil {
			_ = os.Remove(next)
			return fail("keep the running binary as %s: %v", prev, err)
		}
	}
	if err := os.Rename(next, u.Binary); err != nil {
		_ = os.Remove(next)
		return fail("move the new binary into place: %v", err)
	}

	step(StatusRestarting)
	if err := u.schedule(rec); err != nil {
		return fail("schedule the restart: %v; the running version stays", err)
	}
	return rec
}

func copyFile(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, data, 0755)
}

// schedule has the old binary check the new one CheckDelay from now and
// restarts the agent RestartDelay from now, both as transient systemd units:
// they run whatever happens to this process. If either cannot be scheduled,
// the old binary goes back.
func (u *Updater) schedule(rec Record) error {
	stamp := strconv.FormatInt(u.now().Unix(), 10)
	check := []string{"--on-active=" + seconds(CheckDelay), "--collect", "--unit=gpu-agent-update-check-" + stamp,
		u.Binary + ".prev", "update-check", "--to", rec.To, "--binary", u.Binary}
	restart := []string{"--on-active=" + seconds(RestartDelay), "--collect", "--unit=gpu-agent-update-restart-" + stamp,
		"systemctl", "restart", ServiceUnit}
	if _, err := u.exec("systemd-run", check...); err != nil {
		_ = os.Rename(u.Binary+".prev", u.Binary)
		return err
	}
	if _, err := u.exec("systemd-run", restart...); err != nil {
		_ = os.Rename(u.Binary+".prev", u.Binary)
		_, _ = u.exec("systemctl", "stop", "gpu-agent-update-check-"+stamp+".timer")
		return err
	}
	return nil
}

func seconds(d time.Duration) string { return strconv.Itoa(int(d / time.Second)) }

// Verify is the check the old binary runs three minutes after an update
// (`gpu-agent update-check`): the service must be running, on the new binary,
// and the binary must report the new version. Otherwise the old binary goes
// back and the service restarts on it. The outcome goes to update.json.
func (u *Updater) Verify(to string) Record {
	rec := Record{To: to, Status: StatusRestarting}
	if r := Load(u.DataDir); r != nil && r.To == to {
		rec = *r
	}
	problem := ""
	if _, err := u.exec("systemctl", "is-active", "--quiet", ServiceUnit); err != nil {
		problem = "the agent was not running three minutes after the update"
	} else if out, _ := u.exec(u.Binary, "-version"); strings.TrimSpace(out) != "gpu-agent "+to {
		problem = fmt.Sprintf("the installed binary reports %q, not gpu-agent %s", strings.TrimSpace(out), to)
	} else if pid, _ := u.exec("systemctl", "show", "-p", "MainPID", "--value", ServiceUnit); strings.TrimSpace(pid) != "" && strings.TrimSpace(pid) != "0" {
		if exe, err := u.readlink("/proc/" + strings.TrimSpace(pid) + "/exe"); err == nil && strings.HasSuffix(exe, " (deleted)") {
			problem = "the agent was not restarted on the new binary"
		}
	}
	rec.FinishedAt = u.now().Unix()
	if problem == "" {
		rec.Status, rec.Error = StatusOK, ""
		_ = u.save(rec)
		return rec
	}
	rec.Status, rec.Error = StatusRolledBack, problem+"; the previous version is back"
	if err := os.Rename(u.Binary+".prev", u.Binary); err != nil {
		rec.Status, rec.Error = StatusFailed, problem+"; and the previous version could not be put back: "+err.Error()
	}
	_, _ = u.exec("systemctl", "restart", ServiceUnit)
	_ = u.save(rec)
	return rec
}

// Latest asks GitHub for the latest release's version.
func (u *Updater) Latest() (string, error) {
	body, err := u.get(LatestURL)
	if err != nil {
		return "", err
	}
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.TagName == "" {
		return "", errors.New("the latest release has no version")
	}
	return r.TagName, nil
}
