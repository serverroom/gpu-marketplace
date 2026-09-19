package provisioner

import (
	"fmt"
	"net"
	"strings"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// PairProvision starts this machine's half of a linked-pair rental (POST
// /pair/provision): like Provision, in the background, but with the
// ConnectX card handed to the VM and the cable configured in the guest. It
// refuses with 503 unless the machine can host a rental and be half of a pair
// right now, 400 for any field that does not validate, and 409 for a link on
// a port this machine does not have or a machine that is not free.
func (p *Provisioner) PairProvision(req control.PairProvisionRequest) error {
	_, err := p.PairProvisionBy(req)
	return err
}

// PairProvisionBy is PairProvision that, on a machine the host is using,
// waits for the host up to req.StartBy, as ProvisionBy does: the answer is
// then the pending rental. The request is checked in full first, and again
// when the rental starts.
func (p *Provisioner) PairProvisionBy(req control.PairProvisionRequest) (*control.PendingRental, error) {
	o, err := p.pairOptions(req)
	if err != nil {
		return nil, err
	}
	var startBy int64
	if req.StartBy != nil {
		startBy = *req.StartBy
	}
	needTest := !p.fullTestIsCurrent()
	use := p.hostUseNow()
	desktop := !use.Busy() && startBy > p.clock().Unix() && len(p.loggedInDesktop()) > 0

	// Take the machine, and the ports: no cable check, peer announcement or
	// pair test may touch the card while it is being handed over.
	p.mu.Lock()
	if view, err := p.pendingConflictLocked(req.RentalID); view != nil || err != nil {
		p.mu.Unlock()
		return view, err
	}
	switch {
	case p.withdrawn || p.settingUp || !p.capability.Ready:
		p.mu.Unlock()
		return nil, control.Unavailable("%v: it is setting itself up, or was removed from the marketplace", ErrNotReady)
	case p.checking:
		p.mu.Unlock()
		return nil, control.Conflict("a cable check is running on this machine; retry shortly")
	case p.status != StatusFree:
		status := p.status
		p.mu.Unlock()
		return nil, control.Conflict("this machine is %s, not free", status)
	}
	stored := req
	rec := &pendingRecord{RentalID: req.RentalID, RenterPubkey: o.Pubkey, Pair: &stored, StartBy: startBy}
	if use.Busy() || desktop {
		view, err := p.waitLocked(rec, use) // unlocks
		if err == nil && desktop && p.desktopHold(rec) {
			view = p.PendingRental()
		}
		if err == nil {
			p.mu.Lock()
			if p.pending == rec && !rec.running {
				p.launchHoldLocked(rec)
			}
			p.mu.Unlock()
		}
		return view, err
	}
	if rec.pairHold() {
		// Free: held until the other half is ready too (pairhold.go), which a
		// free peer is within the minute -- answered as starting meanwhile.
		rec.State, rec.Since = control.PendingStarting, p.clock().Unix()
		p.pending = rec
		p.status = StatusProvisioning
		p.lastErr = ""
		p.setHostUseLocked(vmrt.HostUse{}, rec.Since)
		_ = p.savePendingLocked(rec)
		p.launchHoldLocked(rec)
		p.mu.Unlock()
		return nil, nil
	}
	p.status = StatusProvisioning
	p.lastErr = ""
	p.startingLocked()
	machine := p.machine
	if needTest {
		return nil, p.launchLocked(rec) // unlocks
	}
	p.mu.Unlock()
	unlock, err := p.takeFrames()
	if err != nil {
		p.mu.Lock()
		p.status = StatusFree
		p.mu.Unlock()
		return nil, err
	}
	if !p.async {
		defer unlock()
		return nil, p.start(machine, o)
	}
	go func() {
		defer unlock()
		_ = p.start(machine, o)
	}()
	return nil, nil
}

// takeFrames takes the cable-check lock for a pair rental's start.
func (p *Provisioner) takeFrames() (func(), error) {
	unlock, ok, err := p.lockFrames()
	if err != nil {
		return nil, fmt.Errorf("take the cable-check lock: %w", err)
	}
	if !ok {
		return nil, control.Conflict("a cable check or pair test is running on this machine; retry shortly")
	}
	return unlock, nil
}

// pairOptions checks a pair request against this machine as it is now, and
// turns it into the rental's start options.
func (p *Provisioner) pairOptions(req control.PairProvisionRequest) (vmrt.StartOptions, error) {
	v := p.view()
	if !p.hasPairRuntime() || v.runtime == nil {
		return vmrt.StartOptions{}, control.Unavailable("this agent cannot host a pair rental on this machine")
	}
	if !v.vendor.CanHost() {
		return vmrt.StartOptions{}, control.Unavailable("%v (vendor %s)", ErrVendorCannotIsolate, v.vendor)
	}
	if c := p.Capability(); !c.Ready {
		return vmrt.StartOptions{}, control.Unavailable("%v: %s", ErrNotReady, strings.Join(c.Reasons, "; "))
	}
	if !control.ValidRentalID(req.RentalID) {
		return vmrt.StartOptions{}, control.Invalid("rental_id %q is not a rental id", req.RentalID)
	}
	if vmrt.IsSetupID(req.RentalID) {
		return vmrt.StartOptions{}, control.Invalid("rental_id %q is one this agent keeps for its own test boots", req.RentalID)
	}
	key, err := vmrt.NormalizePubkey(req.RenterPubkey)
	if err != nil {
		return vmrt.StartOptions{}, control.Invalid("renter_pubkey: %v", err)
	}

	// The ports, their addresses and the card itself are read again now: a
	// capability from the agent's start is not proof of the machine as it is.
	rep := p.freshPair()
	p.refreshWith(rep)
	if !rep.Ready() {
		return vmrt.StartOptions{}, control.Unavailable("this machine cannot be half of a linked pair: %s", strings.Join(rep.Reasons(), "; "))
	}
	opts := &vmrt.PairOptions{Node: req.Node, PeerHostname: req.PeerHostname, MTU: req.MTU, Functions: rep.Functions()}
	for _, l := range req.Links {
		opts.Links = append(opts.Links, vmrt.GuestLink{LocalMAC: l.LocalMAC, CIDR: l.CIDR, PeerIP: l.PeerIP})
	}
	if req.IntraKey != nil {
		opts.IntraKey = &vmrt.IntraKey{PrivateOpenSSH: req.IntraKey.PrivateOpenSSH, Public: req.IntraKey.Public}
	}
	ports := map[string]bool{}
	for _, m := range rep.PortMACs() {
		ports[strings.ToLower(m)] = true
	}
	linked := map[string]bool{}
	for i, l := range opts.Links {
		mac := strings.ToLower(strings.TrimSpace(l.LocalMAC))
		if _, err := net.ParseMAC(mac); err == nil && !ports[mac] {
			return vmrt.StartOptions{}, control.Conflict("link %d: %s is not a ConnectX-7 port of this machine", i, mac)
		}
		linked[mac] = true
	}
	for m := range ports {
		if !linked[m] {
			opts.OtherMACs = append(opts.OtherMACs, m)
		}
	}
	if err := vmrt.ValidatePair(req.RentalID, opts); err != nil {
		return vmrt.StartOptions{}, control.Invalid("%v", err)
	}
	return vmrt.StartOptions{ID: req.RentalID, Pubkey: key, Pair: opts}, nil
}

// PairStatus is the pair rental on this machine for /status: its node and what
// its guest has said about each link. nil when no pair rental is here.
func (p *Provisioner) PairStatus() *control.PairStatus {
	rt := p.Runtime()
	if rt == nil {
		return nil
	}
	pair, ok, fail := rt.PairRental()
	if pair == nil {
		return nil
	}
	return &control.PairStatus{Node: pair.Node, GuestLink: control.GuestLinkStatus{OK: ok, Fail: fail, Links: len(pair.Links)}}
}
