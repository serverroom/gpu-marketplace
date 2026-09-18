package vmrt

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// A pair rental hands the whole ConnectX card to its VM, and a guest that owns
// a card can change it: its MACs, its firmware (NVIDIA-signed only, but any
// version), its persistent configuration. So the card's state is recorded
// before the rental and compared after it; anything different leaves the
// machine dirty, like an unverified wipe.

// NICReturnTimeout bounds how long a teardown waits for the card's interfaces
// to come back once it is on its driver again.
var (
	NICReturnTimeout = 60 * time.Second
	nicPoll          = 2 * time.Second
)

// NICBaseline is a ConnectX card as it was before a rental.
type NICBaseline struct {
	// MACs are every interface the card's functions had, MAC -> interface.
	MACs map[string]string `json:"macs"`
	// Firmware is the firmware version per function (ethtool -i).
	Firmware map[string]string `json:"firmware"`
	// NVConfig is the fingerprint of each function's persistent configuration
	// (mstconfig q).
	NVConfig map[string]string `json:"nv_config"`
}

// RecordNICBaseline records the card's functions as they are now. Every
// function must be on the host driver with an interface, or there is nothing
// to compare the card with afterwards.
func RecordNICBaseline(h Host, functions []string) (*NICBaseline, error) {
	b := &NICBaseline{MACs: map[string]string{}, Firmware: map[string]string{}, NVConfig: map[string]string{}}
	for _, bdf := range functions {
		netdevs := NetdevsOf(h, bdf)
		if len(netdevs) == 0 {
			return nil, fmt.Errorf("ConnectX function %s has no network interface on this machine (driver %q)", bdf, driverOf(h, bdf))
		}
		for _, nd := range netdevs {
			mac := NetdevMAC(h, nd)
			if mac == "" {
				return nil, fmt.Errorf("the MAC of %s cannot be read", nd)
			}
			b.MACs[mac] = nd
		}
		fw, err := NICFirmware(h, netdevs[0])
		if err != nil {
			return nil, fmt.Errorf("firmware of %s: %w", bdf, err)
		}
		b.Firmware[bdf] = fw
		hash, err := NVConfigHash(h, bdf)
		if err != nil {
			return nil, fmt.Errorf("persistent configuration of %s: %w", bdf, err)
		}
		b.NVConfig[bdf] = hash
	}
	return b, nil
}

// VerifyNICBaseline checks, once the card is back on its driver, that it is
// the card that went: every interface back with its MAC (waiting up to
// NICReturnTimeout for the driver), the same firmware, the same persistent
// configuration, and no address on any port -- nothing on the host (a
// NetworkManager profile, say) may configure a port the moment it returns.
func VerifyNICBaseline(h Host, b *NICBaseline) (bool, []string) {
	if b == nil {
		return false, []string{"there is no record of the ConnectX card from before the rental, so it cannot be verified"}
	}
	var functions []string
	for bdf := range b.Firmware {
		functions = append(functions, bdf)
	}
	sort.Strings(functions)
	var macs map[string]string
	for waited := time.Duration(0); ; waited += nicPoll {
		macs = FunctionMACs(h, functions)
		missing := false
		for mac := range b.MACs {
			if _, ok := macs[mac]; !ok {
				missing = true
			}
		}
		if !missing || waited >= NICReturnTimeout {
			break
		}
		h.Sleep(nicPoll)
	}
	var detail []string
	var want []string
	for mac := range b.MACs {
		want = append(want, mac)
	}
	sort.Strings(want)
	for _, mac := range want {
		if _, ok := macs[mac]; !ok {
			detail = append(detail, fmt.Sprintf("the ConnectX port %s (%s) did not come back with its MAC after the rental", b.MACs[mac], mac))
		}
	}
	for _, bdf := range functions {
		netdevs := NetdevsOf(h, bdf)
		if len(netdevs) == 0 {
			detail = append(detail, fmt.Sprintf("the ConnectX function %s has no network interface after the rental", bdf))
			continue
		}
		if fw, err := NICFirmware(h, netdevs[0]); err != nil || fw != b.Firmware[bdf] {
			detail = append(detail, fmt.Sprintf("the firmware of the ConnectX function %s changed during the rental (%s, now %q)", bdf, b.Firmware[bdf], fw))
		}
		if hash, err := NVConfigHash(h, bdf); err != nil || hash != b.NVConfig[bdf] {
			detail = append(detail, fmt.Sprintf("the persistent configuration of the ConnectX function %s changed during the rental (or cannot be read)", bdf))
		}
		for _, nd := range netdevs {
			if addrs := hostAddresses(h, nd); len(addrs) > 0 {
				detail = append(detail, fmt.Sprintf("%s got the address %s on this machine as soon as it came back; whatever configures it must not", nd, strings.Join(addrs, ", ")))
			}
		}
	}
	return len(detail) == 0, detail
}

// hostAddresses are the addresses on an interface other than IPv6 link-local.
// An interface whose addresses cannot be read counts as having one.
func hostAddresses(h Host, netdev string) []string {
	out, err := h.Output("ip", "-j", "addr", "show", "dev", netdev)
	if err != nil {
		return []string{"(unreadable)"}
	}
	var ifs []struct {
		AddrInfo []struct {
			Family string `json:"family"`
			Local  string `json:"local"`
			Scope  string `json:"scope"`
		} `json:"addr_info"`
	}
	if json.Unmarshal([]byte(out), &ifs) != nil {
		return []string{"(unreadable)"}
	}
	var addrs []string
	for _, i := range ifs {
		for _, a := range i.AddrInfo {
			if a.Family == "inet" || (a.Family == "inet6" && a.Scope != "link") {
				addrs = append(addrs, a.Local)
			}
		}
	}
	return addrs
}

// takeNICs hands the ConnectX card to the VM: nothing on the host may be using
// its RDMA devices, the card's state is recorded (and saved) before anything
// changes, and then every function of it -- with anything else in its IOMMU
// groups that is not already going with the GPU -- moves to vfio-pci.
func (rt *Runtime) takeNICs(st *State, r *Rental, functions []string, save func() error) error {
	if len(functions) == 0 {
		return fmt.Errorf("this machine has no ConnectX card to hand to a pair rental")
	}
	if holders := RDMAHolders(rt.h); len(holders) > 0 {
		return fmt.Errorf("the ConnectX card's RDMA devices are in use on this machine by %s; stop them first", strings.Join(holders, ", "))
	}
	have := map[string]bool{}
	for _, f := range r.VFIO {
		have[f] = true
	}
	var take []string
	for _, f := range functions {
		members, err := GroupMembers(rt.h, f)
		if err != nil {
			return err
		}
		for _, m := range members {
			if !have[m] {
				have[m] = true
				take = append(take, m)
			}
		}
	}
	sort.Strings(take)
	base, err := RecordNICBaseline(rt.h, functions)
	if err != nil {
		return fmt.Errorf("record the ConnectX card before the rental: %w", err)
	}
	st.NICBaseline = base
	if err := save(); err != nil {
		return err
	}
	st.NICDevices, err = BindVFIO(rt.h, take)
	if serr := save(); err == nil {
		err = serr
	}
	if err != nil {
		return fmt.Errorf("hand the ConnectX card to the microVM: %w", err)
	}
	r.VFIO = append(r.VFIO, take...)
	return nil
}

// guestLogTail is how much of a guest's serial log the host reads for markers.
const guestLogTail = 64 << 10

// ParseGuestLinks counts a pair VM's link-check markers: for each link index
// below links, the last "GPUAGENT-LINK ok|fail <i>" said.
func ParseGuestLinks(log string, links int) (ok, fail int) {
	for _, v := range GuestLinkStates(log, links) {
		if v == "ok" {
			ok++
		} else {
			fail++
		}
	}
	return ok, fail
}

// GuestLinkStates is the last link-check marker for each link below links:
// link index -> "ok" or "fail".
func GuestLinkStates(log string, links int) map[int]string {
	last := map[int]string{}
	for _, raw := range strings.Split(log, "\n") {
		i := strings.Index(raw, markLink+" ")
		if i < 0 {
			continue
		}
		f := strings.Fields(raw[i+len(markLink)+1:])
		if len(f) != 2 || (f[0] != "ok" && f[0] != "fail") {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(f[1], "%d", &n); err != nil || n < 0 || n >= links || fmt.Sprint(n) != f[1] {
			continue
		}
		last[n] = f[0]
	}
	return last
}

// PairRental is the pair rental on this machine and what its guest has said
// about the cable so far; pair is nil when no pair rental is here.
func (rt *Runtime) PairRental() (pair *PairOptions, ok, fail int) {
	st, err := LoadState(rt.h, rt.spec.DataDir)
	if err != nil || st == nil || st.Pair == nil {
		return nil, 0, 0
	}
	data, err := rt.h.ReadTail(st.Rental.SerialLog, guestLogTail)
	if err != nil {
		return st.Pair, 0, 0
	}
	ok, fail = ParseGuestLinks(string(data), len(st.Pair.Links))
	return st.Pair, ok, fail
}
