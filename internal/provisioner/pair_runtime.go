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
	if !p.hasPairRuntime() || p.runtime == nil {
		return control.Unavailable("this agent cannot host a pair rental on this machine")
	}
	if !p.vendor.CanHost() {
		return control.Unavailable("%v (vendor %s)", ErrVendorCannotIsolate, p.vendor)
	}
	if c := p.Capability(); !c.Ready {
		return control.Unavailable("%v: %s", ErrNotReady, strings.Join(c.Reasons, "; "))
	}
	if !control.ValidRentalID(req.RentalID) {
		return control.Invalid("rental_id %q is not a rental id", req.RentalID)
	}
	key, err := vmrt.NormalizePubkey(req.RenterPubkey)
	if err != nil {
		return control.Invalid("renter_pubkey: %v", err)
	}

	// The ports, their addresses and the card itself are read again now: a
	// capability from the agent's start is not proof of the machine as it is.
	rep := p.freshPair()
	p.refreshWith(rep)
	if !rep.Ready() {
		return control.Unavailable("this machine cannot be half of a linked pair: %s", strings.Join(rep.Reasons(), "; "))
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
			return control.Conflict("link %d: %s is not a ConnectX-7 port of this machine", i, mac)
		}
		linked[mac] = true
	}
	for m := range ports {
		if !linked[m] {
			opts.OtherMACs = append(opts.OtherMACs, m)
		}
	}
	if err := vmrt.ValidatePair(req.RentalID, opts); err != nil {
		return control.Invalid("%v", err)
	}

	// Take the machine, and the ports: no cable check, peer announcement or
	// pair test may touch the card while it is being handed over.
	p.mu.Lock()
	switch {
	case p.withdrawn || p.settingUp || !p.capability.Ready:
		p.mu.Unlock()
		return control.Unavailable("%v: it is setting itself up, or was removed from the marketplace", ErrNotReady)
	case p.checking:
		p.mu.Unlock()
		return control.Conflict("a cable check is running on this machine; retry shortly")
	case p.status != StatusFree:
		status := p.status
		p.mu.Unlock()
		return control.Conflict("this machine is %s, not free", status)
	}
	p.status = StatusProvisioning
	p.lastErr = ""
	machine := p.machine
	p.mu.Unlock()
	unlock, ok, err := p.lockFrames()
	if err != nil || !ok {
		p.mu.Lock()
		p.status = StatusFree
		p.mu.Unlock()
		if err != nil {
			return fmt.Errorf("take the cable-check lock: %w", err)
		}
		return control.Conflict("a cable check or pair test is running on this machine; retry shortly")
	}
	o := vmrt.StartOptions{ID: req.RentalID, Pubkey: key, Pair: opts}
	if !p.async {
		defer unlock()
		return p.start(machine, o)
	}
	go func() {
		defer unlock()
		_ = p.start(machine, o)
	}()
	return nil
}

// PairStatus is the pair rental on this machine for /status: its node and what
// its guest has said about each link. nil when no pair rental is here.
func (p *Provisioner) PairStatus() *control.PairStatus {
	if p.runtime == nil {
		return nil
	}
	pair, ok, fail := p.runtime.PairRental()
	if pair == nil {
		return nil
	}
	return &control.PairStatus{Node: pair.Node, GuestLink: control.GuestLinkStatus{OK: ok, Fail: fail, Links: len(pair.Links)}}
}
