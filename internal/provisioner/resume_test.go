package provisioner

import (
	"errors"
	"testing"

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
