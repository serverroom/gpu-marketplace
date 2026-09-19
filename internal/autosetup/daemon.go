package autosetup

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// Daemon is the automatic setup inside the running agent. Run is called once
// the machine's capability has been reported, and finishes the setup when the
// only things between the machine and ready are steps the agent can take.
type Daemon struct {
	Runner    Runner
	GOOS      string
	ConfigDir string
	PID       int
	// Prov is the agent's provisioner: what the control channel answers from.
	Prov *provisioner.Provisioner
	// Report sends a capability to the marketplace (register.ReportCapability).
	Report    func(control.Capability) error
	Say, Warn func(format string, args ...interface{})
	// After waits; time.After unless a test replaces it.
	After func(d time.Duration) <-chan time.Time
	// Errors records a failed attempt for the marketplace's problem list, and
	// clears it once an attempt passes (agenterrors.Log); nil: nothing.
	Errors Problems

	mu      sync.Mutex
	running bool
	started bool
}

// Problems is where the setup's failures go (agenterrors.Log).
type Problems interface {
	Raise(area, message, detail string) bool
	Resolve(area, message string)
}

// SetupOffProblem is the problem a machine reports while its automatic setup
// is off and the setup is all that keeps it off the market.
const SetupOffProblem = "Automatic setup is off on this machine, so the agent does not install the rental runtime, " +
	"build the rental image or run the test rental by itself; 'sudo gpu-agent setup --on' turns it on, " +
	"and 'sudo gpu-agent setup' runs the setup now"

// AreaOf is the problem area a failed step belongs to: the test boot's own,
// or the setup's (installing packages, building the image with its driver).
func AreaOf(s Step) string {
	if s == StepTestBoot {
		return control.AreaTestBoot
	}
	return control.AreaSetup
}

// Progress is the last attempt for the capability report's setup field; nil
// when the setup never ran here. running says the agent is in the middle of
// one now (an unfinished record without it was interrupted).
func Progress(a *Attempt, running bool) *control.SetupProgress {
	if a == nil || len(a.Steps) == 0 {
		return nil
	}
	p := &control.SetupProgress{Step: string(a.CurrentStep())}
	switch {
	case !a.Finished() && running:
		p.State, p.Message, p.At = "running", ProgressLine(*a, false), a.StepStartedAt
	case !a.Finished():
		p.State, p.Message, p.At = "interrupted", "Automatic setup "+Describe(a), a.StepStartedAt
	case a.Passed:
		p.State, p.Message, p.At = "passed", "Automatic setup "+Describe(a), a.FinishedAt
	default:
		p.State, p.Message, p.At = "failed", FailureLine(*a), a.FinishedAt
	}
	p.Message = Shorten(p.Message, 300)
	return p
}

func (d *Daemon) say(format string, args ...interface{}) {
	if d.Say != nil {
		d.Say(format, args...)
	}
}

func (d *Daemon) warn(format string, args ...interface{}) {
	if d.Warn != nil {
		d.Warn(format, args...)
	}
}

func (d *Daemon) report(c control.Capability) {
	if d.Report == nil {
		return
	}
	if err := d.Report(c); err != nil {
		d.warn("report hosting capability: %v", err)
	}
}

// show makes line the one reason this machine reports, and reports it.
func (d *Daemon) show(base control.Capability, line string) {
	base.Ready = false
	base.Reasons = []string{line}
	d.Prov.SetCapability(base)
	d.report(d.Prov.Capability())
}

// Run evaluates the machine, runs the setup when it is due, and after a
// failure waits for the retry -- until the machine is ready, needs a person,
// or ctx ends (the agent is stopping). It never runs twice at once.
func (d *Daemon) Run(ctx context.Context) {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return
	}
	d.running = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.running = false
		d.mu.Unlock()
	}()

	after := d.After
	if after == nil {
		after = time.After
	}
	for ctx.Err() == nil {
		retryAt, again := d.once(ctx)
		if !again {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-after(retryAt.Sub(d.Runner.now())):
		}
	}
}

// once is one look at the machine and, when due, one attempt. again asks Run
// to come back at retryAt.
func (d *Daemon) once(ctx context.Context) (retryAt time.Time, again bool) {
	d.mu.Lock()
	justStarted := !d.started
	d.started = true
	d.mu.Unlock()

	if d.GOOS != "linux" {
		return
	}
	if !Enabled(d.ConfigDir) {
		d.say("Automatic setup is off (%s says off); 'sudo gpu-agent setup --on' turns it on", OptOutPath(d.ConfigDir))
		// What keeps the machine off the market must reach the marketplace,
		// not only this log.
		if d.Errors != nil && d.Prov.Status() == provisioner.StatusFree && !d.Prov.Withdrawn() {
			fresh := d.Runner.Detect()
			if !fresh.Capability().Ready && PlanFor(fresh.Findings(), AptGet(d.Runner.Host)).Eligible() {
				d.Errors.Raise(control.AreaSetup, SetupOffProblem, "")
			} else {
				d.Errors.Resolve(control.AreaSetup, SetupOffProblem)
			}
		}
		return
	}
	if d.Errors != nil {
		d.Errors.Resolve(control.AreaSetup, SetupOffProblem)
	}
	if d.Prov.Status() != provisioner.StatusFree || d.Prov.Withdrawn() {
		return // a rental or its leftover (never beside one), or removed from the marketplace
	}
	fresh := d.Runner.Detect()
	spec, ok := fresh.RuntimeSpec()
	if !ok || fresh.RentalPresent() {
		return
	}
	if fresh.Capability().Ready {
		// Ready already -- perhaps a person ran the steps meanwhile.
		if !d.Prov.Capability().Ready && d.Prov.Adopt(fresh) {
			d.report(fresh.Capability())
		}
		return
	}
	plan := PlanFor(fresh.Findings(), AptGet(d.Runner.Host))
	if len(plan.Human) > 0 {
		d.say("Automatic setup cannot make this machine ready by itself; a person has to fix: %s", strings.Join(plan.Human, "; "))
		return
	}
	if len(plan.Steps) == 0 {
		return
	}

	a := d.Runner.Begin(plan, spec.GPUs, ByAgent)
	last, err := Load(d.Runner.Host, d.Runner.DataDir)
	if err != nil {
		d.warn("read the last automatic setup (%v); starting a new one", err)
		last = nil
	}
	if due, at := Due(last, a.Key, d.Runner.now(), justStarted); !due {
		if at.IsZero() {
			if onlyTestBoot(plan) {
				// Keep runs it, when it interrupts nothing of the host's.
				d.say("Automatic setup already passed for this agent version and driver; the test rental this machine needs runs when the machine allows it")
				return
			}
			d.say("Automatic setup already passed for this agent version and driver; what is left needs a person")
			return
		}
		if d.Prov.Adopt(fresh) {
			d.show(fresh.Capability(), WaitingLine(*last, at))
		}
		d.say("Automatic setup tries again at %s", at.UTC().Format("2006-01-02 15:04 UTC"))
		return at, true
	}

	release, err := vmrt.AcquireBusy(d.Runner.Host, d.Runner.DataDir, d.PID, "setting this machine up")
	if err != nil {
		d.say("Automatic setup not started: %v", err)
		return
	}
	defer release()
	if !d.Prov.BeginSetup() {
		return
	}
	defer d.Prov.EndSetup()
	d.Prov.Adopt(fresh)
	base := fresh.Capability()
	desktopOnDemand := spec.DesktopOnDemand

	d.say("Automatic setup: %d step(s) to make this machine ready; %s", len(plan.Steps), a.DriverLine())
	runner := d.Runner
	runner.OnStep = func(a Attempt) {
		line := ProgressLine(a, desktopOnDemand)
		d.say("%s", line)
		d.show(base, line)
	}
	a = runner.Run(ctx, a)

	if ctx.Err() != nil {
		d.say("Automatic setup stopped with the agent: %s", a.Error)
		return
	}
	after := d.Runner.Detect()
	d.Prov.EndSetup()
	d.Prov.Adopt(after)
	if a.Passed {
		if d.Errors != nil {
			d.Errors.Resolve(control.AreaSetup, "")
			d.Errors.Resolve(control.AreaTestBoot, "")
		}
		d.say("Automatic setup finished: this machine is ready")
		d.report(after.Capability())
		return
	}
	line := FailureLine(a)
	if d.Errors != nil {
		d.Errors.Raise(AreaOf(a.CurrentStep()), line, a.Error)
	}
	d.warn("%s", line)
	d.show(after.Capability(), line)
	return time.Unix(a.FinishedAt, 0).Add(RetryAfter), true
}
