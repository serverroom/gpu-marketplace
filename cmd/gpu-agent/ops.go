package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/agenterrors"
	"github.com/serverroom/gpu-marketplace/internal/autosetup"
	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/register"
	"github.com/serverroom/gpu-marketplace/internal/update"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// What the agent tells the marketplace about itself (CONTRACT-ops A): its
// recent problems with every capability report, a report at once when a new
// problem appears (at most one a minute), and a report every ReportInterval
// whatever happens -- whose answer names the release this machine should run,
// for the automatic update.

// ReportInterval is how often the capability is reported with nothing new.
var ReportInterval = 30 * time.Minute

// minReportGap is the least time between two reports a new problem asks for.
var minReportGap = time.Minute

// opsState is the agent's reporting state.
type opsState struct {
	errs *agenterrors.Log
	kick chan struct{}

	mu         sync.Mutex
	lastReport time.Time
}

func newOps() *opsState {
	return &opsState{errs: agenterrors.Open(config.DataDir()), kick: make(chan struct{}, 1)}
}

// kickReport asks for a report soon (reportLoop keeps them a minute apart).
func (a *gpuAgent) kickReport() {
	if a.ops == nil {
		return
	}
	select {
	case a.ops.kick <- struct{}{}:
	default:
	}
}

// reportLoop reports the capability every ReportInterval, and when asked to.
func (a *gpuAgent) reportLoop(ctx context.Context) {
	tick := time.NewTicker(ReportInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-a.ops.kick:
			a.ops.mu.Lock()
			wait := minReportGap - time.Since(a.ops.lastReport)
			a.ops.mu.Unlock()
			if wait > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
			}
		}
		if _, err := a.report(a.prov.Capability()); err != nil && !errors.Is(err, register.ErrNotRegistered) {
			a.warn("report hosting capability: %v", err)
		}
	}
}

// decorate adds what the capability report carries about the agent itself:
// its problems, its automatic updates, and where its setup stands.
func (a *gpuAgent) decorate(c control.Capability) control.Capability {
	if a.ops != nil {
		a.noteUpdateRecord()
		a.syncFindings()
		c.Errors = a.ops.errs.Entries()
	}
	if a.host != nil {
		c.AutoUpdate = a.host.AutoUpdateStatus()
	}
	if last, err := autosetup.Load(vmrt.OSHost{}, config.DataDir()); err == nil && last != nil {
		c.Setup = autosetup.Progress(last, a.prov != nil && a.prov.SettingUp())
	}
	return c
}

// report sends a capability; an answer of 410 Gone means the host removed the
// machine in the control panel, and the agent stops hosting as if told so.
// The answer's agent_update is acted on (when the machine is idle).
func (a *gpuAgent) report(c control.Capability) (*register.CapabilityResponse, error) {
	c = a.decorate(c)
	if a.ops != nil {
		a.ops.mu.Lock()
		a.ops.lastReport = time.Now()
		a.ops.mu.Unlock()
	}
	resp, err := register.ReportCapability(c)
	a.reported(err)
	var ee *register.EndpointError
	if errors.As(err, &ee) && ee.Code == http.StatusGone && a.host != nil {
		a.host.Gone()
	}
	if err == nil && a.host != nil {
		a.host.Heard()
		if resp != nil && resp.AgentUpdate != nil {
			if u := control.ParseUpdateOffer(fmt.Sprintf(`{"version":%q,"push":%v}`, resp.AgentUpdate.Version, resp.AgentUpdate.Push)); u != nil {
				if started, why := a.host.Offer(u); !started && why != "" {
					a.say("Agent %s is available; not updating yet: %s", u.Version, why)
				}
			}
		}
	}
	return resp, err
}

// reported records how a report went: a failure stays an active problem
// until a report gets through, which then carries it as resolved.
func (a *gpuAgent) reported(err error) {
	if a.ops == nil {
		return
	}
	errs := a.ops.errs
	var ee *register.EndpointError
	switch {
	case err == nil:
		errs.Resolve(control.AreaReport, "")
		errs.Resolve(control.AreaRegister, "")
	case errors.Is(err, register.ErrNotRegistered):
	case errors.As(err, &ee) && ee.Code == http.StatusGone:
		errs.Raise(control.AreaRegister, register.WithdrawnMessage, "")
	case errors.As(err, &ee) && (ee.Code == http.StatusUnauthorized || ee.Code == http.StatusForbidden):
		errs.Raise(control.AreaRegister, fmt.Sprintf("the marketplace no longer accepts this machine's registration (HTTP %d): "+
			"register it again with a new code from your dashboard ('sudo gpu-agent register --code <code>')", ee.Code), ee.Body)
	default:
		errs.Raise(control.AreaReport, "the agent could not report this machine to the marketplace; it keeps trying", err.Error())
	}
}

// syncFindings makes the preflight and test-boot problems exactly what the
// hosting checks found last -- not while the automatic setup is working on
// them, and not on a withdrawn machine.
func (a *gpuAgent) syncFindings() {
	if a.ops == nil || a.prov == nil || a.prov.SettingUp() || a.prov.Withdrawn() {
		return
	}
	var pre, boot []agenterrors.Problem
	for _, f := range a.prov.Findings() {
		if f.Kind == provisioner.ReasonTestBoot {
			boot = append(boot, agenterrors.Problem{Message: f.Text})
		} else {
			pre = append(pre, agenterrors.Problem{Message: f.Text})
		}
	}
	a.ops.errs.Sync(control.AreaPreflight, pre)
	a.ops.errs.Sync(control.AreaTestBoot, boot)
}

// noteUpdateRecord adds an update that failed or was rolled back (update.json)
// to the problems, once.
func (a *gpuAgent) noteUpdateRecord() {
	r := update.Load(config.DataDir())
	if r == nil || (r.Status != update.StatusFailed && r.Status != update.StatusRolledBack) {
		return
	}
	at := r.FinishedAt
	if at == 0 {
		at = r.StartedAt
	}
	what := "failed; this version kept running"
	if r.Status == update.StatusRolledBack {
		what = "was rolled back to the previous version"
	}
	a.ops.errs.NoteAt(at, control.AreaUpdate, fmt.Sprintf("the agent update from %s to %s %s", r.From, r.To, what), r.Error)
}

// tunnelWatch records a relay tunnel that keeps failing.
type tunnelWatch struct {
	errs *agenterrors.Log
	host string
}

func (w tunnelWatch) Failing(since time.Time, err error, detail string) {
	w.errs.Raise(control.AreaTunnel, fmt.Sprintf("the agent cannot keep its connection to the marketplace relay (%s) open, since %s; "+
		"rentals cannot reach this machine until it can. It keeps retrying: check this machine's internet connection and firewall (outgoing SSH)",
		w.host, since.UTC().Format("2006-01-02 15:04 UTC")), fmt.Sprintf("%v\n%s", err, detail))
}

func (w tunnelWatch) Up() { w.errs.Resolve(control.AreaTunnel, "") }

// noteCommand records a problem a command run by hand hit, for the daemon's
// next report.
func noteCommand(area, message, detail string) {
	agenterrors.Open(config.DataDir()).Note(area, message, detail)
}

// printProblems lists, for `status`, the problems that still stop this
// machine (errors.json), as the marketplace sees them.
func printProblems() {
	var active []control.AgentError
	for _, e := range agenterrors.Open(config.DataDir()).Entries() {
		if e.Active {
			active = append(active, e)
		}
	}
	if len(active) == 0 {
		return
	}
	fmt.Println("Problems (as reported to the marketplace):")
	for _, e := range active {
		fmt.Printf("  - [%s] %s\n", e.Area, e.Message)
	}
}
