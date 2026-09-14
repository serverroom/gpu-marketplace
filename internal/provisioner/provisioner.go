// Package provisioner runs the per-rental Kata microVM lifecycle on the provider
// host: boot an isolated microVM with the GPUs passed through and a fresh
// encrypted disk, and on teardown destroy everything and FAIL CLOSED unless the
// wipe and GPU reset both verify.
//
// The exact Kata/VFIO commands depend on the host and live in the gpu-agent-*
// runtime helpers, which do not ship with the agent; Preflight reports their
// absence and Provision refuses without them. The orchestration, the network
// fence and the fail-closed decisions below are what this package guarantees,
// exercised through an injectable Runner.
package provisioner

import (
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/netguard"
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

// GPUVendor selects the toolchain used to reset and verify the host's GPUs.
// Detection already knows all three (internal/stats), but reset and verify are
// vendor-specific and a host cannot be turned over clean without them.
type GPUVendor string

const (
	VendorNVIDIA GPUVendor = "nvidia"
	VendorAMD    GPUVendor = "amd"
	// VendorApple cannot host rentals: Apple Silicon has no IOMMU passthrough
	// path, and its GPU memory is unified with system RAM, so there is neither a
	// device to hand a guest nor a discrete VRAM to prove clean afterwards.
	VendorApple GPUVendor = "apple"
)

// ErrVendorCannotIsolate is returned when the host's GPUs cannot be passed
// through to a guest at all. Refusing at Provision is deliberate: the previous
// behaviour accepted the rental and only discovered the problem at teardown,
// where an unverifiable wipe quarantines the box dirty - so a host that could
// never have worked ended up permanently unrentable after one booking.
var ErrVendorCannotIsolate = errors.New("this host's GPUs cannot be isolated for rental")

// ErrNotReady is returned when preflight found a reason this machine cannot
// host a rental. The reasons are in the error and in Capability().
var ErrNotReady = errors.New("this machine cannot host a rental")

// Fence isolates a rental's network. netguard.Guard is the real one.
type Fence interface {
	Apply() error
	Remove() error
}

// Provisioner implements the control.Provisioner interface.
type Provisioner struct {
	runner      Runner
	fence       Fence
	goldenImage string
	diskDir     string
	gpuBDFs     []string // PCI addresses of the passthrough GPUs
	vendor      GPUVendor
	status      string
	capability  control.Capability
}

// New builds a provisioner that has NOT been checked against the host, so it
// refuses every rental until Detect (or a test) records a ready capability.
// Failing closed is the default: an unchecked machine is not a ready one.
func New(runner Runner, goldenImage, diskDir string, gpuBDFs []string, vendor GPUVendor) *Provisioner {
	return &Provisioner{
		runner:      runner,
		fence:       netguard.New(runner, netguard.Bridge, netguard.HostNetworks),
		goldenImage: goldenImage,
		diskDir:     diskDir,
		gpuBDFs:     gpuBDFs,
		vendor:      vendor,
		status:      StatusFree,
		capability: control.Capability{
			Kind:    KindKataVFIO,
			Reasons: []string{"the hosting checks have not run"},
		},
	}
}

func (p *Provisioner) Status() string { return p.status }

// Capability reports what preflight found.
func (p *Provisioner) Capability() control.Capability { return p.capability }

func (p *Provisioner) overlayPath(rentalID string) string {
	return filepath.Join(p.diskDir, "rental-"+rentalID+".img")
}

// Provision boots a Kata microVM for the rental, passes through the GPUs, attaches
// a fresh encrypted ephemeral disk, and injects the renter's SSH key. The network
// fence goes up FIRST and must verify: nothing boots on a machine where the
// tenant could reach the provider's LAN.
func (p *Provisioner) Provision(rentalID, renterPubkey string) error {
	if !p.vendor.CanIsolate() {
		return fmt.Errorf("%w (vendor %s)", ErrVendorCannotIsolate, p.vendor)
	}
	if !p.capability.Ready {
		return fmt.Errorf("%w: %s", ErrNotReady, strings.Join(p.capability.Reasons, "; "))
	}
	if !control.ValidRentalID(rentalID) {
		return fmt.Errorf("invalid rental id %q", rentalID)
	}
	if p.status != StatusFree {
		return fmt.Errorf("this machine is %s, not free", p.status)
	}
	if err := p.fence.Apply(); err != nil {
		return fmt.Errorf("isolate network: %w", err)
	}
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
	if !control.ValidRentalID(rentalID) {
		return fmt.Errorf("invalid rental id %q", rentalID)
	}
	_ = p.stopMicroVM(rentalID) // best-effort; the wipe is what matters

	wiped := p.wipeDisk(rentalID)
	gpuClean := p.resetAndVerifyGPU()

	// The fence only ever blocks, so failing to remove it costs the host
	// nothing but a stale table; it is not a reason to quarantine the box.
	if err := p.fence.Remove(); err != nil {
		log.Printf("teardown of %s: could not remove the rental firewall table: %v", rentalID, err)
	}

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
	args := []string{"boot", "--image", p.goldenImage, "--disk", p.overlayPath(rentalID),
		"--bridge", netguard.Bridge}
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

// CanIsolate reports whether a host with these GPUs can hand one to a guest and
// prove it clean afterwards.
func (v GPUVendor) CanIsolate() bool {
	return v == VendorNVIDIA || v == VendorAMD
}

func (p *Provisioner) resetAndVerifyGPU() bool {
	switch p.vendor {
	case VendorNVIDIA:
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

	case VendorAMD:
		for _, bdf := range p.gpuBDFs {
			if err := p.runner.Run("rocm-smi", "--gpureset", "-d", bdf); err != nil {
				return false
			}
		}
		// Same query shape internal/stats already relies on for AMD.
		out, err := p.runner.Output("rocm-smi", "--showmeminfo", "vram", "--csv")
		if err != nil {
			return false
		}
		return VerifyAMDVRAMClear(out)

	default:
		// Unknown or non-isolating vendor: never claim a clean turnover.
		return false
	}
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

// VerifyAMDVRAMClear reads `rocm-smi --showmeminfo vram --csv` and returns true
// only if every GPU is below the clear threshold. rocm-smi reports VRAM in
// BYTES where nvidia-smi reports MiB, which is exactly the kind of difference
// that makes one shared parser wrong for both.
//
// Fails closed on anything it cannot read: no header, no rows, an unparseable
// number, or a used column it cannot find.
func VerifyAMDVRAMClear(rocmSMIOutput string) bool {
	lines := strings.Split(strings.TrimSpace(rocmSMIOutput), "\n")
	if len(lines) < 2 {
		return false
	}

	header := strings.Split(strings.TrimSpace(lines[0]), ",")
	usedCol := -1
	for i, h := range header {
		h = strings.ToLower(strings.TrimSpace(h))
		if strings.Contains(h, "used") && strings.Contains(h, "memory") {
			usedCol = i
			break
		}
	}
	if usedCol < 0 {
		return false
	}

	rows := 0
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		cols := strings.Split(line, ",")
		if usedCol >= len(cols) {
			return false
		}
		usedBytes, err := strconv.ParseInt(strings.TrimSpace(cols[usedCol]), 10, 64)
		if err != nil {
			return false
		}
		if usedBytes/(1024*1024) >= vramClearThresholdMiB {
			return false
		}
		rows++
	}
	return rows > 0
}
