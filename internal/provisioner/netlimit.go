package provisioner

import (
	"context"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// netAccountant is a runtime that counts its rental's monthly data allowance
// (vmrt/netlimit.go).
type netAccountant interface {
	AccountNet(now time.Time) (*vmrt.NetUsage, error)
}

// KeepNetLimits holds a running rental to the host's monthly data allowance
// until ctx ends: every vmrt.NetLimitEvery the rental's use this month is
// counted, and it is slowed once the month's allowance is used and given its
// own speed back when the next month of the rental begins. The speed limits
// themselves are the kernel's from the rental's start; this loop only moves
// them. log says when a rental is slowed, and a problem once until it clears.
func (p *Provisioner) KeepNetLimits(ctx context.Context, log func(format string, args ...interface{})) {
	lastProblem := ""
	slowed := ""
	for {
		p.mu.Lock()
		a, ok := p.machine.(netAccountant)
		ok = ok && p.status == StatusRented
		p.mu.Unlock()
		if ok {
			u, err := a.AccountNet(time.Now())
			problem := ""
			switch {
			case err != nil:
				problem = err.Error()
			case u != nil:
				problem = u.Problem
			}
			if problem != lastProblem && problem != "" && log != nil {
				log("Rental bandwidth limit: %s", problem)
			}
			lastProblem = problem
			if month := monthKey(u); u != nil && u.Slowed && slowed != month {
				slowed = month
				if log != nil {
					log("Rental %s used this month's data allowance; it runs at the slower speed until %s",
						u.RentalID, time.Unix(u.PeriodStart, 0).Add(vmrt.NetMonth).UTC().Format("2 January 2006 15:04 UTC"))
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(vmrt.NetLimitEvery):
		}
	}
}

// monthKey names a rental's month of allowance, so that it is said once a month
// that the rental was slowed.
func monthKey(u *vmrt.NetUsage) string {
	if u == nil {
		return ""
	}
	return u.RentalID + "/" + time.Unix(u.PeriodStart, 0).UTC().Format(time.RFC3339)
}

// netLimitsCapability is Capability.NetLimits: what this machine's last test
// boot found when it held its test rental to a bandwidth limit.
func netLimitsCapability(h vmrt.Host, spec vmrt.Spec) *control.NetLimits {
	rec := vmrt.NetLimitVerdict(h, spec.DataDir, spec.AgentVersion)
	if rec == nil {
		return &control.NetLimits{Pending: true, Why: "not proven yet: the next test boot tries it"}
	}
	return &control.NetLimits{Usable: rec.Usable, Why: rec.Why}
}
