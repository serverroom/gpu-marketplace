package interconnect

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// A pair rental whose machines are rented while their host still uses one of
// them (v0.2.3) must start both halves together: a half that boots alone runs
// its cable check against a peer that is not there, and fails the pair. So a
// half whose machine is free does not start on its own; it meets the other
// half on the cable. On every rendezvous slot (the start of each minute, UTC:
// both machines' clocks agree within seconds, as the peer announcements
// already need) each half that is ready -- free, its test boot done -- holds
// its ports up for RendezvousWindow and broadcasts start frames for the
// rental; a half still held by its host stays silent. A half starts when it
// has heard the other say that it heard this one, early enough in the window
// (RendezvousCutoff before its end) that the other has heard the same: both
// then start within the same minute.

// StartPrefix starts a pair rental's start frame:
// GPUAGENT-START1|<first 8 of the rental id>|<mac>|<heard 0|1>|<seq>|<hex hmac_sha256(key=rental id, msg=rental8|mac|heard|seq)>
// The full rental id -- which only the two machines of the rental were given --
// keys the frame, so nothing else on the cable can start a half.
const StartPrefix = "GPUAGENT-START1"

// Rendezvous timings, overridable by tests.
var (
	// RendezvousSlot: halves meet at the start of every slot, by the clock.
	RendezvousSlot = time.Minute
	// RendezvousWindow is how long a ready half holds its ports up per slot.
	RendezvousWindow = 20 * time.Second
	// RendezvousCutoff: the other half's word that it heard this one counts
	// only when it arrives this long before the window ends.
	RendezvousCutoff = 5 * time.Second
)

// UntilRendezvous is how long from now to the next slot.
func UntilRendezvous(now time.Time) time.Duration {
	return now.Truncate(RendezvousSlot).Add(RendezvousSlot).Sub(now)
}

func rental8(rentalID string) string {
	id := strings.ToLower(rentalID)
	if len(id) > 8 {
		id = id[:8]
	}
	return id
}

func startMAC(rentalID, mac string, heard bool, seq int) string {
	m := hmac.New(sha256.New, []byte(rentalID))
	fmt.Fprintf(m, "%s|%s|%s|%d", rental8(rentalID), mac, flag(heard), seq)
	return hex.EncodeToString(m.Sum(nil))
}

func flag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// StartPayload is one start frame's payload.
func StartPayload(rentalID, mac string, heard bool, seq int) string {
	return fmt.Sprintf("%s|%s|%s|%s|%d|%s", StartPrefix, rental8(rentalID), mac, flag(heard), seq, startMAC(rentalID, mac, heard, seq))
}

// ParseStart reads a start frame for rentalID: the sender's port, and whether
// it has heard this half. ok is false for anything else, a frame for another
// rental or one not made with its id included.
func ParseStart(payload []byte, rentalID string) (mac string, heard bool, ok bool) {
	parts := strings.Split(agentText(payload), "|")
	if len(parts) != 6 || parts[0] != StartPrefix || parts[1] != rental8(rentalID) || !ValidMAC(parts[2]) {
		return "", false, false
	}
	if parts[3] != "0" && parts[3] != "1" {
		return "", false, false
	}
	seq, err := strconv.Atoi(parts[4])
	if err != nil || seq < 0 || strconv.Itoa(seq) != parts[4] {
		return "", false, false
	}
	heard = parts[3] == "1"
	if !hmac.Equal([]byte(startMAC(rentalID, parts[2], heard, seq)), []byte(parts[5])) {
		return "", false, false
	}
	return parts[2], heard, true
}

// Rendezvous holds ports up for window as a ready half of rentalID, and says
// whether both halves are ready: agreed when the other half said, before
// window-cutoff, that it heard this one. heard says whether the other half was
// heard at all. A ctx that ends stops it early, agreeing to nothing.
func Rendezvous(ctx context.Context, h vmrt.Host, open Opener, ports []FramePort, rentalID string, window, cutoff time.Duration, ownMACs map[string]bool, journal string) (agreed, heard bool, err error) {
	if len(ports) == 0 {
		return false, false, fmt.Errorf("no ConnectX-7 port of this pair rental can be used")
	}
	s, err := Open(h, open, ports, journal)
	if err != nil {
		return false, false, err
	}
	var mu sync.Mutex
	start := time.Now()
	s.Exchange(window,
		func(p FramePort, seq int) []byte {
			mu.Lock()
			said := heard
			mu.Unlock()
			f, _ := BuildFrame(p.MAC, StartPayload(rentalID, p.MAC, said, seq))
			return f
		},
		func(p FramePort, f Frame) {
			if ownMACs[f.Src] || f.EtherType != EtherTypeAgent {
				return
			}
			mac, heardUs, ok := ParseStart(f.Payload, rentalID)
			if !ok || mac != f.Src {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			heard = true
			if heardUs && time.Since(start) < window-cutoff {
				agreed = true
			}
		},
		func() bool { return ctx.Err() != nil })
	if problems := s.Close(); len(problems) > 0 {
		return false, heard, fmt.Errorf("the ports were not all put back: %s", strings.Join(problems, "; "))
	}
	mu.Lock()
	defer mu.Unlock()
	if ctx.Err() != nil {
		return false, heard, ctx.Err()
	}
	return agreed, heard, nil
}
