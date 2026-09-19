package provisioner

import (
	"strings"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// The host keeps using its machine until it is rented (v0.2.3): what it uses
// of what a rental would take -- its own programs on the GPU, the memory the
// rental's VM needs -- is reported as the capability's host_busy, and does
// not make the machine "not ready".

// HostUse is what the host was using when last looked at.
func (p *Provisioner) HostUse() vmrt.HostUse {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hostUse
}

// setHostUseLocked puts the host's use into the capability; changed says
// whether what the marketplace sees is different (not counting since).
func (p *Provisioner) setHostUseLocked(u vmrt.HostUse, now int64) (changed bool) {
	var hb *control.HostBusy
	if u.Busy() {
		if p.busySince == 0 {
			p.busySince = now
		}
		hb = &control.HostBusy{Since: p.busySince, Holders: append([]string{}, u.Holders...), MemoryShortGB: u.MemoryShortGB()}
	} else {
		p.busySince = 0
	}
	old := p.capability.HostBusy
	changed = (old == nil) != (hb == nil) ||
		(old != nil && (strings.Join(old.Holders, "\n") != strings.Join(hb.Holders, "\n") || old.MemoryShortGB != hb.MemoryShortGB))
	p.hostUse = u
	p.capability.HostBusy = hb
	return changed
}
