//go:build !linux

package direct

import "net/netip"

// stableIPv6 is src: off Linux the agent cannot tell a temporary privacy
// address from a stable one, and reports the address again when it changes.
func stableIPv6(src netip.Addr) netip.Addr { return src }
