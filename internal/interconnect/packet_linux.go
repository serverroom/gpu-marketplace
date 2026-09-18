//go:build linux

package interconnect

import (
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// afPacket is a raw socket on one interface. It sees every frame on the wire
// (ETH_P_ALL, and promiscuous for as long as the socket is open -- a socket
// membership, which the kernel drops with the socket, so nothing about the
// interface outlives the check).
type afPacket struct {
	fd      int
	ifindex int
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// OpenPacket opens a raw socket bound to netdev.
func OpenPacket(netdev string) (PacketConn, error) {
	ifi, err := net.InterfaceByName(netdev)
	if err != nil {
		return nil, err
	}
	proto := htons(unix.ETH_P_ALL)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, int(proto))
	if err != nil {
		return nil, fmt.Errorf("raw socket on %s: %w", netdev, err)
	}
	c := &afPacket{fd: fd, ifindex: ifi.Index}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: proto, Ifindex: ifi.Index}); err != nil {
		c.Close()
		return nil, fmt.Errorf("bind raw socket to %s: %w", netdev, err)
	}
	mreq := unix.PacketMreq{Ifindex: int32(ifi.Index), Type: unix.PACKET_MR_PROMISC}
	if err := unix.SetsockoptPacketMreq(fd, unix.SOL_PACKET, unix.PACKET_ADD_MEMBERSHIP, &mreq); err != nil {
		c.Close()
		return nil, fmt.Errorf("listen to everything on %s: %w", netdev, err)
	}
	// Frames queued between socket() and bind() may be from any interface.
	buf := make([]byte, 65536)
	for {
		if _, _, err := unix.Recvfrom(fd, buf, unix.MSG_DONTWAIT); err != nil {
			break
		}
	}
	return c, nil
}

func (c *afPacket) WriteFrame(frame []byte) error {
	if len(frame) < ethHeaderLen {
		return errors.New("frame too short")
	}
	var addr [8]byte
	copy(addr[:], frame[0:6])
	return unix.Sendto(c.fd, frame, 0, &unix.SockaddrLinklayer{
		Protocol: htons(uint16(frame[12])<<8 | uint16(frame[13])),
		Ifindex:  c.ifindex,
		Halen:    6,
		Addr:     addr,
	})
}

func (c *afPacket) ReadFrame(buf []byte, deadline time.Time) (int, bool, error) {
	for {
		wait := time.Until(deadline)
		if wait <= 0 {
			return 0, false, ErrTimeout
		}
		ms := int(wait / time.Millisecond)
		if ms < 1 {
			ms = 1
		}
		fds := []unix.PollFd{{Fd: int32(c.fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, ms)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, false, err
		}
		if n == 0 {
			continue
		}
		size, from, err := unix.Recvfrom(c.fd, buf, unix.MSG_DONTWAIT)
		if err == unix.EAGAIN || err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, false, err
		}
		outgoing := false
		if sll, ok := from.(*unix.SockaddrLinklayer); ok {
			outgoing = sll.Pkttype == unix.PACKET_OUTGOING
		}
		return size, outgoing, nil
	}
}

func (c *afPacket) Close() error { return unix.Close(c.fd) }
