package provisioner

import "github.com/serverroom/gpu-marketplace/internal/vmrt"

// RentalReport is what a running rental uses and who is connected to it, for
// the staff's view of live rentals (v0.3.4): the runtime's own look at its CPU,
// memory, GPU, disk and network (vmrt.Usage), and the renter's SSH sessions as
// the forward carrying them has seen them. HostKeys are the rental's own SSH
// host keys, read before the renter was let in, so the marketplace can show
// the renter what their first connection must show (v0.3.15). nil while no
// rental is running.
type RentalReport struct {
	Usage    *vmrt.Usage        `json:"usage,omitempty"`
	SSH      *vmrt.SessionStats `json:"ssh,omitempty"`
	HostKeys *vmrt.HostKeys     `json:"host_keys,omitempty"`
}

type usageReader interface{ Usage() *vmrt.Usage }

type hostKeyReader interface{ HostKeys() *vmrt.HostKeys }

type sessionCounter interface{ Sessions() vmrt.SessionStats }

// RentalReport is the running rental's report, or nil.
func (p *Provisioner) RentalReport() interface{} {
	p.mu.Lock()
	status, machine, fwd := p.status, p.machine, p.forward
	p.mu.Unlock()
	if status != StatusRented {
		return nil
	}
	var r RentalReport
	if u, ok := machine.(usageReader); ok {
		r.Usage = u.Usage()
	}
	if c, ok := fwd.(sessionCounter); ok {
		s := c.Sessions()
		r.SSH = &s
	}
	if k, ok := machine.(hostKeyReader); ok {
		r.HostKeys = k.HostKeys()
	}
	if r.Usage == nil && r.SSH == nil && r.HostKeys == nil {
		return nil
	}
	return &r
}

// recordReader is a runtime that keeps its rentals' measurements.
type recordReader interface{ Records() []vmrt.RentalRecord }

// Records are the readings of the machine from before and after its last
// rentals (vmrt/measure.go), oldest first.
func (p *Provisioner) Records() []vmrt.RentalRecord {
	p.mu.Lock()
	machine := p.machine
	p.mu.Unlock()
	if r, ok := machine.(recordReader); ok {
		return r.Records()
	}
	return nil
}

// Measurements is Records for /status: nil when there are none.
func (p *Provisioner) Measurements() interface{} {
	if records := p.Records(); len(records) > 0 {
		return records
	}
	return nil
}
