package provisioner

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/interconnect"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

const pairID = "1a2b3c4d-0000-4000-8000-000000000001"

// half is one machine of a cabled pair with its host's use, its clock and its
// VM in the test's hands.
type half struct {
	p *Provisioner
	m *fakeMachine

	mu   sync.Mutex
	use  vmrt.HostUse
	now  time.Time
	told []string
}

func (h *half) busy(holders ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.use = vmrt.HostUse{Holders: holders}
}

func (h *half) at(t time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = t
}

func (h *half) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.told...)
}

// fastHold makes a rendezvous a fraction of a second, every moment a slot.
func fastHold(t *testing.T) {
	fastFrames(t)
	window, cutoff, interval, until := interconnect.RendezvousWindow, interconnect.RendezvousCutoff, HostUseInterval, untilRendezvous
	interconnect.RendezvousWindow, interconnect.RendezvousCutoff, HostUseInterval = 300*time.Millisecond, 80*time.Millisecond, 20*time.Millisecond
	untilRendezvous = func(time.Time) time.Duration { return 0 }
	t.Cleanup(func() {
		interconnect.RendezvousWindow, interconnect.RendezvousCutoff, HostUseInterval, untilRendezvous = window, cutoff, interval, until
	})
}

// cabledHalves are Sparks A and B of sparkPair, ready to take a pair rental.
func cabledHalves(t *testing.T) (*half, *half) {
	t.Helper()
	fastHold(t)
	withFakeForward(t, nil)
	a, b, _ := sparkPair(t)
	wire := func(p *Provisioner) *half {
		h := &half{p: p, m: &fakeMachine{stopRes: clean()}, now: time.Unix(1789000000, 0)}
		p.machine = h.m
		p.readHostUse = func() vmrt.HostUse { h.mu.Lock(); defer h.mu.Unlock(); return h.use }
		p.fullTestCurrent = func() bool { return true }
		p.now = func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.now }
		p.notify = func(title, body string) ([]string, []string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.told = append(h.told, title+"\n"+body)
			return nil, nil
		}
		p.baseCtx = context.Background()
		t.Cleanup(func() { p.WaitStarts(5 * time.Second) })
		return h
	}
	return wire(a), wire(b)
}

// halfRequest is node's /pair/provision, as the marketplace sends each half.
func halfRequest(t *testing.T, node string, startBy int64) control.PairProvisionRequest {
	mac, own, peer, other := "58:a2:e1:00:00:01", "10.200.0.1/30", "10.200.0.2", "b"
	if node == "b" {
		mac, own, peer, other = "58:a2:e1:00:01:01", "10.200.0.2/30", "10.200.0.1", "a"
	}
	return control.PairProvisionRequest{RentalID: pairID, RenterPubkey: renterKey(t), Node: node,
		PeerHostname: "gpu-1a2b3c4d-" + other, MTU: 9000, StartBy: &startBy,
		Links: []control.PairLink{{LocalMAC: mac, CIDR: own, PeerIP: peer}}}
}

// eventually waits up to 5 s for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never: %s", what)
}

// The whole pair waits while one machine's host uses it: the free half does
// not boot alone (its cable check would fail the pair) but waits for its
// peer, and both start together once the busy one is free.
func TestAPairWaitsForBothHalvesAndStartsThemTogether(t *testing.T) {
	a, b := cabledHalves(t)
	by := int64(1789000000 + 3600)
	b.busy("llama-server (pid 7)")

	if pending, err := a.p.PairProvisionBy(halfRequest(t, "a", by)); err != nil || pending != nil {
		t.Fatalf("A: %+v %v", pending, err)
	}
	pending, err := b.p.PairProvisionBy(halfRequest(t, "b", by))
	if err != nil || pending == nil || pending.State != control.PendingWaiting || strings.Join(pending.Holders, ",") != "llama-server (pid 7)" {
		t.Fatalf("B: %+v %v", pending, err)
	}
	eventually(t, "A waits for its peer", func() bool {
		v := a.p.PendingRental()
		return a.p.Status() == StatusWaiting && v != nil && v.WaitingForPeer && len(v.Holders) == 0
	})
	time.Sleep(700 * time.Millisecond) // a few more rendezvous windows
	if a.p.Status() == StatusRented || b.p.Status() == StatusRented {
		t.Fatalf("a half started while the other was held: A %s, B %s", a.p.Status(), b.p.Status())
	}
	if told := a.messages(); len(told) != 0 {
		t.Errorf("A's machine, which is free, was told %q", told)
	}
	if told := b.messages(); len(told) != 1 || !strings.Contains(told[0], "(llama-server)") || !strings.Contains(told[0], "The desktop closes when the rental starts") {
		t.Errorf("B was told %q", told)
	}

	b.busy()
	eventually(t, "both halves rented", func() bool {
		return a.p.Status() == StatusRented && b.p.Status() == StatusRented
	})
	for name, h := range map[string]*half{"A": a, "B": b} {
		if h.m.started != 1 || h.m.last.Pair == nil || h.m.last.ID != pairID || h.p.PendingRental() != nil {
			t.Errorf("%s: started %d opts %+v pending %+v", name, h.m.started, h.m.last, h.p.PendingRental())
		}
	}
	if a.m.last.Pair.Node != "a" || b.m.last.Pair.Node != "b" {
		t.Errorf("nodes %s / %s", a.m.last.Pair.Node, b.m.last.Pair.Node)
	}
}

// Two free halves meet at the first slot and start at once, answering as
// starting (not waiting) in between.
func TestTwoFreeHalvesStartAtOnce(t *testing.T) {
	a, b := cabledHalves(t)
	by := int64(1789000000 + 3600)
	var wg sync.WaitGroup
	for _, h := range []*half{a, b} {
		h := h
		wg.Add(1)
		go func() {
			defer wg.Done()
			node := "a"
			if h == b {
				node = "b"
			}
			if pending, err := h.p.PairProvisionBy(halfRequest(t, node, by)); err != nil || pending != nil {
				t.Errorf("%s: %+v %v", node, pending, err)
			}
		}()
	}
	wg.Wait()
	eventually(t, "both rented", func() bool { return a.p.Status() == StatusRented && b.p.Status() == StatusRented })
	if len(a.messages())+len(b.messages()) != 0 {
		t.Errorf("free machines were told: %q %q", a.messages(), b.messages())
	}
}

// A pair whose busy half is not freed in time is given up by both halves at
// the deadline; the free half never started.
func TestAPairNotFreedInTimeIsGivenUpByBothHalves(t *testing.T) {
	a, b := cabledHalves(t)
	by := int64(1789000000 + 3600)
	b.busy("llama-server (pid 7)")
	_, _ = a.p.PairProvisionBy(halfRequest(t, "a", by))
	_, _ = b.p.PairProvisionBy(halfRequest(t, "b", by))
	eventually(t, "A waits", func() bool { return a.p.Status() == StatusWaiting })
	a.at(time.Unix(by, 0))
	b.at(time.Unix(by, 0))
	eventually(t, "both given up", func() bool { return a.p.Status() == StatusFree && b.p.Status() == StatusFree })
	if a.m.started+b.m.started != 0 || a.p.PendingRental() != nil || b.p.PendingRental() != nil {
		t.Errorf("started %d/%d, pending %+v / %+v", a.m.started, b.m.started, a.p.PendingRental(), b.p.PendingRental())
	}
	if told := b.messages(); !strings.Contains(told[len(told)-1], "was cancelled because the machine was still in use (llama-server)") {
		t.Errorf("B was told %q", told)
	}
}

// A teardown of a held half cancels it; a pair rental without a start_by
// starts as it always did (the marketplace sends start_by only to agents that
// hold pairs).
func TestAHeldHalfIsCancelledByATeardown(t *testing.T) {
	a, _ := cabledHalves(t)
	_, _ = a.p.PairProvisionBy(halfRequest(t, "a", 1789000000+3600))
	eventually(t, "A waits", func() bool { return a.p.Status() == StatusWaiting })
	if ok, err := a.p.CancelPending(pairID); !ok || err != nil {
		t.Fatalf("cancel = %v %v", ok, err)
	}
	eventually(t, "A free", func() bool { return a.p.Status() == StatusFree && a.p.PendingRental() == nil })
	time.Sleep(100 * time.Millisecond)
	if a.m.started != 0 || a.p.Status() != StatusFree {
		t.Errorf("a cancelled half: started %d status %s", a.m.started, a.p.Status())
	}

	req := halfRequest(t, "a", 0)
	req.StartBy = nil
	a.p.async = false
	if pending, err := a.p.PairProvisionBy(req); err != nil || pending != nil || a.p.Status() != StatusRented {
		t.Errorf("without start_by: %+v %v %s", pending, err, a.p.Status())
	}
	if _, err := a.p.PairProvisionBy(halfRequest(t, "a", 1789000000+3600)); code(err) != http.StatusConflict {
		t.Errorf("a pair rental on a rented machine: %v", err)
	}
}
