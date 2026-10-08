package vmrt

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Waiting for a VM that started paused, overridable by tests.
var (
	monitorWait = 30 * time.Second
	monitorPoll = 1 * time.Second
)

// pinVCPUs gives each vCPU of a VM in the wider layout (layout.go) its own
// physical core, before the guest runs a single instruction. The VM was
// started paused (-S) on the fastest cluster. Here:
//
//  1. QEMU says which thread is which vCPU, and each is pinned to the core
//     its place in GuestCores names; the pin is read back from the kernel.
//  2. The paused VM is reset once. A reset is where QEMU writes back, on each
//     vCPU's own thread, the registers its main thread read: the write a host
//     kernel refuses when it cannot run a VM on two core types. It is also
//     what a renter's own reboot does, so a VM that cannot take it must never
//     be handed to one.
//  3. Only then is the guest let run.
//
// Any step failing fails the start: the caller tears the VM down, and the
// machine goes back to its one-core-type layout (widerFailed).
func (rt *Runtime) pinVCPUs(r Rental) error {
	mon, ok := rt.h.(Monitor)
	if !ok {
		return errors.New("this host cannot reach a VM's monitor")
	}
	var out []string
	var err error
	for waited := time.Duration(0); ; waited += monitorPoll {
		if out, err = mon.QMP(r.QMP, "query-cpus-fast"); err == nil {
			break
		}
		if !rt.alive(r) {
			if why := rt.exitReason(r); why != "" {
				return fmt.Errorf("the microVM exited as it started: %s", why)
			}
			return errors.New("the microVM exited as it started")
		}
		if waited >= monitorWait {
			return fmt.Errorf("the VM's monitor did not answer in %v: %w", monitorWait, err)
		}
		rt.h.Sleep(monitorPoll)
	}
	threads, err := vcpuThreads(out[0])
	if err != nil {
		return err
	}
	cores := rt.spec.GuestCores
	if len(threads) != len(cores) {
		return fmt.Errorf("the VM has %d vCPUs, and its layout has %d cores", len(threads), len(cores))
	}
	for i, core := range cores {
		tid, ok := threads[i]
		if !ok {
			return fmt.Errorf("the VM names no thread for vCPU %d", i)
		}
		if err := rt.h.Run("taskset", "-pc", strconv.Itoa(core), strconv.Itoa(tid)); err != nil {
			return fmt.Errorf("pin vCPU %d to CPU %d: %w", i, core, err)
		}
		if got := allowedCPUs(rt.h, tid); got != strconv.Itoa(core) {
			return fmt.Errorf("vCPU %d is allowed on CPUs %q after its pin, not on CPU %d alone", i, got, core)
		}
	}

	// The reset says RESET before it writes the registers back, and answers
	// nothing else until it has: the status after it is the reset's verdict.
	if out, err = mon.QMP(r.QMP, "system_reset", "event:RESET", "query-status"); err != nil {
		return fmt.Errorf("reset the paused VM: %w", err)
	}
	if status := vmStatus(out[len(out)-1]); status != "prelaunch" && status != "paused" {
		return fmt.Errorf("the VM did not take a reset with its vCPUs on two types of core (it is %q after it)", status)
	}
	if out, err = mon.QMP(r.QMP, "cont", "query-status"); err != nil {
		return fmt.Errorf("let the VM run: %w", err)
	}
	if status := vmStatus(out[len(out)-1]); status != "running" {
		return fmt.Errorf("the VM is %q after it was let run", status)
	}
	return nil
}

// ErrLayout is matched by a start that failed because the VM could not be put
// in the wider layout: its vCPUs not pinned, or the VM refusing its reset
// there. The machine rents on one core type from then on.
var ErrLayout = errors.New("the VM could not be given a core for each vCPU")

// layoutError is a wider-layout failure with its cause.
type layoutError struct{ cause error }

func (e *layoutError) Error() string        { return ErrLayout.Error() + ": " + e.cause.Error() }
func (e *layoutError) Is(target error) bool { return target == ErrLayout }
func (e *layoutError) Unwrap() error        { return e.cause }

// widerFailed records that the wider layout, proven on this machine by a test
// boot, failed when a rental started in it: the next look at the machine
// (ChooseLayout) puts it back on one core type. The record keeps the agent
// version that proved it, so it holds exactly as long as the proof did.
func (rt *Runtime) widerFailed(why string) {
	rec := LoadLayout(rt.h, rt.spec.DataDir)
	if rec == nil || !rec.Usable {
		return
	}
	_ = MarkLayout(rt.h, rt.spec.DataDir, rec.AgentVersion, rt.spec.GuestCores, false, "a rental did not start that way: "+why)
}

// vcpuThreads reads QMP's query-cpus-fast: each vCPU's host thread, by the
// vCPU's index.
func vcpuThreads(answer string) (map[int]int, error) {
	var cpus []struct {
		Index  *int `json:"cpu-index"`
		Thread *int `json:"thread-id"`
	}
	if err := json.Unmarshal([]byte(answer), &cpus); err != nil {
		return nil, fmt.Errorf("the VM's list of vCPUs could not be read: %w", err)
	}
	threads := map[int]int{}
	for _, c := range cpus {
		if c.Index == nil || c.Thread == nil || *c.Thread <= 0 {
			return nil, errors.New("the VM's list of vCPUs names a vCPU without its thread")
		}
		threads[*c.Index] = *c.Thread
	}
	return threads, nil
}

// vmStatus reads QMP's query-status: "prelaunch", "running", "internal-error".
func vmStatus(answer string) string {
	var st struct {
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(answer), &st) != nil || st.Status == "" {
		return "unknown"
	}
	return st.Status
}

// allowedCPUs is the list of CPUs the kernel lets a thread run on ("2",
// "4-7"), or "" when it cannot be read.
func allowedCPUs(h Host, tid int) string {
	data, err := h.ReadFile("/proc/" + strconv.Itoa(tid) + "/status")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "Cpus_allowed_list" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
