package autosetup

import (
	"fmt"
	"strings"
	"time"
)

// These lines are what a host reads in the control panel while the setup
// runs, or after it failed: one plain line in place of the machine's reasons.

func clock(unix int64) string { return time.Unix(unix, 0).UTC().Format("15:04") + " UTC" }

// ProgressLine is the one line a host sees while a step runs.
func ProgressLine(a Attempt, desktopOnDemand bool) string {
	s := a.CurrentStep()
	line := fmt.Sprintf("Setting up automatically: %s (step %d of %d, started %s). Nothing to do; this takes about %s.",
		s.Doing(), a.Step, len(a.Steps), clock(a.StepStartedAt), s.Takes())
	if s == StepTestBoot && desktopOnDemand {
		line += " This machine's desktop closes during the test and comes back after it."
	}
	return line
}

// detailsCommand is where a host (or support) sees what went wrong: the test
// boot's own report for a test boot, the setup run in the foreground for the
// rest.
func detailsCommand(s Step) string {
	if s == StepTestBoot {
		return "'sudo gpu-agent check --boot'"
	}
	return "'sudo gpu-agent setup'"
}

// FailureLine is the one line a host sees after an attempt failed.
func FailureLine(a Attempt) string {
	s := a.CurrentStep()
	return fmt.Sprintf("Automatic setup failed while %s (step %d of %d): %s. "+
		"The agent tries again after its next restart or in 6 hours; %s shows the details.",
		s.Doing(), a.Step, len(a.Steps), Shorten(strings.TrimSuffix(a.Error, "."), 300), detailsCommand(s))
}

// WaitingLine is the one line a host sees while a failed or interrupted
// attempt waits for its retry.
func WaitingLine(a Attempt, retryAt time.Time) string {
	if a.Finished() {
		return FailureLine(a)
	}
	s := a.CurrentStep()
	return fmt.Sprintf("Automatic setup was interrupted while %s (step %d of %d, started %s); "+
		"the agent tries again at %s. 'sudo gpu-agent setup' runs it now.",
		s.Doing(), a.Step, len(a.Steps), clock(a.StartedAt), retryAt.UTC().Format("2006-01-02 15:04 UTC"))
}

// Describe is the last attempt for `gpu-agent setup --status` and `status`.
func Describe(a *Attempt) string {
	if a == nil {
		return "has not run on this machine"
	}
	when := func(unix int64) string { return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04 UTC") }
	s := a.CurrentStep()
	switch {
	case !a.Finished():
		return fmt.Sprintf("started %s by the %s: %s (step %d of %d, since %s), not finished",
			when(a.StartedAt), a.By, s.Doing(), a.Step, len(a.Steps), clock(a.StepStartedAt))
	case a.Passed:
		return fmt.Sprintf("passed %s (run by the %s, agent %s, rental image driver %s)", when(a.FinishedAt), a.By, a.AgentVersion, a.Driver)
	}
	return fmt.Sprintf("failed %s (run by the %s) while %s (step %d of %d): %s", when(a.FinishedAt), a.By, s.Doing(), a.Step, len(a.Steps), a.Error)
}

// Shorten collapses whitespace and keeps a long message's start and end --
// an apt failure's cause is in its last line -- within max characters.
func Shorten(s string, max int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= max {
		return string(r)
	}
	head := max / 3
	tail := max - head - 5
	return string(r[:head]) + " ... " + string(r[len(r)-tail:])
}
