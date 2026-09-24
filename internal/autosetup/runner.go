package autosetup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/config"
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

	// The steps. Nil ones are the real thing (tests replace them). A test
	// boot is given how it may run without interrupting the host (without
	// the GPU while the host's programs hold it, in a smaller VM while the
	// host uses the memory).
	InstallDeps func(ctx context.Context) error
	// Machine is where a container rental runs when that is not Host, and
	// MachineDataDir the agent's data directory there: on Windows, the WSL 2
	// distribution (Host stays Windows, where the setup's records live).
	Machine        vmrt.Host
	MachineDataDir string
	// PrepareMachine runs first in the tools step: on Windows, WSL 2, the
	// agent's account and its distribution (wsl.Setup).
	PrepareMachine func(ctx context.Context) error
	MoveStorage    func(dir string) error
	BuildImage     func(ctx context.Context, spec vmrt.Spec, drv vmrt.DriverChoice) error
	TestBoot       func(ctx context.Context, rt *vmrt.Runtime, o vmrt.TestOptions) vmrt.SelfTestResult
}

// moveStorage puts the rentals' disks (and the base image, if built) on the
// disk preflight found with room, and records it as `setup --data-dir` would:
// the rest of the setup, and every rental after, uses it.
func (r *Runner) moveStorage() error {
	var dir string
	for _, f := range r.Detect().Findings() {
		if f.Kind == provisioner.ReasonStorage {
			dir = f.Dir
		}
	}
	if dir == "" {
		return nil // the disks fit where they are now
	}
	if r.MoveStorage != nil {
		return r.MoveStorage(dir)
	}
	target, err := provisioner.StorageTarget(r.Host, dir)
	if err != nil {
		return err
	}
	from := provisioner.StorageDir(r.DataDir)
	if err := provisioner.MoveStorage(r.Host, from, target); err != nil {
		return err
	}
	if err := config.SetStorageDir(target); err != nil {
		return fmt.Errorf("could not record %s as where the rentals' disks go: %w", target, err)
	}
	if r.Log != nil {
		r.Log("the rentals' disks and the base image now live in %s", target)
	}
	return nil
}

// testBoot runs one test boot as o says.
func (r *Runner) testBoot(ctx context.Context, rt *vmrt.Runtime, o vmrt.TestOptions) vmrt.SelfTestResult {
	if r.TestBoot != nil {
		return r.TestBoot(ctx, rt, o)
	}
	return rt.SelfTestWith(ctx, r.Version, o)
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

// AptGet reports whether this machine installs packages the agent's way:
// apt-get on Ubuntu, Debian or a system built on them (Armbian included).
func AptGet(h vmrt.Host) bool { return provisioner.AptDistro(h) }

// CanInstall reports whether the setup can install p's missing tools: with
// apt-get on a Linux host, or always on a machine whose setup brings its own
// Linux (Windows, WSL 2).
func CanInstall(h vmrt.Host, p *provisioner.Provisioner) bool {
	return p.SelfInstalls() || AptGet(h)
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

// DriverLine is the attempt's driver choice in words.
func (a Attempt) DriverLine() string {
	if a.Driver == vmrt.NoDriver {
		return "this machine has no NVIDIA GPU, so the rental image gets no NVIDIA driver"
	}
	return fmt.Sprintf("the rental image gets NVIDIA driver %s (this machine runs %s)", a.Driver, a.HostDriver)
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
	if s == StepStorage {
		return r.moveStorage()
	}
	if r.Detect().IsContainer() {
		return r.containerStep(ctx, s, a)
	}
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
		// The host keeps its programs and its desktop: a GPU they hold is
		// left out of the test, and the machine's memory in use shrinks it.
		plan := vmrt.PlanTest(r.Host, rt.Spec(), false)
		if plan.Wait != "" {
			return fmt.Errorf("the test boot cannot run now: %s", plan.Wait)
		}
		if plan.NoGPU {
			if last, err := vmrt.LoadSelfTest(r.Host, rt.Spec().DataDir); err == nil && vmrt.GPUTestFailed(last, rt.Fingerprint(r.Version)) {
				return fmt.Errorf("the GPU could not be handed to the last full test rental, and only a test that takes the GPU "+
					"can clear that; it runs when the GPU is free (in use now by %s)", strings.Join(plan.InUse, ", "))
			}
			r.log("The GPU is in use on this machine (%s): the test runs without it. The GPU's handover is tested "+
				"when the GPU is free, and always before a rental starts.", strings.Join(plan.InUse, ", "))
		}
		res := r.testBoot(ctx, rt, plan.TestOptions)
		if res.InUse != "" && !plan.NoGPU {
			// The host took the GPU back as the test came to it: the test
			// runs without it, as it would have had the host been faster.
			r.log("The GPU was taken while the test started (%s): the test runs without it.", res.InUse)
			plan.NoGPU = true
			res = r.testBoot(ctx, rt, plan.TestOptions)
		}
		if res.InUse != "" {
			return fmt.Errorf("the test boot could not run: %s", res.InUse)
		}
		if !res.Passed {
			return fmt.Errorf("the test boot failed: %s", strings.Join(res.Problems, "; "))
		}
		switch {
		case plan.NoGPU:
			r.log("Test boot passed without the GPU.")
		case len(res.GuestGPUs) > 0:
			r.log("Test boot passed: the VM saw %s.", strings.Join(res.GuestGPUs, "; "))
		default:
			r.log("Test boot passed.")
		}
		return nil
	}
	return fmt.Errorf("unknown setup step %q", s)
}

// containerStep runs a setup step on a machine that hosts as a container: the
// container stack instead of QEMU, the container image instead of the golden
// disk, and the container self-test instead of the microVM test boot.
// machine is where container rentals run, and the agent's data directory there.
func (r *Runner) machine() (vmrt.Host, string) {
	if r.Machine != nil {
		return r.Machine, r.MachineDataDir
	}
	return r.Host, r.DataDir
}

func (r *Runner) containerStep(ctx context.Context, s Step, a Attempt) error {
	h, dataDir := r.machine()
	spec, _ := r.Detect().RuntimeSpec()
	gpu := len(spec.GPUs) > 0
	switch s {
	case StepDeps:
		if r.PrepareMachine != nil {
			if err := r.PrepareMachine(ctx); err != nil {
				return err
			}
		}
		if r.InstallDeps != nil {
			if err := r.InstallDeps(ctx); err != nil {
				return err
			}
		} else if err := vmrt.InstallContainerPackages(h, r.Log); err != nil {
			return err
		}
		return vmrt.EnsureContainerHost(h, gpu, r.Log)

	case StepImage:
		// EnsureContainerHost is idempotent; run it here too so a machine that
		// only needed the image also gets a fresh, normalized CDI spec.
		if err := vmrt.EnsureContainerHost(h, gpu, r.Log); err != nil {
			return err
		}
		return vmrt.PrepareContainerImage(h, dataDir, r.Log)

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
		crt := p.ContainerRuntime()
		if crt == nil {
			return errors.New("the container runtime could not be read")
		}
		if crt.Present() {
			return errors.New("a rental (or the leftover of one) is on this machine")
		}
		res := crt.SelfTestContext(ctx, r.Version)
		if res.Stopped {
			return errors.New("the test boot was stopped before it finished")
		}
		if !res.Passed {
			return fmt.Errorf("the container test boot failed: %s", strings.Join(res.Problems, "; "))
		}
		r.log("Container test boot passed.")
		return nil
	}
	return fmt.Errorf("unknown setup step %q", s)
}
