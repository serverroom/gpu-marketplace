package vmrt

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/stats"
)

// Fence is the rental network firewall. netguard.Guard is the real one.
type Fence interface {
	Apply() error
	Remove() error
}

// Timeouts, overridable by tests.
var (
	BootTimeout     = 10 * time.Minute // first boot grows the disk and loads the GPU driver
	StopGrace       = 30 * time.Second
	pollInterval    = 5 * time.Second
	stopPoll        = 1 * time.Second
	persistencedSvc = "nvidia-persistenced"
)

// Runtime starts and stops the one rental microVM this machine can run.
type Runtime struct {
	h     Host
	spec  Spec
	fence Fence
	// verifyGPU checks, after the GPUs are back on their driver, that nothing of
	// the rental is left on them. Vendor-specific, so the provisioner supplies
	// it. It is given the functions the rental took with the drivers they went
	// back to, as the rental recorded them -- not what the host shows now, which
	// after an agent restart mid-rental is vfio-pci.
	verifyGPU func(returned []BoundDevice) bool
}

// New builds a runtime.
func New(h Host, spec Spec, fence Fence, verifyGPU func(returned []BoundDevice) bool) *Runtime {
	return &Runtime{h: h, spec: spec, fence: fence, verifyGPU: verifyGPU}
}

// Spec is what this runtime gives a rental.
func (rt *Runtime) Spec() Spec { return rt.spec }

// StartOptions is one rental.
type StartOptions struct {
	ID     string
	Pubkey string
	// Probes, when non-nil, makes this a self-test VM that reports what it can
	// reach on its serial console.
	Probes []string
	// NoWait skips waiting for the guest's SSH port (the self-test waits for its
	// serial report instead).
	NoWait bool
	// Pair makes this one machine's half of a linked pair: the ConnectX card
	// goes to the VM with the GPU, and the guest configures the cable.
	Pair *PairOptions
	// PairTest makes a pair's self-test VM run the pair test too.
	PairTest *PairTestPlan
}

// ErrRentalPresent is returned by Start while a rental (or a dirty leftover of
// one) is still on the machine.
var ErrRentalPresent = errors.New("a rental is still on this machine")

// Start brings a rental up, in the order that keeps the host safe at every
// point: fence first (so nothing ever runs unfenced), then the network, the
// encrypted disk, the seed, and only then the GPUs are taken from the host,
// right before boot. Each step is recorded before the next; any failure tears
// down exactly what exists.
func (rt *Runtime) Start(o StartOptions) (err error) {
	if st, lerr := LoadState(rt.h, rt.spec.DataDir); lerr != nil || st != nil {
		if lerr != nil {
			return fmt.Errorf("read rental state: %w", lerr)
		}
		return fmt.Errorf("%w (%s)", ErrRentalPresent, st.RentalID)
	}
	if o.Pair != nil {
		if err := ValidatePair(o.ID, o.Pair); err != nil {
			return fmt.Errorf("pair rental: %w", err)
		}
	}
	userData, err := BuildUserData(SeedOptions{ID: o.ID, Pubkey: o.Pubkey, Probes: o.Probes,
		NoGPU: len(rt.spec.GPUs) == 0, Pair: o.Pair, PairTest: o.PairTest})
	if err != nil {
		return err
	}
	host := Hostname(o.ID)
	if o.Pair != nil {
		host = PairHostname(o.ID, o.Pair.Node)
	}
	// The agent's own GPU queries would hold the GPU while it is taken.
	resume := stats.PauseGPUQueries()
	defer resume()

	r := NewRental(rt.spec.Storage(), o.ID)
	st := &State{RentalID: o.ID, Rental: r, StartedAt: time.Now().Unix(), Pair: o.Pair}
	save := func() error { return SaveState(rt.h, rt.spec.DataDir, st) }
	if err = save(); err != nil {
		return fmt.Errorf("record rental: %w", err)
	}
	defer func() {
		if err != nil {
			rt.Stop()
		}
	}()

	if err = rt.h.MkdirAll(r.Dir, 0700); err != nil {
		return fmt.Errorf("rental directory: %w", err)
	}

	if err = rt.fence.Apply(); err != nil {
		return fmt.Errorf("isolate network: %w", err)
	}
	st.Fenced = true
	if err = save(); err != nil {
		return err
	}

	st.Net, err = SetupNetwork(rt.h)
	if serr := save(); err == nil {
		err = serr
	}
	if err != nil {
		return fmt.Errorf("rental network: %w", err)
	}

	st.Disk, err = CreateDisk(rt.h, r.Dir, o.ID, rt.spec.DiskGB, rt.spec.GoldenImage)
	if serr := save(); err == nil {
		err = serr
	}
	if err != nil {
		return err
	}

	if err = rt.writeSeed(r, userData, metaData(o.ID, host), NetworkConfigFor(o.Pair)); err != nil {
		return err
	}
	if err = rt.copyVars(r); err != nil {
		return err
	}

	// A pair rental's card is checked and recorded before anything is taken
	// from the host: a card that cannot go must not close a desktop first.
	var nics []string
	if o.Pair != nil {
		if nics, err = rt.planNICs(st, o.Pair.Functions, save); err != nil {
			return err
		}
	}
	if len(rt.spec.GPUs) > 0 {
		// Saved even when it fails: the teardown works from what is on disk,
		// and must give back exactly what was taken -- functions already on
		// vfio-pci, services stopped, a desktop closed.
		err = rt.takeGPUs(st, &r, save)
		if serr := save(); err == nil {
			err = serr
		}
		if err != nil {
			return err
		}
	}
	// The host keeps the cable until the last moment: the card leaves it after
	// the GPU, right before the VM boots.
	if o.Pair != nil {
		if err = rt.takeNICs(st, &r, nics, save); err != nil {
			return err
		}
	}
	st.Rental = r
	if err = save(); err != nil {
		return err
	}

	if err = rt.h.Run("systemd-run", LaunchArgs(rt.spec, r)...); err != nil {
		return fmt.Errorf("boot microVM: %w", err)
	}
	if o.NoWait {
		return nil
	}
	return rt.waitGuest(r)
}

func (rt *Runtime) writeSeed(r Rental, userData, metaData, networkConfig string) error {
	files := map[string]string{
		"user-data":      userData,
		"meta-data":      metaData,
		"network-config": networkConfig,
	}
	for name, content := range files {
		if err := rt.h.WriteFile(r.Dir+"/"+name, []byte(content), 0600); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	if err := rt.h.Run("cloud-localds", "--network-config="+r.Dir+"/network-config",
		r.Seed, r.Dir+"/user-data", r.Dir+"/meta-data"); err != nil {
		return fmt.Errorf("build seed: %w", err)
	}
	return nil
}

func (rt *Runtime) copyVars(r Rental) error {
	vars, err := rt.h.ReadFile(rt.spec.Firmware.Vars)
	if err != nil {
		return fmt.Errorf("read UEFI variables template: %w", err)
	}
	if err := rt.h.WriteFile(r.Vars, vars, 0600); err != nil {
		return fmt.Errorf("write UEFI variables: %w", err)
	}
	return nil
}

func gpuInUse(holders []string) error {
	return fmt.Errorf("the GPU is in use on this machine by %s; stop them first", strings.Join(holders, ", "))
}

func (rt *Runtime) takeGPUs(st *State, r *Rental, save func() error) error {
	g := readGPUHolders(rt.h, rt.spec.GPUs)
	if len(g.desktop) > 0 && !rt.spec.DesktopOnDemand {
		return errors.New(DesktopOnGPUProblem(g.desktop))
	}
	// Anything outside the desktop that is not short-lived: refused before the
	// desktop is touched, for a rental that could not start anyway.
	if len(g.other) > 0 {
		return gpuInUse(g.busy())
	}
	if len(g.desktop) == 0 {
		// Short-lived tools (an nvidia-smi) are given a moment to exit.
		g = settleGPUHolders(rt.h, rt.spec.GPUs)
		if busy := g.busy(); len(busy) > 0 {
			return gpuInUse(busy)
		}
		if len(g.desktop) > 0 && !rt.spec.DesktopOnDemand {
			return errors.New(DesktopOnGPUProblem(g.desktop))
		}
	}
	// A DGX Spark's desktop closes for the rental and comes back after it --
	// with everything in it, a tool typed in its terminal included.
	if len(g.desktop) > 0 {
		if err := rt.releaseDesktop(st, g.desktop, save); err != nil {
			return err
		}
	}
	// NVIDIA's own services are stopped for the rental and started again when
	// it ends. Recorded one at a time, so a start that dies halfway restarts
	// exactly the ones it stopped.
	// Each is recorded (and saved) before it is stopped: starting a service
	// that is already running is harmless, one never started again is not.
	for _, svc := range NVIDIAServices {
		if rt.h.Run("systemctl", "is-active", "--quiet", svc.Unit) != nil {
			continue
		}
		st.StoppedServices = append(st.StoppedServices, svc.Unit)
		if err := save(); err != nil {
			return err
		}
		if err := rt.h.Run("systemctl", "stop", svc.Unit); err != nil {
			return fmt.Errorf("stop %s: %w", svc.Unit, err)
		}
	}
	funcs, err := GroupFunctions(rt.h, rt.spec.GPUs)
	if err != nil {
		return err
	}
	st.Devices, err = BindVFIO(rt.h, funcs, func(b []BoundDevice) error {
		st.Devices = b
		return save()
	})
	if err != nil {
		return fmt.Errorf("hand the GPU to the microVM: %w", err)
	}
	r.VFIO = funcs
	return nil
}

func (rt *Runtime) alive(r Rental) bool {
	return r.Unit != "" && rt.h.Run("systemctl", "is-active", "--quiet", r.Unit) == nil
}

// Alive reports whether the rental's microVM is running.
func (rt *Runtime) Alive() bool {
	st, err := LoadState(rt.h, rt.spec.DataDir)
	return err == nil && st != nil && rt.alive(st.Rental)
}

// Present reports whether rental state (a running rental, or a dirty leftover
// of one) is on the machine.
func (rt *Runtime) Present() bool {
	st, err := LoadState(rt.h, rt.spec.DataDir)
	return err != nil || st != nil
}

// Dirty reports whether the last teardown did not verify.
func (rt *Runtime) Dirty() bool {
	st, err := LoadState(rt.h, rt.spec.DataDir)
	return err != nil || (st != nil && st.Dirty)
}

// exitReason is the tail of the VM unit's journal: QEMU's own words for why it
// stopped, which is what anyone debugging a failed boot needs first.
func (rt *Runtime) exitReason(r Rental) string {
	out, err := rt.h.Output("journalctl", "-u", r.Unit, "-n", "15", "--no-pager", "-o", "cat")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (rt *Runtime) waitGuest(r Rental) error {
	addr := net.JoinHostPort(GuestIP, "22")
	for waited := time.Duration(0); waited < BootTimeout; waited += pollInterval {
		if !rt.alive(r) {
			if why := rt.exitReason(r); why != "" {
				return fmt.Errorf("the microVM exited while booting: %s", why)
			}
			return errors.New("the microVM exited while booting")
		}
		if rt.h.DialTCP(addr, 3*time.Second) == nil {
			return nil
		}
		rt.h.Sleep(pollInterval)
	}
	return fmt.Errorf("the microVM did not open SSH within %v", BootTimeout)
}

// StopResult is what a teardown verified.
type StopResult struct {
	Wiped    bool
	GPUClean bool
	// NICDirty: a pair rental's ConnectX card did not come back as it went
	// (its interfaces, MACs, firmware or persistent configuration), or the
	// host put an address on it.
	NICDirty bool
	Detail   []string
}

// Clean is true when the machine can be rented again.
func (s StopResult) Clean() bool { return s.Wiped && s.GPUClean && !s.NICDirty }

// Stop tears down whatever rental state is on the machine and verifies it:
// the VM process gone, the disk mapping, loop device and file gone, the GPUs
// back on their driver and verified, the network and fence removed. A
// teardown that does not verify leaves the state on disk marked dirty, which
// keeps the machine from being rented again.
func (rt *Runtime) Stop() StopResult {
	st, err := LoadState(rt.h, rt.spec.DataDir)
	if err != nil {
		return StopResult{Detail: []string{"read rental state: " + err.Error()}}
	}
	if st == nil {
		return StopResult{Wiped: true, GPUClean: true}
	}
	var res StopResult

	vmGone := rt.killVM(st.Rental)
	if !vmGone {
		res.Detail = append(res.Detail, "the microVM process would not exit")
	}

	wiped, detail := DestroyDisk(rt.h, st.Disk)
	res.Wiped = wiped && vmGone
	res.Detail = append(res.Detail, detail...)

	released, detail := ReleaseVFIO(rt.h, st.Devices)
	res.Detail = append(res.Detail, detail...)
	for _, unit := range st.ServicesToRestart() {
		_ = rt.h.Run("systemctl", "start", unit)
	}
	res.GPUClean = released && vmGone
	if len(st.Devices) > 0 && res.GPUClean && rt.verifyGPU != nil && !rt.verifyGPU(st.Devices) {
		res.GPUClean = false
		res.Detail = append(res.Detail, "the GPU did not verify clean after the rental")
	}
	// The desktop comes back once the GPU is back with its driver. A GPU still
	// on vfio-pci has nothing to draw it; the record stays with the dirty state
	// and the teardown that frees the GPU starts the desktop.
	if st.StoppedDisplayManager != "" && released {
		_ = rt.h.Run("systemctl", "start", st.StoppedDisplayManager)
	}

	if len(st.NICDevices) > 0 || st.NICBaseline != nil {
		nicReleased, detail := ReleaseVFIO(rt.h, st.NICDevices)
		res.Detail = append(res.Detail, detail...)
		clean := nicReleased && vmGone
		if clean {
			ok, detail := VerifyNICBaseline(rt.h, st.NICBaseline)
			clean = ok
			res.Detail = append(res.Detail, detail...)
		}
		res.NICDirty = !clean
	}

	TeardownNetwork(rt.h, st.Net)
	if st.Fenced {
		if err := rt.fence.Remove(); err != nil {
			res.Detail = append(res.Detail, "remove firewall table: "+err.Error())
		}
	}

	// Keep the last serial log for diagnosis; it holds nothing of the tenant's
	// disk, only what the guest printed to its console.
	if rt.h.Exists(st.Rental.SerialLog) {
		if rt.h.Rename(st.Rental.SerialLog, lastSerialLog(rt.spec.DataDir)) != nil {
			// The rentals may be on another disk (setup --data-dir), which a
			// rename cannot cross: keep the log's end.
			if tail, err := rt.h.ReadTail(st.Rental.SerialLog, 1<<20); err == nil {
				_ = rt.h.WriteFile(lastSerialLog(rt.spec.DataDir), tail, 0600)
			}
		}
	}
	_ = rt.h.RemoveAll(st.Rental.Dir)

	if res.Clean() {
		_ = ClearState(rt.h, rt.spec.DataDir)
	} else {
		st.Dirty = true
		st.DirtyDetail = res.Detail
		_ = SaveState(rt.h, rt.spec.DataDir, st)
	}
	return res
}

// lastSerialLog is where the last VM's serial console is kept for diagnosis.
func lastSerialLog(dataDir string) string { return dataDir + "/last-serial.log" }

// killVM stops the VM's unit (SIGTERM, then SIGKILL after the unit's 30 s stop
// timeout) and confirms it is gone, killing it outright if systemd could not.
func (rt *Runtime) killVM(r Rental) bool {
	if !rt.alive(r) {
		return true
	}
	_ = rt.h.Run("systemctl", "stop", r.Unit)
	for waited := time.Duration(0); waited < StopGrace; waited += stopPoll {
		if !rt.alive(r) {
			return true
		}
		rt.h.Sleep(stopPoll)
	}
	_ = rt.h.Run("systemctl", "kill", "--signal=SIGKILL", r.Unit)
	for i := 0; i < 10; i++ {
		if !rt.alive(r) {
			return true
		}
		rt.h.Sleep(stopPoll)
	}
	return !rt.alive(r)
}
