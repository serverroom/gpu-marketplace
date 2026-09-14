package vmrt

import (
	"fmt"
	"net"
	"strings"

	"github.com/serverroom/gpu-marketplace/internal/netguard"
)

// NetState is what SetupNetwork changed, so TeardownNetwork undoes exactly that.
type NetState struct {
	Bridge          bool   `json:"bridge"`
	TapDev          bool   `json:"tap"`
	PreviousForward string `json:"previous_forward"`
	// IptablesChain is the iptables chain the forward-accept rules went into
	// (DOCKER-USER or FORWARD), or "" when none were needed.
	IptablesChain string `json:"iptables_chain"`
}

var forwardRules = [][]string{
	{"-i", netguard.Bridge, "-j", "ACCEPT"},
	{"-o", netguard.Bridge, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
}

// SetupNetwork builds the rental's /30: the bridge with the host's end on it,
// the tap the VM attaches to, IP forwarding (remembering what it was), and --
// where Docker or a firewall has set iptables' FORWARD policy to DROP, as DGX
// OS does -- two accept rules so the tenant's own traffic can leave. Those
// accepts cannot open anything the fence closes: an nftables drop is final no
// matter which table accepted the packet first.
func SetupNetwork(h Host) (NetState, error) {
	var st NetState
	_ = h.Run("ip", "link", "del", Tap)
	_ = h.Run("ip", "link", "del", netguard.Bridge)

	if err := h.Run("ip", "link", "add", netguard.Bridge, "type", "bridge"); err != nil {
		return st, fmt.Errorf("create bridge: %w", err)
	}
	st.Bridge = true
	steps := [][]string{
		{"addr", "add", fmt.Sprintf("%s/%d", HostIP, PrefixLen), "dev", netguard.Bridge},
		{"link", "set", netguard.Bridge, "up"},
	}
	for _, s := range steps {
		if err := h.Run("ip", s...); err != nil {
			return st, fmt.Errorf("configure bridge: %w", err)
		}
	}
	if err := h.Run("ip", "tuntap", "add", "dev", Tap, "mode", "tap"); err != nil {
		return st, fmt.Errorf("create tap: %w", err)
	}
	st.TapDev = true
	for _, s := range [][]string{{"link", "set", Tap, "master", netguard.Bridge}, {"link", "set", Tap, "up"}} {
		if err := h.Run("ip", s...); err != nil {
			return st, fmt.Errorf("attach tap: %w", err)
		}
	}

	prev, err := h.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return st, fmt.Errorf("read ip_forward: %w", err)
	}
	st.PreviousForward = strings.TrimSpace(string(prev))
	if st.PreviousForward != "1" {
		if err := h.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644); err != nil {
			return st, fmt.Errorf("enable forwarding: %w", err)
		}
	}

	if _, err := h.LookPath("iptables"); err == nil {
		chain := ""
		if _, err := h.Output("iptables", "-S", "DOCKER-USER"); err == nil {
			chain = "DOCKER-USER"
		} else if out, err := h.Output("iptables", "-S", "FORWARD"); err == nil && strings.Contains(out, "-P FORWARD DROP") {
			chain = "FORWARD"
		}
		if chain != "" {
			for _, rule := range forwardRules {
				if err := h.Run("iptables", append([]string{"-I", chain}, rule...)...); err != nil {
					return st, fmt.Errorf("allow rental forwarding: %w", err)
				}
			}
			st.IptablesChain = chain
		}
	}
	return st, nil
}

// TeardownNetwork removes what SetupNetwork added and restores forwarding.
func TeardownNetwork(h Host, st NetState) {
	if st.IptablesChain != "" {
		for _, rule := range forwardRules {
			_ = h.Run("iptables", append([]string{"-D", st.IptablesChain}, rule...)...)
		}
	}
	if st.PreviousForward != "" && st.PreviousForward != "1" {
		_ = h.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte(st.PreviousForward), 0644)
	}
	if st.TapDev {
		_ = h.Run("ip", "link", "del", Tap)
	}
	if st.Bridge {
		_ = h.Run("ip", "link", "del", netguard.Bridge)
	}
}

// DefaultProbes are the targets a self-test checks the tenant CANNOT reach: the
// host's end of the rental network, and the host's own address and gateway on
// its real network. Read from `ip route get 1.1.1.1`.
func DefaultProbes(h Host) []string {
	probes := []string{net.JoinHostPort(HostIP, "22")}
	out, err := h.Output("ip", "route", "get", "1.1.1.1")
	if err != nil {
		return probes
	}
	fields := strings.Fields(out)
	for i := 0; i+1 < len(fields); i++ {
		ip := net.ParseIP(fields[i+1])
		if ip == nil || ip.To4() == nil {
			continue
		}
		switch fields[i] {
		case "via":
			probes = append(probes, net.JoinHostPort(ip.String(), "80"))
		case "src":
			probes = append(probes, net.JoinHostPort(ip.String(), "22"))
		}
	}
	return probes
}
