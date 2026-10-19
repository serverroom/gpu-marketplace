//go:build linux

package direct

import (
	"bufio"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// Address flags in /proc/net/if_inet6 (linux/if_addr.h).
const (
	ifaTemporary  = 0x01
	ifaDADFailed  = 0x08
	ifaDeprecated = 0x20
	ifaTentative  = 0x40
)

// ifInet6 is the file listing the machine's IPv6 addresses; a variable so
// tests read their own.
var ifInet6 = "/proc/net/if_inet6"

// stableIPv6 is src when it is not a temporary privacy address, else a stable
// public address on the same interface, the one the temporary ones are made
// from, which renters can be given; the zero Addr when there is none.
func stableIPv6(src netip.Addr) netip.Addr {
	f, err := os.Open(ifInet6)
	if err != nil {
		return src
	}
	defer f.Close()
	type entry struct {
		addr  netip.Addr
		iface string
		flags uint64
	}
	var entries []entry
	srcAt := -1
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 6 || len(fields[0]) != 32 {
			continue
		}
		var raw [16]byte
		ok := true
		for i := 0; i < 16; i++ {
			b, perr := strconv.ParseUint(fields[0][2*i:2*i+2], 16, 8)
			if perr != nil {
				ok = false
				break
			}
			raw[i] = byte(b)
		}
		flags, ferr := strconv.ParseUint(fields[4], 16, 32)
		if !ok || ferr != nil {
			continue
		}
		if a := netip.AddrFrom16(raw); a == src {
			srcAt = len(entries)
		}
		entries = append(entries, entry{netip.AddrFrom16(raw), fields[5], flags})
	}
	if srcAt < 0 || entries[srcAt].flags&ifaTemporary == 0 {
		return src
	}
	for _, e := range entries {
		if e.iface == entries[srcAt].iface && e.flags&(ifaTemporary|ifaDADFailed|ifaDeprecated|ifaTentative) == 0 && IsPublic(e.addr) {
			return e.addr
		}
	}
	return netip.Addr{}
}
