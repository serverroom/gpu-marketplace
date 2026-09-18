// Package fakehost is an in-memory vmrt.Host for tests. It records every
// command and write, answers commands from Outputs, fails the ones named in
// Fail, and simulates just enough of sysfs's PCI driver binding for the VFIO
// code to run against it.
package fakehost

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// Host is the fake. Build it with New.
type Host struct {
	mu sync.Mutex

	// Calls holds "run <command>", "write <path>=<data>", "remove <path>" and
	// "download <url>", in order.
	Calls []string
	// Outputs answers Output for the longest matching command prefix.
	Outputs map[string]string
	// Fail makes a command (by prefix) or a write ("write <path>") fail.
	Fail map[string]error
	// OnRun runs after a successful command whose text starts with the key; it
	// is given the full command.
	OnRun map[string]func(h *Host, cmd string)
	// Files and Links are the filesystem.
	Files map[string][]byte
	Links map[string]string
	// Tools answers LookPath; nil means every tool is installed.
	Tools map[string]bool
	// Dial answers DialTCP.
	Dial map[string]bool
	// Downloads is what Download fetches.
	Downloads map[string][]byte
	// Slept is the total time the code under test asked to sleep.
	Slept time.Duration
	// OnSleep, when set, runs after every Sleep: time passing, for anything
	// the code under test waits for.
	OnSleep func(h *Host)

	home map[string]string // bdf -> driver the device binds to with no override
	nics map[string]*nicDev
}

// nicDev is the network interface a NIC function has while its home driver
// holds it. Unbinding the function takes the interface away; binding it back
// brings it back, with whatever MAC it has by then, unless it was lost.
type nicDev struct {
	netdev string
	mac    string
	lost   bool
}

// New returns an empty fake host.
func New() *Host {
	return &Host{
		Outputs:   map[string]string{},
		Fail:      map[string]error{},
		OnRun:     map[string]func(*Host, string){},
		Files:     map[string][]byte{},
		Links:     map[string]string{},
		Dial:      map[string]bool{},
		Downloads: map[string][]byte{},
		home:      map[string]string{},
		nics:      map[string]*nicDev{},
	}
}

func text(name string, args []string) string {
	return strings.TrimSpace(name + " " + strings.Join(args, " "))
}

func longestPrefix[V any](m map[string]V, s string) (V, bool) {
	var best V
	bestLen := -1
	for k, v := range m {
		if strings.HasPrefix(s, k) && len(k) > bestLen {
			best, bestLen = v, len(k)
		}
	}
	return best, bestLen >= 0
}

// command records a command and returns its failure (if any) and side effects.
func (h *Host) command(k string) (error, []func(*Host, string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Calls = append(h.Calls, "run "+k)
	if err, ok := longestPrefix(h.Fail, k); ok {
		return err, nil
	}
	var effects []func(*Host, string)
	for prefix, fn := range h.OnRun {
		if strings.HasPrefix(k, prefix) {
			effects = append(effects, fn)
		}
	}
	return nil, effects
}

func (h *Host) Run(name string, args ...string) error {
	k := text(name, args)
	err, effects := h.command(k)
	if err != nil {
		return err
	}
	for _, fn := range effects {
		fn(h, k)
	}
	return nil
}

func (h *Host) RunInput(stdin []byte, name string, args ...string) error {
	return h.Run(name, args...)
}

func (h *Host) Output(name string, args ...string) (string, error) {
	k := text(name, args)
	err, effects := h.command(k)
	if err != nil {
		return "", err
	}
	for _, fn := range effects {
		fn(h, k)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out, _ := longestPrefix(h.Outputs, k)
	return out, nil
}

func (h *Host) LookPath(name string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Tools == nil || h.Tools[name] {
		return "/usr/bin/" + name, nil
	}
	return "", fmt.Errorf("%s: not found", name)
}

func (h *Host) ReadFile(p string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	data, ok := h.Files[p]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	return append([]byte(nil), data...), nil
}

func (h *Host) ReadTail(p string, max int64) ([]byte, error) {
	data, err := h.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		data = data[int64(len(data))-max:]
	}
	return data, nil
}

const pciDevices = "/sys/bus/pci/devices/"

func driverLink(bdf string) string { return pciDevices + bdf + "/driver" }

func (h *Host) WriteFile(p string, data []byte, perm os.FileMode) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Calls = append(h.Calls, "write "+p+"="+strings.TrimSpace(string(data)))
	if err, ok := longestPrefix(h.Fail, "write "+p); ok {
		return err
	}
	value := strings.TrimSpace(string(data))
	switch {
	case strings.HasPrefix(p, "/sys/bus/pci/drivers/") && strings.HasSuffix(p, "/unbind"):
		delete(h.Links, driverLink(value))
		h.dropNetdev(value)
	case p == "/sys/bus/pci/drivers_probe":
		if _, bound := h.Links[driverLink(value)]; bound {
			break
		}
		override := strings.TrimSpace(string(h.Files[pciDevices+value+"/driver_override"]))
		if override != "" {
			h.Links[driverLink(value)] = "../../../bus/pci/drivers/" + override
		} else if home := h.home[value]; home != "" {
			h.Links[driverLink(value)] = "../../../bus/pci/drivers/" + home
			h.raiseNetdev(value)
		}
	default:
		h.Files[p] = append([]byte(nil), data...)
	}
	return nil
}

// PCI registers a device: its current driver, class, and IOMMU group members.
func (h *Host) PCI(bdf, driver, class string, group ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if driver != "" {
		h.Links[driverLink(bdf)] = "../../../bus/pci/drivers/" + driver
	}
	h.home[bdf] = driver
	h.Files[pciDevices+bdf+"/class"] = []byte(class + "\n")
	h.Files[pciDevices+bdf+"/reset"] = nil
	for _, m := range group {
		h.Files[pciDevices+bdf+"/iommu_group/devices/"+m] = nil
	}
}

// NIC registers a ConnectX-7 function on mlx5_core with one network interface
// that has carrier at 200 GbE and nothing configured on it: its PCI identity
// and IOMMU group, its sysfs netdev, and canned answers for `ethtool -i`,
// `devlink dev info`, `ip -j addr/route`, `mstconfig q` and `nmcli` (unmanaged).
func (h *Host) NIC(bdf, netdev, mac, serial string, group ...string) {
	h.PCI(bdf, "mlx5_core", "0x020000", group...)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Files[pciDevices+bdf+"/vendor"] = []byte("0x15b3\n")
	h.Files[pciDevices+bdf+"/device"] = []byte("0x1021\n")
	h.nics[bdf] = &nicDev{netdev: netdev, mac: mac}
	h.raiseNetdev(bdf)
	h.Outputs["ethtool -i "+netdev] = "driver: mlx5_core\nversion: 26.04\nfirmware-version: 28.40.1000 (NVD0000000033)\nbus-info: " + bdf + "\n"
	h.Outputs["devlink -j dev info pci/"+bdf] = `{"info":{"pci/` + bdf + `":{"driver":"mlx5_core","serial_number":"` + serial + `","versions":{"fixed":{"fw.psid":"NVD0000000033"}}}}}`
	h.Outputs["ip -j addr show dev "+netdev] = `[{"ifindex":5,"ifname":"` + netdev + `","flags":["BROADCAST","MULTICAST"],"mtu":1500,"addr_info":[]}]`
	h.Outputs["ip -j route show dev "+netdev] = "[]"
	h.Outputs["ip -6 -j route show dev "+netdev] = `[{"dst":"fe80::/64","protocol":"kernel","metric":256,"flags":[],"pref":"medium"}]`
	h.Outputs["mstconfig -d "+bdf+" q"] = "\nDevice #1:\n----------\n\nDevice type:        ConnectX7\nPCI device:         " + bdf + "\n\nConfigurations:                          Next Boot\n        SRIOV_EN                            False(0)\n        LINK_TYPE_P1                        ETH(2)\n"
	h.Outputs["nmcli -t -f DEVICE,STATE device"] += netdev + ":unmanaged\n"
}

func (h *Host) raiseNetdev(bdf string) {
	n := h.nics[bdf]
	if n == nil || n.lost {
		return
	}
	h.Files[pciDevices+bdf+"/net/"+n.netdev] = nil
	for attr, v := range map[string]string{"address": n.mac, "carrier": "1", "speed": "200000", "mtu": "1500", "operstate": "up", "flags": "0x1003"} {
		h.Files["/sys/class/net/"+n.netdev+"/"+attr] = []byte(v + "\n")
	}
}

func (h *Host) dropNetdev(bdf string) {
	n := h.nics[bdf]
	if n == nil {
		return
	}
	delete(h.Files, pciDevices+bdf+"/net/"+n.netdev)
	for k := range h.Files {
		if strings.HasPrefix(k, "/sys/class/net/"+n.netdev+"/") {
			delete(h.Files, k)
		}
	}
}

// SetNICMAC changes the MAC a NIC function's interface comes back with the
// next time its driver binds it (and now, if it is bound).
func (h *Host) SetNICMAC(bdf, mac string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n := h.nics[bdf]; n != nil {
		n.mac = mac
		if _, ok := h.Files["/sys/class/net/"+n.netdev+"/address"]; ok {
			h.Files["/sys/class/net/"+n.netdev+"/address"] = []byte(mac + "\n")
		}
	}
}

// LoseNetdev makes a NIC function come back from its next unbind with no
// network interface at all.
func (h *Host) LoseNetdev(bdf string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n := h.nics[bdf]; n != nil {
		n.lost = true
	}
}

// Driver is the driver a device is bound to now.
func (h *Host) Driver(bdf string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	link, ok := h.Links[driverLink(bdf)]
	if !ok {
		return ""
	}
	return path.Base(link)
}

func (h *Host) Readlink(p string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if link, ok := h.Links[p]; ok {
		return link, nil
	}
	return "", &fs.PathError{Op: "readlink", Path: p, Err: fs.ErrNotExist}
}

func (h *Host) Glob(pattern string) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string][]byte{h.Files} {
		for k := range m {
			if ok, _ := path.Match(pattern, k); ok && !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	for k := range h.Links {
		if ok, _ := path.Match(pattern, k); ok && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (h *Host) Exists(p string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.Files[p]; ok {
		return true
	}
	if _, ok := h.Links[p]; ok {
		return true
	}
	for k := range h.Files {
		if strings.HasPrefix(k, p+"/") {
			return true
		}
	}
	return false
}

func (h *Host) Remove(p string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Calls = append(h.Calls, "remove "+p)
	delete(h.Files, p)
	delete(h.Links, p)
	return nil
}

func (h *Host) RemoveAll(p string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Calls = append(h.Calls, "removeall "+p)
	for k := range h.Files {
		if k == p || strings.HasPrefix(k, p+"/") {
			delete(h.Files, k)
		}
	}
	return nil
}

func (h *Host) Rename(from, to string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	data, ok := h.Files[from]
	if !ok {
		return &fs.PathError{Op: "rename", Path: from, Err: fs.ErrNotExist}
	}
	h.Files[to] = data
	delete(h.Files, from)
	return nil
}

func (h *Host) MkdirAll(p string, perm os.FileMode) error { return nil }

func (h *Host) DialTCP(addr string, timeout time.Duration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Dial[addr] {
		return nil
	}
	return fmt.Errorf("dial %s: connection timed out", addr)
}

func (h *Host) Download(url, p string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Calls = append(h.Calls, "download "+url)
	if err, ok := longestPrefix(h.Fail, "download "+url); ok {
		return err
	}
	data, ok := h.Downloads[url]
	if !ok {
		return fmt.Errorf("GET %s: HTTP 404", url)
	}
	h.Files[p] = append([]byte(nil), data...)
	return nil
}

func (h *Host) SHA256(p string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	data, ok := h.Files[p]
	if !ok {
		return "", &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (h *Host) Sleep(d time.Duration) {
	h.mu.Lock()
	h.Slept += d
	fn := h.OnSleep
	h.mu.Unlock()
	if fn != nil {
		fn(h)
	}
}

// SleptFor is the total time slept so far.
func (h *Host) SleptFor() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.Slept
}

// SetLink makes a symlink, as a side effect would.
func (h *Host) SetLink(p, target string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Links[p] = target
}

// DeleteLink removes a symlink, as a side effect would.
func (h *Host) DeleteLink(p string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.Links, p)
}

// Count is how many calls start with prefix.
func (h *Host) Count(prefix string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.Calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// Index is the position of the first call starting with prefix, or -1.
func (h *Host) Index(prefix string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, c := range h.Calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

// Ran reports whether any call starts with prefix.
func (h *Host) Ran(prefix string) bool { return h.Index(prefix) >= 0 }

// Call is the first call starting with prefix, or "".
func (h *Host) Call(prefix string) string {
	if i := h.Index(prefix); i >= 0 {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.Calls[i]
	}
	return ""
}

// SetFile writes a file, as a side effect would.
func (h *Host) SetFile(p string, data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Files[p] = data
}

// DeleteFile removes a file, as a side effect would.
func (h *Host) DeleteFile(p string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.Files, p)
}

// SetFail makes a command prefix fail with err, or succeed when err is nil.
func (h *Host) SetFail(prefix string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Fail[prefix] = err
}

// LastField is the last word of a command, which is usually its target.
func LastField(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}
