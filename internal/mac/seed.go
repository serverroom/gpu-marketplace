package mac

import (
	"fmt"
	"strings"
)

// SeedUserData is the cloud-init user-data for the agent's VM: root logs in
// with the agent's key and nothing else (no password, no other user), and the
// root filesystem grows to fill the overlay disk on first boot. The container
// stack is installed afterwards by the setup running apt-get inside the VM
// (the same InstallContainerPackages a Linux host runs), not baked here.
func SeedUserData(agentPubkey string) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("ssh_pwauth: false\n")
	b.WriteString("disable_root: false\n")
	b.WriteString("users: []\n")
	b.WriteString("ssh_authorized_keys:\n")
	fmt.Fprintf(&b, "  - %q\n", strings.TrimSpace(agentPubkey))
	b.WriteString("growpart:\n  mode: auto\n  devices: [\"/\"]\n")
	// Key-only root login whatever the image default is (first file wins).
	b.WriteString("write_files:\n")
	b.WriteString("  - path: /etc/ssh/sshd_config.d/10-gpu-agent.conf\n")
	b.WriteString("    permissions: \"0644\"\n")
	b.WriteString("    content: |\n")
	b.WriteString("      PermitRootLogin prohibit-password\n")
	b.WriteString("      PasswordAuthentication no\n")
	return b.String()
}

// SeedMetaData is the cloud-init meta-data: a stable instance id (so cloud-init
// does not re-run its first-boot steps on every boot) and the hostname.
func SeedMetaData() string {
	return "instance-id: " + VMName + "\nlocal-hostname: gpu-agent\n"
}

// parseSWVers reads the major macOS version from `sw_vers -productVersion`
// output ("15.6.1" -> 15, "12" -> 12).
func parseSWVers(out string) int {
	f := strings.SplitN(strings.TrimSpace(out), ".", 2)
	n := 0
	for _, r := range f[0] {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// hvfSupported reads `sysctl -n kern.hv_support` ("1" is supported).
func hvfSupported(out string) bool { return strings.TrimSpace(out) == "1" }
