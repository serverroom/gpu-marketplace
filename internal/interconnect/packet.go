package interconnect

import (
	"errors"
	"time"
)

// PacketConn sends and receives whole Ethernet frames on one interface. On
// Linux it is an AF_PACKET raw socket bound to the interface
// (packet_linux.go); tests connect two in-process ends with a fake wire, so
// every rule of the cable check runs without a ConnectX card.
type PacketConn interface {
	// WriteFrame sends one complete Ethernet frame.
	WriteFrame(frame []byte) error
	// ReadFrame waits until deadline for one frame. outgoing is true for a
	// frame this machine sent (a raw socket sees those too). A deadline that
	// passes returns ErrTimeout.
	ReadFrame(buf []byte, deadline time.Time) (n int, outgoing bool, err error)
	Close() error
}

// Opener opens a PacketConn on a network interface.
type Opener func(netdev string) (PacketConn, error)

// ErrTimeout is ReadFrame's answer when its deadline passes.
var ErrTimeout = errors.New("no frame before the deadline")
