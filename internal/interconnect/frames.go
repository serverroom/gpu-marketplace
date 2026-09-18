package interconnect

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// The frames agents exchange on a ConnectX-7 cable (CONTRACT.md s3). They are
// raw Ethernet broadcasts with the IEEE "local experimental" ethertype, so no
// address is ever placed on a port and nothing on the host routes them.
const (
	EtherTypeAgent = 0x88B5
	EtherTypeLLDP  = 0x88CC

	// LinkPrefix starts an authenticated cable-check frame:
	// GPUAGENT-LINK1|<self>|<mac>|<seq>|<hex hmac_sha256(key=challenge, msg=self|mac|seq)>
	LinkPrefix = "GPUAGENT-LINK1"
	// HelloPrefix starts an unauthenticated peer announcement:
	// GPUAGENT-HELLO1|<listing_id>|<mac>
	HelloPrefix = "GPUAGENT-HELLO1"

	ethHeaderLen = 14
	ethMinFrame  = 60 // without the FCS, which the NIC adds
)

var (
	broadcastMAC = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	// stpMAC is where bridges send spanning-tree BPDUs.
	stpMAC = "01:80:c2:00:00:00"

	macPattern = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)
)

// ValidMAC reports whether s is a MAC in the lower-case colon form every MAC
// in the pair protocol uses.
func ValidMAC(s string) bool { return macPattern.MatchString(s) }

// LinkMAC is the frame authenticator: HMAC-SHA256 keyed with the challenge (its
// 32 hex characters, as text) over "self|mac|seq", in lower-case hex.
func LinkMAC(challenge, self, mac string, seq int) string {
	m := hmac.New(sha256.New, []byte(challenge))
	fmt.Fprintf(m, "%s|%s|%d", self, mac, seq)
	return hex.EncodeToString(m.Sum(nil))
}

// LinkPayload is one cable-check frame's payload.
func LinkPayload(challenge, self, mac string, seq int) string {
	return fmt.Sprintf("%s|%s|%s|%d|%s", LinkPrefix, self, mac, seq, LinkMAC(challenge, self, mac, seq))
}

// HelloPayload is one peer announcement's payload.
func HelloPayload(listing, mac string) string {
	return HelloPrefix + "|" + listing + "|" + mac
}

// BuildFrame is a broadcast Ethernet frame from src carrying payload under
// EtherTypeAgent, padded to the Ethernet minimum.
func BuildFrame(src string, payload string) ([]byte, error) {
	hw, err := net.ParseMAC(src)
	if err != nil || len(hw) != 6 {
		return nil, fmt.Errorf("bad source MAC %q", src)
	}
	n := ethHeaderLen + len(payload)
	if n < ethMinFrame {
		n = ethMinFrame
	}
	b := make([]byte, n)
	copy(b[0:6], broadcastMAC)
	copy(b[6:12], hw)
	binary.BigEndian.PutUint16(b[12:14], EtherTypeAgent)
	copy(b[ethHeaderLen:], payload)
	return b, nil
}

// Frame is a received Ethernet frame.
type Frame struct {
	Dst, Src  string // lower-case colon form
	EtherType uint16
	Payload   []byte
}

func macString(b []byte) string { return strings.ToLower(net.HardwareAddr(b).String()) }

// ParseFrame reads an Ethernet II header. A single 802.1Q tag is skipped (the
// NIC usually strips it before the socket sees the frame).
func ParseFrame(b []byte) (Frame, bool) {
	if len(b) < ethHeaderLen {
		return Frame{}, false
	}
	f := Frame{Dst: macString(b[0:6]), Src: macString(b[6:12]), EtherType: binary.BigEndian.Uint16(b[12:14])}
	rest := b[ethHeaderLen:]
	if f.EtherType == 0x8100 && len(rest) >= 4 {
		f.EtherType = binary.BigEndian.Uint16(rest[2:4])
		rest = rest[4:]
	}
	f.Payload = rest
	return f, true
}

// agentText is an agent frame's payload as text, without Ethernet padding.
func agentText(p []byte) string { return strings.TrimRight(string(p), "\x00") }

// LinkFrame is a parsed cable-check frame.
type LinkFrame struct {
	Self string
	MAC  string
	Seq  int
	Sum  string
}

// ParseLink reads a cable-check payload; ok is false for anything that is not
// exactly one.
func ParseLink(payload []byte) (LinkFrame, bool) {
	parts := strings.Split(agentText(payload), "|")
	if len(parts) != 5 || parts[0] != LinkPrefix {
		return LinkFrame{}, false
	}
	seq, err := strconv.Atoi(parts[3])
	if err != nil || seq < 0 || strconv.Itoa(seq) != parts[3] {
		return LinkFrame{}, false
	}
	lf := LinkFrame{Self: parts[1], MAC: parts[2], Seq: seq, Sum: parts[4]}
	if !listingPattern.MatchString(lf.Self) || !ValidMAC(lf.MAC) || len(lf.Sum) != 64 {
		return LinkFrame{}, false
	}
	return lf, true
}

// Authentic reports whether a cable-check frame was made with challenge.
func (lf LinkFrame) Authentic(challenge string) bool {
	want := LinkMAC(challenge, lf.Self, lf.MAC, lf.Seq)
	return hmac.Equal([]byte(want), []byte(lf.Sum))
}

// ParseHello reads a peer announcement.
func ParseHello(payload []byte) (listing, mac string, ok bool) {
	parts := strings.Split(agentText(payload), "|")
	if len(parts) != 3 || parts[0] != HelloPrefix || !listingPattern.MatchString(parts[1]) || !ValidMAC(parts[2]) {
		return "", "", false
	}
	return parts[1], parts[2], true
}

var listingPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// LLDPChassis is the chassis id an LLDP frame carries, in words: a MAC when
// the sender identifies itself by one, else its printable text (capped), else
// hex.
func LLDPChassis(payload []byte) string {
	for len(payload) >= 2 {
		hdr := binary.BigEndian.Uint16(payload[0:2])
		typ, n := int(hdr>>9), int(hdr&0x1ff)
		payload = payload[2:]
		if n > len(payload) {
			return ""
		}
		value := payload[:n]
		payload = payload[n:]
		if typ == 0 {
			return ""
		}
		if typ != 1 || n < 2 {
			continue
		}
		subtype, id := value[0], value[1:]
		if subtype == 4 && len(id) == 6 {
			return macString(id)
		}
		return printable(id, 80)
	}
	return ""
}

func printable(b []byte, max int) string {
	ok := len(b) > 0
	for _, c := range b {
		if c < 0x20 || c >= 0x7f {
			ok = false
			break
		}
	}
	s := string(b)
	if !ok {
		s = hex.EncodeToString(b)
	}
	if len(s) > max {
		s = s[:max]
	}
	return s
}
