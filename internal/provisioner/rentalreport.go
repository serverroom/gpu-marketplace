package provisioner

import "github.com/serverroom/gpu-marketplace/internal/vmrt"

// RentalReport is what a running rental uses and who is connected to it, for
// the staff's view of live rentals (v0.3.4): the runtime's own look at its CPU,
// memory, GPU, disk and network (vmrt.Usage), and the renter's SSH sessions as
// the forward carrying them has seen them. nil while no rental is running.
type RentalReport struct {
	Usage *vmrt.Usage        `json:"usage,omitempty"`
	SSH   *vmrt.SessionStats `json:"ssh,omitempty"`
}

type usageReader interface{ Usage() *vmrt.Usage }

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
	if r.Usage == nil && r.SSH == nil {
		return nil
	}
	return &r
}
