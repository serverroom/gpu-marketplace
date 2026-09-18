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

	// Heard again an hour later: the same peers, seen again.
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

func pairRequest(macs ...string) control.PairProvisionRequest {
	req := control.PairProvisionRequest{RentalID: "1a2b3c4d-0000-4000-8000-000000000001", Node: "a",
		PeerHostname: "gpu-1a2b3c4d-b", MTU: 9000}
	for i, m := range macs {
		req.Links = append(req.Links, control.PairLink{LocalMAC: m,
			CIDR: "10.200." + string(rune('0'+i)) + ".1/30", PeerIP: "10.200." + string(rune('0'+i)) + ".2"})
	}
	return req
}

// sparkForPair is Spark A with a recording machine behind it, so a pair
// rental's start can be inspected without booting anything.
func sparkForPair(t *testing.T) (*Provisioner, *fakeMachine) {
	t.Helper()
	a, _, _ := sparkPair(t)
	m := &fakeMachine{stopRes: clean()}
	a.machine = m
	a.async = false
	withFakeForward(t, nil)
	return a, m
}

func TestPairProvisionHandsTheCardToTheRental(t *testing.T) {
	a, m := sparkForPair(t)
	req := pairRequest("58:a2:e1:00:00:01")
	req.RenterPubkey = renterKey(t)
	if err := a.PairProvision(req); err != nil {
		t.Fatal(err)
	}
	if a.Status() != StatusRented || m.started != 1 || m.last.Pair == nil {
		t.Fatalf("status %s started %d opts %+v", a.Status(), m.started, m.last)
	}
	p := m.last.Pair
	if p.Node != "a" || p.MTU != 9000 || len(p.Links) != 1 || p.Links[0].Name != "cx7p0" ||
		strings.Join(p.Functions, " ") != strings.Join(sparkNICs, " ") || strings.Join(p.OtherMACs, " ") != "58:a2:e1:00:00:02" {
		t.Errorf("pair = %+v", p)
	}
	// A second pair rental, or a cable check, while this one is on the machine.
	if err := a.PairProvision(req); code(err) != http.StatusConflict {
		t.Errorf("a second pair rental: %v", err)
	}
	if _, err := a.LinkVerify(control.LinkVerifyRequest{Challenge: challenge, Self: listingA, Peer: listingB}); code(err) != http.StatusConflict {
		t.Errorf("a cable check during a rental: %v", err)
	}
}

func code(err error) int {
	var re *control.RequestError
	if errors.As(err, &re) {
		return re.Code
	}
	return 0
}

func TestPairProvisionRefusals(t *testing.T) {
	key := renterKey(t)
	cases := []struct {
		name  string
		setup func(p *Provisioner)
		req   control.PairProvisionRequest
		code  int
		says  string
	}{
		{"a port this machine lacks", nil, pairRequest("58:a2:e1:00:00:09"), http.StatusConflict, "not a ConnectX-7 port"},
		{"a bad address", nil, func() control.PairProvisionRequest {
			r := pairRequest("58:a2:e1:00:00:01")
			r.Links[0].CIDR = "10.200.0.1/29"
			return r
		}(), http.StatusBadRequest, "/30"},
		{"no pair test any more", func(p *Provisioner) {
			delete(p.host.(*fakehost.Host).Files, vmrt.PairTestPath(dataDir))
		}, pairRequest("58:a2:e1:00:00:01"), http.StatusServiceUnavailable, "pair test"},
		{"an address appeared on a port", func(p *Provisioner) {
			p.host.(*fakehost.Host).Outputs["ip -j addr show dev enP1p1s0f1np1"] = `[{"addr_info":[{"family":"inet","local":"192.168.9.9","prefixlen":24}]}]`
		}, pairRequest("58:a2:e1:00:00:01"), http.StatusServiceUnavailable, "192.168.9.9"},
		{"not free", func(p *Provisioner) { p.status = StatusDirty }, pairRequest("58:a2:e1:00:00:01"), http.StatusConflict, "dirty"},
		{"a check running", func(p *Provisioner) { p.checking = true }, pairRequest("58:a2:e1:00:00:01"), http.StatusConflict, "cable check"},
		{"a bad rental id", nil, func() control.PairProvisionRequest {
			r := pairRequest("58:a2:e1:00:00:01")
			r.RentalID = "../x"
			return r
		}(), http.StatusBadRequest, "rental id"},
	}
	for _, c := range cases {
		a, m := sparkForPair(t)
		if c.setup != nil {
			c.setup(a)
		}
		c.req.RenterPubkey = key
		err := a.PairProvision(c.req)
		if code(err) != c.code || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v (code %d), want %d mentioning %q", c.name, err, code(err), c.code, c.says)
		}
		if m.started != 0 {
			t.Errorf("%s: a refused pair rental started", c.name)
		}
	}
	a, _ := sparkForPair(t)
	a.capability.Ready = false
	if err := a.PairProvision(pairRequest("58:a2:e1:00:00:01")); code(err) != http.StatusServiceUnavailable {
		t.Errorf("a machine that cannot host: %v", err)
	}
}

func TestPairStatusReadsTheGuestsLinkCheck(t *testing.T) {
	a, _, _ := sparkPair(t)
	if a.PairStatus() != nil {
		t.Fatal("a pair on a free machine")
	}
	h := a.host.(*fakehost.Host)
	r := vmrt.NewRental(dataDir, "1a2b3c4d-0000-4000-8000-000000000001")
	st := &vmrt.State{RentalID: r.ID, Rental: r, Pair: &vmrt.PairOptions{Node: "b", Links: []vmrt.GuestLink{{}, {}}}}
	if err := vmrt.SaveState(h, dataDir, st); err != nil {
		t.Fatal(err)
	}
	h.SetFile(r.SerialLog, []byte("GPUAGENT-LINK ok 0\nGPUAGENT-LINK fail 1\n"))
	ps := a.PairStatus()
	if ps == nil || ps.Node != "b" || ps.GuestLink != (control.GuestLinkStatus{OK: 1, Fail: 1, Links: 2}) {
		t.Errorf("pair status = %+v", ps)
	}
}

func fastPairTest(t *testing.T) {
	fastFrames(t)
	wait, settle := PairTestWait, interconnect.PeerSettle
	PairTestWait, interconnect.PeerSettle = 2*time.Second, 30*time.Millisecond
	t.Cleanup(func() { PairTestWait, interconnect.PeerSettle = wait, settle })
}

// Both machines run the pair test: each finds the other, they agree on one
// plan, and each boots its half with the whole card and every other port named.
func TestPairTestFindsTheOtherMachineAndAgrees(t *testing.T) {
	fastPairTest(t)
	a, b, _ := sparkPair(t)
	var got [2]vmrt.PairSelfTestOptions
	for i, p := range []*Provisioner{a, b} {
		i := i
		p.pairTest = func(version string, o vmrt.PairSelfTestOptions) vmrt.PairTestResult {
			got[i] = o
			return vmrt.PairTestResult{Passed: true, AgentVersion: version, Node: o.Pair.Node}
		}
	}
	var ra, rb vmrt.PairTestResult
	var ea, eb error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); ra, ea = a.PairSelfTest(PairTestRun{MinRDMAGbps: 100}) }()
	go func() { defer wg.Done(); rb, eb = b.PairSelfTest(PairTestRun{MinRDMAGbps: 100}) }()
	wg.Wait()
	if ea != nil || eb != nil || !ra.Passed || !rb.Passed {
		t.Fatalf("A %+v %v / B %+v %v", ra, ea, rb, eb)
	}
	oa, ob := got[0], got[1]
	if oa.ID != ob.ID || oa.Pair.Node != "a" || ob.Pair.Node != "b" || oa.PeerListing != listingB || ob.PeerListing != listingA {
		t.Fatalf("plans: %+v / %+v", oa, ob)
	}
	if strings.Join(oa.Pair.Functions, " ") != strings.Join(sparkNICs, " ") || len(oa.Pair.Links) != 2 || oa.Pair.MTU != 9000 || oa.MinRDMAGbps != 100 {
		t.Errorf("A's half = %+v", oa.Pair)
	}
	// The other machine is on the peer list now.
	if peers := a.Capability().Interconnect.Peers; len(peers) != 2 || peers[0].ListingID != listingB {
		t.Errorf("A's peers = %+v", peers)
	}
}

func TestPairTestRefusalsAndFailures(t *testing.T) {
	fastPairTest(t)
	a, _, _ := sparkPair(t)
	a.pairTest = func(string, vmrt.PairSelfTestOptions) vmrt.PairTestResult {
		t.Fatal("a test VM booted")
		return vmrt.PairTestResult{}
	}
	// Nobody on the other end: a recorded failure.
	PairTestWait = 100 * time.Millisecond
	res, err := a.PairSelfTest(PairTestRun{})
	if err != nil || res.Passed || !strings.Contains(strings.Join(res.Problems, " "), "no other agent answered") {
		t.Fatalf("res %+v err %v", res, err)
	}
	if saved, _ := vmrt.LoadPairTest(a.host, dataDir); saved == nil || saved.Passed || saved.MinRDMAGbps != vmrt.DefaultMinRDMAGbps {
		t.Errorf("not recorded: %+v", saved)
	}
	if a.Capability().Interconnect.Ready {
		t.Errorf("still ready for a pair after a failed pair test")
	}

	// Something that has to be fixed first: nothing runs, nothing is recorded.
	h := a.host.(*fakehost.Host)
	h.Outputs["ip -j addr show dev enP1p1s0f0np0"] = `[{"addr_info":[{"family":"inet","local":"10.9.9.9","prefixlen":24}]}]`
	delete(h.Files, vmrt.PairTestPath(dataDir))
	if _, err := a.PairSelfTest(PairTestRun{}); !errors.Is(err, ErrPairTestBlocked) || !strings.Contains(err.Error(), "10.9.9.9") {
		t.Errorf("err = %v", err)
	}
	if saved, _ := vmrt.LoadPairTest(a.host, dataDir); saved != nil {
		t.Errorf("a blocked test recorded %+v", saved)
	}
}

// An agent that starts while a pair test from the command line holds the ports
// leaves them to it; once nobody holds them it puts them back from the record.
func TestRecoverPortsLeavesARunningTestAlone(t *testing.T) {
	a, _, _ := sparkPair(t)
	h := a.host.(*fakehost.Host)
	journal := interconnect.JournalPath(dataDir)
	h.Files[journal] = []byte(`[{"netdev":"enP1p1s0f0np0","was_up":false}]`)
	unlock, _, _ := a.lockFrames()
	a.RecoverPorts()
	if h.Ran("run ip link set dev enP1p1s0f0np0 down") || !h.Exists(journal) {
		t.Fatalf("recovered under a running test: %v", h.Calls)
	}
	unlock()
	a.RecoverPorts()
	if !h.Ran("run ip link set dev enP1p1s0f0np0 down") || h.Exists(journal) {
		t.Errorf("not recovered: %v", h.Calls)
	}
}

// While the automatic setup works on the machine, or after it was removed from
// the marketplace, nothing puts frames on the cable and no pair rental starts.
func TestCableAndPairRentalsWaitForTheSetupAndStopWhenWithdrawn(t *testing.T) {
	fastFrames(t)
	a, _, w := sparkPair(t)
	req := control.LinkVerifyRequest{Challenge: challenge, Self: listingA, Peer: listingB}
	pr := pairRequest("58:a2:e1:00:00:01")
	pr.RenterPubkey = renterKey(t)

	if !a.BeginSetup() {
		t.Fatal("setup did not begin")
	}
	if _, err := a.LinkVerify(req); code(err) != http.StatusConflict {
		t.Errorf("cable check during the setup: %v", err)
	}
	if ran, _, _ := a.DiscoverPeers(); ran {
		t.Errorf("peer announcement during the setup")
	}
	if err := a.PairProvision(pr); code(err) != http.StatusServiceUnavailable {
		t.Errorf("pair rental during the setup: %v", err)
	}
	a.EndSetup()

	a.Withdraw("removed")
	if _, err := a.LinkVerify(req); code(err) != http.StatusServiceUnavailable {
		t.Errorf("cable check on a withdrawn machine: %v", err)
	}
	if ran, _, _ := a.DiscoverPeers(); ran {
		t.Errorf("peer announcement on a withdrawn machine")
	}
	if len(w.Opened()) != 0 {
		t.Errorf("sockets opened: %v", w.Opened())
	}
}
