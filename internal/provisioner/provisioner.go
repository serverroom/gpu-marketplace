// Package provisioner decides whether this machine can host a rental and runs
// rentals on it through the vmrt microVM runtime. It refuses what the machine
// cannot do, runs the slow start in the background so the control channel can
// answer at once, keeps the renter's SSH forward up while rented, picks a
// rental back up after an agent restart, and fails closed on teardown: a
// machine whose wipe or GPU turnover does not verify is quarantined dirty
// rather than re-let.
package provisioner

import (
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/register"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// Runner executes host commands. vmrt.Host satisfies it.
type Runner interface {
	Run(name string, args ...string) error
	Output(name string, args ...string) (string, error)
}

const (
	StatusFree         = "free"
	StatusProvisioning = "provisioning"
	StatusRented       = "rented"
	StatusWiping       = "wiping"
	// StatusDirty: wipe or GPU turnover did not verify. NOT re-rentable until
	// the leftover is cleaned up and the machine passes again.
	StatusDirty = "dirty"

	// vramClearThresholdMiB is the per-GPU used-VRAM ceiling that counts as "clear"
	// after a reset.
	vramClearThresholdMiB = 512
)

// GPUVendor selects the toolchain used to verify the host's GPUs after a rental.
type GPUVendor string

const (
	VendorNVIDIA GPUVendor = "nvidia"
	VendorAMD    GPUVendor = "amd"
	// VendorApple cannot host rentals: Apple Silicon has no IOMMU passthrough
	// path, so there is no device to hand a guest. (Unified memory on its own is
	// not the problem -- a GB10 has it and can host.)
	VendorApple GPUVendor = "apple"
)

// ErrVendorCannotIsolate is returned when the host's GPUs cannot be passed
// through to a guest at all.
var ErrVendorCannotIsolate = errors.New("this host's GPUs cannot be isolated for rental")

// ErrNotReady is returned when preflight found a reason this machine cannot
// host a rental. The reasons are in the error and in Capability().
var ErrNotReady = errors.New("this machine cannot host a rental")

// CanIsolate reports whether a host with these GPUs can hand one to a guest and
// prove it clean afterwards.
func (v GPUVendor) CanIsolate() bool {
	return v == VendorNVIDIA || v == VendorAMD
}

// Machine is the rental runtime. *vmrt.Runtime is the real one.
type Machine interface {
	Start(o vmrt.StartOptions) error
	Stop() vmrt.StopResult
	Alive() bool
	Present() bool
	Dirty() bool
}

type stopper interface{ Stop() }

// startForward opens the renter's SSH forward; a variable so tests need no socket.
var startForward = func(listen, target string) (stopper, error) {
	f, err := vmrt.StartForward(listen, target)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// SSHListen is where the renter's SSH arrives from the relay tunnel.
var SSHListen = net.JoinHostPort("127.0.0.1", strconv.Itoa(register.MicroVMSSHPort))

// Provisioner implements control.Provisioner.
type Provisioner struct {
	mu         sync.Mutex
	machine    Machine
	runtime    *vmrt.Runtime
	vendor     GPUVendor
	unified    bool
	gpuBDFs    []string
	status     string
	lastErr    string
	capability control.Capability
	forward    stopper
	async      bool
}

// New builds a provisioner that has NOT been checked against the host, so it
// refuses every rental until Detect (or a test) records a ready capability.
// Failing closed is the default: an unchecked machine is not a ready one.
func New(machine Machine, vendor GPUVendor, gpuBDFs []string, unified bool) *Provisioner {
	return &Provisioner{
		machine: machine,
		vendor:  vendor,
		gpuBDFs: gpuBDFs,
		unified: unified,
		status:  StatusFree,
		async:   true,
		capability: control.Capability{
			Kind:    KindQEMUVFIO,
			Reasons: []string{"the hosting checks have not run"},
		},
	}
}

func (p *Provisioner) Status() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

// LastError is why the last rental did not start, or its teardown did not verify.
func (p *Provisioner) LastError() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastErr
}

// Capability reports what preflight found.
func (p *Provisioner) Capability() control.Capability { return p.capability }

// Runtime is the microVM runtime behind this provisioner (nil in tests).
func (p *Provisioner) Runtime() *vmrt.Runtime { return p.runtime }

// Provision checks the request and starts the rental in the background:
// fence, network, encrypted disk, GPUs, boot, and the renter's SSH forward.
// /status says rented once the guest answers, or carries the error.
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
	if _, err := vmrt.NormalizePubkey(renterPubkey); err != nil {
		return fmt.Errorf("renter key: %w", err)
	}
	p.mu.Lock()
	if p.status != StatusFree {
		status := p.status
		p.mu.Unlock()
		return fmt.Errorf("this machine is %s, not free", status)
	}
	p.status = StatusProvisioning
	p.lastErr = ""
	p.mu.Unlock()

	if !p.async {
		return p.start(rentalID, renterPubkey)
	}
	go p.start(rentalID, renterPubkey)
	return nil
}

func (p *Provisioner) start(id, key string) error {
	err := p.machine.Start(vmrt.StartOptions{ID: id, Pubkey: key})
	var fwd stopper
	if err == nil {
		fwd, err = startForward(SSHListen, net.JoinHostPort(vmrt.GuestIP, "22"))
		if err != nil {
			err = fmt.Errorf("open the renter's SSH forward: %w", err)
			p.machine.Stop()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.lastErr = err.Error()
		p.status = StatusFree
		if p.machine.Dirty() {
			p.status = StatusDirty
		}
		log.Printf("rental %s did not start: %v", id, err)
		return err
	}
	p.forward = fwd
	p.status = StatusRented
	return nil
}

// Teardown destroys the rental and FAILS CLOSED: the machine returns to free
// only if the runtime verified the wipe AND the GPU turnover.
func (p *Provisioner) Teardown(rentalID string) error {
	if !control.ValidRentalID(rentalID) {
		return fmt.Errorf("invalid rental id %q", rentalID)
	}
	p.mu.Lock()
	if p.status == StatusProvisioning || p.status == StatusWiping {
		status := p.status
		p.mu.Unlock()
		return fmt.Errorf("the rental is still %s; retry shortly", status)
	}
	p.status = StatusWiping
	fwd := p.forward
	p.forward = nil
	p.mu.Unlock()

	if fwd != nil {
		fwd.Stop()
	}
	res := p.machine.Stop()

	p.mu.Lock()
	defer p.mu.Unlock()
	if res.Clean() {
		p.status = StatusFree
		p.lastErr = ""
		return nil
	}
	p.status = StatusDirty
	p.lastErr = strings.Join(res.Detail, "; ")
	log.Printf("teardown of %s NOT verified clean (wiped=%v gpuClean=%v); quarantined dirty: %s",
		rentalID, res.Wiped, res.GPUClean, p.lastErr)
	return fmt.Errorf("teardown not verified clean; machine quarantined dirty: %s", p.lastErr)
}

// Resume picks up whatever rental state the agent finds when it starts: a VM
// still running (its unit outlives the agent) gets its SSH forward back; a
// rental whose VM is gone -- the machine rebooted -- is torn down now; a dirty
// leftover keeps the machine quarantined.
func (p *Provisioner) Resume() {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case !p.machine.Present():
		return
	case p.machine.Dirty():
		p.status = StatusDirty
	case p.machine.Alive():
		fwd, err := startForward(SSHListen, net.JoinHostPort(vmrt.GuestIP, "22"))
		if err != nil {
			p.lastErr = "reopen the renter's SSH forward: " + err.Error()
		} else {
			p.forward = fwd
		}
		p.status = StatusRented
	default:
		res := p.machine.Stop()
		if res.Clean() {
			p.status = StatusFree
		} else {
			p.status = StatusDirty
			p.lastErr = strings.Join(res.Detail, "; ")
		}
	}
}

// verifySleep is how long the verifier waits between looks; a variable for tests.
var verifySleep = func() { time.Sleep(5 * time.Second) }

// gpuVerifier checks, once the runtime has given the GPUs back, that they are
// back with their driver and that nothing of the rental is on them.
func gpuVerifier(r Runner, vendor GPUVendor, unified bool, bdfs []string) func() bool {
	return func() bool {
		switch vendor {
		case VendorNVIDIA:
			// The driver takes a moment to bring a GPU back after it is rebound.
			back := false
			for i := 0; i < 12 && !back; i++ {
				out, err := r.Output("nvidia-smi", "--query-gpu=pci.bus_id", "--format=csv,noheader")
				if err == nil && allListed(out, bdfs) {
					back = true
					break
				}
				verifySleep()
			}
			if !back {
				return false
			}
			if unified {
				// No VRAM figure exists: the GPU's memory is the machine's pool,
				// which the kernel took back when the VM exited and only ever
				// hands out again zeroed. What must be true is that nothing still
				// holds the GPU.
				out, err := r.Output("nvidia-smi", "--query-compute-apps=pid", "--format=csv,noheader")
				return err == nil && strings.TrimSpace(out) == ""
			}
			out, err := r.Output("nvidia-smi", "--query-gpu=memory.used", "--format=csv,noheader,nounits")
			return err == nil && VerifyVRAMClear(out)
		case VendorAMD:
			out, err := r.Output("rocm-smi", "--showmeminfo", "vram", "--csv")
			return err == nil && VerifyAMDVRAMClear(out)
		}
		return false
	}
}

func allListed(out string, bdfs []string) bool {
	listed := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if bdf := NormalizeBDF(line); bdf != "" {
			listed[bdf] = true
		}
	}
	for _, bdf := range bdfs {
		if !listed[bdf] {
			return false
		}
	}
	return len(bdfs) > 0
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
