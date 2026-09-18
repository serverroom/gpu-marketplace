package vmrt

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
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
	// the rental is left on them. Vendor-specific, so the provisioner supplies it.
	verifyGPU func() bool
}

// New builds a runtime.
func New(h Host, spec Spec, fence Fence, verifyGPU func() bool) *Runtime {
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
	userData, err := UserData(o.ID, o.Pubkey, o.Probes)
	if err != nil {
		return err
	}

	r := NewRental(rt.spec.DataDir, o.ID)
	st := &State{RentalID: o.ID, Rental: r, StartedAt: time.Now().Unix()}
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

	if err = rt.writeSeed(r, userData, o.ID); err != nil {
		return err
	}
	if err = rt.copyVars(r); err != nil {
		return err
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

func (rt *Runtime) writeSeed(r Rental, userData, id string) error {
	files := map[string]string{
		"user-data":      userData,
		"meta-data":      MetaData(id),
		"network-config": NetworkConfig(),
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
	desktop, other := ClassifyGPUHolders(rt.h)
	if len(desktop) > 0 && !rt.spec.DesktopOnDemand {
		return errors.New(DesktopOnGPUProblem(desktop))
	}
	if len(other) > 0 {
		return gpuInUse(other)
	}
	// A DGX Spark's desktop closes for the rental and comes back after it.
	if len(desktop) > 0 {
		if err := rt.releaseDesktop(st, desktop, save); err != nil {
			return err
		}
		if _, other := ClassifyGPUHolders(rt.h); len(other) > 0 {
			return gpuInUse(other)
		}
	}
	// NVIDIA's own services are stopped for the rental and started again when
	// it ends. Recorded one at a time, so a start that dies halfway restarts
	// exactly the ones it stopped.
	for _, svc := range NVIDIAServices {
		if rt.h.Run("systemctl", "is-active", "--quiet", svc.Unit) != nil {
			continue
		}
		if err := rt.h.Run("systemctl", "stop", svc.Unit); err != nil {
			return fmt.Errorf("stop %s: %w", svc.Unit, err)
		}
		st.StoppedServices = append(st.StoppedServices, svc.Unit)
	}
	funcs, err := GroupFunctions(rt.h, rt.spec.GPUs)
	if err != nil {
		return err
	}
	st.Devices, err = BindVFIO(rt.h, funcs)
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
	Detail   []string
}

// Clean is true when the machine can be rented again.
func (s StopResult) Clean() bool { return s.Wiped && s.GPUClean }

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
	if len(st.Devices) > 0 && res.GPUClean && rt.verifyGPU != nil && !rt.verifyGPU() {
		res.GPUClean = false
		res.Detail = append(res.Detail, "the GPU did not verify clean after the rental")
	}
	// The desktop comes back once the GPU is back with its driver. A GPU still
	// on vfio-pci has nothing to draw it; the record stays with the dirty state
	// and the teardown that frees the GPU starts the desktop.
	if st.StoppedDisplayManager != "" && released {
		_ = rt.h.Run("systemctl", "start", st.StoppedDisplayManager)
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
		_ = rt.h.Rename(st.Rental.SerialLog, rt.spec.DataDir+"/last-serial.log")
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
