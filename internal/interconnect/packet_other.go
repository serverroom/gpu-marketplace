//go:build !linux

package interconnect

import "errors"

// OpenPacket is Linux-only: the pair runtime, like the rental runtime, runs on
// a Linux KVM host.
func OpenPacket(netdev string) (PacketConn, error) {
	return nil, errors.New("raw Ethernet frames need Linux (AF_PACKET)")
}
