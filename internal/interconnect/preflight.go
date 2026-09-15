package interconnect

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// Checks a pair preflight problem can come from (DESIGN.md s4.2).
const (
	CheckPlatform = "platform"
	CheckP1       = "P1" // a ConnectX card, whole, on the host driver
	CheckP2       = "P2" // no address, no route
	CheckP3       = "P3" // no bond/bridge/team/VLAN
	CheckP4       = "P4" // not the management path
	CheckP5       = "P5" // not NetworkManager's, not in netplan
	CheckP6       = "P6" // clean IOMMU groups
	CheckP7       = "P7" // no RDMA users
	CheckP8       = "P8" // tools
	CheckP9       = "P9" // RDMA in the base image
	CheckIdentity = "identity"
	CheckP10      = "P10" // a passing pair test boot
)

// Problem is one reason this machine cannot be half of a linked pair.
type Problem struct {
	Check string
	Text  string
}

// Options is what the pair preflight needs besides the machine.
type Options struct {
	GOOS, Arch string
	Spec       vmrt.Spec // DataDir, GoldenImage, GPUs
	Version    string
	GPUModels  []string
	// RelayAddrs are the IP addresses of the relay this agent tunnels to (P4).
	// Called only on a machine that has ConnectX ports.
	RelayAddrs func() []string
}

// Report is what the pair preflight found.
type Report struct {
	Identity control.Identity
	NICs     []NIC
	// Eligible names the ports raw frames may be sent on: on the host driver,
	// and nothing the host uses them for.
	Eligible map[string]bool
	Problems []Problem
	PairTest *vmrt.PairTestResult
}

// Ready: nothing stands between this machine and a linked pair.
func (r Report) Ready() bool { return len(r.Problems) == 0 }

// Reasons are the problems in words.
func (r Report) Reasons() []string {
	out := make([]string, 0, len(r.Problems))
	for _, p := range r.Problems {
		out = append(out, p.Text)
	}
	return out
}

// Ports are every ConnectX port on the host.
func (r Report) Ports() []Port {
	var out []Port
	for _, n := range r.NICs {
		out = append(out, n.Ports...)
	}
	return out
}

// EligiblePorts are the ports raw frames may be sent on.
func (r Report) EligiblePorts() []Port {
	var out []Port
	for _, p := range r.Ports() {
		if r.Eligible[p.Netdev] {
			out = append(out, p)
		}
	}
	return out
}

// PortMACs are the MACs of every ConnectX port.
func (r Report) PortMACs() []string {
	var out []string
	for _, p := range r.Ports() {
		if p.MAC != "" {
			out = append(out, p.MAC)
		}
	}
	return out
}

// Functions are every PCI function of every ConnectX card: what a pair rental
// hands to its VM.
func (r Report) Functions() []string {
	var out []string
	for _, n := range r.NICs {
		out = append(out, n.BDFs()...)
	}
	sort.Strings(out)
	return out
}

// Capability is the report as the control plane receives it.
func (r Report) Capability(peers []control.Peer) (*control.Identity, *control.Interconnect) {
	id := r.Identity
	ic := &control.Interconnect{Supported: true, Ready: r.Ready(), Reasons: r.Reasons(), Peers: peers}
	for _, p := range r.Ports() {
		ic.Ports = append(ic.Ports, control.NICPort{BDF: p.BDF, Netdev: p.Netdev, MAC: p.MAC, Carrier: p.Carrier,
			SpeedMbps: p.SpeedMbps, MTU: p.MTU, Driver: p.Driver, Firmware: p.Firmware, Serial: p.Serial})
	}
	if t := r.PairTest; t != nil {
		ic.PairTest = &control.PairTest{Passed: t.Passed, AgentVersion: t.AgentVersion, LocalMACs: t.LocalMACs,
			PeerMACs: t.PeerMACs, RDMAGbps: t.RDMAGbps, At: t.At}
	}
	ic.Bound()
	return &id, ic
}

// PairTools are the host commands the pair runtime needs (P8), and the
// packages that provide them.
var (
	PairTools    = []string{"ip", "ethtool", "devlink", "mstconfig"}
	PairPackages = []string{"iproute2", "ethtool", "mstflint"}
)

// Preflight checks, without changing anything, whether this machine can be
// half of a linked pair. It never affects whether the machine can host a
// single rental. Every failing check is listed.
func Preflight(h vmrt.Host, o Options) Report {
	rep := Report{Identity: ReadIdentity(h, o.GOOS, o.Arch, o.GPUModels), Eligible: map[string]bool{}}
	add := func(check, format string, a ...interface{}) {
		rep.Problems = append(rep.Problems, Problem{Check: check, Text: fmt.Sprintf(format, a...)})
	}
	identity := func() {
		if !rep.Identity.ConfirmedDGXSpark {
			add(CheckIdentity, "%s%s", identityReasonPrefix, rep.Identity.Reason)
		}
	}
	if o.GOOS != "linux" {
		add(CheckPlatform, "linked pairs run on two Linux DGX Sparks, and this machine runs %s", o.GOOS)
		identity()
		return rep
	}

	rep.NICs = Discover(h)
	if len(rep.NICs) == 0 {
		add(CheckP1, "no NVIDIA ConnectX network card was found (PCI vendor %s); a linked pair runs over the DGX Spark's ConnectX-7 ports", VendorMellanox)
		identity()
		return rep
	}
	for _, n := range rep.NICs {
		for _, p := range n.Problems {
			add(CheckP1, "%s", p)
		}
		for _, f := range n.Functions {
			switch {
			case f.VF:
			case f.Driver == "vfio-pci":
				add(CheckP1, "ConnectX function %s is on vfio-pci, not mlx5_core: a rental (or the leftover of one) holds it, or something else bound it; a linked pair needs the whole card back on its driver", f.BDF)
			case len(f.Netdevs) == 0:
				add(CheckP1, "ConnectX function %s has no network interface on this machine (driver %q); load mlx5_core", f.BDF, f.Driver)
			}
		}
	}

	holders := vmrt.RDMAHolders(h)
	if len(holders) > 0 {
		add(CheckP7, "the ConnectX card's RDMA devices are in use on this machine by %s; a linked pair hands the whole card to the rental, so stop them", strings.Join(holders, ", "))
	}

	mgmt := managementDevices(h, o.RelayAddrs)
	nm := networkManagerStates(h)
	plans := netplanFiles(h)
	for _, port := range rep.Ports() {
		before := len(rep.Problems)
		portProblems(h, port, mgmt, nm, plans, add)
		if len(rep.Problems) == before && len(holders) == 0 && port.Driver != "vfio-pci" {
			rep.Eligible[port.Netdev] = true
		}
	}

	gpuSlots := map[string]bool{}
	for _, g := range o.Spec.GPUs {
		gpuSlots[vmrt.SlotOf(g)] = true
	}
	for _, n := range rep.NICs {
		own := map[string]bool{}
		for _, b := range n.BDFs() {
			own[b] = true
		}
		foreign, err := vmrt.ForeignGroupMembers(h, n.BDFs(), func(b string) bool { return own[b] || gpuSlots[vmrt.SlotOf(b)] })
		if err != nil {
			add(CheckP6, "%v", err)
		}
		for _, f := range foreign {
			add(CheckP6, "PCI device %s shares an IOMMU group with the ConnectX card %s, so handing the card to a rental would take it from this machine too; enable ACS or move the device", f, n.Serial)
		}
	}

	var missing []string
	for _, t := range PairTools {
		if _, err := h.LookPath(t); err != nil {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		add(CheckP8, "the pair runtime's tools are missing (%s); install them with: apt-get install -y %s", strings.Join(missing, ", "), strings.Join(PairPackages, " "))
	}
	if !h.Exists(o.Spec.GoldenImage) && !h.Exists(o.Spec.GoldenImage+".json") {
		add(CheckP9, "the rental base image has not been built; run 'sudo gpu-agent runtime prepare' (agent v0.2.0 or later bakes in the RDMA tools a linked pair needs)")
	} else if !vmrt.GoldenHasExtra(h, o.Spec, vmrt.ExtraRDMA) {
		add(CheckP9, "the rental base image was built without the RDMA tools a linked pair needs; rebuild it with 'sudo gpu-agent runtime prepare' (agent v0.2.0 or later bakes them in)")
	}
	identity()

	res, err := vmrt.LoadPairTest(h, o.Spec.DataDir)
	if err != nil {
		if len(rep.Problems) == 0 {
			add(CheckP10, "its last pair test boot could not be read (%v); %s", err, vmrt.PairTestCommand)
		}
		return rep
	}
	rep.PairTest = res
	// Like the single self-test, the pair test only means anything once the
	// checks above pass.
	if len(rep.Problems) == 0 {
		if problem := vmrt.PairTestProblem(res, o.Version, rep.PortMACs()); problem != "" {
			add(CheckP10, "%s", problem)
		}
	}
	return rep
}

// BlockingPairTest are the problems that stop a pair test boot: all of them
// except the pair test itself and the machine's identity (a test boot on a
// staging machine that is not a Spark still proves the runtime).
func (r Report) BlockingPairTest() []Problem {
	var out []Problem
	for _, p := range r.Problems {
		if p.Check != CheckP10 && p.Check != CheckIdentity {
			out = append(out, p)
		}
	}
	return out
}

func portProblems(h vmrt.Host, p Port, mgmt map[string]string, nm map[string]string, plans []netplanFile, add func(string, string, ...interface{})) {
	nd := p.Netdev
	// P2: no address, no route. An IPv6 link-local address is the kernel's own
	// doing on any up interface and carries no route off the cable.
	if out, err := h.Output("ip", "-j", "addr", "show", "dev", nd); err != nil {
		add(CheckP2, "the addresses on %s could not be read (%v)", nd, err)
	} else {
		var ifs []struct {
			AddrInfo []struct {
				Family    string `json:"family"`
				Local     string `json:"local"`
				PrefixLen int    `json:"prefixlen"`
				Scope     string `json:"scope"`
			} `json:"addr_info"`
		}
		if json.Unmarshal([]byte(out), &ifs) != nil {
			add(CheckP2, "the addresses on %s could not be read", nd)
		}
		for _, i := range ifs {
			for _, a := range i.AddrInfo {
				if a.Family == "inet" || (a.Family == "inet6" && a.Scope != "link") {
					add(CheckP2, "%s has the address %s/%d on this machine; a ConnectX-7 port in a linked pair must carry no host address (remove it, and whatever configured it)", nd, a.Local, a.PrefixLen)
				}
			}
		}
	}
	for _, fam := range [][]string{{"-j"}, {"-6", "-j"}} {
		args := append(append([]string(nil), fam...), "route", "show", "dev", nd)
		out, err := h.Output("ip", args...)
		if err != nil {
			add(CheckP2, "the routes on %s could not be read (%v)", nd, err)
			continue
		}
		var routes []struct {
			Dst string `json:"dst"`
		}
		if json.Unmarshal([]byte(out), &routes) != nil {
			add(CheckP2, "the routes on %s could not be read", nd)
			continue
		}
		for _, r := range routes {
			if r.Dst == "fe80::/64" {
				continue
			}
			add(CheckP2, "%s carries the route %s on this machine; a ConnectX-7 port in a linked pair must carry no host route", nd, r.Dst)
		}
	}

	// P3: not part of anything else on the host.
	if master, err := h.Readlink("/sys/class/net/" + nd + "/master"); err == nil {
		add(CheckP3, "%s is a member of %s (bond, bridge or team) on this machine; take it out", nd, path.Base(master))
	} else if h.Exists("/sys/class/net/" + nd + "/master") {
		add(CheckP3, "%s is a member of a bond, bridge or team on this machine; take it out", nd)
	}
	if uppers, _ := h.Glob("/sys/class/net/" + nd + "/upper_*"); len(uppers) > 0 {
		var names []string
		for _, u := range uppers {
			names = append(names, strings.TrimPrefix(path.Base(u), "upper_"))
		}
		add(CheckP3, "%s has %s (VLAN, macvlan or ipvlan) on top of it on this machine; remove them", nd, strings.Join(names, ", "))
	}

	// P4: the host's own way out never goes to a rental.
	if what, ok := mgmt[nd]; ok {
		add(CheckP4, "%s is this machine's route to %s; the management network must stay on another interface", nd, what)
	}

	// P5: nothing on the host will put an address on it later.
	if state, ok := nm[nd]; ok && state != "unmanaged" {
		add(CheckP5, "NetworkManager manages %s (state %s), and would configure it when the cable comes up; make it unmanaged: add 'unmanaged-devices=interface-name:%s' under [keyfile] in /etc/NetworkManager/conf.d/99-gpu-pair.conf and restart NetworkManager", nd, state, nd)
	}
	for _, f := range plans {
		if f.names(nd, p.MAC) {
			add(CheckP5, "%s is configured in %s; remove it from netplan and run 'netplan apply'", nd, f.path)
		}
	}
}

// managementDevices maps the interface the host reaches the internet and its
// relay through to what it is the route to.
func managementDevices(h vmrt.Host, relayAddrs func() []string) map[string]string {
	out := map[string]string{}
	targets := []string{"1.1.1.1"}
	if relayAddrs != nil {
		targets = append(targets, relayAddrs()...)
	}
	for i, t := range targets {
		o, err := h.Output("ip", "route", "get", t)
		if err != nil {
			continue
		}
		f := strings.Fields(o)
		for j := 0; j+1 < len(f); j++ {
			if f[j] == "dev" {
				what := "the internet"
				if i > 0 {
					what = "the relay (" + t + ")"
				}
				if _, seen := out[f[j+1]]; !seen {
					out[f[j+1]] = what
				}
				break
			}
		}
	}
	return out
}

// networkManagerStates is `nmcli device` as interface -> state, or empty when
// NetworkManager is absent or not running.
func networkManagerStates(h vmrt.Host) map[string]string {
	out := map[string]string{}
	if _, err := h.LookPath("nmcli"); err != nil {
		return out
	}
	o, err := h.Output("nmcli", "-t", "-f", "DEVICE,STATE", "device")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(o, "\n") {
		dev, state, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && dev != "" {
			out[strings.ReplaceAll(dev, `\:`, ":")] = strings.TrimSpace(state)
		}
	}
	return out
}

type netplanFile struct {
	path string
	raw  string
	doc  *netplanDoc
}

type netplanDoc struct {
	Network struct {
		Ethernets map[string]struct {
			Match struct {
				Name       string `yaml:"name"`
				MACAddress string `yaml:"macaddress"`
			} `yaml:"match"`
			SetName string `yaml:"set-name"`
		} `yaml:"ethernets"`
	} `yaml:"network"`
}

func netplanFiles(h vmrt.Host) []netplanFile {
	var paths []string
	for _, pat := range []string{"/etc/netplan/*.yaml", "/etc/netplan/*.yml"} {
		m, _ := h.Glob(pat)
		paths = append(paths, m...)
	}
	sort.Strings(paths)
	var out []netplanFile
	for _, p := range paths {
		data, err := h.ReadFile(p)
		if err != nil {
			continue
		}
		f := netplanFile{path: p, raw: string(data)}
		var doc netplanDoc
		if yaml.Unmarshal(data, &doc) == nil {
			f.doc = &doc
		}
		out = append(out, f)
	}
	return out
}

// names reports whether a netplan file configures the interface: by id, by a
// match on its name (globs allowed) or MAC, or by renaming something to it. A
// file netplan's schema cannot be read from counts if it mentions either.
func (f netplanFile) names(netdev, mac string) bool {
	if f.doc == nil {
		return strings.Contains(f.raw, netdev) || (mac != "" && strings.Contains(strings.ToLower(f.raw), mac))
	}
	for id, e := range f.doc.Network.Ethernets {
		if e.SetName == netdev {
			return true
		}
		if e.Match.Name == "" && e.Match.MACAddress == "" && id == netdev {
			return true
		}
		if e.Match.Name != "" {
			if ok, _ := path.Match(e.Match.Name, netdev); ok {
				return true
			}
		}
		if e.Match.MACAddress != "" && strings.EqualFold(e.Match.MACAddress, mac) {
			return true
		}
	}
	return false
}
