package hostctl

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/update"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

func autoAgent(t *testing.T) *agent {
	t.Helper()
	a := newAgent(t)
	a.ConfigDir = t.TempDir()
	clock := time.Unix(1789000000, 0)
	a.Now = func() time.Time { return clock }
	// An update a test started finishes before its files are removed.
	t.Cleanup(func() {
		select {
		case <-a.release:
		default:
			close(a.release)
		}
		for i := 0; i < 1000; i++ {
			if r := update.Load(a.DataDir); r == nil || !r.InProgress() || r.Status == update.StatusRestarting {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		// The last save's rename lands a moment after the record reads so.
		time.Sleep(50 * time.Millisecond)
	})
	return a
}

func waitRestarting(t *testing.T, a *agent) {
	t.Helper()
	select {
	case <-a.release:
	default:
		close(a.release)
	}
	for i := 0; i < 300; i++ {
		if r := update.Load(a.DataDir); r != nil && r.Status == update.StatusRestarting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("update.json = %+v", update.Load(a.DataDir))
}

// On by default: a newer release the marketplace names is installed when the
// machine is idle, the same way the control panel's update is.
func TestAutomaticUpdateWhenIdle(t *testing.T) {
	a := autoAgent(t)
	if s := a.AutoUpdateStatus(); !s.Enabled || s.LastCheck != 0 || s.Last != nil {
		t.Fatalf("status before = %+v", s)
	}
	started, why := a.Offer(&control.AgentUpdate{Version: "v0.1.11"})
	if !started || why != "" {
		t.Fatalf("Offer = %v %q", started, why)
	}
	waitRestarting(t, a)
	s := a.AutoUpdateStatus()
	if s.LastCheck != 1789000000 {
		t.Errorf("last_check = %d", s.LastCheck)
	}
	if r, ok := s.Last.(*update.Record); !ok || r.To != "v0.1.11" {
		t.Errorf("last = %#v", s.Last)
	}
}

// Nothing newer, or no answer about it: nothing happens.
func TestNothingNewerDoesNothing(t *testing.T) {
	a := autoAgent(t)
	for _, u := range []*control.AgentUpdate{nil, {Version: "v0.1.10"}, {Version: "v0.1.9", Push: true}, {Version: "latest"}} {
		if started, why := a.Offer(u); started || why != "" {
			t.Errorf("%+v: %v %q", u, started, why)
		}
	}
	if update.Load(a.DataDir) != nil {
		t.Error("an update started")
	}
}

// A busy machine waits: no try is spent, and the next answer (once idle)
// installs it.
func TestABusyMachineWaitsForTheNextAnswer(t *testing.T) {
	for name, busy := range map[string]func(a *agent){
		"rented":          func(a *agent) { a.m.status = provisioner.StatusRented },
		"automatic setup": func(a *agent) { a.m.settingUp = true },
		"a leftover":      func(a *agent) { a.m.present = true },
		"a pair test": func(a *agent) {
			a.h.Files["/proc/9999"] = nil
			a.h.Files[vmrt.BusyPath(a.DataDir)] = []byte(`{"pid":9999,"what":"running a pair test boot"}`)
		},
	} {
		a := autoAgent(t)
		busy(a)
		if started, why := a.Offer(&control.AgentUpdate{Version: "v0.1.11"}); started || why == "" {
			t.Errorf("%s: %v %q", name, started, why)
		}
		if update.Load(a.DataDir) != nil {
			t.Errorf("%s: an update started", name)
		}
		// Idle again.
		a.m.status, a.m.settingUp, a.m.present = provisioner.StatusFree, false, false
		delete(a.h.Files, vmrt.BusyPath(a.DataDir))
		if started, why := a.Offer(&control.AgentUpdate{Version: "v0.1.11"}); !started {
			t.Errorf("%s, then idle: %q", name, why)
		}
	}
}

// Paused by the host: automatic ones wait, a push still applies.
func TestPausedUpdatesStillTakeAPush(t *testing.T) {
	a := autoAgent(t)
	if err := SetAutoUpdate(a.ConfigDir, false); err != nil {
		t.Fatal(err)
	}
	if started, why := a.Offer(&control.AgentUpdate{Version: "v0.1.11"}); started || !strings.Contains(why, "--auto on") {
		t.Errorf("paused: %v %q", started, why)
	}
	if s := a.AutoUpdateStatus(); s.Enabled {
		t.Error("auto_update.enabled while paused")
	}
	if started, why := a.Offer(&control.AgentUpdate{Version: "v0.1.11", Push: true}); !started {
		t.Errorf("a push while paused: %q", why)
	}
	if err := SetAutoUpdate(a.ConfigDir, true); err != nil || !AutoUpdateEnabled(a.ConfigDir) {
		t.Errorf("on again: %v", err)
	}
}

// A release that was rolled back is not tried again at every answer: a day
// for an automatic update, an hour for a push -- across the restart the
// rollback made, since update.json says so.
func TestARolledBackReleaseIsNotRetriedAtOnce(t *testing.T) {
	a := autoAgent(t)
	rec := update.Record{From: "v0.1.10", To: "v0.1.11", StartedAt: 1788999000, FinishedAt: 1788999500, Status: update.StatusRolledBack,
		Error: "the agent was not running three minutes after the update; the previous version is back"}
	data, _ := json.Marshal(rec)
	if err := os.MkdirAll(a.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(update.Path(a.DataDir), data, 0600); err != nil {
		t.Fatal(err)
	}
	if started, why := a.Offer(&control.AgentUpdate{Version: "v0.1.11"}); started || !strings.Contains(why, "rolled back") {
		t.Errorf("automatic, just after the rollback: %v %q", started, why)
	}
	if started, _ := a.Offer(&control.AgentUpdate{Version: "v0.1.11", Push: true}); started {
		t.Error("a push retried within the hour")
	}
	a.Now = func() time.Time { return time.Unix(1788999500, 0).Add(2 * time.Hour) }
	if started, why := a.Offer(&control.AgentUpdate{Version: "v0.1.11", Push: true}); !started {
		t.Errorf("a push two hours later: %q", why)
	}
}

// One try at a time: a second answer right after the first does not start
// another download.
func TestOneTryAtATime(t *testing.T) {
	a := autoAgent(t)
	if started, _ := a.Offer(&control.AgentUpdate{Version: "v0.1.11"}); !started {
		t.Fatal("not started")
	}
	if started, why := a.Offer(&control.AgentUpdate{Version: "v0.1.11"}); started || why == "" {
		t.Errorf("second offer: %v %q", started, why)
	}
}

// GET /status -- the marketplace's heartbeat -- carries auto_update, and
// takes the update offer from its header.
func TestStatusCarriesAndTakesUpdates(t *testing.T) {
	for offer, want := range map[string]bool{
		`{"version":"v0.1.11","push":false}`: true,
		`{"version":"v0.1.11","push":true}`:  true,
		`{"version":"v0.1.10","push":true}`:  false, // not newer
		`{"version":"v0.1.11","url":"x"}`:    false, // unknown field
		`{"version":"0.1.11"}`:               false,
		`v0.1.11`:                            false,
		``:                                   false,
	} {
		a := autoAgent(t)
		srv := control.New("127.0.0.1:0", "secret", noRentals{})
		srv.SetHost(a.Agent)
		req := httptest.NewRequest("GET", "/status", nil)
		req.Header.Set("Authorization", "Bearer secret")
		if offer != "" {
			req.Header.Set(control.UpdateOfferHeader, offer)
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"auto_update":{"enabled":true`) {
			t.Fatalf("%s: %d %s", offer, w.Code, w.Body)
		}
		r := update.Load(a.DataDir)
		if got := r != nil && r.To == "v0.1.11"; got != want {
			t.Errorf("%q: update started = %v, want %v", offer, got, want)
		}
		if want && !strings.Contains(w.Body.String(), `"update":{"from":"v0.1.10","to":"v0.1.11"`) {
			t.Errorf("%q: /status does not show the update it started: %s", offer, w.Body)
		}
	}
}
