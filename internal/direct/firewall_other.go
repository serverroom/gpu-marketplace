//go:build !windows

package direct

// AllowInbound is a no-op off Windows: the agent does not manage a Linux or
// macOS host's own firewall (the host's guide says which ports to open).
func AllowInbound() error { return nil }

// RemoveFirewallRule is a no-op off Windows.
func RemoveFirewallRule() error { return nil }
