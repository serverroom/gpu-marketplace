package provisioner

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/interconnect"
)

// The linked-pair half of the provisioner: the raw-frame cable check the
// control plane asks for, the peer announcements the agent makes on its own,
// and (pair_runtime.go) one machine's half of a pair rental. None of it ever
// changes whether this machine can host a single rental.

// HelloWindow is how long one peer announcement listens (CONTRACT.md s3.1).
var HelloWindow = 5 * time.Second

// SetListingID tells the provisioner how to learn this machine's listing id,
// which a cable check must be addressed to and peer announcements carry.
func (p *Provisioner) SetListingID(f func() string) { p.listingID = f }

func (p *Provisioner) selfListing() string {
	if p.listingID == nil {
		return ""
	}
	return p.listingID()
}

func (p *Provisioner) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// hasPairRuntime: this provisioner was built by Detect, on a machine it can
// act on.
func (p *Provisioner) hasPairRuntime() bool {
	return p.host != nil && p.openPacket != nil && p.lockFrames != nil
}

// beginFrames takes the ports for one raw-frame run: nothing else may be
// sending on them, and no rental (or leftover, or test boot) may be on the
// machine, because a rental's ConnectX card belongs to its VM. The returned
// func gives them back.
func (p *Provisioner) beginFrames() (func(), error) {
	unlock, ok, err := p.lockFrames()
	if err != nil {
		return nil, fmt.Errorf("take the cable-check lock: %w", err)
	}
	if !ok {
		return nil, control.Conflict("a cable check is already running on this machine")
	}
	p.mu.Lock()
	switch {
	case p.withdrawn:
		p.mu.Unlock()
		unlock()
		return nil, control.Unavailable("this machine was removed from the marketplace")
	case p.settingUp:
		p.mu.Unlock()
		unlock()
		return nil, control.Conflict("the agent is setting this machine up; the cable is checked once that has finished")
	case p.status != StatusFree:
		status := p.status
		p.mu.Unlock()
		unlock()
		return nil, control.Conflict("this machine is %s; the cable is checked only while it is free", status)
	case p.checking:
		p.mu.Unlock()
		unlock()
		return nil, control.Conflict("a cable check is already running on this machine")
	case p.machine.Present():
		p.mu.Unlock()
		unlock()
		return nil, control.Conflict("a rental (or the leftover of one, or a test boot) is on this machine")
	}
	p.checking = true
	p.frames.Add(1)
	p.mu.Unlock()
	done := func() {
		p.mu.Lock()
		p.checking = false
		p.mu.Unlock()
		p.frames.Done()
		unlock()
	}
	if err := p.recoverPortsLocked(); err != nil {
		done()
		return nil, control.Unavailable("%v", err)
	}
	return done, nil
}

// recoverPortsLocked plays back a record an unfinished run left, holding the
// frame lock: a new run starts only from the ports' original state.
func (p *Provisioner) recoverPortsLocked() error {
	problems := interconnect.RecoverPorts(p.host, p.dataDir)
	if len(problems) == 0 && !p.host.Exists(interconnect.JournalPath(p.dataDir)) {
		return nil
	}
	for _, problem := range problems {
		log.Printf("restoring a ConnectX-7 port after an unfinished cable check: %s", problem)
	}
	if len(problems) == 0 {
		return interconnect.ErrUnfinishedRun
	}
	return fmt.Errorf("%w (%s)", interconnect.ErrUnfinishedRun, strings.Join(problems, "; "))
}

// WaitFrames waits up to timeout for a raw-frame run in progress to put its
// ports back, so an agent that is stopping does not exit halfway through one.
func (p *Provisioner) WaitFrames(timeout time.Duration) {
	done := make(chan struct{})
	go func() { p.frames.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// freshPair re-runs the pair preflight: carrier, addresses and the ports
// themselves can change while the agent runs.
func (p *Provisioner) freshPair() interconnect.Report {
	rep := interconnect.Preflight(p.host, p.view().pairOpts)
	p.mu.Lock()
	p.pair = rep
	p.mu.Unlock()
	return rep
}

// RefreshPair re-runs the pair preflight and puts it, with the peers heard,
// into the capability. changed says whether what the control plane would see
// is different, so the caller knows to report it.
func (p *Provisioner) RefreshPair() (changed bool) {
	if p.host == nil {
		return false
	}
	return p.refreshWith(p.freshPair())
}

func (p *Provisioner) refreshWith(rep interconnect.Report) bool {
	peers := interconnect.RecentPeers(interconnect.LoadPeers(p.host, p.dataDir), p.clock().Unix(), rep.PortMACs())
	id, ic := rep.Capability(peers)
	p.mu.Lock()
	defer p.mu.Unlock()
	before, _ := json.Marshal([]interface{}{p.capability.Identity, p.capability.Interconnect})
	p.capability.Identity, p.capability.Interconnect = id, ic
	after, _ := json.Marshal([]interface{}{id, ic})
	return string(before) != string(after)
}

// RecoverPorts puts back any ConnectX-7 port a cable check that never finished
// (the agent was killed mid-run) left up or with IPv6 off. A run that still
// holds the ports -- a pair test from the command line -- owns the record, and
// is left alone.
func (p *Provisioner) RecoverPorts() {
	if !p.hasPairRuntime() {
		return
	}
	unlock, ok, err := p.lockFrames()
	if err != nil || !ok {
		return
	}
	defer unlock()
	for _, problem := range interconnect.RecoverPorts(p.host, p.dataDir) {
		log.Printf("restoring a ConnectX-7 port after an unfinished cable check: %s", problem)
	}
}

// LinkVerify runs the authenticated cable check on this machine's eligible
// ConnectX-7 ports (POST /link/verify).
func (p *Provisioner) LinkVerify(req control.LinkVerifyRequest) (*control.LinkVerifyResponse, error) {
	seconds, err := control.ValidateLinkVerify(req)
	if err != nil {
		return nil, err
	}
	if !p.hasPairRuntime() {
		return nil, control.Unavailable("this agent cannot check a cable on this machine")
	}
	if self := p.selfListing(); self != "" && self != req.Self {
		return nil, control.Invalid("this machine is listing %s, not %s", self, req.Self)
	}
	done, err := p.beginFrames()
	if err != nil {
		return nil, err
	}
	defer done()

	rep := p.freshPair()
	resp, peers, err := interconnect.VerifyPorts(p.host, p.openPacket, rep.FramePorts(), interconnect.VerifyInput{
		Challenge: req.Challenge, Self: req.Self, Peer: req.Peer, Seconds: seconds,
		OwnMACs: rep.OwnMACs(), Now: p.clock, Journal: interconnect.JournalPath(p.dataDir),
	})
	if err != nil {
		return nil, fmt.Errorf("the cable check did not run: %w", err)
	}
	// A check that proved the peer is the best sighting there is.
	if len(peers) > 0 {
		if _, err := interconnect.SavePeers(p.host, p.dataDir, peers, p.clock().Unix()); err != nil {
			log.Printf("record the peer the cable check proved: %v", err)
		}
	}
	p.refreshWith(rep)
	return resp, nil
}

// DiscoverPeers announces this machine on its eligible ConnectX-7 ports for
// HelloWindow and records the agents it hears. It is skipped -- ran=false --
// when the machine is not registered, has no eligible port, or is busy (a
// rental, a leftover, a test boot, another check). changed says whether the
// capability the control plane sees is now different.
func (p *Provisioner) DiscoverPeers() (ran, changed bool, err error) {
	self := p.selfListing()
	if !p.hasPairRuntime() || !control.ValidListingID(self) {
		return false, false, nil
	}
	p.mu.Lock()
	busy := p.status != StatusFree || p.checking || p.settingUp || p.withdrawn
	machine := p.machine
	p.mu.Unlock()
	if busy || machine.Present() {
		return false, false, nil
	}
	rep := p.freshPair()
	ports := rep.FramePorts()
	if len(ports) == 0 {
		return false, p.refreshWith(rep), nil
	}
	done, berr := p.beginFrames()
	if berr != nil {
		return false, false, nil
	}
	defer done()
	heard, err := interconnect.HelloPorts(p.host, p.openPacket, ports, self, HelloWindow, rep.OwnMACs(), p.clock(),
		interconnect.JournalPath(p.dataDir))
	if err != nil {
		return true, false, err
	}
	if len(heard) > 0 {
		if _, err := interconnect.SavePeers(p.host, p.dataDir, heard, p.clock().Unix()); err != nil {
			return true, p.refreshWith(rep), err
		}
	}
	return true, p.refreshWith(rep), nil
}

// PeerSummary says what the last announcement round found, for the log.
func PeerSummary(c control.Capability) string {
	if c.Interconnect == nil || len(c.Interconnect.Peers) == 0 {
		return "no other agent heard on the ConnectX-7 ports"
	}
	var names []string
	for _, peer := range c.Interconnect.Peers {
		names = append(names, peer.ListingID)
	}
	return "heard on the ConnectX-7 ports: listing " + strings.Join(names, ", ")
}
