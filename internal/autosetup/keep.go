package autosetup

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// Keep watches a machine that is set up (v0.2.3): every RetestInterval it
// looks whether a test boot is owed, and runs it only when that interrupts
// nothing of the host's -- the host keeps using the machine until it is
// rented, and a test boot never stops a program of the host's.
//
//   - The machine is not ready only for want of a test boot that the attempt
//     record will not run again (the setup passed for this key): the image
//     rebuilt by hand, the full test before a rental failed. The test runs
//     with the GPU when nothing of the host's holds it, else without it -- a
//     pass without the GPU makes the machine sellable again, but cannot lift
//     a failed full test, so after one only a full test runs. A failed test
//     is tried again after RetryAfter, or at the agent's next start.
//   - The machine is ready on an earlier test boot -- another agent
//     version's, or one run without the GPU -- and the running version owes
//     its own full test: it runs once the GPU is free (nothing of the host's
//     holds it, enough memory is free, and on a DGX Spark nobody is logged in
//     to the desktop). Until then the full test runs before a rental starts.
//   - Anything else the setup can fix (packages, the image) goes to Run.
//
// Nothing runs while a rental waits for the host, is starting or runs, while
// the automatic setup is off, or on a withdrawn machine.

var (
	// RetestInterval is how often Keep looks.
	RetestInterval = 5 * time.Minute
	// KeepFirstLook is how long after the agent starts Keep looks first.
	KeepFirstLook = time.Minute
)

// Keep runs KeepOnce every RetestInterval until ctx ends.
func (d *Daemon) Keep(ctx context.Context) {
	after := d.After
	if after == nil {
		after = time.After
	}
	wait, first := KeepFirstLook, true
	for {
		select {
		case <-ctx.Done():
			return
		case <-after(wait):
		}
		d.KeepOnce(ctx, first)
		first, wait = false, RetestInterval
	}
}

// onlyTestBoot: all a plan would do is the test boot.
func onlyTestBoot(p Plan) bool { return len(p.Steps) == 1 && p.Steps[0] == StepTestBoot }

// KeepOnce is one look (Keep). justStarted: the first since the agent started,
// when a failed test is tried again at once.
func (d *Daemon) KeepOnce(ctx context.Context, justStarted bool) {
	if d.GOOS != "linux" && d.GOOS != "windows" || !Enabled(d.ConfigDir) {
		return
	}
	if d.Prov.Status() != provisioner.StatusFree || d.Prov.Withdrawn() || d.Prov.SettingUp() {
		return
	}
	// Nothing owed, or nothing the agent can do: no need to look closer.
	if c := d.Prov.Capability(); c.Ready && !c.RetestPending {
		return
	} else if !c.Ready && len(PlanFor(d.Prov.Findings(), CanInstall(d.Runner.Host, d.Prov), d.Prov.SelfInstalls()).Human) > 0 {
		return
	}
	fresh := d.Runner.Detect()
	spec, ok := fresh.RuntimeSpec()
	if !ok || fresh.RentalPresent() {
		return
	}
	c := fresh.Capability()
	if c.Ready {
		if c.RetestPending {
			d.keepTest(ctx, fresh, false, justStarted)
		}
		return
	}
	plan := PlanFor(fresh.Findings(), CanInstall(d.Runner.Host, fresh), fresh.SelfInstalls())
	if len(plan.Human) > 0 || len(plan.Steps) == 0 {
		return
	}
	if onlyTestBoot(plan) {
		a := d.Runner.Begin(plan, spec.GPUs, ByAgent)
		last, err := Load(d.Runner.Host, d.Runner.DataDir)
		if due, at := Due(last, a.Key, d.Runner.now(), false); err == nil && !due && at.IsZero() {
			d.keepTest(ctx, fresh, true, justStarted)
			return
		}
	}
	// The setup's own rules: an attempt, or its retry.
	d.Run(ctx)
}

// keepTest runs one test boot on fresh's machine: needed says the machine is
// not ready without it (else it is the running version's full retest).
func (d *Daemon) keepTest(ctx context.Context, fresh *provisioner.Provisioner, needed, justStarted bool) {
	if fresh.IsContainer() {
		d.keepContainerTest(ctx, fresh)
		return
	}
	h := d.Runner.Host
	rt := fresh.Runtime()
	spec := rt.Spec()
	res, err := vmrt.LoadSelfTest(h, spec.DataDir)
	if err != nil {
		res = nil
	}
	now := d.Runner.now()
	gpuFailed := vmrt.GPUTestFailed(res, rt.Fingerprint(d.Runner.Version))
	if needed && !justStarted && res != nil {
		// A failed test waits for its retry.
		if !res.Passed && now.Before(time.Unix(res.At, 0).Add(RetryAfter)) {
			return
		}
		if gpuFailed && now.Before(time.Unix(res.LastFull.At, 0).Add(RetryAfter)) {
			return
		}
	}
	plan := vmrt.PlanTest(h, spec, false)
	switch {
	case plan.Wait != "":
		return
	case plan.NoGPU && !needed:
		return // the retest is the full one: it waits for the GPU
	case plan.NoGPU && gpuFailed:
		return // only a full test lifts a failed one
	}

	release, err := vmrt.AcquireBusy(h, d.Runner.DataDir, d.PID, "running a test rental")
	if err != nil {
		return
	}
	defer release()
	if !d.Prov.BeginSetup() {
		return
	}
	defer d.Prov.EndSetup()
	d.Prov.Adopt(fresh)
	line := KeepLine(plan, spec.DesktopOnDemand, now)
	d.say("%s", line)
	d.show(fresh.Capability(), line)

	out := d.Runner.testBoot(ctx, rt, plan.TestOptions)
	if out.InUse != "" && needed && !plan.NoGPU && !gpuFailed {
		// The host took the GPU back as the test came to it: without it, then.
		plan.NoGPU = true
		out = d.Runner.testBoot(ctx, rt, plan.TestOptions)
	}
	d.Prov.EndSetup()
	after := d.Runner.Detect()
	d.Prov.Adopt(after)
	switch {
	case ctx.Err() != nil:
		d.say("The test rental stopped with the agent")
		return
	case out.InUse != "":
		// Nothing was recorded; the next look tries again.
		d.say("The test rental did not run: %s", out.InUse)
	case out.Passed && plan.NoGPU:
		d.say("Test rental passed without the GPU, which is in use; the GPU's handover is tested when it is free, and always before a rental starts")
	case out.Passed:
		d.say("Test rental passed with the GPU handed over: this agent version is fully tested on this machine")
	default:
		d.warn("Test rental failed: %s", strings.Join(out.Problems, "; "))
	}
	if out.Passed && d.Errors != nil {
		d.Errors.Resolve(control.AreaTestBoot, "")
	}
	// A failed test is on the report as the reason the machine is not ready
	// (the agent syncs the hosting checks' findings with every report).
	d.report(after.Capability())
}

// keepContainerTest runs one container test boot: the container test always
// runs (it shares the GPU rather than taking it, so it never waits for the host).
func (d *Daemon) keepContainerTest(ctx context.Context, fresh *provisioner.Provisioner) {
	crt := fresh.ContainerRuntime()
	if crt == nil || crt.Present() {
		return
	}
	release, err := vmrt.AcquireBusy(d.Runner.Host, d.Runner.DataDir, d.PID, "running a test rental")
	if err != nil {
		return
	}
	defer release()
	if !d.Prov.BeginSetup() {
		return
	}
	defer d.Prov.EndSetup()
	d.Prov.Adopt(fresh)
	line := "Checking this machine with a test rental container (started " + clock(d.Runner.now().Unix()) + "). Nothing to do; this takes about a minute."
	d.say("%s", line)
	d.show(fresh.Capability(), line)

	res := crt.SelfTestContext(ctx, d.Runner.Version)
	d.Prov.EndSetup()
	after := d.Runner.Detect()
	d.Prov.Adopt(after)
	switch {
	case ctx.Err() != nil:
		d.say("The test rental stopped with the agent")
		return
	case res.Passed:
		d.say("Test rental passed: this machine is ready to host container rentals")
		if d.Errors != nil {
			d.Errors.Resolve(control.AreaTestBoot, "")
		}
	default:
		d.warn("Test rental failed: %s", strings.Join(res.Problems, "; "))
	}
	d.report(after.Capability())
}

// KeepLine is what a host reads while Keep's test boot runs.
func KeepLine(plan vmrt.TestPlan, desktopOnDemand bool, start time.Time) string {
	if plan.NoGPU {
		return fmt.Sprintf("Checking this machine with a test rental, without its GPU, which is in use (started %s). "+
			"Nothing to do; this takes about 5-15 minutes. The GPU's handover is tested when it is free, and always before a rental starts.",
			clock(start.Unix()))
	}
	line := fmt.Sprintf("Checking this machine with a test rental (started %s). Nothing to do; this takes about 5-15 minutes.", clock(start.Unix()))
	if desktopOnDemand {
		line += " This machine's login screen closes during the test and comes back after it."
	}
	return line
}
