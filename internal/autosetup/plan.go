// Package autosetup finishes a machine's setup by itself once a host has
// linked it: it installs the rental runtime's packages, builds the rental
// image and runs the test boot -- the three steps a host used to have to type
// -- whenever those are the only things between the machine and ready.
// Anything a person has to fix (no KVM, the IOMMU off, a GPU sharing its
// group, too little memory) is left to a person and never retried in a loop.
//
// The daemon runs it in the background after a successful capability report
// (Daemon); `gpu-agent setup` runs the same steps in the foreground (Runner).
package autosetup

import (
	"github.com/serverroom/gpu-marketplace/internal/provisioner"
)

// Step is one part of the setup.
type Step string

const (
	StepDeps     Step = "deps"
	StepImage    Step = "image"
	StepTestBoot Step = "test-boot"
)

// order is the order steps run in: packages, then the image built with them,
// then the proof that the machine can host.
var order = []Step{StepDeps, StepImage, StepTestBoot}

// Doing is the step in words, as the host reads it in the panel.
func (s Step) Doing() string {
	switch s {
	case StepDeps:
		return "installing the rental runtime (QEMU, UEFI firmware, cloud-image-utils, cryptsetup, nftables)"
	case StepImage:
		return "building the rental image"
	case StepTestBoot:
		return "running a test rental"
	}
	return string(s)
}

// Takes is how long the step usually takes.
func (s Step) Takes() string {
	switch s {
	case StepDeps:
		return "a few minutes"
	case StepImage:
		return "20-30 minutes"
	case StepTestBoot:
		return "5-15 minutes"
	}
	return "a while"
}

// Plan is what the automatic setup would do on this machine, and what it
// cannot.
type Plan struct {
	Steps []Step
	// Human are the reasons only a person can fix. The setup does not run
	// while there is any: it would end at the same reason.
	Human []string
}

// Eligible reports whether the setup can make this machine ready by itself.
func (p Plan) Eligible() bool { return len(p.Human) == 0 && len(p.Steps) > 0 }

// PlanFor turns preflight's findings into steps. Missing packages are the
// setup's to install only where apt-get is (aptGet); a finished setup is
// always proven by a test boot, so any step brings the test boot with it.
func PlanFor(findings []provisioner.Finding, aptGet bool) Plan {
	var plan Plan
	need := map[Step]bool{}
	for _, f := range findings {
		switch {
		case f.Kind == provisioner.ReasonTools && aptGet:
			need[StepDeps] = true
		case f.Kind == provisioner.ReasonImage:
			need[StepImage] = true
		case f.Kind == provisioner.ReasonTestBoot:
			need[StepTestBoot] = true
		default:
			plan.Human = append(plan.Human, f.Text)
		}
	}
	if len(need) > 0 {
		need[StepTestBoot] = true
	}
	for _, s := range order {
		if need[s] {
			plan.Steps = append(plan.Steps, s)
		}
	}
	return plan
}
