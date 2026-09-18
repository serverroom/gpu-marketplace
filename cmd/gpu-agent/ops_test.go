package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/hostctl"
	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/update"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// marketplace is a fake capability endpoint: it records each report's
// capability and answers with the next code and body.
type marketplace struct {
	mu      sync.Mutex
	reports []map[string]json.RawMessage
	code    int
	answer  string
}

func (m *marketplace) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&body)
	body = openReport(body)
	m.mu.Lock()
	defer m.mu.Unlock()
	var c map[string]json.RawMessage
	_ = json.Unmarshal(body["capability"], &c)
	m.reports = append(m.reports, c)
	if m.code != 0 && m.code != 200 {
		w.WriteHeader(m.code)
		io.WriteString(w, "no")
		return
	}
	io.WriteString(w, m.answer)
}

func (m *marketplace) last(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.reports) == 0 {
		t.Fatal("nothing reported")
	}
	return m.reports[len(m.reports)-1]
}

// opsAgent is a registered agent (v0.2.0) on a free machine, reporting to m.
func opsAgent(t *testing.T, m *marketplace) *gpuAgent {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(config.ConfigDirEnv, dir)
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	reg, _ := json.Marshal(map[string]string{"listing_id": "L-42", "capability_url": srv.URL + "/api/marketplace/capability"})
	if err := os.WriteFile(filepath.Join(dir, "registration.json"), reg, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "control.token"), []byte("tok"), 0600); err != nil {
		t.Fatal(err)
	}
	a := &gpuAgent{ops: newOps(), prov: provisioner.New(nil, provisioner.VendorNone, nil, false)}
	bin := filepath.Join(dir, "gpu-agent")
	_ = os.WriteFile(bin, []byte("old"), 0755)
	a.host = &hostctl.Agent{
		Version: "v0.2.0", DataDir: config.DataDir(), PID: os.Getpid(), Host: fakehost.New(), Machine: a.prov,
		ConfigDir: config.ConfigDir(),
		Updater: &update.Updater{GOOS: "linux", GOARCH: "amd64", Current: "v0.2.0", Binary: bin, DataDir: config.DataDir(),
			Get: func(string) ([]byte, error) { return nil, io.ErrUnexpectedEOF }},
	}
	t.Cleanup(func() {
		// An update the test started ends before its files go.
		for i := 0; i < 1000; i++ {
			if r := update.Load(config.DataDir()); r == nil || !r.InProgress() {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
	})
	return a
}

func errorsOf(t *testing.T, c map[string]json.RawMessage) []control.AgentError {
	t.Helper()
	var errs []control.AgentError
	if raw, ok := c["errors"]; ok {
		if err := json.Unmarshal(raw, &errs); err != nil {
			t.Fatal(err)
		}
	}
	return errs
}

// Every report carries the problems and auto_update; a failed report is a
// problem until one gets through, which carries it resolved.
func TestReportsCarryTheProblems(t *testing.T) {
	m := &marketplace{answer: `{"status":"ok"}`}
	a := opsAgent(t, m)
	a.ops.errs.Raise(control.AreaTunnel, "the relay cannot be reached", "timed out")

	m.code = 502
	if _, err := a.report(control.Capability{Ready: true}); err == nil {
		t.Fatal("a 502 was a success")
	}
	m.code = 200
	if _, err := a.report(control.Capability{Ready: true}); err != nil {
		t.Fatal(err)
	}
	c := m.last(t)
	errs := errorsOf(t, c)
	areas := map[string]bool{}
	for _, e := range errs {
		areas[e.Area] = e.Active
	}
	if active, ok := areas[control.AreaReport]; !ok || !active {
		t.Errorf("the failed report is not in the next one: %+v", errs)
	}
	if !areas[control.AreaTunnel] {
		t.Errorf("the tunnel problem is missing: %+v", errs)
	}
	var auto control.AutoUpdate
	if err := json.Unmarshal(c["auto_update"], &auto); err != nil || !auto.Enabled {
		t.Errorf("auto_update = %s", c["auto_update"])
	}
	// Once through, the report problem is resolved.
	for _, e := range a.ops.errs.Entries() {
		if e.Area == control.AreaReport && e.Active {
			t.Errorf("still active after a report got through: %+v", e)
		}
	}
}

// The answer's agent_update is acted on: a push updates even with automatic
// updates paused; a malformed one is ignored.
func TestTheAnswerOffersTheUpdate(t *testing.T) {
	m := &marketplace{answer: `{"status":"ok","agent_update":{"version":"v0.2.1; rm -rf /","push":true}}`}
	a := opsAgent(t, m)
	if err := hostctl.SetAutoUpdate(config.ConfigDir(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.report(control.Capability{Ready: true}); err != nil {
		t.Fatal(err)
	}
	if update.Load(config.DataDir()) != nil {
		t.Fatal("a malformed offer started an update")
	}
	m.answer = `{"status":"ok","agent_update":{"version":"v0.2.1","push":true}}`
	if _, err := a.report(control.Capability{Ready: true}); err != nil {
		t.Fatal(err)
	}
	if r := update.Load(config.DataDir()); r == nil || r.To != "v0.2.1" {
		t.Fatalf("the pushed update did not start: %+v", r)
	}
	if s := a.host.AutoUpdateStatus(); s.LastCheck == 0 || s.Enabled {
		t.Errorf("auto_update = %+v", s)
	}
}

// openReport opens a report's base64 envelope the way the marketplace does,
// keeping the outer listing_id; a clear body is returned as it is.
func openReport(body map[string]json.RawMessage) map[string]json.RawMessage {
	var wrapped string
	if json.Unmarshal(body["report_b64"], &wrapped) != nil {
		return body
	}
	raw, err := base64.StdEncoding.DecodeString(wrapped)
	if err != nil {
		return body
	}
	var inner map[string]json.RawMessage
	if json.Unmarshal(raw, &inner) != nil {
		return body
	}
	inner["listing_id"] = body["listing_id"]
	return inner
}

// A 403 comes from a firewall on the way, not from the marketplace: it is a
// report problem the agent keeps retrying, never "register again", and the
// firewall's HTML page is not kept (it would get every later report refused).
func TestAFirewall403IsNotARegistrationProblem(t *testing.T) {
	m := &marketplace{code: 403}
	a := opsAgent(t, m)
	_, _ = a.report(control.Capability{})
	var report bool
	for _, e := range a.ops.errs.Entries() {
		if e.Area == control.AreaRegister {
			t.Errorf("a 403 was read as a refused registration: %+v", e)
		}
		if e.Area == control.AreaReport && strings.Contains(e.Message, "firewall") {
			report = true
			if strings.Contains(e.Detail, "<") {
				t.Errorf("markup kept in the detail: %q", e.Detail)
			}
		}
	}
	if !report {
		t.Errorf("problems = %+v", a.ops.errs.Entries())
	}
}

// A capability report answered 401 is a registration problem, in words that
// say what to do.
func TestARejectedRegistrationIsAProblem(t *testing.T) {
	m := &marketplace{code: 401}
	a := opsAgent(t, m)
	_, _ = a.report(control.Capability{})
	var found bool
	for _, e := range a.ops.errs.Active() {
		if e.Area == control.AreaRegister && strings.Contains(e.Message, "register --code") {
			found = true
		}
	}
	if !found {
		t.Errorf("problems = %+v", a.ops.errs.Entries())
	}
}

// A new problem is reported at once, but at most once a minute.
func TestNewProblemsAreReportedAtOnceButNotTooOften(t *testing.T) {
	m := &marketplace{answer: `{"status":"ok"}`}
	a := opsAgent(t, m)
	oldGap := minReportGap
	minReportGap = 300 * time.Millisecond
	t.Cleanup(func() { minReportGap = oldGap })
	a.ops.errs.OnNew(a.kickReport)
	ctx, cancel := contextWithCancel()
	done := make(chan struct{})
	go func() { a.reportLoop(ctx); close(done) }()
	count := func() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.reports) }

	a.ops.errs.Raise(control.AreaAgent, "first", "")
	waitFor(t, func() bool { return count() == 1 })
	a.ops.errs.Raise(control.AreaAgent, "second", "")
	time.Sleep(100 * time.Millisecond)
	if count() != 1 {
		t.Errorf("reported %d times within the gap", count())
	}
	waitFor(t, func() bool { return count() == 2 })
	cancel()
	<-done
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

// A failed or rolled-back update is a problem, recorded once.
func TestAFailedUpdateIsNotedOnce(t *testing.T) {
	m := &marketplace{answer: `{"status":"ok"}`}
	a := opsAgent(t, m)
	rec := update.Record{From: "v0.2.0", To: "v0.2.1", StartedAt: 1789000000, FinishedAt: 1789000200, Status: update.StatusRolledBack,
		Error: "the agent was not running three minutes after the update; the previous version is back"}
	data, _ := json.Marshal(rec)
	_ = os.MkdirAll(config.DataDir(), 0700)
	if err := os.WriteFile(update.Path(config.DataDir()), data, 0600); err != nil {
		t.Fatal(err)
	}
	a.noteUpdateRecord()
	a.noteUpdateRecord()
	n := 0
	for _, e := range a.ops.errs.Entries() {
		if e.Area == control.AreaUpdate && strings.Contains(e.Message, "rolled back") && e.At == 1789000200 {
			n++
		}
	}
	if n != 1 {
		t.Errorf("recorded %d times: %+v", n, a.ops.errs.Entries())
	}
}

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}
