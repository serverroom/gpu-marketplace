package provisioner

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/interconnect"
	"github.com/serverroom/gpu-marketplace/internal/interconnect/wiretest"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

const (
	listingA  = "1a2b3c4d-0000-4000-8000-00000000000a"
	listingB  = "1a2b3c4d-0000-4000-8000-00000000000b"
	challenge = "0123456789abcdef0123456789abcdef"
)

var sparkNICs = []string{"0001:01:00.0", "0001:01:00.1"}

// sparkMachine is a DGX Spark that can host a single rental and is ready to be
// half of a linked pair: identified, two ConnectX-7 ports nobody on the host
// uses, the RDMA base image, and a passing pair test over those ports.
func sparkMachine(t *testing.T, macPrefix string) *fakehost.Host {
	t.Helper()
	h := goodHost(t, "0000000F:01:00.0, NVIDIA GB10, [N/A]\n")
	h.Files["/proc/meminfo"] = []byte("MemTotal:       128000000 kB\n")
	h.Files[dataDir] = nil
	h.Outputs["df --output=avail"] = " Avail\n  900G\n"
	h.Files["/sys/class/dmi/id/sys_vendor"] = []byte("NVIDIA\n")
	h.Files["/sys/class/dmi/id/product_name"] = []byte("DGX Spark\n")
	h.Files["/usr/share/AAVMF/AAVMF_CODE.fd"] = nil
	h.Files["/usr/share/AAVMF/AAVMF_VARS.fd"] = []byte("vars")
	golden, _ := json.Marshal(vmrt.GoldenInfo{Base: "resolute-server-cloudimg-arm64.img", Driver: "580-server-open", Extras: []string{"rdma"}})
	h.Files[filepath.Join(dataDir, "golden.img")+".json"] = golden
	var macs []string
	for i, bdf := range sparkNICs {
		nd := []string{"enP1p1s0f0np0", "enP1p1s0f1np1"}[i]
		mac := macPrefix + string(rune('1'+i))
		h.NIC(bdf, nd, mac, "MT2412X"+strings.ReplaceAll(macPrefix, ":", ""), bdf)
		h.Files["/proc/sys/net/ipv6/conf/"+nd+"/disable_ipv6"] = []byte("0\n")
		macs = append(macs, mac)
	}
	pass, _ := json.Marshal(vmrt.PairTestResult{Passed: true, AgentVersion: version, LocalMACs: macs, RDMAGbps: []float64{110, 111}})
	h.Files[vmrt.PairTestPath(dataDir)] = pass
	return h
}

// memLock is a frame lock that lives in the test, not on disk.
type memLock struct{ mu sync.Mutex }

func (l *memLock) try() (func(), bool, error) {
	if !l.mu.TryLock() {
		return nil, false, nil
	}
	return l.mu.Unlock, true, nil
}

func noRelay(t *testing.T) {
	orig := RelayAddrs
	RelayAddrs = func() []string { return nil }
	t.Cleanup(func() { RelayAddrs = orig })
}

func fastFrames(t *testing.T) {
	unit, interval, hello := interconnect.SecondUnit, interconnect.FrameInterval, HelloWindow
	interconnect.SecondUnit, interconnect.FrameInterval, HelloWindow = 40*time.Millisecond, 2*time.Millisecond, 60*time.Millisecond
	t.Cleanup(func() { interconnect.SecondUnit, interconnect.FrameInterval, HelloWindow = unit, interval, hello })
}

// sparkPair is two Sparks, A and B, whose ports are cabled to each other.
func sparkPair(t *testing.T) (*Provisioner, *Provisioner, *wiretest.Wire) {
	t.Helper()
	noRelay(t)
	w := wiretest.New()
	w.Join("A/enP1p1s0f0np0", "B/enP1p1s0f0np0")
	w.Join("A/enP1p1s0f1np1", "B/enP1p1s0f1np1")
	build := func(name, prefix, listing string) *Provisioner {
		p := Detect(sparkMachine(t, prefix), "linux", "arm64", dataDir, version)
		if c := p.Capability(); !c.Ready || c.Interconnect == nil || !c.Interconnect.Ready {
			t.Fatalf("%s is not a ready Spark: %v / %+v", name, c.Reasons, c.Interconnect)
		}
		p.openPacket = w.Opener(name)
		lock := &memLock{}
		p.lockFrames = lock.try
		p.SetListingID(func() string { return listing })
		p.now = func() time.Time { return time.Unix(1789000000, 0) }
		return p
	}
	return build("A", "58:a2:e1:00:00:0", listingA), build("B", "58:a2:e1:00:01:0", listingB), w
}

func TestTwoAgentsProveTheirCable(t *testing.T) {
	fastFrames(t)
	a, b, w := sparkPair(t)
	var ra, rb *control.LinkVerifyResponse
	var ea, eb error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ra, ea = a.LinkVerify(control.LinkVerifyRequest{Challenge: challenge, Self: listingA, Peer: listingB})
	}()
	go func() {
		defer wg.Done()
		rb, eb = b.LinkVerify(control.LinkVerifyRequest{Challenge: challenge, Self: listingB, Peer: listingA})
	}()
	wg.Wait()
	if ea != nil || eb != nil {
		t.Fatal(ea, eb)
	}
	if len(ra.Ports) != 2 || len(rb.Ports) != 2 {
		t.Fatalf("ports: %+v / %+v", ra.Ports, rb.Ports)
	}
	for _, p := range ra.Ports {
		if len(p.PeerFrames) != 1 || p.PeerFrames[0].Listing != listingB || p.BadFrames != 0 || len(p.ForeignSrc) != 0 {
			t.Errorf("A port = %+v", p)
		}
	}
	// The proven peer is on the capability now, for the control plane's mutual-peer rule.
	if peers := a.Capability().Interconnect.Peers; len(peers) != 2 || peers[0].ListingID != listingB {
		t.Errorf("A's peers = %+v", peers)
	}
	if len(w.Opened()) != 0 {
		t.Errorf("sockets left open: %v", w.Opened())
	}
}

func TestLinkVerifyRefusals(t *testing.T) {
	fastFrames(t)
	a, _, _ := sparkPair(t)
	req := control.LinkVerifyRequest{Challenge: challenge, Self: listingA, Peer: listingB}
	code := func(err error) int {
		var re *control.RequestError
		if errors.As(err, &re) {
			return re.Code
		}
		return 0
	}

	if _, err := a.LinkVerify(control.LinkVerifyRequest{Challenge: challenge, Self: listingB, Peer: listingA}); code(err) != http.StatusBadRequest ||
		!strings.Contains(err.Error(), "this machine is listing "+listingA) {
		t.Errorf("a check addressed to another listing: %v", err)
	}

	a.status = StatusRented
	if _, err := a.LinkVerify(req); code(err) != http.StatusConflict {
		t.Errorf("rented: %v", err)
	}
	a.status = StatusFree

	unlock, _, _ := a.lockFrames()
	if _, err := a.LinkVerify(req); code(err) != http.StatusConflict || !strings.Contains(err.Error(), "already running") {
		t.Errorf("a second check at once: %v", err)
	}
	unlock()

	// A test boot from the command line leaves rental state the daemon does
	// not know about: the ports belong to its VM.
	a.host.(*fakehost.Host).Files[vmrt.StatePath(dataDir)] = []byte(`{"rental_id":"selftest-1"}`)
	if _, err := a.LinkVerify(req); code(err) != http.StatusConflict {
		t.Errorf("rental state present: %v", err)
	}
}

func TestPeersAnnounceThemselves(t *testing.T) {
	fastFrames(t)
	a, b, _ := sparkPair(t)
	type out struct {
		ran, changed bool
		err          error
	}
	var oa, ob out
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); oa.ran, oa.changed, oa.err = a.DiscoverPeers() }()
	go func() { defer wg.Done(); ob.ran, ob.changed, ob.err = b.DiscoverPeers() }()
	wg.Wait()
	if !oa.ran || !oa.changed || oa.err != nil || !ob.ran || !ob.changed || ob.err != nil {
		t.Fatalf("A %+v B %+v", oa, ob)
	}
	peers := a.Capability().Interconnect.Peers
	if len(peers) != 2 || peers[0].ListingID != listingB {
		t.Errorf("A heard %+v", peers)
	}
	if !strings.Contains(PeerSummary(b.Capability()), listingA) {
		t.Errorf("summary = %s", PeerSummary(b.Capability()))
	}

	// Heard again an hour later: the same peers, so nothing new to report.
	a.now = func() time.Time { return time.Unix(1789003600, 0) }
	b.now = a.now
	wg.Add(2)
	go func() { defer wg.Done(); oa.ran, oa.changed, oa.err = a.DiscoverPeers() }()
	go func() { defer wg.Done(); ob.ran, ob.changed, ob.err = b.DiscoverPeers() }()
	wg.Wait()
	if !oa.ran || oa.err != nil {
		t.Fatalf("A %+v", oa)
	}
	if got := a.Capability().Interconnect.Peers; len(got) != 2 || got[0].SeenAt != 1789003600 {
		t.Errorf("peers not refreshed: %+v", got)
	}
}

func TestPeerAnnouncementsSkipABusyOrUnregisteredMachine(t *testing.T) {
	fastFrames(t)
	a, _, w := sparkPair(t)
	a.SetListingID(func() string { return "" })
	if ran, _, _ := a.DiscoverPeers(); ran {
		t.Errorf("an unregistered machine announced itself")
	}
	a.SetListingID(func() string { return listingA })
	a.status = StatusRented
	if ran, _, _ := a.DiscoverPeers(); ran {
		t.Errorf("a rented machine announced itself")
	}
	if len(w.Opened()) != 0 {
		t.Errorf("sockets opened: %v", w.Opened())
	}
}
