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

	"github.com/serverroom/gpu-marketplace/internal/pcidev"
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

// VMUser is the login a rental's VM accepts the renter's key for. The marketplace
// reads it from the capability report, so the ssh command it hands out names the
// user this agent actually created.
const VMUser = "root"

// MetaData is the NoCloud meta-data for a rental.
func MetaData(id string) string { return metaData(id, Hostname(id)) }

func metaData(id, host string) string {
	return "instance-id: gpu-rental-" + id + "\nlocal-hostname: " + host + "\n"
}

// Hostname is the guest's name: gpu- and the first eight characters of the
// rental id, the same name the marketplace gives the machine in the renter's ssh
// command. Every rental's first boot makes a new host key, so a name shared
// across rentals would trip ssh's changed-host-key refusal on the next one.
func Hostname(id string) string {
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	return "gpu-" + strings.ToLower(strings.TrimRight(short, "-"))
}

// NetworkConfig pins the guest's one NIC to the rental /30 with a static
// address, so there is no DHCP server to run and nothing on the host for the
// tenant to talk to beyond its gateway.
func NetworkConfig() string { return NetworkConfigFor(nil) }

// NetworkConfigFor is NetworkConfig plus, for one half of a pair, the cable:
// each verified link's port renamed cx7p<i> by its MAC, at the pair's MTU,
// with its /30 and nothing else -- no gateway, no DNS, no IPv6 link-local --
// so traffic between the two machines never touches the fenced rental
// network. The card's other ports are only named (and never waited for).
func NetworkConfigFor(pair *PairOptions) string {
	var b strings.Builder
	fmt.Fprintf(&b, `version: 2
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
	if pair == nil {
		return b.String()
	}
	for _, l := range pair.Links {
		fmt.Fprintf(&b, "  %s:\n    match:\n      macaddress: \"%s\"\n    set-name: %s\n    mtu: %d\n    addresses: [%s]\n    link-local: []\n    optional: true\n",
			l.Name, l.LocalMAC, l.Name, pair.MTU, l.CIDR)
	}
	for i, m := range pair.OtherMACs {
		name := linkName(len(pair.Links) + i)
		fmt.Fprintf(&b, "  %s:\n    match:\n      macaddress: \"%s\"\n    set-name: %s\n    link-local: []\n    optional: true\n", name, m, name)
	}
	return b.String()
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
	// markLink is what a pair VM's link check says about each cable:
	// "GPUAGENT-LINK ok <i>" or "GPUAGENT-LINK fail <i>".
	markLink = "GPUAGENT-LINK"
)

// rootLoginConf keeps root's login key-only whatever the image's sshd defaults
// are. sshd reads sshd_config.d in name order and the first value wins, so 10-
// is ahead of cloud-init's own 50-cloud-init.conf. cloud-init writes it before
// sshd starts.
const rootLoginConf = `  - path: /etc/ssh/sshd_config.d/10-gpu-rental.conf
    permissions: "0644"
    content: |
      PermitRootLogin prohibit-password
      PasswordAuthentication no
      KbdInteractiveAuthentication no
`

// SeedOptions is everything one rental's cloud-init seed is made from.
type SeedOptions struct {
	ID     string
	Pubkey string
	// Probes, when non-nil, makes this a self-test VM.
	Probes []string
	// NoGPU: the machine has no GPU, so a self-test VM does not look for one.
	NoGPU bool
	// Pair is one machine's half of a pair rental, already through ValidatePair.
	Pair *PairOptions
	// PairTest makes a pair's self-test VM also run the pair test.
	PairTest *PairTestPlan
}

// UserData is the cloud-init user-data for a rental: the renter logs in as root
// (the VM is theirs) with their key and nothing else -- no password anywhere, no
// other account. `users: []` stops cloud-init creating the image's default user,
// and with disable_root false the key goes to root without the "log in as ubuntu"
// command cloud-init otherwise puts in front of it. probes non-nil makes it a
// self-test VM.
func UserData(id, pubkey string, probes []string) (string, error) {
	return BuildUserData(SeedOptions{ID: id, Pubkey: pubkey, Probes: probes})
}

// BuildUserData is UserData for any rental: a single one or one half of a
// pair, a renter's or a self-test's.
func BuildUserData(o SeedOptions) (string, error) {
	key, err := NormalizePubkey(o.Pubkey)
	if err != nil {
		return "", err
	}
	if o.PairTest != nil && (o.Pair == nil || o.Probes == nil) {
		return "", errors.New("a pair test runs in the self-test VM of a pair")
	}
	host := Hostname(o.ID)
	if o.Pair != nil {
		host = PairHostname(o.ID, o.Pair.Node)
	}
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("hostname: " + host + "\n")
	if o.Pair != nil {
		// The whole of /etc/hosts comes from write_files: both machines' names.
		b.WriteString("manage_etc_hosts: false\n")
	}
	b.WriteString("ssh_pwauth: false\n")
	b.WriteString("disable_root: false\n")
	b.WriteString("users: []\n")
	b.WriteString("ssh_authorized_keys:\n")
	fmt.Fprintf(&b, "  - %q\n", key)
	if o.Pair != nil && o.Pair.IntraKey != nil {
		// The other VM of the pair may log in too, from the cable only.
		fmt.Fprintf(&b, "  - %q\n", `from="`+pairNetwork.String()+`" `+o.Pair.IntraKey.Public)
	}
	b.WriteString("growpart:\n  mode: auto\n  devices: [\"/\"]\n")
	b.WriteString("write_files:\n")
	b.WriteString(rootLoginConf)
	b.WriteString(sayScript)
	var runcmd []string
	if o.Probes != nil {
		script, err := selfTestScript(o.Probes, !o.NoGPU)
		if err != nil {
			return "", err
		}
		writeText(&b, "/usr/local/sbin/gpuagent-selftest", "0755", script)
		runcmd = append(runcmd, "  - [bash, /usr/local/sbin/gpuagent-selftest]\n")
	}
	if o.Pair != nil {
		writePairFiles(&b, o.ID, o.Pair)
		// A unit of its own, so the first boot finishes while it waits for the
		// other machine, and the renter can log in meanwhile.
		runcmd = append(runcmd, "  - [systemd-run, --unit=gpuagent-linkcheck, --no-block, /usr/local/sbin/gpuagent-linkcheck]\n")
	}
	if o.PairTest != nil {
		writeText(&b, "/usr/local/sbin/gpuagent-pairtest", "0755", pairTestScript(o.Pair, *o.PairTest))
		runcmd = append(runcmd, "  - [bash, /usr/local/sbin/gpuagent-pairtest]\n")
	}
	if len(runcmd) > 0 {
		b.WriteString("runcmd:\n")
		for _, c := range runcmd {
			b.WriteString(c)
		}
	}
	return b.String(), nil
}

// writeText adds a file to write_files as a literal block. Only content built
// from validated values is written this way.
func writeText(b *strings.Builder, path, perm, content string) {
	b.WriteString("  - path: " + path + "\n")
	b.WriteString("    permissions: \"" + perm + "\"\n")
	b.WriteString("    content: |\n")
	for _, line := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		b.WriteString("      " + line + "\n")
	}
}

// writeB64 adds a file to write_files base64-encoded, so whatever it holds
// can never be read as YAML. A deferred file is written at the end of the
// first boot, after cloud-init has set up root's SSH directory.
func writeB64(b *strings.Builder, path, perm string, content []byte, deferred bool) {
	b.WriteString("  - path: " + path + "\n")
	b.WriteString("    permissions: \"" + perm + "\"\n")
	b.WriteString("    encoding: b64\n")
	b.WriteString("    content: " + base64.StdEncoding.EncodeToString(content) + "\n")
	if deferred {
		b.WriteString("    defer: true\n")
	}
}

var driverPattern = regexp.MustCompile(`^[0-9]{3}(-server)?(-open)?$`)

// ValidDriver reports whether a driver branch name is safe to put in a package
// name: "580-server-open", "570", "580-server".
func ValidDriver(d string) bool { return driverPattern.MatchString(d) }

// RDMAPackages are the RDMA userspace tools every rental image carries, so a
// linked pair's VMs can use their ConnectX-7 cards (owner decision Q8: the
// tools only -- no DOCA, no NCCL).
var RDMAPackages = []string{"rdma-core", "ibverbs-utils", "perftest", "infiniband-diags", "rdmacm-utils", "ethtool"}

// GPUPackages is what the rental image needs for this machine's GPUs: the
// NVIDIA driver on the branch chosen for it (none when driver is NoDriver),
// and the firmware AMD's amdgpu and Intel's i915 and xe load -- those drivers
// are in the cloud image's kernel already, but the image ships no firmware and
// the GPUs do not come up without it. Any other make gets what the kernel has.
// vendors are PCI vendor IDs.
func GPUPackages(driver string, vendors []string) []string {
	var pkgs []string
	if driver != NoDriver {
		branch := strings.TrimSuffix(strings.TrimSuffix(driver, "-open"), "-server")
		utils := "nvidia-utils-" + branch
		if strings.Contains(driver, "-server") {
			utils += "-server"
		}
		pkgs = append(pkgs, "linux-headers-generic", "nvidia-driver-"+driver, utils)
	}
	for _, v := range vendors {
		switch v {
		case pcidev.AMD:
			pkgs = append(pkgs, "linux-firmware-amd-graphics")
		case pcidev.Intel:
			pkgs = append(pkgs, "linux-firmware-intel-graphics")
		}
	}
	return pkgs
}

// BakeUserData installs what this machine's GPUs need (GPUPackages) and the
// RDMA tools into the base image once (and the kernel's extra modules when the
// image's kernel lacks mlx5_ib), then wipes cloud-init's memory of this boot
// (so every rental's first boot is a first boot) and powers off. The host reads
// the result from the serial console.
func BakeUserData(driver string, vendors []string) (string, error) {
	if driver != NoDriver && !ValidDriver(driver) {
		return "", fmt.Errorf("invalid driver branch %q", driver)
	}
	install := "apt-get install -y --no-install-recommends"
	gpu := "true"
	if pkgs := GPUPackages(driver, vendors); len(pkgs) > 0 {
		gpu = install + " " + strings.Join(pkgs, " ") + " || ok=0"
	}
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("ssh_pwauth: false\n")
	b.WriteString("disable_root: true\n")
	b.WriteString("write_files:\n")
	b.WriteString(sayScript)
	b.WriteString("runcmd:\n")
	// The RDMA tools are an extra: a bake whose RDMA step fails still makes a
	// base image every single rental can use, and says so by not reporting
	// the extra (a linked pair then asks for a rebuild).
	fmt.Fprintf(&b, "  - [bash, -c, %q]\n", strings.Join([]string{
		"export DEBIAN_FRONTEND=noninteractive",
		"ok=1",
		"rdma=1",
		"apt-get update || ok=0",
		gpu,
		install + " " + strings.Join(RDMAPackages, " ") + " || rdma=0",
		"modinfo mlx5_ib >/dev/null 2>&1 || " + install + " linux-modules-extra-$(uname -r) || rdma=0",
		"if [ $ok = 1 ] && [ $rdma = 1 ]; then /usr/local/sbin/gpuagent-say '" + markBake + " EXTRA " + ExtraRDMA + "'; fi",
		"if [ $ok = 1 ]; then /usr/local/sbin/gpuagent-say '" + markBake + " DONE'; else /usr/local/sbin/gpuagent-say '" + markBake + " FAIL'; fi",
		"cloud-init clean --logs --machine-id",
		"poweroff",
	}, "; "))
	return b.String(), nil
}

// selfTestScript reports, one marker line at a time, what a tenant would see:
// the GPUs the guest has (on a machine that has any), whether the internet is
// reachable, and whether each probe target is BLOCKED. Only a silent drop
// counts as blocked -- `timeout` exits 124 when nothing answered, while a
// refused connection means the packet got through the fence and is reported
// as REACHED.
//
// GPUs are reported from sysfs, whatever their make: each display-class or
// accelerator-class PCI function as "PCI <vendor>:<device> <driver|none>
// <memory MB|0> <guest address> <unmapped BARs>". The VM has no display of its
// own (QEMU runs -nodefaults), so every such function is one the host passed
// through. Memory comes from amdgpu's and xe's own sysfs files where the driver
// has them. A BAR is unmapped when the kernel could not place it -- sysfs then
// shows it at address 0 with its size, or with IORESOURCE_UNSET -- and only
// the six standard memory BARs count: an unplaced expansion ROM, SR-IOV window
// or legacy I/O window (which no GPU driver needs, and which a VM with many
// cards runs out of) is normal in a VM. nvidia-smi, when the image has it, adds NVIDIA's names and
// memory as NVSMI lines.
func selfTestScript(probes []string, gpu bool) (string, error) {
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
	lines := []string{"#!/bin/bash", say + " '" + m + " BEGIN'"}
	if gpu {
		lines = append(lines,
			"udevadm settle --timeout=60 >/dev/null 2>&1",
			"for d in /sys/bus/pci/devices/*; do",
			"  case \"$(cat \"$d/class\" 2>/dev/null)\" in 0x03*|0x12*) ;; *) continue ;; esac",
			"  id=\"$(sed 's/^0x//' \"$d/vendor\"):$(sed 's/^0x//' \"$d/device\")\"",
			"  drv=none; [ -e \"$d/driver\" ] && drv=$(basename \"$(readlink -f \"$d/driver\")\")",
			"  mem=0",
			"  for f in \"$d/mem_info_vram_total\" \"$d/tile0/physical_vram_size_bytes\"; do",
			"    v=$(cat \"$f\" 2>/dev/null); case \"$v\" in ''|*[!0-9]*) continue ;; esac",
			"    mem=$(( v / 1048576 )); break",
			"  done",
			"  unmapped=0; n=0",
			"  while read -r s e fl; do",
			"    n=$((n + 1)); [ \"$n\" -le 6 ] || break",
			"    [ \"$((fl & 0x200))\" -ne 0 ] || continue",
			"    if [ \"$((fl & 0x20000000))\" -ne 0 ] || { [ \"$((s))\" -eq 0 ] && [ \"$((e))\" -ne 0 ]; }; then unmapped=$((unmapped + 1)); fi",
			"  done < \"$d/resource\"",
			"  "+say+" \""+m+" PCI $id $drv $mem ${d##*/} $unmapped\"",
			"done",
			"if command -v nvidia-smi >/dev/null 2>&1; then",
			"  if out=$(nvidia-smi --query-gpu=pci.bus_id,name,memory.total --format=csv,noheader,nounits 2>&1); then",
			"    while IFS= read -r line; do "+say+" \""+m+" NVSMI $line\"; done <<< \"$out\"",
			"  else",
			"    "+say+" \""+m+" NVSMIFAIL $(printf '%s' \"$out\" | tr '\\n' ' ' | cut -c1-300)\"",
			"  fi",
			"fi")
	}
	lines = append(lines,
		"probe() { timeout \"$2\" bash -c \"exec 3<>/dev/tcp/${1%:*}/${1##*:}\" 2>/dev/null; echo $?; }",
		"if [ \"$(probe 1.1.1.1:443 10)\" = 0 ]; then "+say+" '"+m+" INTERNET ok'; else "+say+" '"+m+" INTERNET fail'; fi",
		"for t in "+strings.Join(probes, " ")+"; do",
		"  rc=$(probe \"$t\" 6)",
		"  if [ \"$rc\" = 124 ]; then "+say+" \""+m+" BLOCKED $t\"; else "+say+" \""+m+" REACHED $t rc=$rc\"; fi",
		"done",
		say+" '"+m+" END'",
	)
	return strings.Join(lines, "\n") + "\n", nil
}
