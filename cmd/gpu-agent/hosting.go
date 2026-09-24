package main

import (
	"context"
	"fmt"
	"runtime"

	"github.com/serverroom/gpu-marketplace/internal/autosetup"
	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/mac"
	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
	"github.com/serverroom/gpu-marketplace/internal/wsl"
)

func init() {
	provisioner.ReadWindows = wsl.Facts
	provisioner.WindowsMemAvailableMB = wsl.MemAvailableMB
	provisioner.ReadMac = mac.Facts
	provisioner.MacMemAvailableMB = mac.MemAvailableMB
}

// hostsRentals: this OS can host rentals -- Linux natively, Windows in WSL 2,
// macOS in a Linux VM.
func hostsRentals() bool {
	switch runtime.GOOS {
	case "linux", "windows", "darwin":
		return true
	}
	return false
}

// adminHint is how to run command with the rights it needs on this OS.
func adminHint(command string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("run 'gpu-agent %s' in an Administrator PowerShell", command)
	}
	return fmt.Sprintf("run 'sudo gpu-agent %s'", command)
}

// machineHost is where container rentals run, and the agent's data directory
// there: this machine (Linux), or the Linux environment the agent owns on
// Windows (WSL 2) or macOS (a VM).
func machineHost() (vmrt.Host, string) {
	switch runtime.GOOS {
	case "windows":
		return vmrt.ExecHost{Exec: wsl.Exec, Dial: wsl.Dial, Keep: wsl.Keep}, provisioner.WSLDataDir
	case "darwin":
		return vmrt.ExecHost{Exec: mac.Exec, Dial: mac.Dial, Keep: mac.Keep}, provisioner.MacVMDataDir
	}
	return vmrt.OSHost{}, config.DataDir()
}

// prepareMachine is the setup's tools step where the agent brings its own
// Linux: WSL 2 on Windows, a QEMU VM on macOS, both sized for p's rentals.
func prepareMachine(log func(format string, a ...interface{})) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		spec, _ := detectProvisioner().RuntimeSpec()
		switch runtime.GOOS {
		case "windows":
			return wsl.Setup(ctx, wsl.Options{
				Arch: runtime.GOARCH, MemMB: wsl.VMMemoryMB(spec.TotalMemMB, spec.GuestMemoryMB()), CPUs: spec.CPUs, Log: log,
			})
		case "darwin":
			return mac.Setup(ctx, mac.Options{
				Arch: runtime.GOARCH, MemMB: mac.VMMemoryMB(spec.TotalMemMB, spec.GuestMemoryMB()), CPUs: spec.CPUs, Log: log,
			})
		}
		return nil
	}
}

// onMachine points a setup runner at where rentals run (the WSL 2 distro on
// Windows, the VM on macOS); a no-op on Linux, which runs them directly.
func onMachine(r *autosetup.Runner) {
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		return
	}
	r.Machine, r.MachineDataDir = machineHost()
	r.PrepareMachine = prepareMachine(r.Log)
}
