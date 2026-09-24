package netguard

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
)

type fakeRunner struct {
	fail   map[string]bool // "nft -f" / "nft list" -> should error
	listed string          // what `nft list table` prints
	calls  []string
	loaded string          // the ruleset fed to `nft -f -`
}

func (f *fakeRunner) key(name string, args []string) string {
	return strings.TrimSpace(name + " " + strings.Join(args, " "))
}

func (f *fakeRunner) Run(name string, args ...string) error {
	k := f.key(name, args)
	f.calls = append(f.calls, k)
	for prefix := range f.fail {
		if strings.HasPrefix(k, prefix) {
			return fmt.Errorf("simulated failure: %s", k)
		}
	}
	return nil
}

func (f *fakeRunner) RunInput(stdin []byte, name string, args ...string) error {
	f.loaded = string(stdin)
	return f.Run(name, args...)
}

func (f *fakeRunner) Output(name string, args ...string) (string, error) {
	if err := f.Run(name, args...); err != nil {
		return "", err
	}
	return f.listed, nil
}

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	ip, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	n.IP = ip.Mask(n.Mask)
	return n
}

// The LAN a provider's NAS sits on is the whole point: every private range
// must be in the blocked set whatever the host's own addressing looks like.
func TestRulesetBlocksPrivateRangesAndTheHost(t *testing.T) {
	rs := Ruleset(Bridge, "", nil)
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"100.64.0.0/10", "169.254.0.0/16", "224.0.0.0/4", "fc00::/7", "fe80::/10"} {
		if !strings.Contains(rs, cidr) {
			t.Errorf("ruleset does not block %s", cidr)
		}
	}
	for _, rule := range []string{
		`iifname "gpurent0" ip daddr @blocked4 drop`,
		`iifname "gpurent0" ip6 daddr @blocked6 drop`,
		`oifname "gpurent0" drop`,
		`iifname "gpurent0" drop`,
	} {
		if !strings.Contains(rs, rule) {
			t.Errorf("ruleset is missing %q", rule)
		}
	}
}

// A LAN numbered from public space is not covered by RFC 1918, so the host's
// own networks are fenced off explicitly.
func TestRulesetBlocksHostNetworksOutsidePrivateSpace(t *testing.T) {
	rs := Ruleset(Bridge, "", []*net.IPNet{
		mustCIDR(t, "203.0.113.7/24"),
		mustCIDR(t, "2001:db8:1::5/64"),
	})
	if !strings.Contains(rs, "203.0.113.0/24") {
		t.Errorf("host IPv4 network not blocked:\n%s", rs)
	}
	if !strings.Contains(rs, "2001:db8:1::/64") {
		t.Errorf("host IPv6 network not blocked:\n%s", rs)
	}
	// IPv4 goes in the v4 set only.
	v6 := rs[strings.Index(rs, "set blocked6"):strings.Index(rs, "chain forward")]
	if strings.Contains(v6, "203.0.113.0/24") {
		t.Errorf("IPv4 host network leaked into the IPv6 set")
	}
}

// The tenant must reach the internet, and only by NAT out of the host: the
// rental subnet is masqueraded on every interface but its own bridge.
func TestRulesetMasqueradesOnlyTheRentalSubnet(t *testing.T) {
	rs := Ruleset(Bridge, "10.254.254.0/30", nil)
	if !strings.Contains(rs, `ip saddr 10.254.254.0/30 oifname != "gpurent0" masquerade`) {
		t.Errorf("no masquerade for the rental subnet:\n%s", rs)
	}
	if strings.Contains(Ruleset(Bridge, "", nil), "masquerade") {
		t.Errorf("a ruleset with no rental subnet masquerades anyway")
	}
}

func TestRulesetIsDeterministicAndDeduplicated(t *testing.T) {
	nets := []*net.IPNet{mustCIDR(t, "192.168.1.10/24"), mustCIDR(t, "198.51.100.1/24"), mustCIDR(t, "192.168.1.11/24")}
	a := Ruleset(Bridge, "", nets)
	b := Ruleset(Bridge, "", []*net.IPNet{nets[2], nets[1], nets[0]})
	if a != b {
		t.Errorf("ruleset depends on interface order")
	}
	if strings.Count(a, "192.168.1.0/24") != 1 {
		t.Errorf("duplicate host network in ruleset")
	}
}

func TestApplyVerifiesTheTableIsLive(t *testing.T) {
	r := &fakeRunner{listed: Ruleset(Bridge, "", nil)}
	g := New(r, Bridge, "10.254.254.0/30", func() ([]*net.IPNet, error) { return nil, nil })
	if err := g.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	joined := strings.Join(r.calls, "\n")
	if !strings.Contains(joined, "nft -f ") || !strings.Contains(joined, "nft list table inet gpu_rental") {
		t.Errorf("Apply did not load and read back the table: %v", r.calls)
	}
}

// Rules that were "loaded" but cannot be seen in the kernel are no fence.
func TestApplyFailsClosedWhenTheTableDoesNotReadBack(t *testing.T) {
	r := &fakeRunner{listed: "table inet gpu_rental {\n}\n"}
	g := New(r, Bridge, "10.254.254.0/30", func() ([]*net.IPNet, error) { return nil, nil })
	if err := g.Apply(); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("Apply = %v, want ErrNotVerified", err)
	}
}

func TestApplyFailsWhenNftRejectsTheRules(t *testing.T) {
	r := &fakeRunner{fail: map[string]bool{"nft -f": true}, listed: Ruleset(Bridge, "", nil)}
	g := New(r, Bridge, "10.254.254.0/30", func() ([]*net.IPNet, error) { return nil, nil })
	if err := g.Apply(); err == nil {
		t.Fatal("Apply succeeded although nft rejected the rules")
	}
}

func TestApplyFailsWhenHostNetworksCannotBeRead(t *testing.T) {
	r := &fakeRunner{listed: Ruleset(Bridge, "", nil)}
	g := New(r, Bridge, "10.254.254.0/30", func() ([]*net.IPNet, error) { return nil, errors.New("boom") })
	if err := g.Apply(); err == nil {
		t.Fatal("Apply succeeded without knowing the host's networks")
	}
	if len(r.calls) != 0 {
		t.Errorf("Apply touched nft before it knew what to block: %v", r.calls)
	}
}
