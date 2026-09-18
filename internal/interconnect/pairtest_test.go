package interconnect_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/interconnect"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

func settleFast(t *testing.T) {
	t.Helper()
	fast(t)
	orig := interconnect.PeerSettle
	interconnect.PeerSettle = 30 * time.Millisecond
	t.Cleanup(func() { interconnect.PeerSettle = orig })
}

type findResult struct {
	found *interconnect.FoundPeer
	err   error
}

func findBoth(a, b side, w interface {
	Opener(string) interconnect.Opener
}, wait time.Duration) (findResult, findResult) {
	var ra, rb findResult
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ra.found, ra.err = interconnect.FindPeer(a.h, w.Opener("A"), a.ports, listingA, wait, own(a), "")
	}()
	go func() {
		defer wg.Done()
		// The other machine's command is typed a little later.
		time.Sleep(20 * time.Millisecond)
		rb.found, rb.err = interconnect.FindPeer(b.h, w.Opener("B"), b.ports, listingB, wait, own(b), "")
	}()
	wg.Wait()
	return ra, rb
}

// Both machines hear each other and arrive at the same plan: the same test
// name, one node each, the same numbering of links and their addresses.
func TestPairTestMachinesAgreeOnOnePlan(t *testing.T) {
	settleFast(t)
	a, b, w := cabled()
	ra, rb := findBoth(a, b, w, 2*time.Second)
	if ra.err != nil || rb.err != nil {
		t.Fatal(ra.err, rb.err)
	}
	if ra.found.Listing != listingB || rb.found.Listing != listingA || len(ra.found.Links) != 2 || len(rb.found.Links) != 2 {
		t.Fatalf("found %+v / %+v", ra.found, rb.found)
	}
	pa, pb := interconnect.PlanPairTest(ra.found), interconnect.PlanPairTest(rb.found)
	if pa.ID != pb.ID || !strings.HasSuffix(pa.ID, "-pairtest") || pa.Node != "a" || pb.Node != "b" {
		t.Fatalf("plans: %+v / %+v", pa, pb)
	}
	if pa.PeerHostname != vmrt.PairHostname(pa.ID, "b") || pb.PeerHostname != vmrt.PairHostname(pa.ID, "a") {
		t.Errorf("names: %s / %s", pa.PeerHostname, pb.PeerHostname)
	}
	for i := range pa.Links {
		la, lb := pa.Links[i], pb.Links[i]
		if la.LocalMAC != pb.PeerMACs[i] || lb.LocalMAC != pa.PeerMACs[i] || la.PeerIP+"/30" != lb.CIDR || lb.PeerIP+"/30" != la.CIDR {
			t.Errorf("link %d: a %+v b %+v", i, la, lb)
		}
	}
	if pa.Links[0].CIDR != "10.200.0.1/30" || pb.Links[1].CIDR != "10.200.1.2/30" {
		t.Errorf("addresses: %+v / %+v", pa.Links, pb.Links)
	}
	// Both plans are valid pair rentals.
	for _, plan := range []interconnect.PairPlan{pa, pb} {
		opts := &vmrt.PairOptions{Node: plan.Node, PeerHostname: plan.PeerHostname, Links: plan.Links, MTU: 9000, Functions: []string{"0000:01:00.0"}}
		if err := vmrt.ValidatePair(plan.ID, opts); err != nil {
			t.Errorf("plan %s does not validate: %v", plan.Node, err)
		}
	}
}

func TestPlanNumbersLinksByNodeAsMAC(t *testing.T) {
	// B has the lower MACs, and its cabling crosses: B's port 1 goes to A's port 0.
	found := &interconnect.FoundPeer{Listing: "B", Links: []interconnect.PeerLink{
		{Netdev: "p0", LocalMAC: "58:a2:e1:00:05:01", PeerMAC: "58:a2:e1:00:00:02"},
		{Netdev: "p1", LocalMAC: "58:a2:e1:00:05:02", PeerMAC: "58:a2:e1:00:00:01"},
		{Netdev: "p2", LocalMAC: "58:a2:e1:00:05:03", PeerMAC: "58:a2:e1:00:00:03"},
	}}
	plan := interconnect.PlanPairTest(found)
	if plan.Node != "b" || len(plan.Links) != interconnect.MaxPairLinks {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.PeerMACs[0] != "58:a2:e1:00:00:01" || plan.Links[0].LocalMAC != "58:a2:e1:00:05:02" || plan.Links[0].CIDR != "10.200.0.2/30" || plan.Links[0].PeerIP != "10.200.0.1" {
		t.Errorf("links = %+v peers %v", plan.Links, plan.PeerMACs)
	}
}

func TestPairTestWithNobodyOnTheCable(t *testing.T) {
	settleFast(t)
	a, _, w := cabled()
	_, err := interconnect.FindPeer(a.h, w.Opener("A"), a.ports, listingA, 80*time.Millisecond, own(a), "")
	if err == nil || !strings.Contains(err.Error(), "run the same command on the other machine") {
		t.Fatalf("err = %v", err)
	}
	// IPv6 went off for the wait and came back on after it.
	if !a.h.Ran("write /proc/sys/net/ipv6/conf/enp1s0f0np0/disable_ipv6=1") ||
		string(a.h.Files["/proc/sys/net/ipv6/conf/enp1s0f0np0/disable_ipv6"]) != "0" {
		t.Errorf("port not put back: %v", a.h.Calls)
	}
	if len(w.Opened()) != 0 {
		t.Errorf("sockets left open: %v", w.Opened())
	}
}

func TestPairTestRefusesASwitch(t *testing.T) {
	settleFast(t)
	// Long enough that every machine on the switch is heard even on a busy
	// test runner.
	interconnect.PeerSettle = 300 * time.Millisecond
	a, b, w := cabled()
	third := sparkSide("58:a2:e1:00:07:0")
	// All of A's port 0, B's port 0 and a third machine on one switch.
	w.Join("A/enp1s0f0np0", "B/enp1s0f0np0", "C/enp1s0f0np0")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = interconnect.FindPeer(third.h, w.Opener("C"), third.ports[:1], "listing-c", 2*time.Second, own(third), "")
	}()
	time.Sleep(50 * time.Millisecond) // the third machine is already on the switch
	ra, _ := findBoth(a, b, w, 2*time.Second)
	wg.Wait()
	if ra.err == nil || !strings.Contains(ra.err.Error(), "more than one other machine") {
		t.Fatalf("err = %v", ra.err)
	}
}
