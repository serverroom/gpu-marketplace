package autosetup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/netguard"
	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// Runner runs a plan's steps, recording each in the attempt record. The
// daemon and `gpu-agent setup` share it, so both do exactly the same thing.
type Runner struct {
	Host    vmrt.Host
	Arch    string
	DataDir string
	Version string
	// Detect re-reads the machine: provisioner.Detect in production.
	Detect func() *provisioner.Provisioner
	Log    func(format string, args ...interface{})
	Now    func() time.Time
	// OnStep runs as each step starts, once the record says so.
	OnStep func(a Attempt)

	// The steps. Nil ones are the real thing (tests replace them).
	InstallDeps func(ctx context.Context) error
	BuildImage  func(ctx context.Context, spec vmrt.Spec, drv vmrt.DriverChoice) error
	TestBoot    func(ctx context.Context, rt *vmrt.Runtime) vmrt.SelfTestResult
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) log(format string, args ...interface{}) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

// AptGet reports whether this machine installs packages with apt-get.
func AptGet(h vmrt.Host) bool {
	_, err := h.LookPath("apt-get")
	return err == nil
}

// Begin is a new attempt at plan: the driver the image gets (matched to the
// host's), and the key it is for.
func (r *Runner) Begin(plan Plan, gpus []string, by string) Attempt {
	drv := vmrt.ChooseImageDriver(r.Host)
	steps := make([]string, len(plan.Steps))
	for i, s := range plan.Steps {
		steps[i] = string(s)
	}
	return Attempt{
		Key:          Key(r.Version, drv, r.Arch, gpus),
		AgentVersion: r.Version,
		Driver:       drv.Driver,
		DriverSource: drv.Source,
		HostDriver:   drv.Describe(),
		Steps:        steps,
		StartedAt:    r.now().Unix(),
		By:           by,
	}
}

func (a Attempt) driverChoice() vmrt.DriverChoice {
	return vmrt.DriverChoice{Driver: a.Driver, Source: a.DriverSource}
}

// Run runs the attempt's steps in order and returns it finished: Passed when a
// fresh look at the machine afterwards says ready, else Error says why. It is
// saved as each step starts and when it ends. A cancelled ctx stops it at the
// next point where the machine is whole (a build or test boot tears its VM
// down first).
func (r *Runner) Run(ctx context.Context, a Attempt) (out Attempt) {
	save := func() {
		if err := Save(r.Host, r.DataDir, a); err != nil {
			r.log("could not record the setup's progress: %v", err)
		}
	}
	finish := func(err error) Attempt {
		a.FinishedAt = r.now().Unix()
		if err != nil {
			a.Passed, a.Error = false, err.Error()
		} else {
			a.Passed, a.Error = true, ""
		}
		save()
		return a
	}
	defer func() {
		// A bug in a step must not take the agent down with it, nor leave an
		// attempt that looks like it is still running.
		if p := recover(); p != nil {
			out = finish(fmt.Errorf("the setup stopped on an internal error: %v", p))
		}
	}()

	for i, s := range a.Steps {
		step := Step(s)
		if ctx.Err() != nil {
			return finish(errors.New("the agent stopped before " + step.Doing()))
		}
		a.Step = i + 1
		a.StepStartedAt = r.now().Unix()
		save()
		if r.OnStep != nil {
			r.OnStep(a)
		}
		r.log("Step %d of %d: %s ...", a.Step, len(a.Steps), step.Doing())
		if err := r.step(ctx, step, a); err != nil {
			if ctx.Err() != nil {
				err = fmt.Errorf("the agent stopped while %s", step.Doing())
			}
			return finish(err)
		}
	}
	after := r.Detect()
	if c := after.Capability(); !c.Ready {
		return finish(fmt.Errorf("the setup finished, but this machine is still not ready: %s", strings.Join(c.Reasons, "; ")))
	}
	return finish(nil)
}

func (r *Runner) step(ctx context.Context, s Step, a Attempt) error {
	switch s {
	case StepDeps:
		install := r.InstallDeps
		if install == nil {
			install = func(context.Context) error { return vmrt.InstallPackages(r.Host, r.Arch, r.Log) }
		}
		if err := install(ctx); err != nil {
			return err
		}
		if missing := vmrt.MissingTools(r.Host, r.Arch); len(missing) > 0 {
			return fmt.Errorf("the packages were installed, but %s is still missing", strings.Join(missing, ", "))
		}
		if _, ok := vmrt.FindFirmware(r.Host, r.Arch); !ok {
			return fmt.Errorf("the packages were installed, but no UEFI firmware for %s was found", r.Arch)
		}
		return nil

	case StepImage:
		build := r.BuildImage
		if build == nil {
			build = func(ctx context.Context, spec vmrt.Spec, drv vmrt.DriverChoice) error {
				fence := netguard.New(r.Host, netguard.Bridge, vmrt.GuestSubnet, netguard.HostNetworks)
				return vmrt.Prepare(r.Host, spec, fence, r.Version, vmrt.PrepareOptions{
					Driver: drv.Driver, DriverSource: drv.Source, Ctx: ctx, Log: r.Log,
				})
			}
		}
		rt := r.Detect().Runtime()
		if rt == nil {
			return errors.New("the rental runtime could not be read")
		}
		return build(ctx, rt.Spec(), a.driverChoice())

	case StepTestBoot:
		p := r.Detect()
		var blocking []string
		for _, f := range p.Findings() {
			if f.Kind != provisioner.ReasonTestBoot {
				blocking = append(blocking, f.Text)
			}
		}
		if len(blocking) > 0 {
			return fmt.Errorf("this machine cannot run its test boot yet: %s", strings.Join(blocking, "; "))
		}
		rt := p.Runtime()
		if rt == nil {
			return errors.New("the rental runtime could not be read")
		}
		if rt.Present() {
			return errors.New("a rental (or the leftover of one) is on this machine")
		}
		boot := r.TestBoot
		if boot == nil {
			boot = func(ctx context.Context, rt *vmrt.Runtime) vmrt.SelfTestResult {
				return rt.SelfTestContext(ctx, r.Version)
			}
		}
		res := boot(ctx, rt)
		if !res.Passed {
			return fmt.Errorf("the test boot failed: %s", strings.Join(res.Problems, "; "))
		}
		if len(res.GuestGPUs) > 0 {
			r.log("Test boot passed: the VM saw %s.", strings.Join(res.GuestGPUs, "; "))
		} else {
			r.log("Test boot passed.")
		}
		return nil
	}
	return fmt.Errorf("unknown setup step %q", s)
}
