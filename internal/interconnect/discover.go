// Package interconnect decides whether a machine can be one half of a linked
// pair -- two NVIDIA DGX Sparks cabled to each other over their ConnectX-7
// ports and rented as one -- and proves the cable. It finds the ConnectX
// cards and their ports, checks that the host keeps its hands off them (no
// address, no route, no bond, no NetworkManager, no netplan, clean IOMMU
// groups, no RDMA users), identifies the machine, listens for other agents on
// the ports, and runs the authenticated raw-frame link check.
//
// Everything that touches the machine goes through vmrt.Host (and the raw
// frames through PacketConn), so every rule is tested without a ConnectX card.
// What only hardware can settle is listed in DESIGN.md s15 (A1-A8).
package interconnect

import (
	"encoding/json"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// VendorMellanox is the PCI vendor id of NVIDIA networking (Mellanox) cards.
const VendorMellanox = "0x15b3"

// Function is one PCI function of a ConnectX card.
type Function struct {
	BDF     string
	Class   string
	Driver  string
	Netdevs []string
	Serial  string
	// VF: an SR-IOV virtual function.
	VF bool
}

// Port is a function's network interface on the host.
type Port struct {
	BDF       string
	Netdev    string
	MAC       string
	Carrier   bool
	SpeedMbps int
	MTU       int
	Operstate string
	Driver    string
	Firmware  string
	Serial    string
}

// NIC is one physical card: every PCI function that shares its serial number.
// A card whose serial cannot be read is keyed by its PCI slot and carries a
// problem, because nothing then proves which functions belong together.
type NIC struct {
	Serial    string
	Functions []Function
	Ports     []Port
	Problems  []string
}

// BDFs are the card's PCI functions.
func (n NIC) BDFs() []string {
	out := make([]string, 0, len(n.Functions))
	for _, f := range n.Functions {
		out = append(out, f.BDF)
	}
	return out
}

func isConnectXClass(class string) bool {
	c := strings.ToLower(strings.TrimSpace(class))
	return strings.HasPrefix(c, "0x0200") || strings.HasPrefix(c, "0x0207")
}

func readTrim(h vmrt.Host, p string) string {
	data, err := h.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Discover finds every NVIDIA/Mellanox Ethernet or InfiniBand PCI function
// and groups them into physical cards by serial number. Read-only.
func Discover(h vmrt.Host) []NIC {
	vendors, _ := h.Glob("/sys/bus/pci/devices/*/vendor")
	var funcs []Function
	for _, v := range vendors {
		bdf := path.Base(path.Dir(v))
		if strings.ToLower(readTrim(h, v)) != VendorMellanox {
			continue
		}
		class := readTrim(h, vmrt.DevPath(bdf)+"/class")
		if !isConnectXClass(class) {
			continue
		}
		funcs = append(funcs, Function{
			BDF:     bdf,
			Class:   class,
			Driver:  vmrt.DriverOf(h, bdf),
			Netdevs: vmrt.NetdevsOf(h, bdf),
			Serial:  serialOf(h, bdf),
			VF:      h.Exists(vmrt.DevPath(bdf) + "/physfn"),
		})
	}
	sort.Slice(funcs, func(i, j int) bool { return funcs[i].BDF < funcs[j].BDF })

	// A function whose serial cannot be read (on vfio-pci, say) belongs with a
	// function of the same PCI slot that has one, if any.
	slotSerial := map[string]string{}
	for _, f := range funcs {
		if f.Serial != "" {
			slotSerial[vmrt.SlotOf(f.BDF)] = f.Serial
		}
	}
	byKey := map[string]*NIC{}
	var keys []string
	for _, f := range funcs {
		serial := f.Serial
		if serial == "" {
			serial = slotSerial[vmrt.SlotOf(f.BDF)]
		}
		key := "serial:" + serial
		if serial == "" {
			key = "slot:" + vmrt.SlotOf(f.BDF)
		}
		n := byKey[key]
		if n == nil {
			n = &NIC{Serial: serial}
			if serial == "" {
				n.Problems = append(n.Problems, "the serial number of the ConnectX card at "+f.BDF+
					" cannot be read (devlink dev info, PCI VPD), so it is not known which PCI functions belong to it; bind it to mlx5_core and install iproute2")
			}
			byKey[key] = n
			keys = append(keys, key)
		}
		f.Serial = serial
		n.Functions = append(n.Functions, f)
	}
	sort.Strings(keys)
	out := make([]NIC, 0, len(keys))
	for _, k := range keys {
		n := byKey[k]
		for _, f := range n.Functions {
			if f.VF {
				n.Problems = append(n.Problems, "SR-IOV virtual function "+f.BDF+" is enabled on the ConnectX card; "+
					"a linked pair hands the whole card to the rental, so disable them (echo 0 > "+vmrt.DevPath(f.BDF)+"/../sriov_numvfs on its physical function)")
				continue
			}
			for _, nd := range f.Netdevs {
				n.Ports = append(n.Ports, readPort(h, f, nd, n.Serial))
			}
		}
		out = append(out, *n)
	}
	return out
}

func readPort(h vmrt.Host, f Function, netdev, serial string) Port {
	p := Port{
		BDF:       f.BDF,
		Netdev:    netdev,
		MAC:       vmrt.NetdevMAC(h, netdev),
		Carrier:   vmrt.NetAttr(h, netdev, "carrier") == "1",
		Operstate: vmrt.NetAttr(h, netdev, "operstate"),
		Driver:    f.Driver,
		Serial:    serial,
	}
	if s, err := strconv.Atoi(vmrt.NetAttr(h, netdev, "speed")); err == nil && s > 0 {
		p.SpeedMbps = s
	}
	if m, err := strconv.Atoi(vmrt.NetAttr(h, netdev, "mtu")); err == nil && m > 0 {
		p.MTU = m
	}
	if fw, err := vmrt.NICFirmware(h, netdev); err == nil {
		p.Firmware = fw
	}
	return p
}

// serialOf is a function's board serial number: from devlink, or failing that
// from the PCI VPD, which reads the same whichever driver holds the function.
func serialOf(h vmrt.Host, bdf string) string {
	if out, err := h.Output("devlink", "-j", "dev", "info", "pci/"+bdf); err == nil {
		var info struct {
			Info map[string]struct {
				Serial      string `json:"serial_number"`
				BoardSerial string `json:"board.serial_number"`
			} `json:"info"`
		}
		if json.Unmarshal([]byte(out), &info) == nil {
			for _, v := range info.Info {
				if s := cleanSerial(v.Serial); s != "" {
					return s
				}
				if s := cleanSerial(v.BoardSerial); s != "" {
					return s
				}
			}
		}
	}
	if vpd, err := h.ReadFile(vmrt.DevPath(bdf) + "/vpd"); err == nil {
		return cleanSerial(vpdSerial(vpd))
	}
	return ""
}

func cleanSerial(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if r > 0x20 && r < 0x7f {
			b.WriteRune(r)
		}
	}
	if b.Len() > 64 {
		return b.String()[:64]
	}
	return b.String()
}

// vpdSerial finds the "SN" keyword in PCI Vital Product Data: a sequence of
// resources, each a tag byte and a little-endian length for large resources;
// the read-only resource (tag 0x90) holds keyword entries of 2 name bytes, 1
// length byte and the value.
func vpdSerial(vpd []byte) string {
	i := 0
	for i < len(vpd) {
		tag := vpd[i]
		if tag&0x80 == 0 { // small resource
			if tag>>3 == 0x0f { // end tag
				return ""
			}
			i += 1 + int(tag&0x07)
			continue
		}
		if i+3 > len(vpd) {
			return ""
		}
		n := int(vpd[i+1]) | int(vpd[i+2])<<8
		start, end := i+3, i+3+n
		if end > len(vpd) {
			end = len(vpd)
		}
		if tag == 0x90 || tag == 0x91 {
			for j := start; j+3 <= end; {
				kw := string(vpd[j : j+2])
				l := int(vpd[j+2])
				if j+3+l > end {
					break
				}
				if kw == "SN" {
					return string(vpd[j+3 : j+3+l])
				}
				j += 3 + l
			}
		}
		i = i + 3 + n
	}
	return ""
}
