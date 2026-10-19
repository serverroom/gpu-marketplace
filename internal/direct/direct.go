// Package direct is the machine whose own addresses are public: renters and the
// marketplace can reach it where it is, with no tunnel to one of our relays.
//
// The addresses that count are the ones the machine leaves by: the source
// addresses the kernel picks for a connection to the marketplace, one IPv4 and
// one IPv6. A public one is on the machine itself (no router translates it), so
// a connection to it arrives here. For IPv6 the agent takes a stable address,
// never a temporary privacy one, which changes every day or so.
//
// The agent reports the public addresses it has, and serves its control
// channel on them over TLS with a certificate it made itself; the marketplace
// pins that certificate's SHA-256. Its host chooses the route renters take:
// Private relay (the tunnel), Public IPv4, Public IPv6, or both. The
// marketplace uses an address only once it has reached /health there from the
// internet, and answers the route the machine uses now; the agent applies it.
// A firewall in the way, or a port another program holds, and the machine stays
// on its relay as before.
package direct

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// The ports a direct machine is reached on: the control channel (the same
// port the agent's loopback control server uses) and the rental's SSH (the
// same port the provisioner forwards on loopback for the relay).
const (
	ControlPort = 9101
	SSHPort     = 2222
)

// The routes, as the marketplace names them.
const (
	Relay = "relay"
	IPv4  = "ipv4"
	IPv6  = "ipv6"
	Dual  = "dual"
)

// Labels are the routes in the host's words.
var Labels = map[string]string{
	Relay: "Private relay",
	IPv4:  "Public IPv4",
	IPv6:  "Public IPv6",
	Dual:  "Public IPv4 + IPv6",
}

// Report is what the agent tells the marketplace: the public addresses it
// has ("" for a family it has none of), the ports, and its certificate.
type Report struct {
	IPv4        string `json:"ipv4"`
	IPv6        string `json:"ipv6"`
	ControlPort int    `json:"control_port"`
	SSHPort     int    `json:"ssh_port"`
	CertSHA256  string `json:"cert_sha256"`
}

// Addrs are the report's addresses, IPv4 first.
func (r Report) Addrs() []string { return nonEmpty(r.IPv4, r.IPv6) }

// Route is the route the marketplace accepted, kept in direct.json: the
// addresses renters and the marketplace reach the machine at.
type Route struct {
	Connection  string `json:"connection"`
	IPv4        string `json:"ipv4,omitempty"`
	IPv6        string `json:"ipv6,omitempty"`
	ControlPort int    `json:"control_port"`
	SSHPort     int    `json:"ssh_port"`
}

// Addrs are the route's addresses, IPv4 first.
func (r Route) Addrs() []string { return nonEmpty(r.IPv4, r.IPv6) }

// ControlAddrs is where the control channel listens on the public addresses.
func (r Route) ControlAddrs() []string { return withPort(r.Addrs(), r.ControlPort) }

// SSHAddrs is where the rental's SSH is forwarded on the public addresses.
func (r Route) SSHAddrs() []string { return withPort(r.Addrs(), r.SSHPort) }

// Describe is the route in one line for the host.
func (r Route) Describe() string {
	return fmt.Sprintf("%s (%s, renters connect on port %d)", Labels[r.Connection], joinAddrs(r.Addrs()), r.SSHPort)
}

func nonEmpty(values ...string) []string {
	var out []string
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func withPort(addrs []string, port int) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, net.JoinHostPort(a, strconv.Itoa(port)))
	}
	return out
}

func joinAddrs(addrs []string) string {
	s := ""
	for i, a := range addrs {
		if i > 0 {
			s += " and "
		}
		s += a
	}
	return s
}

// special4 are the IPv4 ranges netip does not already call private, loopback,
// link-local, multicast or unspecified, and that the internet cannot reach a
// machine at (RFC 6890).
var special4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT, in front of many homes
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// special6 are the IPv6 ranges that are global unicast to netip but are no
// machine's own public address: documentation, IPv4 carried in IPv6 (6to4,
// Teredo, NAT64) and the discard prefix.
var special6 = []netip.Prefix{
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
}

// IsPublic reports whether a is a public IPv4 or IPv6 address.
func IsPublic(a netip.Addr) bool {
	if a.Is4In6() {
		return false
	}
	if !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	ranges := special6
	if a.Is4() {
		ranges = special4
	}
	for _, p := range ranges {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// SourceAddr is the address this machine leaves by for host over network
// ("udp4" or "udp6"): the source the kernel picks for a connection there.
// Nothing is sent: a UDP "dial" only asks the routing table.
func SourceAddr(network, host string) (netip.Addr, error) {
	conn, err := net.Dial(network, net.JoinHostPort(host, "443"))
	if err != nil {
		return netip.Addr{}, err
	}
	defer conn.Close()
	ap, err := netip.ParseAddrPort(conn.LocalAddr().String())
	if err != nil {
		return netip.Addr{}, err
	}
	return ap.Addr().WithZone(""), nil
}

// Detect is the public addresses this machine leaves by for host: its IPv4
// and a stable IPv6, "" for a family it has no public address in.
func Detect(host string) (ipv4, ipv6 string) {
	if a, err := SourceAddr("udp4", host); err == nil && a.Unmap().Is4() && IsPublic(a.Unmap()) {
		ipv4 = a.Unmap().String()
	}
	if a, err := SourceAddr("udp6", host); err == nil && a.Is6() && IsPublic(a) {
		if stable := stableIPv6(a); stable.IsValid() {
			ipv6 = stable.String()
		}
	}
	return ipv4, ipv6
}

// HostOf is the host name in a URL such as the marketplace's capability URL.
func HostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// CanListen reports whether every port can be bound on every address now; the
// error names the first one that cannot (another program holds it).
func CanListen(addrs []string, ports ...int) error {
	for _, a := range addrs {
		for _, port := range ports {
			ln, err := net.Listen("tcp", net.JoinHostPort(a, strconv.Itoa(port)))
			if err != nil {
				return fmt.Errorf("port %d on %s is in use by another program: %w", port, a, err)
			}
			ln.Close()
		}
	}
	return nil
}

// Cert is the agent's own TLS certificate for the public control channel,
// made once and kept in dir: a self-signed ECDSA P-256 certificate that names
// no host, because the marketplace checks nothing about it but its SHA-256.
// The key never leaves the machine. Returns the certificate and that SHA-256.
func Cert(dir string) (tls.Certificate, string, error) {
	certPath, keyPath := filepath.Join(dir, "direct_tls.crt"), filepath.Join(dir, "direct_tls.key")
	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil && len(cert.Certificate) > 0 {
		return cert, Fingerprint(cert.Certificate[0]), nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "gpu-agent"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(30, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		return tls.Certificate{}, "", err
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return cert, Fingerprint(der), nil
}

// Fingerprint is the SHA-256 of a certificate's DER bytes, in lowercase hex.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// TLSConfig is how the public control channel speaks TLS.
func TLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
}

// ServeHealth answers /health at every addr over TLS, the way the control
// channel does, until stop is called: what the marketplace reaches while it
// decides on the addresses reported at register, before the agent's service
// is running.
func ServeHealth(addrs []string, cert tls.Certificate) (stop func(), err error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	// The listeners are closed by hand as well as through their servers: a
	// server closed before its Serve has started does not know its listener.
	var listeners []net.Listener
	var servers []*http.Server
	stop = func() {
		for _, s := range servers {
			s.Close()
		}
		for _, l := range listeners {
			l.Close()
		}
	}
	for _, addr := range addrs {
		ln, err := tls.Listen("tcp", addr, TLSConfig(cert))
		if err != nil {
			stop()
			return nil, err
		}
		listeners = append(listeners, ln)
	}
	for _, ln := range listeners {
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, srv)
		go srv.Serve(ln)
	}
	return stop, nil
}

// Path is where the accepted route is kept.
func Path(dir string) string { return filepath.Join(dir, "direct.json") }

// Save keeps the route the marketplace accepted.
func Save(dir string, r Route) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(r, "", "  ")
	return os.WriteFile(Path(dir), data, 0600)
}

// Load is the accepted route, or nil when the machine is on its relay.
func Load(dir string) (*Route, error) {
	data, err := os.ReadFile(Path(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Route
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	for _, a := range r.Addrs() {
		addr, perr := netip.ParseAddr(a)
		if perr != nil || !IsPublic(addr) {
			return nil, fmt.Errorf("%s holds an address that is not public: %q", Path(dir), a)
		}
	}
	if len(r.Addrs()) == 0 || r.ControlPort == 0 || r.SSHPort == 0 || Labels[r.Connection] == "" || r.Connection == Relay {
		return nil, fmt.Errorf("%s does not hold a public route", Path(dir))
	}
	return &r, nil
}

// Remove forgets the accepted route: the machine is back on its relay.
func Remove(dir string) error {
	err := os.Remove(Path(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
