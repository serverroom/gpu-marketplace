package direct

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
)

// FirewallRule is the name of the Windows Firewall rule the agent keeps.
const FirewallRule = "GPU Marketplace Agent (direct connections)"

var allowOnce sync.Once
var allowErr error

// AllowInbound lets renters and the marketplace reach this agent's own
// program on the direct-route ports (ControlPort, SSHPort) through Windows
// Firewall, which drops them by default: without it a machine with a public
// address is only ever reached through the relay. The rule names the agent's
// executable, so nothing else on the machine is opened. Replaced once per
// process (the binary's path can change with an update); later calls answer
// what that did.
func AllowInbound() error {
	allowOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			allowErr = err
			return
		}
		ports := strconv.Itoa(ControlPort) + "," + strconv.Itoa(SSHPort)
		_ = netsh("advfirewall", "firewall", "delete", "rule", "name="+FirewallRule)
		allowErr = netsh("advfirewall", "firewall", "add", "rule", "name="+FirewallRule, "dir=in", "action=allow",
			"program="+exe, "protocol=TCP", "localport="+ports, "profile=any", "enable=yes")
	})
	return allowErr
}

// RemoveFirewallRule takes the rule away again (gpu-agent remove).
func RemoveFirewallRule() error {
	return netsh("advfirewall", "firewall", "delete", "rule", "name="+FirewallRule)
}

func netsh(args ...string) error {
	cmd := exec.Command("netsh.exe", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("netsh %v: %v: %s", args[2], err, out)
	}
	return nil
}
