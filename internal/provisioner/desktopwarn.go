package provisioner

import (
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// A DGX Spark's desktop closes when a rental starts (the owner's rule,
// v0.2.3). When a rental is due -- the host's own programs are gone -- and
// someone is logged in to the desktop, they are warned on the screen (a
// desktop notification and a wall message), and the desktop closes
// DesktopWarning later: the rental waits that long (status waiting_for_host,
// pending.desktop_closes_at). With nobody logged in (the login screen only)
// the rental starts at once. A rental without a start_by cannot wait, and
// starts at once as before.

// DesktopWarning is how long a logged-in desktop is given before a rental
// closes it; a variable for tests.
var DesktopWarning = 15 * time.Minute

// Desktop warning texts.
const (
	DesktopWarningTitle = "GPU marketplace: the desktop closes in 15 minutes"
	DesktopWarningText  = "This machine's rental starts in 15 minutes: the desktop will close then; save your work"
)

// loggedInDesktop names the people logged in to the desktop of a machine
// whose desktop closes for rentals (a DGX Spark); nil anywhere else.
func (p *Provisioner) loggedInDesktop() []string {
	if p.desktopLogins != nil {
		return p.desktopLogins()
	}
	rt := p.Runtime()
	if p.host == nil || rt == nil || !rt.Spec().DesktopOnDemand {
		return nil
	}
	var users []string
	for _, l := range vmrt.GraphicalLogins(p.host) {
		users = append(users, l.User)
	}
	return users
}

// desktopHold says whether rec must wait for its desktop warning to run out:
// someone is logged in to the desktop, and they were warned less than
// DesktopWarning ago (the warning goes out on the first look).
func (p *Provisioner) desktopHold(rec *pendingRecord) bool {
	if len(p.loggedInDesktop()) == 0 {
		return false
	}
	now := p.clock()
	p.mu.Lock()
	if p.pending != rec {
		p.mu.Unlock()
		return true
	}
	if len(rec.Holders) > 0 || rec.MemoryShortGB > 0 || rec.State != control.PendingWaiting {
		rec.Holders, rec.MemoryShortGB, rec.State, rec.PeerWait = []string{}, 0, control.PendingWaiting, false
		_ = p.savePendingLocked(rec)
	}
	p.status = StatusWaiting
	if rec.DesktopWarnedAt == 0 {
		rec.DesktopWarnedAt = now.Unix()
		_ = p.savePendingLocked(rec)
		p.mu.Unlock()
		p.tellHost(DesktopWarningTitle, DesktopWarningText)
		return true
	}
	closes := time.Unix(rec.DesktopWarnedAt, 0).Add(DesktopWarning)
	p.mu.Unlock()
	return now.Before(closes)
}
