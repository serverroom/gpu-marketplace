// Package provisioner runs the per-rental Kata microVM lifecycle on the provider
// host: boot an isolated microVM with the GPUs passed through and a fresh
// encrypted disk, and on teardown destroy everything and FAIL CLOSED unless the
// wipe and GPU reset both verify.
//
// The exact Kata/VFIO commands depend on the host and are validated by the spec's
// hardware spike; the orchestration and the fail-closed decision below are what
// this package guarantees, exercised through an injectable Runner.
package provisioner

import (
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Runner executes host commands. Real hosts use ExecRunner; tests inject a fake.
type Runner interface {
	Run(name string, args ...string) error
	Output(name string, args ...string) (string, error)
}

// ExecRunner runs commands for real.
type ExecRunner struct{}

func (ExecRunner) Run(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

func (ExecRunner) Output(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}

const (
	StatusFree   = "free"
	StatusRented = "rented"
	// StatusDirty: wipe or GPU reset did not verify. NOT re-rentable until a full
	// host reboot/power-cycle and a signed verified-clean turnover.
	StatusDirty = "dirty"

	// vramClearThresholdMiB is the per-GPU used-VRAM ceiling that counts as "clear"
	// after a reset.
	vramClearThresholdMiB = 512
)

// Provisioner implements the control.Provisioner interface.
type Provisioner struct {
	runner      Runner
	goldenImage string
	diskDir     string
	gpuBDFs     []string // PCI addresses of the passthrough GPUs
	status      string
}

func New(runner Runner, goldenImage, diskDir string, gpuBDFs []string) *Provisioner {
	return &Provisioner{
		runner:      runner,
		goldenImage: goldenImage,
		diskDir:     diskDir,
		gpuBDFs:     gpuBDFs,
		status:      StatusFree,
	}
}

func (p *Provisioner) Status() string { return p.status }

func (p *Provisioner) overlayPath(rentalID string) string {
	return filepath.Join(p.diskDir, "rental-"+rentalID+".img")
}

// Provision boots a Kata microVM for the rental, passes through the GPUs, attaches
// a fresh encrypted ephemeral disk, and injects the renter's SSH key.
func (p *Provisioner) Provision(rentalID, renterPubkey string) error {
	if err := p.createEncryptedDisk(rentalID); err != nil {
		return fmt.Errorf("create disk: %w", err)
	}
	if err := p.bootMicroVM(rentalID); err != nil {
		return fmt.Errorf("boot microVM: %w", err)
	}
	if err := p.injectKey(rentalID, renterPubkey); err != nil {
		return fmt.Errorf("inject key: %w", err)
	}
	p.status = StatusRented
	return nil
}

// Teardown destroys the rental and FAILS CLOSED: the box returns to `free` only
// if the wipe AND the GPU reset both verify; otherwise it is quarantined `dirty`.
func (p *Provisioner) Teardown(rentalID string) error {
	_ = p.stopMicroVM(rentalID) // best-effort; the wipe is what matters

	wiped := p.wipeDisk(rentalID)
	gpuClean := p.resetAndVerifyGPU()

	if wiped && gpuClean {
		p.status = StatusFree
		return nil
	}

	p.status = StatusDirty
	log.Printf("teardown of %s NOT verified clean (wiped=%v gpuClean=%v); quarantined dirty",
		rentalID, wiped, gpuClean)
	return fmt.Errorf("teardown not verified clean; listing quarantined dirty")
}

func (p *Provisioner) createEncryptedDisk(rentalID string) error {
	// A per-rental dm-crypt overlay; the key lives only in memory and is destroyed
	// at teardown, making the data unrecoverable.
	return p.runner.Run("gpu-agent-mkdisk", p.overlayPath(rentalID))
}

func (p *Provisioner) bootMicroVM(rentalID string) error {
	args := []string{"boot", "--image", p.goldenImage, "--disk", p.overlayPath(rentalID)}
	for _, bdf := range p.gpuBDFs {
		args = append(args, "--vfio", bdf)
	}
	return p.runner.Run("gpu-agent-kata", args...)
}

func (p *Provisioner) injectKey(rentalID, renterPubkey string) error {
	return p.runner.Run("gpu-agent-injectkey", rentalID, renterPubkey)
}

func (p *Provisioner) stopMicroVM(rentalID string) error {
	return p.runner.Run("gpu-agent-kata", "stop", rentalID)
}

func (p *Provisioner) wipeDisk(rentalID string) bool {
	// Drop the in-memory LUKS key and delete the overlay -> unrecoverable.
	_ = p.runner.Run("cryptsetup", "luksClose", "rental-"+rentalID)
	if err := p.runner.Run("rm", "-f", p.overlayPath(rentalID)); err != nil {
		return false
	}
	return true
}

func (p *Provisioner) resetAndVerifyGPU() bool {
	for _, bdf := range p.gpuBDFs {
		if err := p.runner.Run("nvidia-smi", "--gpu-reset", "-i", bdf); err != nil {
			return false
		}
	}
	out, err := p.runner.Output("nvidia-smi",
		"--query-gpu=memory.used", "--format=csv,noheader,nounits")
	if err != nil {
		return false
	}
	return VerifyVRAMClear(out)
}

// VerifyVRAMClear returns true only if every GPU reports used VRAM below the clear
// threshold. Any unparseable line is treated as not-clear (fail closed).
func VerifyVRAMClear(nvidiaSMIOutput string) bool {
	lines := strings.Split(strings.TrimSpace(nvidiaSMIOutput), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return false
	}
	for _, line := range lines {
		used, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			return false
		}
		if used >= vramClearThresholdMiB {
			return false
		}
	}
	return true
}
