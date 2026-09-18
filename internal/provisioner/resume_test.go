package provisioner

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

type nopFence struct{}

func (nopFence) Apply() error  { return nil }
func (nopFence) Remove() error { return nil }

// runtimeWithState is a real runtime on a fake machine whose disk already
// holds st: what an agent finds when it starts.
func runtimeWithState(t *testing.T, st *vmrt.State, desktopOnDemand bool) (*fakehost.Host, *vmrt.Runtime) {
	t.Helper()
	h := fakehost.New()
	h.Fail["systemctl is-active --quiet gpu-rental-"] = errors.New("inactive") // the VM is gone
	h.Fail["iptables -S DOCKER-USER"] = errors.New("no such chain")
	if err := vmrt.SaveState(h, dataDir, st); err != nil {
		t.Fatal(err)
	}
	s := spec()
	s.DesktopOnDemand = desktopOnDemand
	return h, vmrt.New(h, s, nopFence{}, func() bool { return true })
}

// An agent restarted halfway through its own base image build must not take
// the bake VM for a rental: it tears it down and the machine is free.
func TestResumeTearsDownAnOrphanedSetupVM(t *testing.T) {
	for _, id := range []string{vmrt.BakeID, vmrt.SelfTestPrefix + "1726600000"} {
		h, rt := runtimeWithState(t, &vmrt.State{RentalID: id, Rental: vmrt.NewRental(dataDir, id)}, false)
		h.SetFail("systemctl is-active --quiet gpu-rental-"+id, nil) // still running: its unit outlives the agent
		h.OnRun["systemctl stop gpu-rental-"+id] = func(h *fakehost.Host, cmd string) {
			h.SetFail("systemctl is-active --quiet "+fakehost.LastField(cmd), errors.New("inactive"))
		}
		p := New(rt, VendorNVIDIA, []string{gpu}, false)
		withFakeForward(t, nil)
		p.Resume()
		if p.Status() != StatusFree || rt.Present() {
			t.Errorf("%s: status=%s present=%v; an orphaned setup VM must be torn down", id, p.Status(), rt.Present())
		}
		if !h.Ran("run systemctl stop gpu-rental-" + id) {
			t.Errorf("%s: the VM was not stopped", id)
		}
	}
}

// A test boot a person is running right now (its process holds busy.json) is
// theirs to finish.
func TestResumeLeavesASetupVMAnotherProcessRuns(t *testing.T) {
	h, rt := runtimeWithState(t, &vmrt.State{RentalID: vmrt.BakeID, Rental: vmrt.NewRental(dataDir, vmrt.BakeID)}, false)
	other := os.Getpid() + 1
	h.Files[vmrt.BusyPath(dataDir)] = []byte(fmt.Sprintf(`{"pid":%d,"what":"building the rental image"}`, other))
	h.Files[fmt.Sprintf("/proc/%d", other)] = nil
	p := New(rt, VendorNVIDIA, []string{gpu}, false)
	p.Resume()
	if !rt.Present() || h.Ran("run systemctl stop") {
		t.Error("another process's bake was torn down")
	}
	if p.Status() != StatusFree {
		t.Errorf("status = %s", p.Status())
	}
}

// A Spark rental whose VM died with the machine: the resume teardown starts
// the desktop again.
func TestResumeTeardownStartsTheSparkDesktop(t *testing.T) {
	st := &vmrt.State{RentalID: "R1", Rental: vmrt.NewRental(dataDir, "R1"), StoppedDisplayManager: "display-manager.service"}
	h, rt := runtimeWithState(t, st, true)
	p := New(rt, VendorNVIDIA, []string{gpu}, false)
	p.Resume()
	if p.Status() != StatusFree {
		t.Errorf("status = %s (%s)", p.Status(), p.LastError())
	}
	if !h.Ran("run systemctl start display-manager.service") {
		t.Error("the desktop did not come back")
	}
}

func TestAdoptTakesAFreshDetectOnlyWhileFree(t *testing.T) {
	m := &fakeMachine{}
	p := readyProv(m)
	p.SetCapability(control.Capability{Reasons: []string{"Setting up automatically: ..."}})
	fresh := readyProv(&fakeMachine{})
	fresh.capability.Reasons = nil
	if !p.Adopt(fresh) || !p.Capability().Ready {
		t.Fatalf("a free machine did not adopt the fresh detect: %+v", p.Capability())
	}
	p.status = StatusRented
	stale := New(&fakeMachine{}, VendorNVIDIA, nil, false)
	if p.Adopt(stale) || !p.Capability().Ready {
		t.Error("a rented machine's view was replaced")
	}
}

func TestSetCapabilityNeverMakesAMachineReady(t *testing.T) {
	p := New(&fakeMachine{}, VendorNVIDIA, nil, false)
	p.SetCapability(control.Capability{Ready: true, Reasons: []string{"x"}})
	if p.Capability().Ready {
		t.Error("SetCapability made a machine ready")
	}
}

// While the automatic setup runs, the only VM on the machine is its own: no
// rental starts, and a teardown does not reach the setup's VM.
func TestSettingUpRefusesProvisionAndTeardown(t *testing.T) {
	m := &fakeMachine{}
	p := readyProv(m)
	if !p.BeginSetup() || p.BeginSetup() {
		t.Fatal("BeginSetup on a free machine, twice")
	}
	if err := p.Provision("R1", renterKey(t)); err == nil || !errors.Is(err, ErrNotReady) {
		t.Errorf("Provision = %v", err)
	}
	if err := p.Teardown("R1"); err == nil || !strings.Contains(err.Error(), "setting itself up") {
		t.Errorf("Teardown = %v", err)
	}
	if m.started != 0 || m.stopped != 0 {
		t.Errorf("machine touched: started=%d stopped=%d", m.started, m.stopped)
	}
	p.EndSetup()
	p.capability.Ready = true // the setup finished and a fresh detect said ready
	if err := p.Provision("R1", renterKey(t)); err != nil {
		t.Errorf("Provision after the setup = %v", err)
	}
	// And never the other way round: no setup beside a rental.
	if p.BeginSetup() {
		t.Error("the setup began on a rented machine")
	}
}

// A machine the host removed in the control panel refuses every rental, and
// nothing makes it ready again: not the setup, not a fresh detect.
func TestAWithdrawnMachineStaysWithdrawn(t *testing.T) {
	p := readyProv(&fakeMachine{})
	p.Withdraw("This machine was removed from the marketplace in the control panel.")
	if c := p.Capability(); c.Ready || len(c.Reasons) != 1 || !strings.Contains(c.Reasons[0], "removed from the marketplace") || !p.Withdrawn() {
		t.Fatalf("capability = %+v", c)
	}
	if err := p.Provision("R1", renterKey(t)); !errors.Is(err, ErrNotReady) {
		t.Errorf("Provision = %v", err)
	}
	if p.BeginSetup() {
		t.Error("the setup began on a withdrawn machine")
	}
	if p.Adopt(readyProv(&fakeMachine{})) || p.Capability().Ready {
		t.Error("a fresh detect made a withdrawn machine ready")
	}
	p.SetCapability(control.Capability{Reasons: []string{"Setting up automatically: ..."}})
	if !strings.Contains(p.Capability().Reasons[0], "removed from the marketplace") {
		t.Error("the setup's progress replaced the withdrawn reason")
	}
}
