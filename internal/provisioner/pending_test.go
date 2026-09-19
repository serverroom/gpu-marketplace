package provisioner

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// waitRig is a ready provisioner with its host's use, its test boot, its
// notifications and its clock in the test's hands.
type waitRig struct {
	t     *testing.T
	p     *Provisioner
	h     *fakehost.Host
	m     *fakeMachine
	probs *recordedProblems

	mu    sync.Mutex
	use   vmrt.HostUse
	full  bool
	test  vmrt.SelfTestResult
	tests int
	told  []string
	now   time.Time
}

const startBy = 1789819200 // 2026-09-19 12:00 UTC

func newWaitRig(t *testing.T) *waitRig {
	t.Helper()
	noRelay(t)
	withFakeForward(t, nil)
	r := &waitRig{t: t, h: hostedHost(t), m: &fakeMachine{stopRes: clean()}, probs: &recordedProblems{},
		full: true, test: vmrt.SelfTestResult{Passed: true, GPUVerified: true}, now: time.Unix(startBy-24*3600, 0)}
	r.p = Detect(r.h, "linux", "amd64", dataDir, version)
	if !r.p.Capability().Ready {
		t.Fatalf("not ready: %v", r.p.Capability().Reasons)
	}
	r.wire(r.p)
	return r
}

// wire puts the rig's hands on p (again, after a restart).
func (r *waitRig) wire(p *Provisioner) {
	p.machine, p.async = r.m, false
	p.readHostUse = func() vmrt.HostUse { r.mu.Lock(); defer r.mu.Unlock(); return r.use }
	p.fullTestCurrent = func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.full }
	p.preRentalTest = func(context.Context) (vmrt.SelfTestResult, string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.tests++
		if r.test.Passed {
			r.full = true
		}
		return r.test, ""
	}
	p.notify = func(title, body string) ([]string, []string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.told = append(r.told, title+"\n"+body)
		return []string{"every terminal"}, nil
	}
	p.now = func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now }
	p.loc = time.FixedZone("EEST", 3*3600)
	p.SetProblems(r.probs)
	p.redetect = nil
}

func (r *waitRig) busy(holders ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.use = vmrt.HostUse{Holders: holders}
}

func (r *waitRig) at(t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = t
}

func (r *waitRig) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.told...)
}

func (r *waitRig) noted() string {
	r.probs.mu.Lock()
	defer r.probs.mu.Unlock()
	return strings.Join(r.probs.noted, " | ")
}

// A rental that arrives while the host uses the machine waits for it: 202 with
// the host's programs, recorded on disk, and the host told on the machine.
func TestARentalWaitsForTheHost(t *testing.T) {
	r := newWaitRig(t)
	r.busy("llama-server (pid 11435)")
	pending, err := r.p.ProvisionBy("R1", renterKey(t), startBy)
	if err != nil || pending == nil {
		t.Fatalf("ProvisionBy = %+v, %v", pending, err)
	}
	if pending.State != control.PendingWaiting || pending.StartBy != startBy || strings.Join(pending.Holders, ",") != "llama-server (pid 11435)" {
		t.Errorf("pending = %+v", pending)
	}
	if r.p.Status() != StatusWaiting || r.m.started != 0 {
		t.Errorf("status %s, started %d", r.p.Status(), r.m.started)
	}
	var rec pendingRecord
	if err := json.Unmarshal(r.h.Files[PendingPath(dataDir)], &rec); err != nil || rec.RentalID != "R1" || rec.RenterPubkey == "" || rec.NotifiedAt == 0 {
		t.Errorf("pending.json = %+v %v", rec, err)
	}
	told := r.messages()
	want := "This machine has been rented. Please stop your programs that use it (llama-server) by 2026-09-19 15:00 EEST (12:00 UTC); " +
		"the rental starts as soon as they have stopped. If they are still running then, the rental is cancelled and the machine is paused."
	if len(told) != 1 || !strings.HasSuffix(told[0], want) || !strings.HasPrefix(told[0], "GPU marketplace: this machine has been rented\n") {
		t.Errorf("told %q", told)
	}
	if got := r.p.PendingRental(); got == nil || got.RentalID != "R1" || got.State != control.PendingWaiting {
		t.Errorf("/status pending = %+v", got)
	}

	// The same request again is the same answer; another rental is refused.
	if again, err := r.p.ProvisionBy("R1", renterKey(t), startBy); err != nil || again == nil || again.RentalID != "R1" {
		t.Errorf("the same rental again: %+v %v", again, err)
	}
	if _, err := r.p.ProvisionBy("R2", renterKey(t), startBy); code(err) != http.StatusConflict {
		t.Errorf("another rental: %v", err)
	}
}

// Without a start_by, or past it, a rental cannot wait: refused, nothing kept.
func TestARentalThatCannotWaitIsRefused(t *testing.T) {
	r := newWaitRig(t)
	r.busy("llama-server (pid 11435)")
	for _, by := range []int64{0, startBy - 25*3600} {
		if _, err := r.p.ProvisionBy("R1", renterKey(t), by); code(err) != http.StatusConflict || !strings.Contains(err.Error(), "llama-server") {
			t.Errorf("start_by %d: %v", by, err)
		}
	}
	if r.p.Status() != StatusFree || r.h.Exists(PendingPath(dataDir)) || len(r.messages()) != 0 {
		t.Errorf("status %s, pending kept %v, told %v", r.p.Status(), r.h.Exists(PendingPath(dataDir)), r.messages())
	}
	// A free machine starts at once, start_by or not.
	r.busy()
	if pending, err := r.p.ProvisionBy("R1", renterKey(t), startBy); err != nil || pending != nil || r.p.Status() != StatusRented {
		t.Errorf("a free machine: %+v %v status %s", pending, err, r.p.Status())
	}
}

// Once the host has stopped its programs the rental starts -- after the full
// test boot the running agent owes, and never before it.
func TestAWaitingRentalStartsWhenTheHostIsDone(t *testing.T) {
	r := newWaitRig(t)
	r.full = false // this version has not handed the GPU to a test VM yet
	r.busy("llama-server (pid 11435)")
	if _, err := r.p.ProvisionBy("R1", renterKey(t), startBy); err != nil {
		t.Fatal(err)
	}
	r.p.Tick()
	if r.m.started != 0 || r.tests != 0 || r.p.Status() != StatusWaiting {
		t.Fatalf("started while the host still used it: started=%d tests=%d %s", r.m.started, r.tests, r.p.Status())
	}
	r.busy()
	r.p.Tick()
	if r.tests != 1 || r.m.started != 1 || r.m.lastID != "R1" || r.p.Status() != StatusRented {
		t.Fatalf("tests=%d started=%d id=%s status=%s", r.tests, r.m.started, r.m.lastID, r.p.Status())
	}
	if r.p.PendingRental() != nil || r.h.Exists(PendingPath(dataDir)) {
		t.Error("the pending rental outlived its start")
	}
}

// A machine that has already passed this version's full test starts at once.
func TestAWaitingRentalNeedsNoSecondTest(t *testing.T) {
	r := newWaitRig(t)
	r.busy("llama-server (pid 11435)")
	_, _ = r.p.ProvisionBy("R1", renterKey(t), startBy)
	r.busy()
	r.p.Tick()
	if r.tests != 0 || r.p.Status() != StatusRented {
		t.Errorf("tests=%d status=%s", r.tests, r.p.Status())
	}
}

// The full test before the rental failing is the GPU not handed over: the
// rental fails with exactly that reason, for the marketplace to cancel,
// refund and pause, and the host is told.
func TestAFailedTestBeforeTheRentalFailsIt(t *testing.T) {
	r := newWaitRig(t)
	r.full = false
	r.test = vmrt.SelfTestResult{Problems: []string{"the VM did not see GPU 0000:01:00.0"}}
	r.busy("llama-server (pid 11435)")
	_, _ = r.p.ProvisionBy("R1", renterKey(t), startBy)
	r.busy()
	r.p.Tick()
	got := r.p.PendingRental()
	if got == nil || got.State != control.PendingFailed || got.Reason != "its GPU could not be handed to the rental" ||
		got.Detail != "the VM did not see GPU 0000:01:00.0" {
		t.Fatalf("pending = %+v", got)
	}
	if r.p.Status() != StatusFree || !strings.HasPrefix(r.p.LastError(), "its GPU could not be handed to the rental") || r.m.started != 0 {
		t.Errorf("status %s error %q started %d", r.p.Status(), r.p.LastError(), r.m.started)
	}
	if !strings.Contains(r.noted(), "rental R1 was cancelled: its GPU could not be handed to the rental") {
		t.Errorf("noted %s", r.noted())
	}
	if told := r.messages(); !strings.Contains(told[len(told)-1], "its GPU could not be handed to the rental. The machine is paused") {
		t.Errorf("told %q", told)
	}
	// The marketplace ends it with a teardown, which forgets it.
	if ok, err := r.p.CancelPending("R1"); !ok || err != nil || r.p.PendingRental() != nil {
		t.Errorf("CancelPending = %v %v, pending %+v", ok, err, r.p.PendingRental())
	}
}

// Reminders every 2 hours and once more an hour before the deadline; at the
// deadline the rental is given up, the host told why, and the machine is free.
func TestAWaitingRentalRemindsAndGivesUp(t *testing.T) {
	r := newWaitRig(t)
	r.busy("llama-server (pid 11435)")
	_, _ = r.p.ProvisionBy("R1", renterKey(t), startBy)
	for _, c := range []struct {
		after time.Duration
		told  int
	}{{time.Hour, 1}, {2*time.Hour + time.Minute, 2}, {3 * time.Hour, 2}, {23*time.Hour + 5*time.Minute, 3}, {23*time.Hour + 30*time.Minute, 3}} {
		r.at(time.Unix(startBy-24*3600, 0).Add(c.after))
		r.p.Tick()
		if got := len(r.messages()); got != c.told {
			t.Errorf("after %v: told %d times, want %d", c.after, got, c.told)
		}
	}
	if told := r.messages(); !strings.Contains(told[2], "under an hour left") {
		t.Errorf("last reminder: %q", told[2])
	}
	r.at(time.Unix(startBy, 0))
	r.p.Tick()
	if r.p.Status() != StatusFree || r.p.PendingRental() != nil || r.h.Exists(PendingPath(dataDir)) || r.m.started != 0 {
		t.Fatalf("after the deadline: status %s pending %+v", r.p.Status(), r.p.PendingRental())
	}
	told := r.messages()
	if last := told[len(told)-1]; !strings.Contains(last, "was cancelled because the machine was still in use (llama-server)") {
		t.Errorf("told %q", last)
	}
	if !strings.Contains(r.noted(), "rental R1 was cancelled: this machine was still in use by its owner (llama-server) at its start deadline, 2026-09-19 12:00 UTC") {
		t.Errorf("noted %s", r.noted())
	}
	if r.p.Capability().HostBusy == nil {
		t.Error("the machine no longer says it is in use")
	}
}

// A teardown of a waiting rental cancels it: nothing to wipe, the host keeps
// the machine, and is told.
func TestATeardownCancelsAWaitingRental(t *testing.T) {
	r := newWaitRig(t)
	r.busy("llama-server (pid 11435)")
	_, _ = r.p.ProvisionBy("R1", renterKey(t), startBy)
	if err := r.p.Teardown("R9"); err != nil || r.p.Status() != StatusWaiting {
		t.Errorf("a teardown of another rental: %v, status %s", err, r.p.Status())
	}
	ok, err := r.p.CancelPending("R1")
	if !ok || err != nil || r.p.Status() != StatusFree || r.h.Exists(PendingPath(dataDir)) || r.m.stopped != 0 {
		t.Fatalf("cancel = %v %v, status %s, stopped %d", ok, err, r.p.Status(), r.m.stopped)
	}
	if told := r.messages(); !strings.Contains(told[len(told)-1], "was cancelled. You can keep using the machine") {
		t.Errorf("told %q", told)
	}
	if ok, _ := r.p.CancelPending("R1"); ok {
		t.Error("cancelled twice")
	}
}

// A waiting rental survives an agent restart: the new agent reads it back,
// waits on, keeps the reminders' rhythm, and starts it when the host is done.
func TestAWaitingRentalSurvivesARestart(t *testing.T) {
	r := newWaitRig(t)
	r.busy("llama-server (pid 11435)")
	_, _ = r.p.ProvisionBy("R1", renterKey(t), startBy)

	q := Detect(r.h, "linux", "amd64", dataDir, version)
	r.wire(q)
	q.Resume()
	if q.Status() != StatusWaiting || q.PendingRental() == nil || q.PendingRental().RentalID != "R1" {
		t.Fatalf("after the restart: status %s pending %+v", q.Status(), q.PendingRental())
	}
	q.Tick()
	if len(r.messages()) != 1 {
		t.Errorf("the restart reminded the host at once: %q", r.messages())
	}
	r.busy()
	q.Tick()
	if q.Status() != StatusRented || r.m.lastID != "R1" {
		t.Errorf("status %s, started %q", q.Status(), r.m.lastID)
	}
}

// A rental with no start_by whose test boot was cut short by an agent restart
// did not start: the new agent drops it and says so.
func TestAStartCutShortByARestartIsDropped(t *testing.T) {
	r := newWaitRig(t)
	rec := pendingRecord{RentalID: "R1", RenterPubkey: renterKey(t), State: control.PendingStarting, Since: 1}
	data, _ := json.Marshal(rec)
	r.h.Files[PendingPath(dataDir)] = data
	q := Detect(r.h, "linux", "amd64", dataDir, version)
	r.wire(q)
	q.Resume()
	if q.Status() != StatusFree || q.PendingRental() != nil || r.h.Exists(PendingPath(dataDir)) || !strings.Contains(q.LastError(), "restarted") {
		t.Errorf("status %s pending %+v error %q", q.Status(), q.PendingRental(), q.LastError())
	}
}

// The host's use is looked at on every tick, and a change is reported.
func TestTheHostsUseIsFollowed(t *testing.T) {
	r := newWaitRig(t)
	changes := 0
	r.p.OnCapabilityChange(func() { changes++ })
	r.busy("llama-server (pid 11435)")
	r.p.Tick()
	hb := r.p.Capability().HostBusy
	if hb == nil || hb.Since != r.now.Unix() || changes != 1 {
		t.Fatalf("host_busy = %+v, changes %d", hb, changes)
	}
	r.at(r.now.Add(time.Minute))
	r.p.Tick()
	if hb := r.p.Capability().HostBusy; hb.Since != startBy-24*3600 || changes != 1 {
		t.Errorf("since moved, or an unchanged look was reported: %+v, %d", hb, changes)
	}
	r.busy()
	r.p.Tick()
	if r.p.Capability().HostBusy != nil || changes != 2 {
		t.Errorf("host_busy = %+v, changes %d", r.p.Capability().HostBusy, changes)
	}
}

// A direct start on a free machine whose running agent owes the full test
// runs it first, and a failure there is the same GPU failure as for a waiting
// rental.
func TestADirectStartRunsTheOwedTestFirst(t *testing.T) {
	r := newWaitRig(t)
	r.full = false
	r.test = vmrt.SelfTestResult{Problems: []string{"GPU 0000:01:00.0 reached the VM but 1 of its memory windows could not be mapped there"}}
	if pending, err := r.p.ProvisionBy("R1", renterKey(t), 0); pending != nil || err == nil {
		t.Fatalf("ProvisionBy = %+v %v", pending, err)
	}
	if got := r.p.PendingRental(); got == nil || got.State != control.PendingFailed || got.Reason != control.PendingReasonGPU {
		t.Errorf("pending = %+v", got)
	}
	if r.p.Status() != StatusFree || r.m.started != 0 {
		t.Errorf("status %s started %d", r.p.Status(), r.m.started)
	}
	// Another rental may be tried; the failed record gives way to it.
	r.test = vmrt.SelfTestResult{Passed: true, GPUVerified: true}
	if _, err := r.p.ProvisionBy("R2", renterKey(t), 0); err != nil || r.p.Status() != StatusRented || r.tests != 2 {
		t.Errorf("R2: %v status %s tests %d", err, r.p.Status(), r.tests)
	}
}

func TestRentedNoticeWords(t *testing.T) {
	loc := time.FixedZone("EEST", 3*3600)
	memOnly := RentedNotice(&control.PendingRental{StartBy: startBy, MemoryShortGB: 12}, false, loc)
	if memOnly != "This machine has been rented. Please stop your programs so that 12 GB more of its memory is free by "+
		"2026-09-19 15:00 EEST (12:00 UTC); the rental starts as soon as it is. If the memory is still in use then, the rental is cancelled and the machine is paused." {
		t.Errorf("memory only: %q", memOnly)
	}
	spark := RentedNotice(&control.PendingRental{StartBy: startBy, Holders: []string{"ollama (pid 9)", "ollama (pid 10)"}, MemoryShortGB: 4}, true, time.UTC)
	if !strings.Contains(spark, "(ollama) and free 4 GB of memory by 2026-09-19 12:00 UTC;") ||
		!strings.HasSuffix(spark, " The desktop closes when the rental starts; save your work.") {
		t.Errorf("spark: %q", spark)
	}
	if got := FormatDeadline(startBy, time.FixedZone("PDT", -7*3600)); got != "2026-09-19 05:00 PDT (12:00 UTC)" {
		t.Errorf("deadline = %q", got)
	}
	if got := FormatDeadline(startBy+10*3600, loc); got != "2026-09-20 01:00 EEST (2026-09-19 22:00 UTC)" {
		t.Errorf("deadline = %q", got)
	}
}

// A host that takes the GPU back in the moment before the start (the runtime
// refuses a GPU in use) does not lose the rental: it waits on.
func TestAStartTheHostCutInOnWaitsOn(t *testing.T) {
	r := newWaitRig(t)
	r.full = false
	r.busy("llama-server (pid 11435)")
	_, _ = r.p.ProvisionBy("R1", renterKey(t), startBy)
	r.busy()
	r.p.preRentalTest = func(context.Context) (vmrt.SelfTestResult, string) {
		r.busy("llama-server (pid 11500)") // started again while the test ran
		return vmrt.SelfTestResult{Passed: true, GPUVerified: true}, ""
	}
	r.m.startErr = errorString("the GPU is in use on this machine by llama-server (pid 11500); stop them first")
	r.p.Tick()
	if got := r.p.PendingRental(); r.p.Status() != StatusWaiting || got == nil || got.State != control.PendingWaiting || r.p.LastError() != "" {
		t.Errorf("status %s pending %+v error %q", r.p.Status(), got, r.p.LastError())
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }
