package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/kardianos/service"

	"github.com/serverroom/gpu-marketplace/internal/autosetup"
	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// autoSetup is the daemon's automatic setup, over this agent's provisioner.
func (a *gpuAgent) autoSetup() *autosetup.Daemon {
	return &autosetup.Daemon{
		Runner: autosetup.Runner{
			Host:    vmrt.OSHost{},
			Arch:    runtime.GOARCH,
			DataDir: config.DataDir(),
			Version: version,
			Detect:  detectProvisioner,
			Log:     a.say,
		},
		GOOS:      runtime.GOOS,
		ConfigDir: config.ConfigDir(),
		PID:       os.Getpid(),
		Prov:      a.prov,
		Report: func(c control.Capability) error {
			_, err := a.report(c)
			return err
		},
		Say:  a.say,
		Warn: a.warn,
	}
}

// setupSummary is the automatic setup's state in one line, for status/check.
func setupSummary() string {
	state := "on"
	if !autosetup.Enabled(config.ConfigDir()) {
		state = "off ('sudo gpu-agent setup --on' turns it on)"
	}
	last, err := autosetup.Load(vmrt.OSHost{}, config.DataDir())
	switch {
	case err != nil:
		return fmt.Sprintf("automatic setup is %s; its last run could not be read (%v)", state, err)
	case last == nil:
		return fmt.Sprintf("automatic setup is %s; it has not run on this machine", state)
	}
	return fmt.Sprintf("automatic setup is %s; its last run %s", state, autosetup.Describe(last))
}

func printSetup() {
	if runtime.GOOS == "linux" {
		fmt.Printf("Setup:        %s\n", setupSummary())
	}
}

// runSetup is `gpu-agent setup`: the automatic setup, run in the foreground
// with its output (for support), or switched on and off, or its last run shown.
func runSetup(svc service.Service, args []string) {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	status := fs.Bool("status", false, "print the last setup attempt and whether the automatic setup is on")
	off := fs.Bool("off", false, "turn the automatic setup off (the agent then waits for the manual commands)")
	on := fs.Bool("on", false, "turn the automatic setup back on")
	fs.Parse(args)

	if runtime.GOOS != "linux" {
		exitf("the rental runtime, and so its setup, runs on Linux only")
	}
	if *status {
		fmt.Println(setupSummary())
		return
	}
	if os.Geteuid() != 0 {
		exitf("setup needs root: run 'sudo gpu-agent setup'")
	}
	if *off || *on {
		if err := autosetup.SetEnabled(config.ConfigDir(), *on); err != nil {
			exitf("could not write %s: %v", autosetup.OptOutPath(config.ConfigDir()), err)
		}
		if *on {
			fmt.Println("The automatic setup is on. It runs at the agent's next start ('sudo gpu-agent stop && sudo gpu-agent start').")
		} else {
			fmt.Println("The automatic setup is off: the agent no longer installs packages, builds the rental image or runs a test boot by itself.")
			fmt.Println("A setup that is running now finishes; restarting the agent stops it. 'sudo gpu-agent setup' still runs it by hand.")
		}
		return
	}

	h := vmrt.OSHost{}
	p := detectProvisioner()
	if p.Capability().Ready {
		fmt.Println("This machine is ready to host a rental; there is nothing to set up.")
		return
	}
	rt := p.Runtime()
	if rt == nil || rt.Present() {
		exitf("a rental (or the leftover of one) is on this machine; the setup does not run beside it")
	}
	plan := autosetup.PlanFor(p.Findings(), autosetup.AptGet(h))
	if len(plan.Human) > 0 {
		fmt.Println("This machine needs a person before it can be set up:")
		for _, r := range plan.Human {
			fmt.Printf("  - %s\n", r)
		}
		os.Exit(2)
	}
	release, err := vmrt.AcquireBusy(h, config.DataDir(), os.Getpid(), "setting this machine up")
	if err != nil {
		exitf("%v; 'gpu-agent setup --status' shows where it is", err)
	}

	fmt.Printf("This sets the machine up in %d step(s):\n", len(plan.Steps))
	for i, s := range plan.Steps {
		fmt.Printf("  %d. %s (about %s)\n", i+1, s.Doing(), s.Takes())
	}
	if rt.Spec().DesktopOnDemand {
		fmt.Println("The test rental takes the GPU: this machine's desktop closes during it (anything open on its screen closes")
		fmt.Println("with it) and comes back after it.")
	}
	fmt.Println()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runner := autosetup.Runner{
		Host:    h,
		Arch:    runtime.GOARCH,
		DataDir: config.DataDir(),
		Version: version,
		Detect:  detectProvisioner,
		Log:     func(format string, a ...interface{}) { fmt.Printf(format+"\n", a...) },
		OnStep: func(a autosetup.Attempt) {
			fmt.Printf("\n== Step %d of %d: %s ==\n", a.Step, len(a.Steps), a.CurrentStep().Doing())
		},
	}
	a := runner.Begin(plan, rt.Spec().GPUs, autosetup.ByCommand)
	fmt.Printf("The rental image gets NVIDIA driver %s (this machine runs %s).\n", a.Driver, a.HostDriver)
	a = runner.Run(ctx, a)
	release()
	fmt.Println()
	if !a.Passed {
		fmt.Printf("FAILED while %s (step %d of %d):\n  %s\n", a.CurrentStep().Doing(), a.Step, len(a.Steps), a.Error)
		os.Exit(2)
	}
	fmt.Println("PASSED: this machine is ready to host a rental.")
	if _, err := svc.Status(); err == nil {
		if err := service.Control(svc, "restart"); err == nil {
			fmt.Println("The agent service was restarted so it reports this machine as ready.")
		}
	}
}
