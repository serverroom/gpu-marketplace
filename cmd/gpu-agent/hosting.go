package main

import (
	"context"
	"fmt"
	"runtime"

	"github.com/serverroom/gpu-marketplace/internal/autosetup"
	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
	"github.com/serverroom/gpu-marketplace/internal/wsl"
)

func init() {
	provisioner.ReadWindows = wsl.Facts
	provisioner.WindowsMemAvailableMB = wsl.MemAvailableMB
}

// hostsRentals: this OS can host rentals -- Linux natively, Windows in WSL 2.
func hostsRentals() bool { return runtime.GOOS == "linux" || runtime.GOOS == "windows" }

// adminHint is how to run command with the rights it needs on this OS.
func adminHint(command string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("run 'gpu-agent %s' in an Administrator PowerShell", command)
	}
	return fmt.Sprintf("run 'sudo gpu-agent %s'", command)
}

// machineHost is where container rentals run, and the agent's data directory
// there: this machine, or on Windows the agent's WSL 2 distribution.
func machineHost() (vmrt.Host, string) {
	if runtime.GOOS == "windows" {
		return vmrt.ExecHost{Exec: wsl.Exec, Dial: wsl.Dial, Keep: wsl.Keep}, provisioner.WSLDataDir
	}
	return vmrt.OSHost{}, config.DataDir()
}

// prepareWindows is the Windows half of the setup's tools step: WSL 2, the
// agent's account and its distribution, the VM sized for p's rentals.
func prepareWindows(log func(format string, a ...interface{})) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		spec, _ := detectProvisioner().RuntimeSpec()
		return wsl.Setup(ctx, wsl.Options{
			Arch: runtime.GOARCH, MemMB: wsl.VMMemoryMB(spec.TotalMemMB, spec.GuestMemoryMB()), CPUs: spec.CPUs, Log: log,
		})
	}
}

// onMachine points a setup runner at where rentals run (Windows: WSL 2).
func onMachine(r *autosetup.Runner) {
	if runtime.GOOS != "windows" {
		return
	}
	r.Machine, r.MachineDataDir = machineHost()
	r.PrepareMachine = prepareWindows(r.Log)
}
