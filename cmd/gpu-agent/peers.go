package main

import (
	"context"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/provisioner"
)

// PeerInterval is how often the agent announces itself on its ConnectX-7 ports
// (CONTRACT.md s3.1). A cable only carries frames while BOTH ends are up, so
// two machines hear each other only when their announcements overlap: every
// agent announces on the same wall-clock slots (00:00, 06:00, 12:00 and 18:00
// UTC), which clock-synchronised machines hit within a second of each other.
// The announcement at agent start is on no slot and is heard only if the other
// machine happens to be announcing too.
const PeerInterval = 6 * time.Hour

// nextPeerSlot is the next announcement slot after now.
func nextPeerSlot(now time.Time) time.Time {
	return now.UTC().Truncate(PeerInterval).Add(PeerInterval)
}

// announcePeers runs the peer announcements for as long as the agent runs, and
// re-reports the capability whenever what it says about the pair changed.
func (a *gpuAgent) announcePeers(ctx context.Context) {
	for {
		a.announceOnce()
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(nextPeerSlot(time.Now()))):
		}
	}
}

func (a *gpuAgent) announceOnce() {
	ran, changed, err := a.prov.DiscoverPeers()
	if err != nil {
		a.warn("peer announcement on the ConnectX-7 ports: %v", err)
	}
	if ran {
		a.say("Peer announcement: %s", provisioner.PeerSummary(a.prov.Capability()))
	}
	if !changed {
		return
	}
	if _, err := a.report(a.prov.Capability()); err != nil {
		a.warn("report the pair capability: %v", err)
	}
}
