package vmrt

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

var pubkeyTypes = map[string]bool{
	"ssh-ed25519":                        true,
	"ssh-rsa":                            true,
	"ecdsa-sha2-nistp256":                true,
	"ecdsa-sha2-nistp384":                true,
	"ecdsa-sha2-nistp521":                true,
	"sk-ssh-ed25519@openssh.com":         true,
	"sk-ecdsa-sha2-nistp256@openssh.com": true,
}

// NormalizePubkey returns a renter's public key as "type base64", or an error.
// The comment is dropped and nothing but the two fields survives, because the
// key is written into the guest's cloud-init YAML and must not be able to carry
// anything else in with it.
func NormalizePubkey(key string) (string, error) {
	if strings.ContainsAny(key, "\r\n") {
		return "", errors.New("a public key is one line")
	}
	fields := strings.Fields(key)
	if len(fields) < 2 || !pubkeyTypes[fields[0]] {
		return "", errors.New("not an SSH public key")
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil || len(blob) < 16 {
		return "", errors.New("not an SSH public key")
	}
	// The blob names its own type; it must agree with the prefix.
	if n := binary.BigEndian.Uint32(blob); int(n)+4 > len(blob) || string(blob[4:4+n]) != fields[0] {
		return "", errors.New("SSH public key type does not match its data")
	}
	return fields[0] + " " + fields[1], nil
}

// ThrowawayPubkey makes a key nobody holds the private half of, for a self-test
// VM that must accept no login at all.
func ThrowawayPubkey() (string, error) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	blob := sshString("ssh-ed25519")
	blob = append(blob, sshString(string(pub))...)
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob), nil
}

func sshString(s string) []byte {
	b := make([]byte, 4+len(s))
	binary.BigEndian.PutUint32(b, uint32(len(s)))
	copy(b[4:], s)
	return b
}

// MetaData is the NoCloud meta-data for a rental.
func MetaData(id string) string {
	return "instance-id: gpu-rental-" + id + "\nlocal-hostname: gpu-rental\n"
}

// NetworkConfig pins the guest's one NIC to the rental /30 with a static
// address, so there is no DHCP server to run and nothing on the host for the
// tenant to talk to beyond its gateway.
func NetworkConfig() string {
	return fmt.Sprintf(`version: 2
ethernets:
  rental:
    match:
      macaddress: "%s"
    addresses: [%s/%d]
    routes:
      - to: default
        via: %s
    nameservers:
      addresses: [1.1.1.1, 8.8.8.8]
`, GuestMAC, GuestIP, PrefixLen, HostIP)
}

// sayScript writes one line to whichever serial console the guest has (ttyS0
// on x86, ttyAMA0 on Arm). The serial log is the only channel the host reads
// from a guest, and it is one-way.
const sayScript = `  - path: /usr/local/sbin/gpuagent-say
    permissions: "0755"
    content: |
      #!/bin/sh
      for t in /dev/ttyS0 /dev/ttyAMA0; do
        [ -c "$t" ] && printf '%s\n' "$*" > "$t" 2>/dev/null
      done
      exit 0
`

// Serial markers the host looks for.
const (
	markSelfTest = "GPUAGENT-SELFTEST"
	markBake     = "GPUAGENT-BAKE"
)

// UserData is the cloud-init user-data for a rental: one user, `renter`, who can
// only log in with the renter's key, with sudo inside the VM (it is theirs), no
// password anywhere, root login off. probes non-nil makes it a self-test VM.
func UserData(pubkey string, probes []string) (string, error) {
	key, err := NormalizePubkey(pubkey)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("hostname: gpu-rental\n")
	b.WriteString("ssh_pwauth: false\n")
	b.WriteString("disable_root: true\n")
	b.WriteString("users:\n")
	b.WriteString("  - name: renter\n")
	b.WriteString("    groups: [sudo]\n")
	b.WriteString("    shell: /bin/bash\n")
	b.WriteString("    sudo: \"ALL=(ALL) NOPASSWD:ALL\"\n")
	b.WriteString("    lock_passwd: true\n")
	b.WriteString("    ssh_authorized_keys:\n")
	fmt.Fprintf(&b, "      - %q\n", key)
	b.WriteString("growpart:\n  mode: auto\n  devices: [\"/\"]\n")
	b.WriteString("write_files:\n")
	b.WriteString(sayScript)
	if probes != nil {
		script, err := selfTestScript(probes)
		if err != nil {
			return "", err
		}
		b.WriteString("  - path: /usr/local/sbin/gpuagent-selftest\n")
		b.WriteString("    permissions: \"0755\"\n")
		b.WriteString("    content: |\n")
		for _, line := range strings.Split(strings.TrimRight(script, "\n"), "\n") {
			b.WriteString("      " + line + "\n")
		}
		b.WriteString("runcmd:\n")
		b.WriteString("  - [bash, /usr/local/sbin/gpuagent-selftest]\n")
	}
	return b.String(), nil
}

var driverPattern = regexp.MustCompile(`^[0-9]{3}(-server)?(-open)?$`)

// ValidDriver reports whether a driver branch name is safe to put in a package
// name: "580-server-open", "570", "580-server".
func ValidDriver(d string) bool { return driverPattern.MatchString(d) }

// BakeUserData installs the NVIDIA driver into the base image once, then wipes
// cloud-init's memory of this boot (so every rental's first boot is a first
// boot) and powers off. The host reads the result from the serial console.
func BakeUserData(driver string) (string, error) {
	if !ValidDriver(driver) {
		return "", fmt.Errorf("invalid driver branch %q", driver)
	}
	branch := strings.TrimSuffix(strings.TrimSuffix(driver, "-open"), "-server")
	utils := "nvidia-utils-" + branch
	if strings.Contains(driver, "-server") {
		utils += "-server"
	}
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("ssh_pwauth: false\n")
	b.WriteString("disable_root: true\n")
	b.WriteString("write_files:\n")
	b.WriteString(sayScript)
	b.WriteString("runcmd:\n")
	fmt.Fprintf(&b, "  - [bash, -c, %q]\n", strings.Join([]string{
		"export DEBIAN_FRONTEND=noninteractive",
		"if apt-get update && apt-get install -y --no-install-recommends linux-headers-generic nvidia-driver-" + driver + " " + utils + "; then /usr/local/sbin/gpuagent-say '" + markBake + " DONE'; else /usr/local/sbin/gpuagent-say '" + markBake + " FAIL'; fi",
		"cloud-init clean --logs --machine-id",
		"poweroff",
	}, "; "))
	return b.String(), nil
}

// selfTestScript reports, one marker line at a time, what a tenant would see:
// the GPUs the guest has, whether the internet is reachable, and whether each
// probe target is BLOCKED. Only a silent drop counts as blocked -- `timeout`
// exits 124 when nothing answered, while a refused connection means the packet
// got through the fence and is reported as REACHED.
func selfTestScript(probes []string) (string, error) {
	for _, p := range probes {
		host, port, err := net.SplitHostPort(p)
		if err != nil || net.ParseIP(host) == nil {
			return "", fmt.Errorf("bad probe target %q", p)
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("bad probe target %q", p)
		}
	}
	say := "/usr/local/sbin/gpuagent-say"
	m := markSelfTest
	return strings.Join([]string{
		"#!/bin/bash",
		say + " '" + m + " BEGIN'",
		"if out=$(nvidia-smi --query-gpu=pci.bus_id,name,memory.total --format=csv,noheader 2>&1); then",
		"  while IFS= read -r line; do " + say + " \"" + m + " GPU $line\"; done <<< \"$out\"",
		"else",
		"  " + say + " \"" + m + " GPUFAIL $(printf '%s' \"$out\" | tr '\\n' ' ' | cut -c1-300)\"",
		"fi",
		"probe() { timeout \"$2\" bash -c \"exec 3<>/dev/tcp/${1%:*}/${1##*:}\" 2>/dev/null; echo $?; }",
		"if [ \"$(probe 1.1.1.1:443 10)\" = 0 ]; then " + say + " '" + m + " INTERNET ok'; else " + say + " '" + m + " INTERNET fail'; fi",
		"for t in " + strings.Join(probes, " ") + "; do",
		"  rc=$(probe \"$t\" 6)",
		"  if [ \"$rc\" = 124 ]; then " + say + " \"" + m + " BLOCKED $t\"; else " + say + " \"" + m + " REACHED $t rc=$rc\"; fi",
		"done",
		say + " '" + m + " END'",
	}, "\n") + "\n", nil
}
