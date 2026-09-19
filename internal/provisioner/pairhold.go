package provisioner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/interconnect"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// A pair rental that may wait for its host (a /pair/provision with start_by)
// starts both halves together, or neither: a half that booted alone would run
// its cable check against a peer that is not there, and fail the pair. Each
// half is held by a goroutine of its own (holdPair) until both machines are
// free: while its host uses it, it waits as any rental does (and the host is
// told); once free, it runs its full test boot if the running agent owes one,
// then meets the other half on the cable at each rendezvous slot
// (interconnect.Rendezvous). When both halves have heard each other ready they
// start in the same minute. Until then a free half waits for its peer
// (waiting_for_peer) -- after its first rendezvous; before it, it answers as
// starting, since two free halves meet within the minute.

// pairHold: a pair half that starts only together with its peer.
func (r *pendingRecord) pairHold() bool { return r.Pair != nil && r.StartBy > 0 }

// launchHoldLocked starts rec's hold, with p.mu held. It always runs in the
// background: the two halves meet on the cable at the same moment.
func (p *Provisioner) launchHoldLocked(rec *pendingRecord) {
	base := p.baseCtx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	rec.cancel, rec.running = cancel, true
	p.starts.Add(1)
	go p.holdPair(ctx, cancel, rec)
}

// pauseFor waits d, or until ctx ends; false when ctx ended.
func pauseFor(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// holdPair holds one pair half until both halves start, or it is given up,
// cancelled or fails.
func (p *Provisioner) holdPair(ctx context.Context, cancel context.CancelFunc, rec *pendingRecord) {
	defer p.starts.Done()
	defer func() {
		p.mu.Lock()
		rec.running = false
		p.mu.Unlock()
		cancel()
	}()
	for {
		p.mu.Lock()
		cancelled, mine := rec.cancelled, p.pending == rec && rec.State != control.PendingFailed
		p.mu.Unlock()
		switch {
		case cancelled:
			p.finishCancelled(rec)
			return
		case !mine || ctx.Err() != nil:
			// Torn down, or the agent is stopping (the record stays on disk,
			// and the next start takes it up).
			return
		}
		if p.clock().Unix() >= rec.StartBy {
			p.giveUp(rec)
			return
		}
		use := p.hostUseNow()
		if use.Busy() {
			p.holdForHost(rec, use)
			pauseFor(ctx, HostUseInterval)
			continue
		}
		if !p.canStartHeld(rec) {
			pauseFor(ctx, HostUseInterval)
			continue
		}
		if p.desktopHold(rec) {
			// Someone is logged in to the desktop: warned, it closes later.
			pauseFor(ctx, HostUseInterval)
			continue
		}
		if !p.fullTestIsCurrent() {
			if !p.heldTest(ctx, rec) {
				return
			}
			continue
		}
		// Ready: meet the other half at the next slot, by the clock both
		// machines keep.
		if !pauseFor(ctx, interconnect.UntilRendezvous(time.Now())) {
			continue
		}
		slot := time.Now().Truncate(interconnect.RendezvousSlot)
		if p.hostUseNow().Busy() {
			continue
		}
		agreed, heard, unlock, err := p.meet(ctx, rec, slot)
		if agreed {
			if p.bootHeld(rec, unlock) {
				return
			}
			continue // the host took the GPU back in the last moment
		}
		p.holdForPeer(rec, heard, err)
	}
}

// canStartHeld: nothing but the host (or the peer) keeps this half from
// starting. A machine that cannot host any more fails the rental; one that is
// setting itself up is waited for.
func (p *Provisioner) canStartHeld(rec *pendingRecord) bool {
	p.mu.Lock()
	ready, settingUp, reasons := p.capability.Ready, p.settingUp, strings.Join(p.capability.Reasons, "; ")
	p.mu.Unlock()
	switch {
	case settingUp:
		return false
	case !ready:
		p.failPending(rec, control.PendingReasonStart, "this machine cannot host a rental now: "+reasons)
		return false
	}
	return true
}

// holdForHost: the host uses this half's machine. It waits as any rental does,
// and the host is told (and reminded) what to stop by when.
func (p *Provisioner) holdForHost(rec *pendingRecord, use vmrt.HostUse) {
	p.mu.Lock()
	if p.pending != rec {
		p.mu.Unlock()
		return
	}
	holders := append([]string{}, use.Holders...)
	if rec.State != control.PendingWaiting || rec.PeerWait || strings.Join(holders, "\n") != strings.Join(rec.Holders, "\n") ||
		rec.MemoryShortGB != use.MemoryShortGB() {
		rec.State, rec.PeerWait = control.PendingWaiting, false
		rec.Holders, rec.MemoryShortGB = holders, use.MemoryShortGB()
		_ = p.savePendingLocked(rec)
	}
	p.status = StatusWaiting
	p.mu.Unlock()
	p.tellRented(rec) // when a reminder is due
}

// holdForPeer: this half is free and ready, and the other half did not meet it.
func (p *Provisioner) holdForPeer(rec *pendingRecord, heard bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending != rec {
		return
	}
	if rec.State != control.PendingWaiting || !rec.PeerWait || len(rec.Holders) > 0 {
		rec.State, rec.PeerWait, rec.Holders, rec.MemoryShortGB = control.PendingWaiting, true, []string{}, 0
		_ = p.savePendingLocked(rec)
	}
	p.status = StatusWaiting
	if err != nil {
		p.lastErr = "waiting for the other machine of the pair: " + err.Error()
	} else {
		p.lastErr = ""
	}
	_ = heard
}

// heldTest runs the full test boot before this half is ready to start; false
// when the hold ends with it (cancelled, failed, the agent stopping).
func (p *Provisioner) heldTest(ctx context.Context, rec *pendingRecord) bool {
	p.mu.Lock()
	if p.pending != rec {
		p.mu.Unlock()
		return false
	}
	rec.State = control.PendingStarting
	p.status = StatusProvisioning
	_ = p.savePendingLocked(rec)
	p.mu.Unlock()
	res, busy := p.runPreRentalTest(ctx)
	p.mu.Lock()
	cancelled := rec.cancelled
	p.mu.Unlock()
	switch {
	case cancelled:
		p.finishCancelled(rec)
		return false
	case ctx.Err() != nil || res.Stopped:
		return false
	case busy != "":
		p.mu.Lock()
		if p.pending == rec {
			rec.State = control.PendingWaiting
			p.status = StatusWaiting
			_ = p.savePendingLocked(rec)
		}
		p.mu.Unlock()
		return true
	case !res.Passed:
		p.failPending(rec, control.PendingReasonGPU, strings.Join(res.Problems, "; "))
		return false
	}
	p.refreshTestView()
	return true
}

// meet holds the rental's ports up for one rendezvous window. When both
// halves agreed, the frame lock is kept for the start and unlock gives it back.
func (p *Provisioner) meet(ctx context.Context, rec *pendingRecord, slot time.Time) (agreed, heard bool, unlock func(), err error) {
	if !p.hasPairRuntime() {
		return false, false, nil, errors.New("this agent cannot check the cable on this machine")
	}
	links := map[string]bool{}
	for _, l := range rec.Pair.Links {
		links[strings.ToLower(strings.TrimSpace(l.LocalMAC))] = true
	}
	rep := p.freshPair()
	var ports []interconnect.FramePort
	for _, fp := range rep.FramePorts() {
		if links[fp.MAC] {
			ports = append(ports, fp)
		}
	}
	unlock, ok, err := p.lockFrames()
	if err != nil || !ok {
		return false, false, nil, err
	}
	if err := p.recoverPortsLocked(); err != nil {
		unlock()
		return false, false, nil, err
	}
	agreed, heard, err = interconnect.Rendezvous(ctx, p.host, p.openPacket, ports, rec.RentalID, slot, interconnect.RendezvousWindow,
		interconnect.RendezvousCutoff, rep.OwnMACs(), interconnect.JournalPath(p.dataDir))
	if !agreed || err != nil {
		unlock()
		return false, heard, nil, err
	}
	return true, heard, unlock, nil
}

// bootHeld starts this half, now that both are ready, holding the frame lock
// (unlock) until its VM has the card. ended is false when the host took the
// GPU back in the last moment: the half waits on (the other half's VM, up
// already, waits for it in its cable check, and the marketplace judges the
// pair).
func (p *Provisioner) bootHeld(rec *pendingRecord, unlock func()) (ended bool) {
	defer unlock()
	p.mu.Lock()
	if p.pending != rec || rec.cancelled {
		cancelled := rec.cancelled
		p.mu.Unlock()
		if cancelled {
			p.finishCancelled(rec)
		}
		return true
	}
	rec.State, rec.PeerWait, rec.booting = control.PendingStarting, false, true
	p.status = StatusProvisioning
	p.lastErr = ""
	p.setHostUseLocked(vmrt.HostUse{}, p.clock().Unix())
	_ = p.savePendingLocked(rec)
	machine := p.machine
	p.mu.Unlock()

	o, err := p.pairOptions(*rec.Pair)
	if err != nil {
		p.failPending(rec, control.PendingReasonStart, err.Error())
		return true
	}
	waits := rec.StartBy > p.clock().Unix()
	err = p.startNoting(machine, o, !waits)
	if err != nil && waits && errors.Is(err, vmrt.ErrGPUInUse) {
		_ = p.backToWaiting(rec, err.Error())
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending != rec {
		return true
	}
	if err == nil {
		p.pending = nil
		p.removePendingLocked()
		return true
	}
	rec.State, rec.Reason, rec.Detail, rec.FailedAt = control.PendingFailed, control.PendingReasonStart, err.Error(), p.clock().Unix()
	rec.booting = false
	_ = p.savePendingLocked(rec)
	return true
}

// giveUp drops a waiting rental at its start_by: the machine was still in use
// (by its host, or -- for a pair half -- the other machine never became free).
func (p *Provisioner) giveUp(rec *pendingRecord) {
	p.mu.Lock()
	if p.pending != rec {
		p.mu.Unlock()
		return
	}
	p.pending = nil
	p.removePendingLocked()
	if p.status == StatusWaiting || p.status == StatusProvisioning {
		p.status = StatusFree
	}
	pr, notify := p.problems, p.onChange
	p.mu.Unlock()
	if rec.PeerWait {
		if pr != nil {
			pr.Note(control.AreaRental, fmt.Sprintf("pair rental %s was cancelled: the other machine of the pair was not free by its start deadline, %s",
				rec.RentalID, time.Unix(rec.StartBy, 0).UTC().Format("2006-01-02 15:04 UTC")), "")
		}
	} else {
		p.gaveUp(rec, pr)
	}
	if notify != nil {
		notify()
	}
}
