package vmrt

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
)

// A linked-pair rental is two ordinary rentals, one on each of two machines
// cabled to each other over their ConnectX-7 ports, plus: the whole ConnectX
// card of each machine handed to its VM (after the GPU, right before boot),
// the cable configured inside the guest (one /30 per verified link, MTU 9000,
// no gateway), the two VMs naming each other in /etc/hosts, a per-rental key
// they log in to each other with, and a guest-side check that each link
// carries full-size frames, reported on the serial console.

// PairMTU is the MTU a pair's links run at.
const PairMTU = 9000

// PairNetwork is where a pair's link addresses come from.
var pairNetwork = mustCIDR("10.200.0.0/16")

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// GuestLink is one cable as the guest configures it.
type GuestLink struct {
	LocalMAC string `json:"local_mac"`
	CIDR     string `json:"cidr"`
	PeerIP   string `json:"peer_ip"`
	// Name is the interface name the guest gives it: cx7p<i>.
	Name string `json:"name"`
}

// IntraKey is the key a pair's two VMs log in to each other with. It exists
// in memory and in the rental's own directory only, never in the state file.
type IntraKey struct {
	PrivateOpenSSH string
	Public         string
}

// PairOptions is one machine's half of a pair rental.
type PairOptions struct {
	Node         string      `json:"node"`
	PeerHostname string      `json:"peer_hostname"`
	Links        []GuestLink `json:"links"`
	MTU          int         `json:"mtu"`
	// Functions are the ConnectX PCI functions handed to the VM; the whole
	// card goes, so a guest can never share a socket-direct NIC with the host.
	Functions []string `json:"functions"`
	// OtherMACs are the card's ports that carry no verified link: the guest
	// names them, and configures nothing on them.
	OtherMACs []string  `json:"other_macs,omitempty"`
	IntraKey  *IntraKey `json:"-"`
}

var (
	pairHostPattern = regexp.MustCompile(`^gpu-[0-9a-f]{8}-[ab]$`)
	pairMACPattern  = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)
	bdfPattern      = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)
)

// PairHostname is a pair VM's name: the rental's name and its node.
func PairHostname(id, node string) string { return Hostname(id) + "-" + node }

func otherNode(node string) string {
	if node == "a" {
		return "b"
	}
	return "a"
}

// linkName is the guest's name for link i.
func linkName(i int) string { return fmt.Sprintf("cx7p%d", i) }

// ValidatePair checks every field of a pair rental before any of it can reach
// the guest's cloud-init YAML or the host: the node, both names, each link's
// MAC and addresses (a /30 inside 10.200.0.0/16, the two ends of it), the MTU,
// the ConnectX functions, and the intra-pair key. It names the links cx7p<i>.
func ValidatePair(id string, p *PairOptions) error {
	if p == nil {
		return errors.New("no pair")
	}
	if p.Node != "a" && p.Node != "b" {
		return fmt.Errorf("node must be a or b, not %q", p.Node)
	}
	self := PairHostname(id, p.Node)
	if !pairHostPattern.MatchString(self) {
		return fmt.Errorf("rental id %q does not give a pair host name (gpu-<8 hex>-%s)", id, p.Node)
	}
	if !pairHostPattern.MatchString(p.PeerHostname) {
		return fmt.Errorf("peer_hostname %q is not gpu-<8 hex>-a|b", p.PeerHostname)
	}
	if want := PairHostname(id, otherNode(p.Node)); p.PeerHostname != want {
		return fmt.Errorf("peer_hostname %q is not the other half of this rental (%s)", p.PeerHostname, want)
	}
	if p.MTU < 1500 || p.MTU > PairMTU {
		return fmt.Errorf("mtu must be 1500..%d, not %d", PairMTU, p.MTU)
	}
	if len(p.Links) < 1 || len(p.Links) > 2 {
		return fmt.Errorf("a pair has 1 or 2 links, not %d", len(p.Links))
	}
	macs := map[string]bool{}
	nets := map[string]bool{}
	for i := range p.Links {
		l := &p.Links[i]
		l.LocalMAC = strings.ToLower(strings.TrimSpace(l.LocalMAC))
		if !pairMACPattern.MatchString(l.LocalMAC) {
			return fmt.Errorf("link %d: local_mac %q is not a MAC", i, l.LocalMAC)
		}
		if macs[l.LocalMAC] {
			return fmt.Errorf("link %d: local_mac %s is used twice", i, l.LocalMAC)
		}
		macs[l.LocalMAC] = true
		ip, n, err := net.ParseCIDR(l.CIDR)
		if err != nil || ip.To4() == nil {
			return fmt.Errorf("link %d: cidr %q is not an IPv4 address with a prefix", i, l.CIDR)
		}
		if ones, _ := n.Mask.Size(); ones != 30 {
			return fmt.Errorf("link %d: cidr %s must be a /30", i, l.CIDR)
		}
		if !pairNetwork.Contains(ip) {
			return fmt.Errorf("link %d: cidr %s is outside %s", i, l.CIDR, pairNetwork)
		}
		peer := net.ParseIP(l.PeerIP)
		if peer == nil || peer.To4() == nil || !n.Contains(peer) {
			return fmt.Errorf("link %d: peer_ip %q is not in %s", i, l.PeerIP, n)
		}
		host := ip.To4()[3] & 3
		peerHost := peer.To4()[3] & 3
		if host == 0 || host == 3 || peerHost == 0 || peerHost == 3 || host == peerHost {
			return fmt.Errorf("link %d: %s and %s must be the two ends of %s", i, ip, peer, n)
		}
		if nets[n.String()] {
			return fmt.Errorf("link %d: %s is used by two links", i, n)
		}
		nets[n.String()] = true
		l.CIDR = fmt.Sprintf("%s/30", ip.To4())
		l.PeerIP = peer.To4().String()
		l.Name = linkName(i)
	}
	sort.Strings(p.OtherMACs)
	for _, m := range p.OtherMACs {
		if !pairMACPattern.MatchString(m) || macs[m] {
			return fmt.Errorf("the ConnectX port %q cannot be named", m)
		}
		macs[m] = true
	}
	if len(p.Functions) == 0 {
		return errors.New("no ConnectX function to hand to the VM")
	}
	for _, f := range p.Functions {
		if !bdfPattern.MatchString(f) {
			return fmt.Errorf("%q is not a PCI address", f)
		}
	}
	if k := p.IntraKey; k != nil {
		pub, err := NormalizePubkey(k.Public)
		if err != nil || !strings.HasPrefix(pub, "ssh-ed25519 ") {
			return errors.New("intra_key.public must be an ssh-ed25519 public key")
		}
		canonical, err := checkOpenSSHPrivateKey(k.PrivateOpenSSH, pub)
		if err != nil {
			return fmt.Errorf("intra_key.private_openssh: %w", err)
		}
		k.Public, k.PrivateOpenSSH = pub, canonical
	}
	return nil
}

const (
	opensshBegin = "-----BEGIN OPENSSH PRIVATE KEY-----"
	opensshEnd   = "-----END OPENSSH PRIVATE KEY-----"
	opensshMagic = "openssh-key-v1\x00"
)

// checkOpenSSHPrivateKey accepts exactly one unencrypted ed25519 key in
// OpenSSH's format whose public half is pub, and returns it re-encoded
// canonically, so nothing but the key itself can ride along into the guest.
func checkOpenSSHPrivateKey(pemText, pub string) (string, error) {
	if len(pemText) > 8192 {
		return "", errors.New("too long")
	}
	text := strings.TrimSpace(strings.ReplaceAll(pemText, "\r\n", "\n"))
	if !strings.HasPrefix(text, opensshBegin+"\n") || !strings.HasSuffix(text, "\n"+opensshEnd) {
		return "", errors.New("not an OpenSSH private key")
	}
	body := strings.TrimSuffix(strings.TrimPrefix(text, opensshBegin+"\n"), "\n"+opensshEnd)
	var b64 strings.Builder
	for _, line := range strings.Split(body, "\n") {
		for _, c := range line {
			if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=') {
				return "", errors.New("not an OpenSSH private key")
			}
		}
		b64.WriteString(line)
	}
	raw, err := base64.StdEncoding.DecodeString(b64.String())
	if err != nil {
		return "", errors.New("not an OpenSSH private key")
	}
	r := &sshReader{b: raw}
	if !bytes.HasPrefix(raw, []byte(opensshMagic)) {
		return "", errors.New("not an OpenSSH private key")
	}
	r.b = raw[len(opensshMagic):]
	cipher, kdf, _ := r.str(), r.str(), r.str()
	n := r.u32()
	if r.err != nil || string(cipher) != "none" || string(kdf) != "none" {
		return "", errors.New("the key must be unencrypted")
	}
	if n != 1 {
		return "", errors.New("exactly one key")
	}
	pubBlob := r.str()
	priv := r.str()
	wantBlob, _ := base64.StdEncoding.DecodeString(strings.Fields(pub)[1])
	if r.err != nil || len(r.b) != 0 || !bytes.Equal(pubBlob, wantBlob) {
		return "", errors.New("the key does not match intra_key.public")
	}
	pr := &sshReader{b: priv}
	c1, c2 := pr.u32(), pr.u32()
	typ, pk, sk := pr.str(), pr.str(), pr.str()
	_ = pr.str() // comment
	if pr.err != nil || c1 != c2 || string(typ) != "ssh-ed25519" || len(pk) != 32 || len(sk) != 64 ||
		!bytes.Equal(sk[32:], pk) || !bytes.Equal(append(sshString("ssh-ed25519"), sshString(string(pk))...), wantBlob) {
		return "", errors.New("not a valid ed25519 key")
	}
	for i, c := range pr.b {
		if int(c) != i+1 {
			return "", errors.New("not a valid ed25519 key")
		}
	}
	enc := base64.StdEncoding.EncodeToString(raw)
	var out strings.Builder
	out.WriteString(opensshBegin + "\n")
	for len(enc) > 70 {
		out.WriteString(enc[:70] + "\n")
		enc = enc[70:]
	}
	out.WriteString(enc + "\n" + opensshEnd + "\n")
	return out.String(), nil
}

type sshReader struct {
	b   []byte
	err error
}

func (r *sshReader) u32() uint32 {
	if r.err != nil || len(r.b) < 4 {
		r.err = errors.New("short")
		return 0
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *sshReader) str() []byte {
	n := r.u32()
	if r.err != nil || uint64(n) > uint64(len(r.b)) {
		r.err = errors.New("short")
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}
