package interconnect

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// The pair test boot (`gpu-agent check --boot --pair`) runs on both machines
// within ten minutes of each other. Each first finds the other on the cable
// with hello frames -- the port routine every raw-frame run uses, held up for
// as long as it waits, since a cable carries frames only while both ends are
// up -- and from what both heard, both work out the same plan: which machine
// is node a (the one with the lower MAC), which links there are, their
// addresses, and one test name.

// PeerSettle is how long a pair test keeps listening once it has heard the
// other machine, so every link has come up and been heard.
var PeerSettle = 10 * time.Second

// PeerLink is one cable the pair test found.
type PeerLink struct {
	Netdev   string
	LocalMAC string
	PeerMAC  string
}

// FoundPeer is the other machine a pair test found on the cable.
type FoundPeer struct {
	Listing string
	Links   []PeerLink
}

// Peers are the sightings to record for the capability's peer list.
func (f *FoundPeer) Peers(now time.Time) []control.Peer {
	var out []control.Peer
	for _, l := range f.Links {
		out = append(out, control.Peer{ListingID: f.Listing, LocalMAC: l.LocalMAC, PeerMAC: l.PeerMAC, SeenAt: now.Unix()})
	}
	return out
}

// FindPeer holds the ports up, announcing self, until another agent has been
// heard and PeerSettle has passed, or wait runs out. It fails when nobody
// answers, when more than one other machine does, or when the cabling is not
// port to port.
func FindPeer(h vmrt.Host, open Opener, ports []FramePort, self string, wait time.Duration, ownMACs map[string]bool, journal string) (*FoundPeer, error) {
	if len(ports) == 0 {
		return nil, fmt.Errorf("no ConnectX-7 port on this machine can be used for a pair (see 'gpu-agent check --pair')")
	}
	s, err := Open(h, open, ports, journal)
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	var first time.Time
	heard := map[string]map[string]string{} // local MAC -> peer MAC -> listing
	s.Exchange(wait,
		func(p FramePort, seq int) []byte {
			f, _ := BuildFrame(p.MAC, HelloPayload(self, p.MAC))
			return f
		},
		func(p FramePort, f Frame) {
			if ownMACs[f.Src] || f.EtherType != EtherTypeAgent {
				return
			}
			listing, mac, ok := ParseHello(f.Payload)
			if !ok || mac != f.Src || listing == self {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if heard[p.MAC] == nil {
				heard[p.MAC] = map[string]string{}
			}
			heard[p.MAC][f.Src] = listing
			if first.IsZero() {
				first = time.Now()
			}
		},
		func() bool {
			mu.Lock()
			defer mu.Unlock()
			return !first.IsZero() && time.Since(first) >= PeerSettle
		})
	if problems := s.Close(); len(problems) > 0 {
		return nil, fmt.Errorf("the ports were not all put back: %s", strings.Join(problems, "; "))
	}

	listings := map[string]bool{}
	for _, peers := range heard {
		for _, l := range peers {
			listings[l] = true
		}
	}
	switch len(listings) {
	case 0:
		return nil, fmt.Errorf("no other agent answered on the ConnectX-7 cable within %v: run the same command on the other machine, and check the cable", wait.Round(time.Second))
	case 1:
	default:
		var names []string
		for l := range listings {
			names = append(names, l)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("the cable reaches more than one other machine (%s): a pair is two machines cabled port to port", strings.Join(names, ", "))
	}
	found := &FoundPeer{}
	for l := range listings {
		found.Listing = l
	}
	netdev := map[string]string{}
	for _, p := range ports {
		netdev[p.MAC] = p.Netdev
	}
	seenFar := map[string]string{}
	for local, peers := range heard {
		if len(peers) > 1 {
			return nil, fmt.Errorf("%s hears more than one port of the other machine: the cabling runs through a switch", netdev[local])
		}
		for far := range peers {
			if other, dup := seenFar[far]; dup {
				return nil, fmt.Errorf("%s and %s both hear the other machine's port %s: the cabling runs through a switch", netdev[other], netdev[local], far)
			}
			seenFar[far] = local
			found.Links = append(found.Links, PeerLink{Netdev: netdev[local], LocalMAC: local, PeerMAC: far})
		}
	}
	sort.Slice(found.Links, func(i, j int) bool { return found.Links[i].LocalMAC < found.Links[j].LocalMAC })
	return found, nil
}

// MaxPairLinks is how many links a pair rental configures (CONTRACT.md s3.2).
const MaxPairLinks = 2

// PairPlan is what both machines of a pair test agree on from what they heard.
type PairPlan struct {
	// ID is the test's rental id: the same on both machines.
	ID           string
	Node         string
	PeerListing  string
	PeerHostname string
	Links        []vmrt.GuestLink
	LocalMACs    []string // per link
	PeerMACs     []string // per link
}

// PlanPairTest turns what one machine heard into the plan both machines
// arrive at: node a is the machine with the lower MAC on the cable, links are
// numbered by node a's MAC (as the control plane numbers a rental's), link i
// is 10.200.i.0/30 with a on .1 and b on .2, and at most MaxPairLinks are
// used.
func PlanPairTest(f *FoundPeer) PairPlan {
	links := append([]PeerLink(nil), f.Links...)
	minLocal, minPeer := "", ""
	for _, l := range links {
		if minLocal == "" || l.LocalMAC < minLocal {
			minLocal = l.LocalMAC
		}
		if minPeer == "" || l.PeerMAC < minPeer {
			minPeer = l.PeerMAC
		}
	}
	node := "b"
	if minLocal < minPeer {
		node = "a"
	}
	aMAC := func(l PeerLink) string {
		if node == "a" {
			return l.LocalMAC
		}
		return l.PeerMAC
	}
	sort.Slice(links, func(i, j int) bool { return aMAC(links[i]) < aMAC(links[j]) })
	if len(links) > MaxPairLinks {
		links = links[:MaxPairLinks]
	}
	sum := sha256.New()
	for _, l := range links {
		b := l.PeerMAC
		if node == "b" {
			b = l.LocalMAC
		}
		fmt.Fprintf(sum, "%s|%s;", aMAC(l), b)
	}
	id := hex.EncodeToString(sum.Sum(nil))[:8] + vmrt.PairTestSuffix
	plan := PairPlan{ID: id, Node: node, PeerListing: f.Listing}
	other := "b"
	if node == "b" {
		other = "a"
	}
	plan.PeerHostname = vmrt.PairHostname(id, other)
	for i, l := range links {
		own, peer := fmt.Sprintf("10.200.%d.1", i), fmt.Sprintf("10.200.%d.2", i)
		if node == "b" {
			own, peer = peer, own
		}
		plan.Links = append(plan.Links, vmrt.GuestLink{LocalMAC: l.LocalMAC, CIDR: own + "/30", PeerIP: peer})
		plan.LocalMACs = append(plan.LocalMACs, l.LocalMAC)
		plan.PeerMACs = append(plan.PeerMACs, l.PeerMAC)
	}
	return plan
}
