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
	"github.com/serverroom/gpu-marketplace/internal/interconnect"
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
	// VendorNone is a Linux machine without a GPU: it hosts too, renting its
	// CPUs, memory and disk (CONTRACT-v2 s1).
	VendorNone GPUVendor = "none"
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

// CanHost reports whether a machine with these GPUs -- or none -- can host a
// rental: its GPUs can be isolated, or it has none to isolate.
func (v GPUVendor) CanHost() bool { return v.CanIsolate() || v == VendorNone }

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
	findings   []Finding
	forward    stopper
	async      bool
	// settingUp is set while the agent's automatic setup works on the
	// machine: no rental can be torn down or started then.
	settingUp bool
	// withdrawn: the host removed the machine in the control panel. Nothing
	// makes it ready again in this agent's lifetime.
	withdrawn bool

	// The linked-pair half (pair.go). host is nil in tests that build a
	// provisioner by hand, which then has no pair runtime to offer.
	host     vmrt.Host
	dataDir  string
	version  string
	pairOpts interconnect.Options
	pair     interconnect.Report
	// openPacket opens a raw socket on a ConnectX-7 port.
	openPacket interconnect.Opener
	// lockFrames excludes every other raw-frame run on this machine.
	lockFrames func() (unlock func(), ok bool, err error)
	// listingID is this machine's listing, "" before registration.
	listingID func() string
	// checking is set while a cable check or peer announcement holds the
	// ports; a pair rental is refused meanwhile.
	checking bool
	frames   sync.WaitGroup
	now      func() time.Time
	// pairTest runs the pair test VM; nil means the runtime's own.
	pairTest func(version string, o vmrt.PairSelfTestOptions) vmrt.PairTestResult
}

// Withdraw stops this machine hosting: every rental is refused from now on,
// with reason, and neither the setup nor a fresh detect makes it ready again.
func (p *Provisioner) Withdraw(reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.withdrawn = true
	p.capability.Ready = false
	p.capability.Reasons = []string{reason}
}

// Withdrawn reports whether Withdraw was called.
func (p *Provisioner) Withdrawn() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.withdrawn
}

// RentalPresent reports whether rental state -- a rental, its leftover, or a
// test boot or image build -- is on the machine.
func (p *Provisioner) RentalPresent() bool {
	p.mu.Lock()
	m := p.machine
	p.mu.Unlock()
	return m != nil && m.Present()
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

// Capability reports what preflight found -- or, while the automatic setup
// runs, what it is doing.
func (p *Provisioner) Capability() control.Capability {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.capability
}

// Findings are preflight's reasons with their kinds.
func (p *Provisioner) Findings() []Finding {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Finding(nil), p.findings...)
}

// SetCapability replaces what this machine reports. The automatic setup uses
// it to say what it is doing; it never makes a machine ready (only Adopt of a
// fresh Detect does).
func (p *Provisioner) SetCapability(c control.Capability) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.withdrawn {
		return
	}
	c.Ready = false
	p.capability = c
}

// BeginSetup marks the automatic setup as running on this machine, which is
// not ready from here on. It refuses (false) unless the machine is free: the
// setup never runs beside a rental, and a rental never starts beside it.
func (p *Provisioner) BeginSetup() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.status != StatusFree || p.settingUp || p.withdrawn {
		return false
	}
	p.settingUp = true
	p.capability.Ready = false
	return true
}

// EndSetup marks the automatic setup as done.
func (p *Provisioner) EndSetup() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.settingUp = false
}

// SettingUp reports whether the automatic setup is running.
func (p *Provisioner) SettingUp() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.settingUp
}

// Adopt takes over a fresh Detect's view of the machine -- its runtime, GPUs
// and capability -- once the setup has changed what is installed. Only a free
// machine is re-read; adopted reports whether it was.
func (p *Provisioner) Adopt(q *Provisioner) (adopted bool) {
	if q == nil || q == p {
		return false
	}
	q.mu.Lock()
	machine, rt, vendor, bdfs, unified := q.machine, q.runtime, q.vendor, q.gpuBDFs, q.unified
	c, findings := q.capability, q.findings
	q.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.status != StatusFree || p.withdrawn {
		return false
	}
	p.machine, p.runtime, p.vendor, p.gpuBDFs, p.unified = machine, rt, vendor, bdfs, unified
	p.capability, p.findings = c, findings
	return true
}

// Runtime is the microVM runtime behind this provisioner (nil in tests).
func (p *Provisioner) Runtime() *vmrt.Runtime {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runtime
}

// Provision checks the request and starts the rental in the background:
// fence, network, encrypted disk, GPUs, boot, and the renter's SSH forward.
// /status says rented once the guest answers, or carries the error.
func (p *Provisioner) Provision(rentalID, renterPubkey string) error {
	c := p.Capability()
	p.mu.Lock()
	vendor := p.vendor
	p.mu.Unlock()
	if !vendor.CanHost() {
		return fmt.Errorf("%w (vendor %s)", ErrVendorCannotIsolate, vendor)
	}
	if !c.Ready {
		return fmt.Errorf("%w: %s", ErrNotReady, strings.Join(c.Reasons, "; "))
	}
	if !control.ValidRentalID(rentalID) {
		return fmt.Errorf("invalid rental id %q", rentalID)
	}
	if _, err := vmrt.NormalizePubkey(renterPubkey); err != nil {
		return fmt.Errorf("renter key: %w", err)
	}
	p.mu.Lock()
	if !p.capability.Ready || p.settingUp || p.withdrawn {
		p.mu.Unlock()
		return fmt.Errorf("%w: it is setting itself up", ErrNotReady)
	}
	if p.status != StatusFree {
		status := p.status
		p.mu.Unlock()
		return fmt.Errorf("this machine is %s, not free", status)
	}
	p.status = StatusProvisioning
	p.lastErr = ""
	machine := p.machine
	p.mu.Unlock()

	o := vmrt.StartOptions{ID: rentalID, Pubkey: renterPubkey}
	if !p.async {
		return p.start(machine, o)
	}
	go p.start(machine, o)
	return nil
}

func (p *Provisioner) start(machine Machine, o vmrt.StartOptions) error {
	id := o.ID
	err := machine.Start(o)
	var fwd stopper
	if err == nil {
		fwd, err = startForward(SSHListen, net.JoinHostPort(vmrt.GuestIP, "22"))
		if err != nil {
			err = fmt.Errorf("open the renter's SSH forward: %w", err)
			machine.Stop()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.lastErr = err.Error()
		p.status = StatusFree
		if machine.Dirty() {
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
	if p.settingUp {
		// The only VM on the machine is the setup's own (a base image build
		// or a test boot); it tears that down itself.
		p.mu.Unlock()
		return fmt.Errorf("no rental is on this machine: it is setting itself up")
	}
	p.status = StatusWiping
	fwd := p.forward
	p.forward = nil
	machine := p.machine
	p.mu.Unlock()

	if fwd != nil {
		fwd.Stop()
	}
	res := machine.Stop()

	p.mu.Lock()
	defer p.mu.Unlock()
	if res.Clean() {
		p.status = StatusFree
		p.lastErr = ""
		return nil
	}
	p.status = StatusDirty
	p.lastErr = strings.Join(res.Detail, "; ")
	log.Printf("teardown of %s NOT verified clean (wiped=%v gpuClean=%v nicDirty=%v); quarantined dirty: %s",
		rentalID, res.Wiped, res.GPUClean, res.NICDirty, p.lastErr)
	return fmt.Errorf("teardown not verified clean; machine quarantined dirty: %s", p.lastErr)
}

// Resume picks up whatever rental state the agent finds when it starts: a VM
// still running (its unit outlives the agent) gets its SSH forward back; a
// rental whose VM is gone -- the machine rebooted -- is torn down now; a dirty
// leftover keeps the machine quarantined.
func (p *Provisioner) Resume() {
	p.mu.Lock()
	defer p.mu.Unlock()
	// A base image build or test boot is nobody's rental. One whose process
	// still runs (a person's `check --boot`) is left to it; one whose process
	// is gone -- this agent, restarted mid-setup -- is torn down now.
	if s, ok := p.machine.(interface{ SetupVM() (present, owned bool) }); ok {
		if present, owned := s.SetupVM(); present {
			if owned {
				return
			}
			if res := p.machine.Stop(); !res.Clean() {
				p.status = StatusDirty
				p.lastErr = strings.Join(res.Detail, "; ")
			}
			return
		}
	}
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
