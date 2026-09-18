package interconnect

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// Bounds on one port's answer.
const (
	maxPeerFrames = 8
	maxLLDP       = 8
	maxForeign    = 16
)

// VerifyInput is one cable check.
type VerifyInput struct {
	Challenge  string
	Self, Peer string
	Seconds    int
	// OwnMACs are every ConnectX MAC on this machine. A frame from one of them
	// is this machine's own (a NIC can echo one function's frames to another);
	// it is neither the peer nor a stranger.
	OwnMACs map[string]bool
	Now     func() time.Time
	// Journal, when set, is where the ports' original state is kept while
	// the check runs (JournalPath).
	Journal string
}

// heard is what one port received, before it is put into the answer.
type heard struct {
	peers   map[[2]string]int // (src, listing) -> valid frames
	lldp    []control.LLDPEntry
	stp     bool
	foreign map[string]bool
	other   map[string]bool // sources of non-agent traffic, judged at the end
	authed  map[string]bool // sources proven to be the peer
	bad     int
}

func newHeard() *heard {
	return &heard{peers: map[[2]string]int{}, foreign: map[string]bool{}, other: map[string]bool{}, authed: map[string]bool{}}
}

// classify files one frame (DESIGN.md s4.3 step 4).
func (hd *heard) classify(f Frame, in VerifyInput) {
	switch {
	case in.OwnMACs[f.Src]:
		return
	case f.EtherType == EtherTypeLLDP:
		e := control.LLDPEntry{Src: f.Src, Chassis: LLDPChassis(f.Payload)}
		for _, x := range hd.lldp {
			if x == e {
				return
			}
		}
		if len(hd.lldp) < maxLLDP {
			hd.lldp = append(hd.lldp, e)
		}
	case f.Dst == stpMAC:
		hd.stp = true
	case f.EtherType == EtherTypeAgent:
		text := agentText(f.Payload)
		switch {
		case strings.HasPrefix(text, LinkPrefix+"|"):
			lf, ok := ParseLink(f.Payload)
			if !ok || !lf.Authentic(in.Challenge) || lf.MAC != f.Src {
				hd.bad++
				return
			}
			// An authentic frame names its sender. Anyone but the peer is
			// reported as what it is, and the control plane refuses the pair.
			hd.peers[[2]string{f.Src, lf.Self}]++
			if lf.Self == in.Peer {
				hd.authed[f.Src] = true
			}
		case strings.HasPrefix(text, HelloPrefix+"|"):
			listing, mac, ok := ParseHello(f.Payload)
			switch {
			case !ok || mac != f.Src:
				hd.bad++
			case listing != in.Peer:
				// Another agent's announcement: a third machine on the wire.
				hd.foreign[f.Src] = true
			}
			// The peer announcing itself (its peer discovery can overlap a
			// check) is neither proof nor a problem.
		default:
			hd.bad++
		}
	default:
		hd.other[f.Src] = true
	}
}

// port is the answer for one port.
func (hd *heard) port(p FramePort, lr linkResult) control.LinkPort {
	out := control.LinkPort{Netdev: p.Netdev, BDF: p.BDF, MAC: p.MAC, Carrier: lr.Carrier, SpeedMbps: lr.SpeedMbps,
		PeerFrames: []control.PeerFrames{}, LLDP: hd.lldp, STP: hd.stp, ForeignSrc: []string{}, BadFrames: hd.bad}
	if out.LLDP == nil {
		out.LLDP = []control.LLDPEntry{}
	}
	for k, n := range hd.peers {
		out.PeerFrames = append(out.PeerFrames, control.PeerFrames{Src: k[0], Listing: k[1], Count: n})
	}
	sort.Slice(out.PeerFrames, func(i, j int) bool {
		a, b := out.PeerFrames[i], out.PeerFrames[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Src+a.Listing < b.Src+b.Listing
	})
	if len(out.PeerFrames) > maxPeerFrames {
		out.PeerFrames = out.PeerFrames[:maxPeerFrames]
	}
	// Ordinary traffic from the proven peer (a last neighbour-discovery packet
	// as its IPv6 went off, say) is the peer, not a stranger. Anything else is.
	for src := range hd.other {
		if !hd.authed[src] {
			hd.foreign[src] = true
		}
	}
	for src := range hd.foreign {
		out.ForeignSrc = append(out.ForeignSrc, src)
	}
	sort.Strings(out.ForeignSrc)
	if len(out.ForeignSrc) > maxForeign {
		out.ForeignSrc = out.ForeignSrc[:maxForeign]
	}
	return out
}

// VerifyPorts runs the cable check on ports (CONTRACT.md s3.2): every port up
// with no address and IPv6 off, an authenticated frame broadcast every
// FrameInterval for in.Seconds, everything heard classified, every port put
// back. It also returns the peers the frames proved, for the peer list.
func VerifyPorts(h vmrt.Host, open Opener, ports []FramePort, in VerifyInput) (*control.LinkVerifyResponse, []control.Peer, error) {
	resp := &control.LinkVerifyResponse{Ports: []control.LinkPort{}}
	if len(ports) == 0 {
		return resp, nil, nil
	}
	now := time.Now
	if in.Now != nil {
		now = in.Now
	}
	for _, p := range ports {
		if !ValidMAC(p.MAC) {
			return nil, nil, fmt.Errorf("port %s has no usable MAC (%q)", p.Netdev, p.MAC)
		}
	}
	s, err := Open(h, open, ports, in.Journal)
	if err != nil {
		return nil, nil, err
	}
	hd := map[string]*heard{}
	for _, p := range ports {
		hd[p.Netdev] = newHeard()
	}
	results := s.Exchange(time.Duration(in.Seconds)*SecondUnit,
		func(p FramePort, seq int) []byte {
			f, _ := BuildFrame(p.MAC, LinkPayload(in.Challenge, in.Self, p.MAC, seq))
			return f
		},
		func(p FramePort, f Frame) { hd[p.Netdev].classify(f, in) },
		nil)
	if problems := s.Close(); len(problems) > 0 {
		return nil, nil, fmt.Errorf("the ports were not all put back: %s", strings.Join(problems, "; "))
	}
	var peers []control.Peer
	at := now().Unix()
	for i, p := range ports {
		port := hd[p.Netdev].port(p, results[i])
		resp.Ports = append(resp.Ports, port)
		for _, f := range port.PeerFrames {
			if f.Listing == in.Peer && f.Count > 0 {
				peers = append(peers, control.Peer{ListingID: in.Peer, LocalMAC: p.MAC, PeerMAC: f.Src, SeenAt: at})
			}
		}
	}
	return resp, peers, nil
}

// HelloPorts announces this machine on ports for window and lists the other
// agents it heard (CONTRACT.md s3.1 peer discovery). Advisory: anyone can send
// a hello; the authenticated cable check is the proof.
func HelloPorts(h vmrt.Host, open Opener, ports []FramePort, self string, window time.Duration, ownMACs map[string]bool, now time.Time, journal string) ([]control.Peer, error) {
	if len(ports) == 0 {
		return nil, nil
	}
	s, err := Open(h, open, ports, journal)
	if err != nil {
		return nil, err
	}
	found := map[[3]string]bool{}
	s.Exchange(window,
		func(p FramePort, seq int) []byte {
			f, _ := BuildFrame(p.MAC, HelloPayload(self, p.MAC))
			return f
		},
		func(p FramePort, f Frame) {
			if ownMACs[f.Src] || f.EtherType != EtherTypeAgent {
				return
			}
			if listing, mac, ok := ParseHello(f.Payload); ok && mac == f.Src && listing != self {
				found[[3]string{listing, p.MAC, f.Src}] = true
			}
		},
		nil)
	if problems := s.Close(); len(problems) > 0 {
		return nil, fmt.Errorf("the ports were not all put back: %s", strings.Join(problems, "; "))
	}
	var peers []control.Peer
	for k := range found {
		peers = append(peers, control.Peer{ListingID: k[0], LocalMAC: k[1], PeerMAC: k[2], SeenAt: now.Unix()})
	}
	sort.Slice(peers, func(i, j int) bool { return peerKey(peers[i]) < peerKey(peers[j]) })
	return peers, nil
}

// FramePorts are a report's eligible ports as frame ports.
func (r Report) FramePorts() []FramePort {
	var out []FramePort
	for _, p := range r.EligiblePorts() {
		out = append(out, FramePort{Netdev: p.Netdev, BDF: p.BDF, MAC: strings.ToLower(p.MAC)})
	}
	return out
}

// OwnMACs are every ConnectX MAC on this machine.
func (r Report) OwnMACs() map[string]bool {
	own := map[string]bool{}
	for _, m := range r.PortMACs() {
		own[strings.ToLower(m)] = true
	}
	return own
}
