package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// The host keeps using its machine until it is rented (v0.2.3). While the
// host's own programs hold the GPU, or use the memory a rental's VM needs, the
// machine stays on the market ("host busy"). A rental that arrives then waits:
// it is recorded in pending.json (so it survives an agent restart), the people
// on the machine are told -- a wall message and a desktop notification, every
// NotifyEvery and once more FinalNotice before the deadline -- and the agent
// looks again every HostUseInterval. As soon as the machine is free the rental
// starts, after a full test boot when the running agent has not passed one. At
// its start_by it is given up. The agent never stops, kills or signals a
// program of the host's.

// StatusWaiting is the provisioner's status while a rental waits for the host.
const StatusWaiting = control.StatusWaitingForHost

// Timings; variables so tests need not wait.
var (
	// HostUseInterval is how often the host's use is looked at, and a
	// waiting rental checked.
	HostUseInterval = 15 * time.Second
	// NotifyEvery is how often the host is reminded while a rental waits.
	NotifyEvery = 2 * time.Hour
	// FinalNotice is how long before the deadline the last reminder goes out.
	FinalNotice = time.Hour
	// FailedPendingKept is how long a rental that did not start stays on
	// /status for the marketplace to read, unless torn down first.
	FailedPendingKept = 48 * time.Hour
)

// pendingRecord is the rental that waits, as pending.json keeps it.
type pendingRecord struct {
	RentalID     string `json:"rental_id"`
	RenterPubkey string `json:"renter_pubkey"`
	// Pair is a pair rental's whole request, its intra-pair key included:
	// the rental must start as asked after a restart. The file is 0600, and
	// is deleted when the rental starts or ends.
	Pair          *control.PairProvisionRequest `json:"pair,omitempty"`
	StartBy       int64                         `json:"start_by"`
	Since         int64                         `json:"since"`
	Holders       []string                      `json:"holders"`
	MemoryShortGB int                           `json:"memory_short_gb,omitempty"`
	State         string                        `json:"state"`
	Reason        string                        `json:"reason,omitempty"`
	Detail        string                        `json:"detail,omitempty"`
	FailedAt      int64                         `json:"failed_at,omitempty"`
	NotifiedAt    int64                         `json:"notified_at,omitempty"`
	FinalNotified bool                          `json:"final_notified,omitempty"`
	// PeerWait: a pair half whose machine is free, waiting for the other half
	// (pairhold.go).
	PeerWait bool `json:"peer_wait,omitempty"`
	// DesktopWarnedAt: when the person logged in to the desktop was told it
	// closes for the rental (desktopwarn.go).
	DesktopWarnedAt int64 `json:"desktop_warned_at,omitempty"`

	// cancelled: torn down while it started; booting: past its test, the VM
	// is being started (a teardown then waits for it, as for any start);
	// running: a pair half's hold is running.
	cancelled, booting, running bool
	cancel                      context.CancelFunc
}

func (r *pendingRecord) view() *control.PendingRental {
	holders := append([]string{}, r.Holders...)
	v := &control.PendingRental{RentalID: r.RentalID, Since: r.Since, StartBy: r.StartBy, Holders: holders,
		MemoryShortGB: r.MemoryShortGB, State: r.State, Reason: r.Reason, Detail: r.Detail, WaitingForPeer: r.PeerWait}
	if r.DesktopWarnedAt > 0 && r.State == control.PendingWaiting && len(r.Holders) == 0 && r.MemoryShortGB == 0 && !r.PeerWait {
		v.DesktopClosesAt = time.Unix(r.DesktopWarnedAt, 0).Add(DesktopWarning).Unix()
	}
	return v
}

// PendingPath is where a rental that waits for the host is kept.
func PendingPath(dataDir string) string { return filepath.Join(dataDir, "pending.json") }

func loadPendingRecord(h vmrt.Host, dataDir string) (*pendingRecord, error) {
	data, err := h.ReadFile(PendingPath(dataDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var rec pendingRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	if !control.ValidRentalID(rec.RentalID) {
		return nil, fmt.Errorf("%s names no rental", PendingPath(dataDir))
	}
	return &rec, nil
}

// LoadPending reads the rental that waits for the host (for `gpu-agent
// status`), or nil.
func LoadPending(h vmrt.Host, dataDir string) (*control.PendingRental, error) {
	rec, err := loadPendingRecord(h, dataDir)
	if err != nil || rec == nil {
		return nil, err
	}
	return rec.view(), nil
}

func (p *Provisioner) savePendingLocked(rec *pendingRecord) error {
	if p.host == nil {
		return nil
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := p.host.MkdirAll(p.dataDir, 0700); err != nil {
		return err
	}
	tmp := PendingPath(p.dataDir) + ".tmp"
	if err := p.host.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return p.host.Rename(tmp, PendingPath(p.dataDir))
}

func (p *Provisioner) removePendingLocked() {
	if p.host != nil {
		_ = p.host.Remove(PendingPath(p.dataDir))
	}
}

// PendingRental is the rental waiting for the host, or one that did not
// start, for /status; nil when there is none. A rental that is starting is
// not shown: /status says provisioning then, as for any start.
func (p *Provisioner) PendingRental() *control.PendingRental {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil || p.pending.State == control.PendingStarting {
		return nil
	}
	return p.pending.view()
}

func (p *Provisioner) location() *time.Location {
	if p.loc != nil {
		return p.loc
	}
	return time.Local
}

// hostUseNow looks at what the host is using now.
func (p *Provisioner) hostUseNow() vmrt.HostUse {
	if p.readHostUse != nil {
		return p.readHostUse()
	}
	spec, ok := p.RuntimeSpec()
	if p.host == nil || !ok {
		return vmrt.HostUse{}
	}
	return vmrt.ReadHostUse(p.host, spec)
}

// fullTestIsCurrent: the running agent has passed a full test boot on this
// machine as it is now (a provisioner built by hand has nothing to test).
func (p *Provisioner) fullTestIsCurrent() bool {
	if p.fullTestCurrent != nil {
		return p.fullTestCurrent()
	}
	rt := p.Runtime()
	if p.host == nil || rt == nil {
		return true
	}
	res, err := vmrt.LoadSelfTest(p.host, rt.Spec().DataDir)
	return err == nil && vmrt.FullTestCurrent(res, rt.Fingerprint(p.version))
}

// runPreRentalTest runs the full test boot right before a rental: the GPU
// handed to a test VM, as it is about to be handed to the rental's. busy says
// why it cannot run now (the host took the GPU again, another test runs).
func (p *Provisioner) runPreRentalTest(ctx context.Context) (vmrt.SelfTestResult, string) {
	if p.preRentalTest != nil {
		return p.preRentalTest(ctx)
	}
	rt := p.Runtime()
	if p.host == nil || rt == nil {
		return vmrt.SelfTestResult{Passed: true, GPUVerified: true}, ""
	}
	// A DGX Spark's desktop closes for the rental anyway: it may for its test.
	plan := vmrt.PlanTest(p.host, rt.Spec(), true)
	switch {
	case plan.Wait != "":
		return vmrt.SelfTestResult{}, plan.Wait
	case plan.NoGPU:
		return vmrt.SelfTestResult{}, "the GPU is in use on this machine by " + strings.Join(plan.InUse, ", ")
	}
	release, err := vmrt.AcquireBusy(p.host, rt.Spec().DataDir, os.Getpid(), "running the test rental before a rental")
	if err != nil {
		return vmrt.SelfTestResult{}, err.Error()
	}
	defer release()
	res := rt.SelfTestWith(ctx, p.version, plan.TestOptions)
	if res.InUse != "" {
		// The host took the GPU back as the test came to it: no verdict.
		return vmrt.SelfTestResult{}, res.InUse
	}
	return res, ""
}

// startingLocked: a rental is starting on a free machine. A failed record of
// an earlier rental gives way to it, and the host's use (none) is cleared.
func (p *Provisioner) startingLocked() {
	if p.pending != nil && p.pending.State == control.PendingFailed {
		p.pending = nil
		p.removePendingLocked()
	}
	p.setHostUseLocked(vmrt.HostUse{}, p.clock().Unix())
}

// refreshTestView puts the last test boot back into the capability.
func (p *Provisioner) refreshTestView() {
	rt := p.Runtime()
	if p.host == nil || rt == nil {
		return
	}
	res, err := vmrt.LoadSelfTest(p.host, rt.Spec().DataDir)
	if err != nil {
		return
	}
	fp := rt.Fingerprint(p.version)
	var gpus []control.GPU
	for _, g := range vmrt.VerifiedGPUs(res, fp) {
		gpus = append(gpus, control.GPU{Model: g.Model, PCIID: g.ID, MemoryMB: g.MemoryMB, Unified: g.Unified, Driver: g.Driver})
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.capability.SelfTest = selfTestSummary(res)
	p.capability.RetestPending = p.capability.Ready && vmrt.SelfTestProblem(res, fp) == "" && !vmrt.FullTestCurrent(res, fp)
	if len(fp.GPUs) > 0 && len(gpus) > 0 {
		p.capability.GPUs = gpus
	}
}

// pendingConflictLocked answers a rental request while a rental is pending:
// the same rental still waiting is answered again (view); anything else is
// refused. A rental that did not start is replaced by a new one.
func (p *Provisioner) pendingConflictLocked(id string) (*control.PendingRental, error) {
	rec := p.pending
	switch {
	case rec == nil || rec.State == control.PendingFailed:
		return nil, nil
	case rec.RentalID == id && rec.State == control.PendingWaiting:
		return rec.view(), nil
	case rec.RentalID == id:
		return nil, control.Conflict("rental %s is starting on this machine", id)
	}
	return nil, control.Conflict("this machine is taken by rental %s, which waits for the machine's owner to free it", rec.RentalID)
}

// waitLocked records rec as waiting for the host, tells the host, and
// answers with it. It is called with p.mu held and releases it.
func (p *Provisioner) waitLocked(rec *pendingRecord, use vmrt.HostUse) (*control.PendingRental, error) {
	now := p.clock().Unix()
	switch {
	case !use.Busy():
		// Free but for a desktop someone is logged in to: it waits for its
		// warning (desktopHold), which the caller sends.
	case rec.StartBy == 0:
		p.mu.Unlock()
		return nil, control.Conflict("this machine is in use by its owner (%s); a rental can wait for it only with a start_by", use.Describe())
	case rec.StartBy <= now:
		p.mu.Unlock()
		return nil, control.Conflict("this machine is in use by its owner (%s), and the rental's start_by has passed", use.Describe())
	}
	rec.State, rec.Since = control.PendingWaiting, now
	rec.Holders, rec.MemoryShortGB = append([]string{}, use.Holders...), use.MemoryShortGB()
	if err := p.savePendingLocked(rec); err != nil {
		p.mu.Unlock()
		return nil, fmt.Errorf("record the rental that waits for this machine: %w", err)
	}
	p.pending = rec
	p.status = StatusWaiting
	p.lastErr = ""
	view := rec.view()
	p.mu.Unlock()
	if p.async {
		go p.tellRented(rec)
	} else {
		p.tellRented(rec)
	}
	return view, nil
}

// launchLocked starts rec: its full test boot first when due, then the rental.
// It is called with p.mu held and the status set to provisioning, and releases
// the lock. In the background unless the provisioner is synchronous (tests).
func (p *Provisioner) launchLocked(rec *pendingRecord) error {
	rec.State = control.PendingStarting
	if rec.Since == 0 {
		rec.Since = p.clock().Unix()
	}
	base := p.baseCtx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	rec.cancel = cancel
	p.pending = rec
	p.setHostUseLocked(vmrt.HostUse{}, p.clock().Unix())
	if err := p.savePendingLocked(rec); err != nil {
		log.Printf("record the rental %s that is starting: %v", rec.RentalID, err)
	}
	p.starts.Add(1)
	p.mu.Unlock()
	if !p.async {
		return p.startPending(ctx, cancel, rec)
	}
	go p.startPending(ctx, cancel, rec)
	return nil
}

// WaitStarts waits up to timeout for rentals starting in the background after
// a test boot, so an agent that stops tears a test VM down first.
func (p *Provisioner) WaitStarts(timeout time.Duration) {
	done := make(chan struct{})
	go func() { p.starts.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// startPending runs the full test boot when the running agent owes one, then
// starts the rental rec stands for.
func (p *Provisioner) startPending(ctx context.Context, cancel context.CancelFunc, rec *pendingRecord) error {
	defer p.starts.Done()
	defer cancel()
	if !p.fullTestIsCurrent() {
		res, busy := p.runPreRentalTest(ctx)
		p.mu.Lock()
		cancelled := rec.cancelled
		p.mu.Unlock()
		switch {
		case cancelled:
			p.finishCancelled(rec)
			return errors.New("the rental was cancelled before it started")
		case ctx.Err() != nil || res.Stopped:
			// The agent is stopping: the record stays on disk as starting,
			// and the next start takes it up again.
			p.mu.Lock()
			if p.pending == rec {
				p.pending = nil
				p.status = StatusFree
			}
			p.mu.Unlock()
			return errors.New("the agent stopped before the rental started")
		case busy != "":
			return p.backToWaiting(rec, busy)
		case !res.Passed:
			detail := strings.Join(res.Problems, "; ")
			p.failPending(rec, control.PendingReasonGPU, detail)
			return errors.New(control.PendingReasonGPU + ": " + detail)
		}
		p.refreshTestView()
	}

	p.mu.Lock()
	if rec.cancelled {
		p.mu.Unlock()
		p.finishCancelled(rec)
		return errors.New("the rental was cancelled before it started")
	}
	rec.booting = true
	machine := p.machine
	p.mu.Unlock()

	o := vmrt.StartOptions{ID: rec.RentalID, Pubkey: rec.RenterPubkey}
	unlock := func() {}
	if rec.Pair != nil {
		var err error
		if o, err = p.pairOptions(*rec.Pair); err != nil {
			p.failPending(rec, control.PendingReasonStart, err.Error())
			return err
		}
		if unlock, err = p.takeFrames(); err != nil {
			return p.backToWaiting(rec, err.Error())
		}
	}
	waits := rec.StartBy > p.clock().Unix()
	err := p.startNoting(machine, o, !waits)
	unlock()
	if err != nil && waits && errors.Is(err, vmrt.ErrGPUInUse) {
		// The host took the machine again in the moment before the start
		// (the runtime refused a GPU in use): the rental waits on.
		return p.backToWaiting(rec, err.Error())
	}
	p.mu.Lock()
	if p.pending != rec {
		p.mu.Unlock()
		return err
	}
	if err == nil {
		p.pending = nil
		p.removePendingLocked()
		p.mu.Unlock()
		return nil
	}
	// p.start recorded the failure (status, error, problem list).
	rec.State, rec.Reason, rec.Detail, rec.FailedAt = control.PendingFailed, control.PendingReasonStart, err.Error(), p.clock().Unix()
	rec.booting = false
	_ = p.savePendingLocked(rec)
	told := rec.NotifiedAt > 0
	p.mu.Unlock()
	if told {
		p.tellHost("GPU marketplace: the rental did not start",
			"The rental of this machine could not start. You can keep using the machine; 'sudo gpu-agent status' shows why.")
	}
	return err
}

// backToWaiting puts a rental that could not start after all back to wait for
// the host -- or, past its start_by (or without one), fails it.
func (p *Provisioner) backToWaiting(rec *pendingRecord, why string) error {
	now := p.clock().Unix()
	p.mu.Lock()
	dirty := p.machine != nil && p.machine.Dirty()
	p.mu.Unlock()
	if rec.StartBy <= now || dirty {
		p.failPending(rec, control.PendingReasonStart, why)
		return errors.New(why)
	}
	p.mu.Lock()
	if p.pending == rec {
		rec.State, rec.booting = control.PendingWaiting, false
		_ = p.savePendingLocked(rec)
		p.status = StatusWaiting
		p.lastErr = ""
	}
	p.mu.Unlock()
	return nil
}

// failPending records that rec did not start, and why, for /status (and the
// marketplace's problem list), and tells the host.
func (p *Provisioner) failPending(rec *pendingRecord, reason, detail string) {
	p.mu.Lock()
	if p.pending != rec {
		p.mu.Unlock()
		return
	}
	rec.State, rec.Reason, rec.Detail, rec.FailedAt, rec.booting = control.PendingFailed, reason, detail, p.clock().Unix(), false
	_ = p.savePendingLocked(rec)
	p.status = StatusFree
	if p.machine != nil && p.machine.Dirty() {
		p.status = StatusDirty
	}
	p.lastErr = reason
	if detail != "" {
		p.lastErr += ": " + detail
	}
	pr, redo, notify := p.problems, p.redetect, p.onChange
	p.mu.Unlock()
	if pr != nil {
		pr.Note(control.AreaRental, "rental "+rec.RentalID+" was cancelled: "+reason, detail)
	}
	if reason == control.PendingReasonGPU {
		p.tellHost("GPU marketplace: the rental was cancelled",
			"The rental of this machine was cancelled: its GPU could not be handed to the rental. The machine is paused on the marketplace; "+
				"'sudo gpu-agent status' shows what the test found.")
	} else {
		p.tellHost("GPU marketplace: the rental did not start",
			"The rental of this machine could not start. 'sudo gpu-agent status' shows why.")
	}
	// A failed test boot takes the machine off the market: read it again.
	if redo != nil && p.Adopt(redo()) && notify != nil {
		notify()
	}
}

// finishCancelled ends a rental torn down while it started.
func (p *Provisioner) finishCancelled(rec *pendingRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending != rec {
		return
	}
	p.pending = nil
	p.removePendingLocked()
	p.status = StatusFree
	if p.machine != nil && p.machine.Dirty() {
		p.status = StatusDirty
	}
}

// CancelPending is POST /teardown for a rental that has not started: one
// still waiting is dropped (the host keeps the machine, and is told), one
// that did not start is forgotten, and one running its test boot stops it.
// A rental whose VM is already starting is left to Teardown.
func (p *Provisioner) CancelPending(rentalID string) (bool, error) {
	p.mu.Lock()
	rec := p.pending
	if rec == nil || rec.RentalID != rentalID {
		p.mu.Unlock()
		return false, nil
	}
	switch rec.State {
	case control.PendingWaiting:
		p.pending = nil
		p.removePendingLocked()
		p.status = StatusFree
		if rec.cancel != nil {
			rec.cancel() // a pair half's hold
		}
		told := rec.NotifiedAt > 0
		p.mu.Unlock()
		if told {
			p.tellHost("GPU marketplace: the rental was cancelled",
				"The rental of this machine was cancelled. You can keep using the machine.")
		}
		return true, nil
	case control.PendingFailed:
		p.pending = nil
		p.removePendingLocked()
		p.mu.Unlock()
		return true, nil
	}
	if rec.booting {
		p.mu.Unlock()
		return false, nil
	}
	rec.cancelled = true
	if rec.cancel != nil {
		rec.cancel()
	}
	p.mu.Unlock()
	return true, nil
}

// resumePending takes up the rental that waited for the host before the
// agent restarted.
func (p *Provisioner) resumePending() {
	if p.host == nil {
		return
	}
	rec, err := loadPendingRecord(p.host, p.dataDir)
	if err != nil {
		log.Printf("read the rental waiting for this machine: %v", err)
		if p.problems != nil {
			p.problems.Note(control.AreaRental, "a rental that was waiting for this machine could not be read back after the agent restarted", err.Error())
		}
		return
	}
	if rec == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if rec.State == control.PendingStarting {
		if p.status == StatusRented {
			// It started before the agent stopped.
			if st, _ := vmrt.LoadState(p.host, p.dataDir); st != nil && st.RentalID == rec.RentalID {
				p.removePendingLocked()
				return
			}
		}
		if rec.StartBy == 0 {
			// A rental that could not wait, stopped with the agent in its
			// test boot: it did not start, as any start the agent's restart
			// cuts short.
			p.removePendingLocked()
			if p.status == StatusFree {
				p.lastErr = "rental " + rec.RentalID + " did not start: the agent restarted before it started"
			}
			if p.problems != nil {
				pr := p.problems
				go pr.Note(control.AreaRental, "rental "+rec.RentalID+" did not start: the agent restarted while it was starting", "")
			}
			return
		}
		rec.State = control.PendingWaiting
	}
	p.pending = rec
	if rec.State != control.PendingWaiting {
		return
	}
	switch p.status {
	case StatusFree:
		p.status = StatusWaiting
		_ = p.savePendingLocked(rec)
	default:
		rec.State, rec.Reason, rec.FailedAt = control.PendingFailed, control.PendingReasonStart, p.clock().Unix()
		rec.Detail = "this machine is " + p.status + " since the agent restarted"
		_ = p.savePendingLocked(rec)
	}
}

// Watch looks at the host's use every HostUseInterval and drives a rental
// that waits for the host, until ctx ends (the agent stops).
func (p *Provisioner) Watch(ctx context.Context) {
	p.mu.Lock()
	p.baseCtx = ctx
	p.mu.Unlock()
	for {
		p.Tick()
		select {
		case <-ctx.Done():
			return
		case <-time.After(HostUseInterval):
		}
	}
}

// Tick is one look: the host's use into the capability (reported when it
// changes), and a waiting rental reminded, started or given up.
func (p *Provisioner) Tick() {
	now := p.clock()
	p.mu.Lock()
	status, settingUp, withdrawn, rec := p.status, p.settingUp, p.withdrawn, p.pending
	var recState string
	var recFailedAt int64
	var recHold, recRunning bool
	if rec != nil {
		recState, recFailedAt, recHold, recRunning = rec.State, rec.FailedAt, rec.pairHold(), rec.running
	}
	p.mu.Unlock()
	if withdrawn {
		return
	}
	changed := false
	var use vmrt.HostUse
	looked := false
	if (status == StatusFree || status == StatusWaiting) && !settingUp {
		use, looked = p.hostUseNow(), true
		p.mu.Lock()
		if p.status == status && !p.settingUp {
			changed = p.setHostUseLocked(use, now.Unix())
		}
		p.mu.Unlock()
	}
	switch {
	case rec == nil:
	case recHold && recState != control.PendingFailed:
		// A pair half is driven by its hold; one taken up after a restart
		// gets its hold back.
		if !recRunning {
			p.mu.Lock()
			if p.pending == rec && !rec.running && !p.withdrawn {
				p.launchHoldLocked(rec)
			}
			p.mu.Unlock()
		}
	case recState == control.PendingWaiting && status == StatusDirty:
		p.failPending(rec, control.PendingReasonStart, DirtyProblem)
	case recState == control.PendingWaiting && looked:
		p.drivePending(rec, use, now)
	case recState == control.PendingFailed && now.Sub(time.Unix(recFailedAt, 0)) > FailedPendingKept:
		p.mu.Lock()
		if p.pending == rec {
			p.pending = nil
			p.removePendingLocked()
		}
		p.mu.Unlock()
	}
	p.mu.Lock()
	notify := p.onChange
	p.mu.Unlock()
	if changed && notify != nil {
		notify()
	}
}

// drivePending moves a waiting rental on: given up at its deadline, started
// once the machine is free, else the host reminded when due.
func (p *Provisioner) drivePending(rec *pendingRecord, use vmrt.HostUse, now time.Time) {
	p.mu.Lock()
	if p.pending != rec || rec.State != control.PendingWaiting || p.status != StatusWaiting {
		p.mu.Unlock()
		return
	}
	if now.Unix() >= rec.StartBy {
		p.mu.Unlock()
		p.giveUp(rec)
		return
	}
	if !use.Busy() && !p.settingUp && !p.withdrawn {
		p.mu.Unlock()
		if p.desktopHold(rec) {
			return
		}
		p.mu.Lock()
		if p.pending != rec || rec.State != control.PendingWaiting || p.status != StatusWaiting || p.settingUp || p.withdrawn {
			p.mu.Unlock()
			return
		}
		if !p.capability.Ready {
			// Something other than the host's use keeps it from hosting now.
			why := strings.Join(p.capability.Reasons, "; ")
			p.mu.Unlock()
			p.failPending(rec, control.PendingReasonStart, "this machine cannot host a rental now: "+why)
			return
		}
		p.status = StatusProvisioning
		p.lastErr = ""
		_ = p.launchLocked(rec) // unlocks
		return
	}
	holders := append([]string{}, use.Holders...)
	if strings.Join(holders, "\n") != strings.Join(rec.Holders, "\n") || rec.MemoryShortGB != use.MemoryShortGB() {
		rec.Holders, rec.MemoryShortGB = holders, use.MemoryShortGB()
		_ = p.savePendingLocked(rec)
	}
	p.mu.Unlock()
	p.tellRented(rec) // when a reminder is due
}

// gaveUp tells the host, and the marketplace's problem list, that a rental
// was given up because the machine was still in use at its deadline.
func (p *Provisioner) gaveUp(rec *pendingRecord, pr Problems) {
	use := vmrt.HostUse{Holders: rec.Holders, MemoryShortMB: rec.MemoryShortGB * 1024}
	what := use.Describe()
	if what == "" {
		// Free by then, but not started in time (the other machine of a pair,
		// a desktop warning, a test boot that ran late).
		p.tellHost("GPU marketplace: the rental was cancelled",
			"The rental of this machine was cancelled: it could not start by its deadline. You can keep using the machine.")
		if pr != nil {
			pr.Note(control.AreaRental, fmt.Sprintf("rental %s was cancelled: it did not start by its start deadline, %s",
				rec.RentalID, time.Unix(rec.StartBy, 0).UTC().Format("2006-01-02 15:04 UTC")), "")
		}
		return
	}
	p.tellHost("GPU marketplace: the rental was cancelled",
		"The rental of this machine was cancelled because the machine was still in use ("+what+") when the rental was due to start. "+
			"The machine is paused on the marketplace; put it back on sale in Marketplace > List a GPU.")
	if pr != nil {
		pr.Note(control.AreaRental, fmt.Sprintf("rental %s was cancelled: this machine was still in use by its owner (%s) at its start deadline, %s",
			rec.RentalID, what, time.Unix(rec.StartBy, 0).UTC().Format("2006-01-02 15:04 UTC")), "")
	}
}

// tellRented tells the people on the machine that it has been rented, and
// what to stop by when -- at once, then every NotifyEvery, and once more
// FinalNotice before the deadline; recorded so the reminders keep their
// rhythm across a restart. It does nothing when no word is due.
func (p *Provisioner) tellRented(rec *pendingRecord) {
	now := p.clock()
	p.mu.Lock()
	if p.pending != rec || rec.State != control.PendingWaiting || rec.PeerWait || (len(rec.Holders) == 0 && rec.MemoryShortGB == 0) {
		p.mu.Unlock()
		return
	}
	first := rec.NotifiedAt == 0
	final := time.Unix(rec.StartBy, 0).Sub(now) <= FinalNotice
	if !first && !(final && !rec.FinalNotified) && now.Sub(time.Unix(rec.NotifiedAt, 0)) < NotifyEvery {
		p.mu.Unlock()
		return
	}
	rec.NotifiedAt = now.Unix()
	if final {
		rec.FinalNotified = true
	}
	_ = p.savePendingLocked(rec)
	body := RentedNotice(rec.view(), p.desktopOnDemandLocked(), p.location())
	pr := p.problems
	p.mu.Unlock()
	title := "GPU marketplace: this machine has been rented"
	if final {
		title = "GPU marketplace: this machine has been rented -- under an hour left to free it"
	}
	problems := p.tellHost(title, body)
	if first && len(problems) > 0 && pr != nil {
		pr.Note(control.AreaRental, "the people on this machine could not all be told that it was rented", strings.Join(problems, "\n"))
	}
}

// tellHost sends title and body to the people on the machine.
func (p *Provisioner) tellHost(title, body string) (problems []string) {
	notify := p.notify
	if notify == nil {
		if p.host == nil {
			return nil
		}
		notify = func(title, body string) ([]string, []string) { return vmrt.NotifyHost(p.host, title, body) }
	}
	reached, problems := notify(title, body)
	log.Printf("told %s: %s", strings.Join(append(reached, "the log"), ", "), body)
	for _, problem := range problems {
		log.Printf("could not tell the host: %s", problem)
	}
	return problems
}

func (p *Provisioner) desktopOnDemandLocked() bool {
	spec, ok := p.runtimeSpecLocked()
	return ok && spec.DesktopOnDemand
}

// RentedNotice is what the people on a machine read while a rental waits for
// it: on the terminals, as a desktop notification, and in `gpu-agent status`.
func RentedNotice(pr *control.PendingRental, spark bool, loc *time.Location) string {
	use := vmrt.HostUse{Holders: pr.Holders, MemoryShortMB: pr.MemoryShortGB * 1024}
	by := FormatDeadline(pr.StartBy, loc)
	progs := strings.Join(use.Programs(), ", ")
	var s string
	switch {
	case progs != "" && pr.MemoryShortGB > 0:
		s = fmt.Sprintf("This machine has been rented. Please stop your programs that use it (%s) and free %d GB of memory by %s; "+
			"the rental starts as soon as they have stopped. If they are still running then, the rental is cancelled and the machine is paused.",
			progs, pr.MemoryShortGB, by)
	case progs != "":
		s = fmt.Sprintf("This machine has been rented. Please stop your programs that use it (%s) by %s; "+
			"the rental starts as soon as they have stopped. If they are still running then, the rental is cancelled and the machine is paused.",
			progs, by)
	default:
		s = fmt.Sprintf("This machine has been rented. Please stop your programs so that %d GB more of its memory is free by %s; "+
			"the rental starts as soon as it is. If the memory is still in use then, the rental is cancelled and the machine is paused.",
			pr.MemoryShortGB, by)
	}
	if spark {
		s += " The desktop closes when the rental starts; save your work."
	}
	return s
}

// FormatDeadline is a deadline as a host reads it: the machine's local time,
// and UTC ("2026-09-19 14:05 EEST (11:05 UTC)").
func FormatDeadline(unix int64, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	t := time.Unix(unix, 0)
	u, l := t.UTC(), t.In(loc)
	if _, offset := l.Zone(); offset == 0 {
		return u.Format("2006-01-02 15:04 UTC")
	}
	if l.Format("2006-01-02") == u.Format("2006-01-02") {
		return l.Format("2006-01-02 15:04 MST") + " (" + u.Format("15:04") + " UTC)"
	}
	return l.Format("2006-01-02 15:04 MST") + " (" + u.Format("2006-01-02 15:04") + " UTC)"
}
