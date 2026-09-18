//go:build linux

package interconnect_test

import (
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/interconnect"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// The real thing on a real kernel: two ends of a veth pair stand in for a
// cable between two machines, and the cable check runs on each over AF_PACKET
// sockets. It needs root (CAP_NET_ADMIN to make the pair, CAP_NET_RAW for the
// sockets), so it skips anywhere else.
func TestVethPairVerifiesOverRealSockets(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: creates a veth pair and opens raw sockets")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("needs iproute2")
	}
	const endA, endB = "gpatest0", "gpatest1"
	if out, err := exec.Command("ip", "link", "add", endA, "type", "veth", "peer", "name", endB).CombinedOutput(); err != nil {
		t.Skipf("cannot create a veth pair: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", endA).Run() })

	port := func(name string) interconnect.FramePort {
		ifi, err := net.InterfaceByName(name)
		if err != nil {
			t.Fatal(err)
		}
		return interconnect.FramePort{Netdev: name, MAC: strings.ToLower(ifi.HardwareAddr.String())}
	}
	a, b := port(endA), port(endB)
	ipv6 := func(name string) string {
		v, _ := os.ReadFile("/proc/sys/net/ipv6/conf/" + name + "/disable_ipv6")
		return strings.TrimSpace(string(v))
	}
	before := map[string]string{endA: ipv6(endA), endB: ipv6(endB)}
	h := vmrt.OSHost{}
	var ra, rb *control.LinkVerifyResponse
	var ea, eb error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ra, _, ea = interconnect.VerifyPorts(h, interconnect.OpenPacket, []interconnect.FramePort{a},
			interconnect.VerifyInput{Challenge: challenge, Self: listingA, Peer: listingB, Seconds: 3, OwnMACs: map[string]bool{a.MAC: true}})
	}()
	go func() {
		defer wg.Done()
		rb, _, eb = interconnect.VerifyPorts(h, interconnect.OpenPacket, []interconnect.FramePort{b},
			interconnect.VerifyInput{Challenge: challenge, Self: listingB, Peer: listingA, Seconds: 3, OwnMACs: map[string]bool{b.MAC: true}})
	}()
	wg.Wait()
	if ea != nil || eb != nil {
		t.Fatalf("errors: %v / %v", ea, eb)
	}
	for _, c := range []struct {
		resp *control.LinkVerifyResponse
		src  string
		peer string
	}{{ra, b.MAC, listingB}, {rb, a.MAC, listingA}} {
		p := c.resp.Ports[0]
		if !p.Carrier || len(p.PeerFrames) != 1 || p.PeerFrames[0].Src != c.src || p.PeerFrames[0].Listing != c.peer ||
			p.BadFrames != 0 || p.STP || len(p.ForeignSrc) != 0 {
			t.Errorf("port = %+v", p)
		}
	}
	// Both ends were down before the check, and are again, with IPv6 as it was.
	for _, name := range []string{endA, endB} {
		ifi, _ := net.InterfaceByName(name)
		if ifi.Flags&net.FlagUp != 0 {
			t.Errorf("%s was left up", name)
		}
		if got := ipv6(name); got != before[name] {
			t.Errorf("%s disable_ipv6 = %s, was %s", name, got, before[name])
		}
	}
}
