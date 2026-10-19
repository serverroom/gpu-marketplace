package provisioner

import (
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// usageMachine is a Machine that reports a rental's usage.
type usageMachine struct {
	Machine
	u *vmrt.Usage
}

func (m usageMachine) Usage() *vmrt.Usage { return m.u }

type countingForward struct{ s vmrt.SessionStats }

func (f countingForward) Stop()                       {}
func (f countingForward) Sessions() vmrt.SessionStats { return f.s }

// The report is the runtime's usage and the forward's sessions while a rental
// runs, and nil otherwise.
func TestTheRentalReport(t *testing.T) {
	u := &vmrt.Usage{RentalID: "R1", Mode: "container"}
	p := New(usageMachine{u: u}, VendorContainerNV, nil, true)
	if p.RentalReport() != nil {
		t.Error("a free machine reports no rental")
	}
	p.status = StatusRented
	p.forward = countingForward{s: vmrt.SessionStats{Active: 1, Sessions: 3}}
	r, ok := p.RentalReport().(*RentalReport)
	if !ok || r.Usage != u || r.SSH == nil || r.SSH.Active != 1 || r.SSH.Sessions != 3 {
		t.Errorf("report = %+v", r)
	}
}

// keyMachine is a Machine whose rental has its SSH host keys.
type keyMachine struct {
	Machine
	k *vmrt.HostKeys
}

func (m keyMachine) HostKeys() *vmrt.HostKeys { return m.k }

// The rental's host keys are in the report while it runs, even when nothing
// else is: they are what its renter checks the first connection against.
func TestTheRentalReportCarriesTheHostKeys(t *testing.T) {
	k := &vmrt.HostKeys{RentalID: "R1", Keys: []string{"ssh-ed25519 AAAA"}, At: 1}
	p := New(keyMachine{k: k}, VendorContainerNV, nil, true)
	if p.RentalReport() != nil {
		t.Error("a free machine reports no rental")
	}
	p.status = StatusRented
	r, ok := p.RentalReport().(*RentalReport)
	if !ok || r.HostKeys != k || r.Usage != nil {
		t.Errorf("report = %+v", r)
	}
}
