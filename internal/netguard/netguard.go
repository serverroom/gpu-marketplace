// Package netguard fences a rental microVM off from the host's own networks.
//
// A provider's machine usually sits on a home or office LAN next to a NAS, a
// router admin page and other people's laptops. A tenant renting the GPU must
// reach the internet and nothing else: not the LAN, not the host itself, not
// link-local metadata, not multicast discovery. This package builds that fence
// as one nftables table keyed on the rental bridge, loads it, and then READS IT
// BACK — the provisioner boots nothing unless the rules are verifiably in the
// kernel. A fence that "was applied" but cannot be seen is treated as absent.
//
// The blocked set is the private/special ranges every LAN uses PLUS every
// network actually configured on the host's interfaces, so a LAN numbered from
// public space (some offices, some ISPs' bridged CPE) is fenced off too.
package netguard

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
)

// Runner executes host commands. vmrt.Host satisfies it. The ruleset is fed to
// nft over stdin (RunInput), not a temp file, so the fence loads the same way
// whether nft runs on this machine or inside a Linux environment the agent
// owns (WSL on Windows, a VM on macOS), where a host temp path would not exist.
type Runner interface {
	Run(name string, args ...string) error
	RunInput(stdin []byte, name string, args ...string) error
	Output(name string, args ...string) (string, error)
}

const (
	// Table is the one nftables table the agent owns. Removing it removes every
	// rule the agent ever added; nothing is written anywhere else.
	Table = "gpu_rental"
	// Bridge is the only interface a rental microVM is attached to.
	Bridge = "gpurent0"
)

// Ranges no tenant may reach, whatever the host's own addressing is.
var blockedV4 = []string{
	"0.0.0.0/8",      // "this network"
	"10.0.0.0/8",     // RFC 1918
	"100.64.0.0/10",  // carrier-grade NAT (and Tailscale)
	"127.0.0.0/8",    // loopback
	"169.254.0.0/16", // link-local, cloud metadata
	"172.16.0.0/12",  // RFC 1918
	"192.0.0.0/24",   // IETF protocol assignments
	"192.168.0.0/16", // RFC 1918
	"198.18.0.0/15",  // benchmarking
	"224.0.0.0/4",    // multicast (mDNS, SSDP discovery)
	"240.0.0.0/4",    // reserved + broadcast
}

var blockedV6 = []string{
	"::1/128",   // loopback
	"fc00::/7",  // unique local
	"fe80::/10", // link-local
	"ff00::/8",  // multicast
}

// Guard loads and removes the rental fence.
type Guard struct {
	runner   Runner
	bridge   string
	subnet   string
	hostNets func() ([]*net.IPNet, error)
}

// New builds a guard for bridge. subnet is the rental network, which is
// masqueraded out of the host so the tenant can reach the internet. hostNets
// reports the networks configured on the host; production passes HostNetworks.
func New(r Runner, bridge, subnet string, hostNets func() ([]*net.IPNet, error)) *Guard {
	return &Guard{runner: r, bridge: bridge, subnet: subnet, hostNets: hostNets}
}

// HostNetworks returns every network configured on this host's interfaces.
func HostNetworks() ([]*net.IPNet, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var nets []*net.IPNet
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		nets = append(nets, &net.IPNet{IP: ipn.IP.Mask(ipn.Mask), Mask: ipn.Mask})
	}
	return nets, nil
}

// Ruleset renders the nftables table for bridge with hostNets added to the
// blocked sets.
//
// forward: a tenant may start connections to anything not in the blocked sets;
// nothing outside may start a connection to the tenant (the renter's SSH comes
// from the agent on the host, through the relay tunnel, not from the LAN).
// input: from the bridge the host answers DHCP and IPv6 neighbour discovery and
// replies on connections it opened itself; every other packet a tenant sends to
// the host — its SSH, its web UI, anything listening on it — is dropped.
// postrouting: the rental subnet leaving by any other interface is masqueraded,
// so what is left — the internet — is reachable at all.
func Ruleset(bridge, subnet string, hostNets []*net.IPNet) string {
	var host4, host6 []string
	for _, n := range hostNets {
		if n == nil {
			continue
		}
		if n.IP.To4() != nil {
			ones, _ := n.Mask.Size()
			if ones > 32 { // a 16-byte mask on a v4 address
				ones -= 96
			}
			host4 = append(host4, fmt.Sprintf("%s/%d", n.IP.To4().String(), ones))
		} else {
			host6 = append(host6, n.String())
		}
	}
	// Host networks are sorted so the rendered table is the same on every
	// start; the fixed ranges keep their annotated order in front.
	sort.Strings(host4)
	sort.Strings(host6)
	v4 := dedupe(append(append([]string(nil), blockedV4...), host4...))
	v6 := dedupe(append(append([]string(nil), blockedV6...), host6...))

	q := `"` + bridge + `"`
	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s {\n", Table)
	fmt.Fprintf(&b, "\tset blocked4 {\n\t\ttype ipv4_addr\n\t\tflags interval\n\t\tauto-merge\n\t\telements = { %s }\n\t}\n", strings.Join(v4, ", "))
	fmt.Fprintf(&b, "\tset blocked6 {\n\t\ttype ipv6_addr\n\t\tflags interval\n\t\tauto-merge\n\t\telements = { %s }\n\t}\n", strings.Join(v6, ", "))
	fmt.Fprintf(&b, "\tchain forward {\n\t\ttype filter hook forward priority -10; policy accept;\n")
	fmt.Fprintf(&b, "\t\tiifname %s ip daddr @blocked4 drop\n", q)
	fmt.Fprintf(&b, "\t\tiifname %s ip6 daddr @blocked6 drop\n", q)
	fmt.Fprintf(&b, "\t\toifname %s ct state established,related accept\n", q)
	fmt.Fprintf(&b, "\t\toifname %s drop\n", q)
	fmt.Fprintf(&b, "\t}\n")
	fmt.Fprintf(&b, "\tchain input {\n\t\ttype filter hook input priority -10; policy accept;\n")
	fmt.Fprintf(&b, "\t\tiifname %s ct state established,related accept\n", q)
	fmt.Fprintf(&b, "\t\tiifname %s udp dport 67 accept\n", q)
	fmt.Fprintf(&b, "\t\tiifname %s icmpv6 type { nd-neighbor-solicit, nd-neighbor-advert, nd-router-solicit } accept\n", q)
	fmt.Fprintf(&b, "\t\tiifname %s drop\n", q)
	fmt.Fprintf(&b, "\t}\n")
	if subnet != "" {
		fmt.Fprintf(&b, "\tchain postrouting {\n\t\ttype nat hook postrouting priority srcnat; policy accept;\n")
		fmt.Fprintf(&b, "\t\tip saddr %s oifname != %s masquerade\n", subnet, q)
		fmt.Fprintf(&b, "\t}\n")
	}
	fmt.Fprintf(&b, "}\n")
	return b.String()
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Loaded reports whether an `nft list table` listing carries the fence for
// bridge: both drop rules into the blocked sets, and the input drop.
func Loaded(listing, bridge string) bool {
	q := `iifname "` + bridge + `"`
	return strings.Contains(listing, q+" ip daddr @blocked4 drop") &&
		strings.Contains(listing, q+" ip6 daddr @blocked6 drop") &&
		strings.Contains(listing, q+" drop")
}

// ErrNotVerified is returned when the rules were loaded but cannot be read back.
var ErrNotVerified = errors.New("the rental firewall did not verify after loading")

// Apply replaces the agent's table with a fresh fence and verifies it is live.
func (g *Guard) Apply() error {
	nets, err := g.hostNets()
	if err != nil {
		return fmt.Errorf("read host networks: %w", err)
	}
	_ = g.runner.Run("nft", "delete", "table", "inet", Table) // absent is fine
	if err := g.runner.RunInput([]byte(Ruleset(g.bridge, g.subnet, nets)), "nft", "-f", "-"); err != nil {
		return fmt.Errorf("load firewall rules: %w", err)
	}
	out, err := g.runner.Output("nft", "list", "table", "inet", Table)
	if err != nil || !Loaded(out, g.bridge) {
		return ErrNotVerified
	}
	return nil
}

// Remove deletes the agent's table and with it every rule the agent added.
func (g *Guard) Remove() error {
	return g.runner.Run("nft", "delete", "table", "inet", Table)
}
