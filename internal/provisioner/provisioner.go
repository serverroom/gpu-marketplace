// Package provisioner decides whether this machine can host a rental and runs
// rentals on it through the vmrt microVM runtime. It refuses what the machine
// cannot do, runs the slow start in the background so the control channel can
// answer at once, keeps the renter's SSH forward up while rented, picks a
// rental back up after an agent restart, and fails closed on teardown: a
// machine whose wipe or GPU turnover does not verify is quarantined dirty
// rather than re-let.
package provisioner

import (
	"context"
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
	"github.com/serverroom/gpu-marketplace/internal/stats"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// Runner executes host commands. vmrt.Host satisfies it.
type Runner interface {
	Run(name string, args ...string) error
	Output(name string, args ...string) (string, error)
	LookPath(name string) (string, error)
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

// GPUVendor is the kind of GPU a machine has, as far as renting it goes.
type GPUVendor string

const (
	// VendorPCI is a Linux machine's GPUs, of any make: PCI functions handed to
	// the microVM over VFIO.
	VendorPCI GPUVendor = "pci"
	// VendorApple cannot host rentals: Apple Silicon has no IOMMU passthrough
	// path, so there is no device to hand a guest. (Unified memory on its own is
	// not the problem -- a GB10 has it and can host.)
	VendorApple GPUVendor = "apple"
	// VendorNone is a Linux machine without a GPU: it hosts too, renting its
	// CPUs, memory and disk (CONTRACT-v2 s1).
	VendorNone GPUVendor = "none"
	// VendorContainerNV is an NVIDIA GPU that cannot be passed through over VFIO
	// (a DGX Spark's GB10 before the signed nvgrace carries its id). It cannot be
	// isolated for a microVM, but it CAN host: the GPU is shared into a hardened
	// container over CDI (KindContainer).
	VendorContainerNV GPUVendor = "container-nv"
)

// ErrVendorCannotIsolate is returned when the host's GPUs cannot be passed
// through to a guest at all.
var ErrVendorCannotIsolate = errors.New("this host's GPUs cannot be isolated for rental")

// ErrNotReady is returned when preflight found a reason this machine cannot
// host a rental. The reasons are in the error and in Capability().
var ErrNotReady = errors.New("this machine cannot host a rental")

// CanIsolate reports whether a host with these GPUs can hand one to a guest.
func (v GPUVendor) CanIsolate() bool {
	return v == VendorPCI
}

// CanHost reports whether a machine with these GPUs -- or none -- can host a
// rental: its GPUs can be isolated for a microVM, it has none to isolate, or it
// can share an NVIDIA GPU into a hardened container.
func (v GPUVendor) CanHost() bool {
	return v.CanIsolate() || v == VendorNone || v == VendorContainerNV
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

	// midRental: the checks ran while a rental (or its leftover) held the
	// machine, so its GPU and ConnectX card could not be read; redetect runs
	// them again once the rental has gone (Detect sets it).
	midRental bool
	redetect  func() *Provisioner
	// onChange is told when the capability changed by itself (after a
	// rental), so the agent reports it; nil: nobody.
	onChange func()
	// problems records rentals that did not start and cleanups that did not
	// verify, for the marketplace's problem list; nil: nowhere.
	problems Problems

	// The host's own use, and a rental that waits for it (pending.go).
	hostUse   vmrt.HostUse
	busySince int64
	pending   *pendingRecord
	// readHostUse looks at the host's use now; nil: vmrt.ReadHostUse on the
	// runtime's spec (nothing, on a provisioner built by hand).
	readHostUse func() vmrt.HostUse
	// notify tells the people on the machine; nil: vmrt.NotifyHost.
	notify func(title, body string) (reached, problems []string)
	// preRentalTest runs the full test boot right before a rental; busy
	// says why it cannot run now. nil: the runtime's.
	preRentalTest func(ctx context.Context) (res vmrt.SelfTestResult, busy string)
	// fullTestCurrent says whether the running agent has passed a full test
	// boot on this machine as it is; nil: selftest.json against the machine.
	fullTestCurrent func() bool
	// loc is the machine's time zone for what the host reads; nil: time.Local.
	loc *time.Location
	// desktopLogins names the people logged in to a DGX Spark's desktop,
	// which a rental closes (desktopwarn.go); nil: logind, on a Spark.
	desktopLogins func() []string
	// baseCtx ends when the agent stops (Watch); a pre-rental test runs in it.
	baseCtx context.Context
	starts  sync.WaitGroup
}

// Problems is where rental failures go (agenterrors.Log).
type Problems interface {
	Raise(area, message, detail string) bool
	Note(area, message, detail string) bool
	Resolve(area, message string)
}

// DirtyProblem is the problem a teardown that did not verify leaves, while
// the machine is held back from renters.
const DirtyProblem = "the last rental's cleanup did not verify, so this machine is held back from renters until it is fixed ('sudo gpu-agent status' shows why)"

// SetProblems sets where rental failures are recorded.
func (p *Provisioner) SetProblems(pr Problems) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.problems = pr
}

// dirtyLocked records the machine held back dirty; the caller holds p.mu.
func (p *Provisioner) dirtyLocked(detail string) {
	if p.problems != nil {
		pr := p.problems
		go pr.Raise(control.AreaRental, DirtyProblem, detail)
	}
}

// OnCapabilityChange sets what is told when the capability changes after a
// rental has left the machine.
func (p *Provisioner) OnCapabilityChange(f func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onChange = f
}

// afterRental reads the machine again once a rental has left it: the ConnectX
// ports are back on the host, and checks that ran while the rental held the
// GPU could not see it.
func (p *Provisioner) afterRental() {
	p.mu.Lock()
	redo, again, notify := p.redetect, p.midRental, p.onChange
	p.mu.Unlock()
	changed := false
	if again && redo != nil {
		changed = p.Adopt(redo())
	} else {
		changed = p.RefreshPair()
	}
	if changed && notify != nil {
		notify()
	}
}

// Withdraw stops this machine hosting: every rental is refused from now on,
// with reason, and neither the setup nor a fresh detect makes it ready again.
func (p *Provisioner) Withdraw(reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.withdrawn = true
	p.capability.Ready = false
	p.capability.Reasons = []string{reason}
	// A rental still waiting for the host cannot start on a withdrawn machine.
	if rec := p.pending; rec != nil && rec.State == control.PendingWaiting {
		p.pending = nil
		p.removePendingLocked()
		p.status = StatusFree
	}
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
	pairOpts, pair := q.pairOpts, q.pair
	hostUse, midRental := q.hostUse, q.midRental
	q.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.status != StatusFree || p.withdrawn {
		return false
	}
	p.machine, p.runtime, p.vendor, p.gpuBDFs, p.unified = machine, rt, vendor, bdfs, unified
	p.capability, p.findings = c, findings
	p.midRental = midRental
	// The host's use keeps the time it began.
	p.hostUse = hostUse
	if hb := p.capability.HostBusy; hb != nil {
		copied := *hb
		if p.busySince != 0 {
			copied.Since = p.busySince
		}
		p.busySince = copied.Since
		p.capability.HostBusy = &copied
	} else {
		p.busySince = 0
	}
	// The pair half follows the machine: the setup can change what it is
	// checked against (the base image, above all).
	if q.host != nil {
		p.pairOpts, p.pair = pairOpts, pair
	}
	return true
}

// Runtime is the microVM runtime behind this provisioner (nil in tests, and on
// a machine that hosts as a container).
func (p *Provisioner) Runtime() *vmrt.Runtime {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runtime
}

// ContainerRuntime is the container runtime behind this provisioner, or nil
// when this machine hosts as a microVM.
func (p *Provisioner) ContainerRuntime() *vmrt.ContainerRuntime {
	p.mu.Lock()
	defer p.mu.Unlock()
	crt, _ := p.machine.(*vmrt.ContainerRuntime)
	return crt
}

// IsContainer reports whether this machine hosts rentals as a hardened
// container rather than a microVM.
func (p *Provisioner) IsContainer() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.capability.Kind == KindContainer
}

// RuntimeSpec is the spec of whichever runtime backs this machine -- the
// microVM runtime or the container runtime -- and whether one exists. It lets
// the automatic setup read the machine's GPUs and desktop state without caring
// which runtime hosts it.
func (p *Provisioner) RuntimeSpec() (vmrt.Spec, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runtimeSpecLocked()
}

// runtimeSpecLocked is RuntimeSpec with p.mu held.
func (p *Provisioner) runtimeSpecLocked() (vmrt.Spec, bool) {
	if p.runtime != nil {
		return p.runtime.Spec(), true
	}
	if crt, ok := p.machine.(*vmrt.ContainerRuntime); ok {
		return crt.Spec(), true
	}
	return vmrt.Spec{}, false
}

// machineView is what Adopt can swap, read together under p.mu.
type machineView struct {
	runtime  *vmrt.Runtime
	machine  Machine
	vendor   GPUVendor
	pairOpts interconnect.Options
}

func (p *Provisioner) view() machineView {
	p.mu.Lock()
	defer p.mu.Unlock()
	return machineView{runtime: p.runtime, machine: p.machine, vendor: p.vendor, pairOpts: p.pairOpts}
}

// Provision checks the request and starts the rental in the background:
// fence, network, encrypted disk, GPUs, boot, and the renter's SSH forward.
// /status says rented once the guest answers, or carries the error. It is
// ProvisionBy for a rental that cannot wait for the host.
func (p *Provisioner) Provision(rentalID, renterPubkey string) error {
	_, err := p.ProvisionBy(rentalID, renterPubkey, 0)
	return err
}

// ProvisionBy is POST /provision. On a machine the host is not using, the
// rental starts in the background, as Provision always did -- after a full
// test boot when the running agent has not passed one. On a machine the host
// is using (its programs hold the GPU, or the memory is short), the rental
// waits for the host, up to startBy (unix; 0: it may not wait, and is
// refused), and the answer is the pending rental: the host is told on the
// machine, and the rental starts as soon as the machine is free.
func (p *Provisioner) ProvisionBy(rentalID, renterPubkey string, startBy int64) (*control.PendingRental, error) {
	c := p.Capability()
	p.mu.Lock()
	vendor := p.vendor
	p.mu.Unlock()
	if !vendor.CanHost() {
		return nil, fmt.Errorf("%w (vendor %s)", ErrVendorCannotIsolate, vendor)
	}
	if !c.Ready {
		return nil, fmt.Errorf("%w: %s", ErrNotReady, strings.Join(c.Reasons, "; "))
	}
	if !control.ValidRentalID(rentalID) {
		return nil, fmt.Errorf("invalid rental id %q", rentalID)
	}
	if vmrt.IsSetupID(rentalID) {
		return nil, fmt.Errorf("rental id %q is one this agent keeps for its own test boots", rentalID)
	}
	if _, err := vmrt.NormalizePubkey(renterPubkey); err != nil {
		return nil, fmt.Errorf("renter key: %w", err)
	}
	needTest := !p.fullTestIsCurrent()
	use := p.hostUseNow()
	// A Spark's desktop someone is logged in to: warned before it closes.
	desktop := !use.Busy() && startBy > p.clock().Unix() && len(p.loggedInDesktop()) > 0
	p.mu.Lock()
	if view, err := p.pendingConflictLocked(rentalID); view != nil || err != nil {
		p.mu.Unlock()
		return view, err
	}
	if !p.capability.Ready || p.settingUp || p.withdrawn {
		p.mu.Unlock()
		return nil, fmt.Errorf("%w: it is setting itself up", ErrNotReady)
	}
	if p.status != StatusFree {
		status := p.status
		p.mu.Unlock()
		return nil, fmt.Errorf("this machine is %s, not free", status)
	}
	req := &pendingRecord{RentalID: rentalID, RenterPubkey: renterPubkey, StartBy: startBy}
	if use.Busy() || desktop {
		view, err := p.waitLocked(req, use) // unlocks
		if err == nil && desktop && p.desktopHold(req) {
			view = p.PendingRental()
		}
		return view, err
	}
	p.status = StatusProvisioning
	p.lastErr = ""
	p.startingLocked()
	machine := p.machine
	if needTest {
		// The running agent has not handed this GPU to a VM yet: a full test
		// boot first. The record shows the rental on /status while it runs.
		return nil, p.launchLocked(req) // unlocks
	}
	p.mu.Unlock()

	o := vmrt.StartOptions{ID: rentalID, Pubkey: renterPubkey}
	if !p.async {
		return nil, p.start(machine, o)
	}
	go p.start(machine, o)
	return nil, nil
}

func (p *Provisioner) start(machine Machine, o vmrt.StartOptions) error {
	return p.startNoting(machine, o, true)
}

// startNoting is start; noteInUse false leaves a start refused because the
// host held the GPU off the problem list -- the rental goes back to wait.
func (p *Provisioner) startNoting(machine Machine, o vmrt.StartOptions, noteInUse bool) error {
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
		if p.problems != nil && (noteInUse || !errors.Is(err, vmrt.ErrGPUInUse)) {
			pr := p.problems
			go pr.Note(control.AreaRental, "rental "+id+" did not start: "+err.Error(), "")
		}
		if p.status == StatusDirty {
			p.dirtyLocked(err.Error())
		}
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
	if p.status == StatusWaiting {
		// Another rental waits for the host (CancelPending handles its own
		// id): nothing of this one is on the machine.
		p.mu.Unlock()
		return nil
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
	if res.Clean() {
		p.status = StatusFree
		p.lastErr = ""
		pr := p.problems
		p.mu.Unlock()
		if pr != nil {
			pr.Resolve(control.AreaRental, DirtyProblem)
		}
		p.afterRental()
		return nil
	}
	defer p.mu.Unlock()
	p.status = StatusDirty
	p.lastErr = strings.Join(res.Detail, "; ")
	p.dirtyLocked(p.lastErr)
	log.Printf("teardown of %s NOT verified clean (wiped=%v gpuClean=%v nicDirty=%v); quarantined dirty: %s",
		rentalID, res.Wiped, res.GPUClean, res.NICDirty, p.lastErr)
	return fmt.Errorf("teardown not verified clean; machine quarantined dirty: %s", p.lastErr)
}

// Resume picks up whatever rental state the agent finds when it starts: a VM
// still running (its unit outlives the agent) gets its SSH forward back; a
// rental whose VM is gone -- the machine rebooted -- is torn down now; a dirty
// leftover keeps the machine quarantined.
func (p *Provisioner) Resume() {
	if p.resume() {
		p.afterRental()
	}
	// A rental that was waiting for the host waits on, across the restart.
	p.resumePending()
}

// resume is Resume; freed says a rental (or setup VM) it found was torn down
// clean, so the machine is to be read again.
func (p *Provisioner) resume() (freed bool) {
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
				p.dirtyLocked(p.lastErr)
				return false
			}
			return true
		}
	}
	switch {
	case !p.machine.Present():
		return
	case p.machine.Dirty():
		p.status = StatusDirty
		p.dirtyLocked("a rental's cleanup from before the agent started did not verify")
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
			return true
		}
		p.status = StatusDirty
		p.lastErr = strings.Join(res.Detail, "; ")
		p.dirtyLocked(p.lastErr)
	}
	return false
}

// verifySleep is how long the verifier waits between looks; a variable for tests.
var verifySleep = func() { time.Sleep(5 * time.Second) }

// gpuVerifier is the vendor's own look at the GPUs, once the runtime has given
// them back. For every make the release has already verified the part that
// needs no vendor: the reset, and each GPU back on the driver it came from.
// This adds what a vendor tool can see, where the tool is installed and the GPU
// went back to that vendor's driver: nvidia-smi lists the GPU again and shows
// nothing of the rental on it, and rocm-smi shows its memory clear. It looks at
// the rented GPUs only -- a GPU the rental left out may be busy with the
// provider's own work. Which GPUs those are, and their drivers, come from what
// the rental recorded, so an agent restarted mid-rental still asks.
func gpuVerifier(r Runner) func(returned []vmrt.BoundDevice) bool {
	return func(returned []vmrt.BoundDevice) bool {
		var nvidia, amd []string
		for _, d := range returned {
			switch d.Driver {
			case "nvidia":
				nvidia = append(nvidia, d.BDF)
			case "amdgpu":
				amd = append(amd, d.BDF)
			}
		}
		if _, err := r.LookPath("nvidia-smi"); len(nvidia) > 0 && err == nil && !nvidiaClear(r, nvidia) {
			return false
		}
		if _, err := r.LookPath("rocm-smi"); len(amd) > 0 && err == nil {
			out, err := r.Output("rocm-smi", "--showbus", "--showmeminfo", "vram", "--csv")
			if err != nil || !amdClear(out, amd) {
				return false
			}
		}
		return true
	}
}

// smiRows reads nvidia-smi's "<bus id>, <name>, <value>" rows by PCI address.
// The name sits between the first and the last comma.
func smiRows(out string) map[string][2]string {
	rows := map[string][2]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		first, last := strings.Index(line, ","), strings.LastIndex(line, ",")
		if first < 0 || last <= first {
			continue
		}
		if bdf := NormalizeBDF(line[:first]); bdf != "" {
			rows[bdf] = [2]string{strings.TrimSpace(line[first+1 : last]), strings.TrimSpace(line[last+1:])}
		}
	}
	return rows
}

func nvidiaClear(r Runner, bdfs []string) bool {
	// The driver takes a moment to bring a GPU back after it is rebound.
	var rows map[string][2]string
	back := false
	for i := 0; i < 12 && !back; i++ {
		out, err := r.Output("nvidia-smi", "--query-gpu=pci.bus_id,name,memory.used", "--format=csv,noheader,nounits")
		if err == nil {
			rows = smiRows(out)
			back = true
			for _, b := range bdfs {
				back = back && rows[b] != [2]string{}
			}
		}
		if !back {
			verifySleep()
		}
	}
	if !back {
		return false
	}
	var used, unified []string
	for _, b := range bdfs {
		// A GPU with no memory figure -- a GB10, whose memory is the machine's
		// pool, or any GPU nvidia-smi says [N/A] for -- is checked by what
		// holds it instead.
		if _, err := strconv.Atoi(rows[b][1]); err != nil || stats.IsUnifiedMemoryModel(rows[b][0]) {
			unified = append(unified, b)
		} else {
			used = append(used, rows[b][1])
		}
	}
	if len(used) > 0 && !VerifyVRAMClear(strings.Join(used, "\n")) {
		return false
	}
	if len(unified) == 0 {
		return true
	}
	// A unified GPU has no VRAM figure: its memory is the machine's pool, which
	// the kernel took back when the VM exited and only ever hands out again
	// zeroed. What must be true is that nothing still holds the GPU.
	out, err := r.Output("nvidia-smi", "--query-compute-apps=gpu_bus_id,pid", "--format=csv,noheader")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		bus, _, _ := strings.Cut(line, ",")
		for _, b := range unified {
			if NormalizeBDF(bus) == b {
				return false
			}
		}
	}
	return true
}

// amdClear is VerifyAMDVRAMClear over the rented GPUs' rows only, found by the
// PCI Bus column --showbus adds. A rented GPU rocm-smi does not list is not
// clear. Output without that column is judged whole, as before.
func amdClear(out string, bdfs []string) bool {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return false
	}
	col := -1
	for i, h := range strings.Split(lines[0], ",") {
		if strings.Contains(strings.ToLower(h), "bus") {
			col = i
			break
		}
	}
	if col < 0 {
		return VerifyAMDVRAMClear(out)
	}
	want := map[string]bool{}
	for _, b := range bdfs {
		want[b] = true
	}
	kept := []string{lines[0]}
	seen := map[string]bool{}
	for _, line := range lines[1:] {
		cols := strings.Split(line, ",")
		if col < len(cols) {
			if bdf := NormalizeBDF(cols[col]); want[bdf] {
				seen[bdf] = true
				kept = append(kept, line)
			}
		}
	}
	return len(seen) == len(want) && VerifyAMDVRAMClear(strings.Join(kept, "\n"))
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
