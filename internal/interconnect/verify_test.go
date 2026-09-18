package interconnect_test

import (
	"encoding/binary"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/interconnect"
	"github.com/serverroom/gpu-marketplace/internal/interconnect/wiretest"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

const (
	challenge = "0123456789abcdef0123456789abcdef"
	listingA  = "1a2b3c4d-0000-4000-8000-00000000000a"
	listingB  = "1a2b3c4d-0000-4000-8000-00000000000b"
)

// fast shrinks a check's seconds and frame interval so a test runs in
// milliseconds of real time.
func fast(t *testing.T) {
	t.Helper()
	unit, interval := interconnect.SecondUnit, interconnect.FrameInterval
	interconnect.SecondUnit, interconnect.FrameInterval = 40*time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { interconnect.SecondUnit, interconnect.FrameInterval = unit, interval })
}

type side struct {
	h     *fakehost.Host
	ports []interconnect.FramePort
}

// sparkSide is one machine with two ConnectX-7 ports on mlx5_core, both up
// (as the fake NIC comes) and IPv6 on.
func sparkSide(macPrefix string) side {
	h := fakehost.New()
	s := side{h: h}
	for i, nd := range []string{"enp1s0f0np0", "enp1s0f1np1"} {
		bdf := "0000:01:00." + string(rune('0'+i))
		mac := macPrefix + string(rune('1'+i))
		h.NIC(bdf, nd, mac, "MT2412X"+strings.ReplaceAll(macPrefix, ":", ""), bdf)
		h.Files["/proc/sys/net/ipv6/conf/"+nd+"/disable_ipv6"] = []byte("0\n")
		s.ports = append(s.ports, interconnect.FramePort{Netdev: nd, BDF: bdf, MAC: mac})
	}
	return s
}

func own(s side) map[string]bool {
	m := map[string]bool{}
	for _, p := range s.ports {
		m[p.MAC] = true
	}
	return m
}

// cabled is two machines, port i of a cabled to port i of b.
func cabled() (side, side, *wiretest.Wire) {
	a, b := sparkSide("58:a2:e1:00:00:0"), sparkSide("58:a2:e1:00:01:0")
	w := wiretest.New()
	for _, nd := range []string{"enp1s0f0np0", "enp1s0f1np1"} {
		w.Join("A/"+nd, "B/"+nd)
	}
	return a, b, w
}

type verdict struct {
	resp  *control.LinkVerifyResponse
	peers []control.Peer
	err   error
}

// both runs the check on both machines at once, as the control plane does.
func both(a, b side, w *wiretest.Wire, challengeA, challengeB string) (verdict, verdict) {
	var va, vb verdict
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		va.resp, va.peers, va.err = interconnect.VerifyPorts(a.h, w.Opener("A"), a.ports,
			interconnect.VerifyInput{Challenge: challengeA, Self: listingA, Peer: listingB, Seconds: 4, OwnMACs: own(a)})
	}()
	go func() {
		defer wg.Done()
		vb.resp, vb.peers, vb.err = interconnect.VerifyPorts(b.h, w.Opener("B"), b.ports,
			interconnect.VerifyInput{Challenge: challengeB, Self: listingB, Peer: listingA, Seconds: 4, OwnMACs: own(b)})
	}()
	wg.Wait()
	return va, vb
}

func TestLinkFramesAuthenticate(t *testing.T) {
	mac := "58:a2:e1:00:00:01"
	frame, err := interconnect.BuildFrame(mac, interconnect.LinkPayload(challenge, listingA, mac, 7))
	if err != nil {
		t.Fatal(err)
	}
	if len(frame) < 60 || binary.BigEndian.Uint16(frame[12:14]) != interconnect.EtherTypeAgent {
		t.Fatalf("frame = %x", frame)
	}
	f, ok := interconnect.ParseFrame(frame)
	if !ok || f.Dst != "ff:ff:ff:ff:ff:ff" || f.Src != mac {
		t.Fatalf("parsed = %+v", f)
	}
	lf, ok := interconnect.ParseLink(f.Payload)
	if !ok || lf.Self != listingA || lf.MAC != mac || lf.Seq != 7 || !lf.Authentic(challenge) {
		t.Fatalf("link frame = %+v ok=%v", lf, ok)
	}
	if lf.Authentic("ffffffffffffffffffffffffffffffff") {
		t.Error("a frame made with another challenge authenticated")
	}
	lf.Seq = 8
	if lf.Authentic(challenge) {
		t.Error("a replayed frame with another sequence number authenticated")
	}
	for _, bad := range []string{
		"GPUAGENT-LINK1|" + listingA + "|" + mac + "|7",
		"GPUAGENT-LINK1|" + listingA + "|" + mac + "|07|" + strings.Repeat("a", 64),
		"GPUAGENT-LINK1|" + listingA + "|58:A2:E1:00:00:01|7|" + strings.Repeat("a", 64),
		"GPUAGENT-LINK1|a b|" + mac + "|7|" + strings.Repeat("a", 64),
	} {
		if _, ok := interconnect.ParseLink([]byte(bad)); ok {
			t.Errorf("parsed %q", bad)
		}
	}
	listing, hmac, ok := interconnect.ParseHello([]byte(interconnect.HelloPayload(listingB, mac) + "\x00\x00"))
	if !ok || listing != listingB || hmac != mac {
		t.Errorf("hello = %q %q %v", listing, hmac, ok)
	}
}

func TestLLDPChassis(t *testing.T) {
	tlv := func(typ int, v []byte) []byte {
		b := make([]byte, 2)
		binary.BigEndian.PutUint16(b, uint16(typ<<9|len(v)))
		return append(b, v...)
	}
	macID := tlv(1, []byte{4, 0x40, 0xb9, 0x3c, 0xbb, 0x00, 0x01})
	if got := interconnect.LLDPChassis(append(macID, tlv(0, nil)...)); got != "40:b9:3c:bb:00:01" {
		t.Errorf("chassis = %q", got)
	}
	named := tlv(1, append([]byte{7}, "core-sw1"...))
	if got := interconnect.LLDPChassis(named); got != "core-sw1" {
		t.Errorf("chassis = %q", got)
	}
	if got := interconnect.LLDPChassis([]byte{0x02}); got != "" {
		t.Errorf("a truncated LLDP frame gave %q", got)
	}
}

// Two machines cabled port to port: each port hears the other machine's port
// on it, authenticated, and nothing else.
func TestTwoMachinesOnACableVerify(t *testing.T) {
	fast(t)
	a, b, w := cabled()
	va, vb := both(a, b, w, challenge, challenge)
	if va.err != nil || vb.err != nil {
		t.Fatalf("errors: %v %v", va.err, vb.err)
	}
	for _, v := range []struct {
		name  string
		resp  *control.LinkVerifyResponse
		peer  string
		peers []string
	}{
		{"A", va.resp, listingB, []string{"58:a2:e1:00:01:01", "58:a2:e1:00:01:02"}},
		{"B", vb.resp, listingA, []string{"58:a2:e1:00:00:01", "58:a2:e1:00:00:02"}},
	} {
		if len(v.resp.Ports) != 2 {
			t.Fatalf("%s ports = %+v", v.name, v.resp.Ports)
		}
		for i, p := range v.resp.Ports {
			if !p.Carrier || p.SpeedMbps != 200000 || p.STP || p.BadFrames != 0 || len(p.ForeignSrc) != 0 || len(p.LLDP) != 0 {
				t.Errorf("%s port %d = %+v", v.name, i, p)
			}
			if len(p.PeerFrames) != 1 || p.PeerFrames[0].Src != v.peers[i] || p.PeerFrames[0].Listing != v.peer || p.PeerFrames[0].Count < 2 {
				t.Errorf("%s port %d heard %+v, want only %s", v.name, i, p.PeerFrames, v.peers[i])
			}
		}
	}
	if len(va.peers) != 2 || va.peers[0].ListingID != listingB || va.peers[0].LocalMAC != "58:a2:e1:00:00:01" || va.peers[0].PeerMAC != "58:a2:e1:00:01:01" {
		t.Errorf("proved peers = %+v", va.peers)
	}
	if len(w.Opened()) != 0 {
		t.Errorf("sockets left open: %v", w.Opened())
	}
}

// A port that reaches a switch hears the switch: its spanning tree, its LLDP,
// another host's ARP, and a third agent announcing itself.
func TestASwitchGivesItselfAway(t *testing.T) {
	fast(t)
	a, b, w := cabled()
	w.Join("A/enp1s0f0np0", "B/enp1s0f0np0", "switch/port7")
	sw, _ := w.Open("switch/port7")
	stop := make(chan struct{})
	go func() {
		stp := make([]byte, 60)
		copy(stp, []byte{0x01, 0x80, 0xc2, 0, 0, 0, 0x40, 0xb9, 0x3c, 0xbb, 0, 0x07})
		arp := make([]byte, 60)
		copy(arp, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x08, 0x06})
		lldp := []byte{0x01, 0x80, 0xc2, 0, 0, 0x0e, 0x40, 0xb9, 0x3c, 0xbb, 0, 0x07, 0x88, 0xcc, 0x02, 0x07, 4, 0x40, 0xb9, 0x3c, 0xbb, 0, 0x07, 0, 0}
		hello, _ := interconnect.BuildFrame("58:a2:e1:00:09:01", interconnect.HelloPayload("third-listing", "58:a2:e1:00:09:01"))
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
				for _, f := range [][]byte{stp, arp, lldp, hello} {
					_ = sw.WriteFrame(f)
				}
			}
		}
	}()
	va, _ := both(a, b, w, challenge, challenge)
	close(stop)
	sw.Close()
	if va.err != nil {
		t.Fatal(va.err)
	}
	p := va.resp.Ports[0]
	if !p.STP {
		t.Errorf("spanning tree not seen: %+v", p)
	}
	if strings.Join(p.ForeignSrc, " ") != "00:11:22:33:44:55 58:a2:e1:00:09:01" {
		t.Errorf("foreign = %v", p.ForeignSrc)
	}
	if len(p.LLDP) != 1 || p.LLDP[0].Src != "40:b9:3c:bb:00:07" || p.LLDP[0].Chassis != "40:b9:3c:bb:00:07" {
		t.Errorf("lldp = %+v", p.LLDP)
	}
	if q := va.resp.Ports[1]; q.STP || len(q.ForeignSrc) != 0 {
		t.Errorf("the port on a plain cable saw the switch: %+v", q)
	}
}

func TestOneWayCableIsHeardOnOneSideOnly(t *testing.T) {
	fast(t)
	a, b, w := cabled()
	w.Cut("B/enp1s0f1np1", "A/enp1s0f1np1")
	va, vb := both(a, b, w, challenge, challenge)
	if len(va.resp.Ports[1].PeerFrames) != 0 {
		t.Errorf("A heard B over a cut direction: %+v", va.resp.Ports[1].PeerFrames)
	}
	if len(vb.resp.Ports[1].PeerFrames) != 1 {
		t.Errorf("B did not hear A: %+v", vb.resp.Ports[1].PeerFrames)
	}
}

// A machine answering with another challenge (a recorded run, a stale check)
// proves nothing: its frames count as bad.
func TestWrongChallengeFramesAreBad(t *testing.T) {
	fast(t)
	a, b, w := cabled()
	va, _ := both(a, b, w, challenge, "ffffffffffffffffffffffffffffffff")
	for _, p := range va.resp.Ports {
		if len(p.PeerFrames) != 0 || p.BadFrames == 0 {
			t.Errorf("port = %+v", p)
		}
	}
	if len(va.peers) != 0 {
		t.Errorf("an unauthenticated peer was proved: %+v", va.peers)
	}
}

// The proven peer's own ordinary traffic (a neighbour-discovery packet as its
// IPv6 goes off) is not a stranger, and neither is this machine's own echo.
func TestPeerAndOwnTrafficAreNotForeign(t *testing.T) {
	fast(t)
	a, b, w := cabled()
	w.Join("A/enp1s0f0np0", "B/enp1s0f0np0", "B/echo")
	extra, _ := w.Open("B/echo")
	stop := make(chan struct{})
	go func() {
		nd := make([]byte, 60)
		copy(nd, []byte{0x33, 0x33, 0, 0, 0, 0x16, 0x58, 0xa2, 0xe1, 0x00, 0x01, 0x01, 0x86, 0xdd})
		echo := make([]byte, 60)
		copy(echo, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x58, 0xa2, 0xe1, 0x00, 0x00, 0x02, 0x08, 0x00})
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
				_ = extra.WriteFrame(nd)
				_ = extra.WriteFrame(echo)
			}
		}
	}()
	va, _ := both(a, b, w, challenge, challenge)
	close(stop)
	extra.Close()
	if p := va.resp.Ports[0]; len(p.ForeignSrc) != 0 || len(p.PeerFrames) != 1 {
		t.Errorf("port = %+v", p)
	}
}

// Every port goes back exactly as it was: down again if it was down, IPv6 as
// it was, and no socket left open. IPv6 goes off before the link comes up, and
// the link goes down before IPv6 comes back.
func TestPortsArePutBackAsTheyWere(t *testing.T) {
	fast(t)
	a, b, w := cabled()
	a.h.Files["/sys/class/net/enp1s0f0np0/flags"] = []byte("0x1002\n")
	a.h.Files["/proc/sys/net/ipv6/conf/enp1s0f1np1/disable_ipv6"] = []byte("1\n")
	journal := interconnect.JournalPath("/var/lib/gpu-agent")
	var err error
	done := make(chan struct{})
	go func() {
		_, _, err = interconnect.VerifyPorts(a.h, w.Opener("A"), a.ports, interconnect.VerifyInput{
			Challenge: challenge, Self: listingA, Peer: listingB, Seconds: 2, OwnMACs: own(a), Journal: journal})
		close(done)
	}()
	<-done
	if err != nil {
		t.Fatal(err)
	}
	h := a.h
	ipv6 := "write /proc/sys/net/ipv6/conf/enp1s0f0np0/disable_ipv6"
	for _, order := range [][2]string{
		{"write " + journal, ipv6 + "=1"},
		{ipv6 + "=1", "run ip link set dev enp1s0f0np0 up"},
		{"run ip link set dev enp1s0f0np0 up", "run ip link set dev enp1s0f0np0 down"},
		{"run ip link set dev enp1s0f0np0 down", ipv6 + "=0"},
	} {
		if x, y := h.Index(order[0]), h.Index(order[1]); x < 0 || y < 0 || x > y {
			t.Errorf("%q (%d) must come before %q (%d); calls: %v", order[0], x, order[1], y, h.Calls)
		}
	}
	if string(h.Files["/proc/sys/net/ipv6/conf/enp1s0f0np0/disable_ipv6"]) != "0" {
		t.Errorf("IPv6 not restored: %q", h.Files["/proc/sys/net/ipv6/conf/enp1s0f0np0/disable_ipv6"])
	}
	// The second port was up with IPv6 already off: nothing to change or undo.
	for _, c := range []string{"run ip link set dev enp1s0f1np1", "write /proc/sys/net/ipv6/conf/enp1s0f1np1"} {
		if h.Ran(c) {
			t.Errorf("%q ran on a port that needed nothing", c)
		}
	}
	if h.Exists(journal) {
		t.Errorf("the journal outlived a clean run")
	}
	if len(w.Opened()) != 0 {
		t.Errorf("sockets left open: %v", w.Opened())
	}
	_ = b
}

// A socket that cannot open leaves nothing changed behind it.
func TestAFailedOpenPutsEverythingBack(t *testing.T) {
	a, _, w := cabled()
	a.h.Files["/sys/class/net/enp1s0f0np0/flags"] = []byte("0x1002\n")
	opener := func(nd string) (interconnect.PacketConn, error) {
		if nd == "enp1s0f1np1" {
			return nil, errors.New("operation not permitted")
		}
		return w.Opener("A")(nd)
	}
	_, _, err := interconnect.VerifyPorts(a.h, opener, a.ports, interconnect.VerifyInput{
		Challenge: challenge, Self: listingA, Peer: listingB, Seconds: 1, OwnMACs: own(a)})
	if err == nil || !strings.Contains(err.Error(), "operation not permitted") {
		t.Fatalf("err = %v", err)
	}
	if !a.h.Ran("run ip link set dev enp1s0f0np0 down") || string(a.h.Files["/proc/sys/net/ipv6/conf/enp1s0f0np0/disable_ipv6"]) != "0" {
		t.Errorf("port not put back after a failed open: %v", a.h.Calls)
	}
	if len(w.Opened()) != 0 {
		t.Errorf("sockets left open: %v", w.Opened())
	}
}

// An agent killed mid-check leaves its journal; the next start puts the ports
// back from it.
func TestRecoverPortsFromAnUnfinishedRun(t *testing.T) {
	h := fakehost.New()
	h.NIC("0000:01:00.0", "enp1s0f0np0", "58:a2:e1:00:00:01", "MT1", "0000:01:00.0")
	h.Files["/proc/sys/net/ipv6/conf/enp1s0f0np0/disable_ipv6"] = []byte("1")
	h.Files["/sys/class/net/enp1s0f0np0"] = nil
	journal := interconnect.JournalPath("/var/lib/gpu-agent")
	h.Files[journal] = []byte(`[{"netdev":"enp1s0f0np0","was_up":false,` +
		`"ipv6_path":"/proc/sys/net/ipv6/conf/enp1s0f0np0/disable_ipv6","ipv6_orig":"0"},` +
		`{"netdev":"x; reboot","was_up":false}]`)
	if problems := interconnect.RecoverPorts(h, "/var/lib/gpu-agent"); len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	if !h.Ran("run ip link set dev enp1s0f0np0 down") || string(h.Files["/proc/sys/net/ipv6/conf/enp1s0f0np0/disable_ipv6"]) != "0" {
		t.Errorf("not recovered: %v", h.Calls)
	}
	if h.Ran("run ip link set dev x; reboot") {
		t.Errorf("a journal entry that is not an interface name was acted on")
	}
	if h.Exists(journal) {
		t.Errorf("the journal was kept after recovery")
	}
}

// A record left by an earlier run means the ports are not in their original
// state: a new run refuses to start (and to overwrite or remove that record)
// rather than record the changed state as the original.
func TestARunRefusesWhileAnEarlierRecordIsThere(t *testing.T) {
	a, _, w := cabled()
	journal := interconnect.JournalPath("/var/lib/gpu-agent")
	left := []byte(`{"boot_id":"b1","ports":[{"netdev":"enp1s0f0np0","was_up":false}]}`)
	a.h.Files[journal] = left
	_, _, err := interconnect.VerifyPorts(a.h, w.Opener("A"), a.ports, interconnect.VerifyInput{
		Challenge: challenge, Self: listingA, Peer: listingB, Seconds: 1, OwnMACs: own(a), Journal: journal})
	if !errors.Is(err, interconnect.ErrUnfinishedRun) {
		t.Fatalf("err = %v", err)
	}
	if string(a.h.Files[journal]) != string(left) {
		t.Errorf("the earlier record was changed: %s", a.h.Files[journal])
	}
	for _, c := range a.h.Calls {
		if strings.HasPrefix(c, "run ip ") || strings.HasPrefix(c, "write /proc/sys/net/ipv6") {
			t.Errorf("a port was touched: %q", c)
		}
	}
	if len(w.Opened()) != 0 {
		t.Errorf("sockets opened: %v", w.Opened())
	}
}

// A run that fails before it records anything (a bad port name) leaves an
// earlier run's record alone.
func TestAFailedStartKeepsAnotherRunsRecord(t *testing.T) {
	a, _, w := cabled()
	journal := interconnect.JournalPath("/var/lib/gpu-agent")
	bad := []interconnect.FramePort{{Netdev: "x; reboot", MAC: "58:a2:e1:00:00:01"}}
	if _, err := interconnect.Open(a.h, w.Opener("A"), bad, journal); err == nil {
		t.Fatal("a bad port name was accepted")
	}
	a.h.Files[journal] = []byte(`{"ports":[]}`)
	if _, err := interconnect.Open(a.h, w.Opener("A"), bad, ""); err == nil {
		t.Fatal("a bad port name was accepted")
	}
	if !a.h.Exists(journal) {
		t.Errorf("a record this run did not write was removed")
	}
}

// A record from an earlier boot has nothing to restore -- the restart reset
// the ports -- and is dropped without touching them.
func TestRecoverPortsDropsARecordFromAnEarlierBoot(t *testing.T) {
	h := fakehost.New()
	h.NIC("0000:01:00.0", "enp1s0f0np0", "58:a2:e1:00:00:01", "MT1", "0000:01:00.0")
	h.Files["/sys/class/net/enp1s0f0np0"] = nil
	h.Files["/proc/sys/kernel/random/boot_id"] = []byte("boot-2\n")
	journal := interconnect.JournalPath("/var/lib/gpu-agent")
	h.Files[journal] = []byte(`{"boot_id":"boot-1","ports":[{"netdev":"enp1s0f0np0","was_up":false,` +
		`"ipv6_path":"/proc/sys/net/ipv6/conf/enp1s0f0np0/disable_ipv6","ipv6_orig":"0"}]}`)
	if problems := interconnect.RecoverPorts(h, "/var/lib/gpu-agent"); len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	if h.Ran("run ip link set dev enp1s0f0np0 down") || h.Exists("/proc/sys/net/ipv6/conf/enp1s0f0np0/disable_ipv6") {
		t.Errorf("a port was changed from an earlier boot's record: %v", h.Calls)
	}
	if h.Exists(journal) {
		t.Errorf("an earlier boot's record was kept")
	}

	// The same boot: played back.
	h.Files[journal] = []byte(`{"boot_id":"boot-2","ports":[{"netdev":"enp1s0f0np0","was_up":false}]}`)
	interconnect.RecoverPorts(h, "/var/lib/gpu-agent")
	if !h.Ran("run ip link set dev enp1s0f0np0 down") {
		t.Errorf("this boot's record was not played back: %v", h.Calls)
	}
}

// A run records the boot it belongs to.
func TestTheRecordNamesTheBoot(t *testing.T) {
	fast(t)
	a, _, w := cabled()
	a.h.Files["/proc/sys/kernel/random/boot_id"] = []byte("boot-7\n")
	journal := interconnect.JournalPath("/var/lib/gpu-agent")
	s, err := interconnect.Open(a.h, w.Opener("A"), a.ports, journal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(a.h.Files[journal]), `"boot_id":"boot-7"`) {
		t.Errorf("record = %s", a.h.Files[journal])
	}
	if problems := s.Close(); len(problems) != 0 || a.h.Exists(journal) {
		t.Errorf("problems %v, record kept %v", problems, a.h.Exists(journal))
	}
}

func TestHelloFindsTheOtherAgent(t *testing.T) {
	fast(t)
	a, b, w := cabled()
	now := time.Unix(1789000000, 0)
	var pa, pb []control.Peer
	var ea, eb error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		pa, ea = interconnect.HelloPorts(a.h, w.Opener("A"), a.ports, listingA, 60*time.Millisecond, own(a), now, "")
	}()
	go func() {
		defer wg.Done()
		pb, eb = interconnect.HelloPorts(b.h, w.Opener("B"), b.ports, listingB, 60*time.Millisecond, own(b), now, "")
	}()
	wg.Wait()
	if ea != nil || eb != nil {
		t.Fatal(ea, eb)
	}
	if len(pa) != 2 || pa[0].ListingID != listingB || pa[0].LocalMAC != "58:a2:e1:00:00:01" || pa[0].PeerMAC != "58:a2:e1:00:01:01" || pa[0].SeenAt != now.Unix() {
		t.Errorf("A heard %+v", pa)
	}
	if len(pb) != 2 || pb[1].ListingID != listingA || pb[1].LocalMAC != "58:a2:e1:00:01:02" {
		t.Errorf("B heard %+v", pb)
	}
}
