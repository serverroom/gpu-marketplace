package vmrt

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// A rental's disk is a dm-crypt mapping, and the tool that makes and removes
// one (cryptsetup, through libdevmapper) does not return until udev has said
// it handled the device: libdevmapper hands the kernel a cookie with the
// request, udev's device-mapper rules end by running `dmsetup udevcomplete`
// with it, and the tool waits on a semaphore for that, with no limit of its
// own. Where udev answers, that is a tenth of a second. Where udev is running
// but never answers (a system image whose device-mapper rules are missing or
// changed), the kernel makes or removes the mapping at once and the tool then
// waits for ever: an open ended there leaves the mapping live in the kernel,
// a close ended there has already removed it, and each leaves its cookie. A
// machine with no udev running at all is not affected: libdevmapper sees that
// and does not wait.
//
// So every device-mapper command for a rental's disk has a time limit
// (dmLimit). One that runs out of it is killed, what it left is removed, and
// the command is run once more with DM_DISABLE_UDEV=1 in its environment:
// libdevmapper then makes or removes the node in /dev/mapper itself and waits
// for nobody. When that works, the machine is recorded as one whose udev does
// not confirm device-mapper devices (DMUdevRecord), and its mappings are made
// and removed that way from the start, with no limit to wait out. The machine
// rents as any other; `gpu-agent check` says what was found.
//
// The variable is not set on a machine whose udev answers. There udev makes
// /dev/mapper/<name> (a symlink) and the links other tools read; with the
// variable set, libdevmapper makes a device node of its own there and tells
// udev's rules to leave the device alone, which is not this agent's to decide
// for a machine that works. (A rental's disk works that way there too, so a
// machine that was only slow one day and recorded this is none the worse for
// it.) A host who sets the variable for the agent's service (a systemd drop-in
// with Environment=DM_DISABLE_UDEV=1) gets the same effect for every command
// and never meets the limit.
//
// It is learned from the first command that runs out of its limit, not from a
// trial mapping made when the agent starts: the limit has to guard every real
// command anyway (udev can stop answering on any day), a trial would add a
// device to a machine at every start, and the first real command is the test
// boot's, which no renter waits for.

// dmLimit is how long one device-mapper command for a rental's disk may take.
// Where udev answers, opening a plain dm-crypt mapping took 0.10 to 0.16
// seconds and closing one 0.07 to 0.10 (30 of each, in a VM on a 2013 Xeon),
// and 0.35 and 0.20 at most with every CPU busy and the disk being written. A
// machine that needs sixty times that is treated as one whose udev
// did not answer, which costs it nothing but the way its mappings are made.
const dmLimit = 20 * time.Second

// noUdev is the environment of a device-mapper command that does not wait for
// udev.
var noUdev = []string{"DM_DISABLE_UDEV=1"}

// DMUdevRecord is what this machine found when a device-mapper command for a
// rental's disk waited for udev: kept only when udev never answered.
type DMUdevRecord struct {
	// Unconfirmed: a command ran out of its limit waiting for udev, and what
	// it was asked to do was done without waiting.
	Unconfirmed bool `json:"unconfirmed"`
	// Why is what showed it, in the agent's own words.
	Why          string `json:"why,omitempty"`
	AgentVersion string `json:"agent_version"`
	At           int64  `json:"at"`
}

// DMUdevPath is where the record is kept.
func DMUdevPath(dataDir string) string { return path.Join(dataDir, "dm-udev.json") }

// LoadDMUdev returns the record, or nil when there is none.
func LoadDMUdev(h Host, dataDir string) *DMUdevRecord {
	data, err := h.ReadFile(DMUdevPath(dataDir))
	if err != nil {
		return nil
	}
	var rec DMUdevRecord
	if json.Unmarshal(data, &rec) != nil {
		return nil
	}
	return &rec
}

// MarkDMUdevUnconfirmed records that this machine's udev did not confirm a
// device-mapper device, and what showed it. It holds for this agent version:
// the next one waits for udev once more, so a machine whose udev was put right
// goes back to the ordinary way by itself.
func MarkDMUdevUnconfirmed(h Host, dataDir, version, why string) error {
	data, err := json.MarshalIndent(DMUdevRecord{Unconfirmed: true, Why: why, AgentVersion: version, At: time.Now().Unix()}, "", "  ")
	if err != nil {
		return err
	}
	if err := h.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	tmp := DMUdevPath(dataDir) + ".tmp"
	if err := h.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return h.Rename(tmp, DMUdevPath(dataDir))
}

// DMUdevUnconfirmed is what showed this agent version that the machine's udev
// does not confirm device-mapper devices, or "" when nothing has.
func DMUdevUnconfirmed(h Host, dataDir, version string) string {
	rec := LoadDMUdev(h, dataDir)
	if rec == nil || !rec.Unconfirmed || rec.AgentVersion != version {
		return ""
	}
	if rec.Why == "" {
		return "a command for a rental's disk did not finish while waiting for udev"
	}
	return rec.Why
}

// DMUdevNote says, for the host, that this machine's udev does not confirm
// device-mapper devices and that the agent handles it, or "" where udev does.
// It is not a reason the machine cannot rent.
func DMUdevNote(h Host, dataDir, version string) string {
	why := DMUdevUnconfirmed(h, dataDir, version)
	if why == "" {
		return ""
	}
	return "this machine's udev does not confirm device-mapper devices (" + why + "), so the agent makes and removes " +
		"a rental's encrypted disk without waiting for udev (DM_DISABLE_UDEV=1). There is nothing for you to do: the machine rents as any other"
}

// mappings makes and removes the mappings of rentals' disks on one machine.
type mappings struct {
	h       Host
	dataDir string
	version string
}

func mappingsOn(h Host, spec Spec) mappings {
	return mappings{h: h, dataDir: spec.DataDir, version: spec.AgentVersion}
}

// run is one device-mapper command, within the limit.
func (d mappings) run(env []string, stdin []byte, name string, args ...string) error {
	_, err := d.h.RunLimited(dmLimit, env, stdin, name, args...)
	return err
}

// noWait: this machine's udev is known not to answer.
func (d mappings) noWait() bool { return DMUdevUnconfirmed(d.h, d.dataDir, d.version) != "" }

// learn records that udev did not answer the command what.
func (d mappings) learn(what string) {
	_ = MarkDMUdevUnconfirmed(d.h, d.dataDir, d.version, fmt.Sprintf("%s was still waiting for udev after %v", what, dmLimit))
}

// open makes the dm-crypt mapping name over dev, keyed with key.
func (d mappings) open(key []byte, dev, name string) error {
	args := []string{"open", "--type", "plain", "--cipher", "aes-xts-plain64", "--key-size", "512",
		"--key-file", "-", "--keyfile-size", "64", dev, name}
	if d.noWait() {
		err := d.run(noUdev, key, "cryptsetup", args...)
		if errors.Is(err, ErrTimedOut) {
			_ = d.remove(name)
		}
		return err
	}
	cookies := udevCookies(d.h)
	err := d.run(nil, key, "cryptsetup", args...)
	if !errors.Is(err, ErrTimedOut) {
		return err
	}
	// It never came back. The kernel has usually made the mapping by then and
	// cryptsetup was waiting for udev: take away what it left, and make the
	// mapping again without waiting.
	releaseCookies(d.h, cookies)
	if rerr := d.remove(name); rerr != nil {
		return fmt.Errorf("cryptsetup had not opened the disk after %v, and the mapping it left could not be removed: %w", dmLimit, rerr)
	}
	if rerr := d.run(noUdev, key, "cryptsetup", args...); rerr != nil {
		_ = d.remove(name)
		return fmt.Errorf("cryptsetup had not opened the disk after %v, and could not open it without waiting for udev either: %w", dmLimit, rerr)
	}
	d.learn("cryptsetup open")
	return nil
}

// close removes the mapping name, and with it the only copy of its key. The
// caller asks the kernel afterwards whether it is gone, and takes an error
// about a mapping that is not there for what it is: a teardown asks for every
// disk a rental could have had, made or not.
func (d mappings) close(name string) error {
	if d.noWait() {
		return d.remove(name)
	}
	cookies := udevCookies(d.h)
	err := d.run(nil, nil, "cryptsetup", "close", name)
	if !errors.Is(err, ErrTimedOut) {
		dropStaleNode(d.h, name)
		return err
	}
	// It never came back. The kernel has usually removed the mapping by then
	// and cryptsetup was waiting for udev; whatever is still there goes now,
	// without waiting.
	releaseCookies(d.h, cookies)
	if rerr := d.remove(name); rerr != nil {
		return fmt.Errorf("cryptsetup had not closed the disk after %v, and it could not be closed without waiting for udev either: %w", dmLimit, rerr)
	}
	d.learn("cryptsetup close")
	return nil
}

// remove takes the mapping name out of the kernel without waiting for udev,
// and its node out of /dev/mapper. No mapping of that name is not an error.
func (d mappings) remove(name string) error {
	if !mapped(d.h, name) {
		dropStaleNode(d.h, name)
		return nil
	}
	err := d.run(noUdev, nil, "cryptsetup", "close", name)
	if err != nil && mapped(d.h, name) {
		// dmsetup removes by name what cryptsetup would not close.
		if derr := d.run(noUdev, nil, "dmsetup", "remove", name); derr != nil {
			err = fmt.Errorf("%w; %v", err, derr)
		}
	}
	if mapped(d.h, name) {
		if err == nil {
			err = errors.New("the mapping is still there")
		}
		return err
	}
	dropStaleNode(d.h, name)
	return nil
}

// dmDevice is the kernel's device for the mapping name ("dm-3"), or "" when
// the kernel has no mapping of that name. It is read from sysfs, which says
// what the kernel has whatever udev did or did not do about it. known is false
// where sysfs is not there to read.
func dmDevice(h Host, name string) (dev string, known bool) {
	if !h.Exists("/sys/block") {
		return "", false
	}
	names, err := h.Glob("/sys/block/dm-*/dm/name")
	if err != nil {
		return "", false
	}
	for _, p := range names {
		if data, err := h.ReadFile(p); err == nil && strings.TrimSpace(string(data)) == name {
			return path.Base(path.Dir(path.Dir(p))), true
		}
	}
	return "", true
}

// mapped reports whether the mapping name is open: the kernel has it or,
// where the kernel cannot be asked, its node is in /dev/mapper. A mapping can
// be open with no node (udev never made one) and a node can outlive its
// mapping (udev never removed it), so the node alone says neither.
func mapped(h Host, name string) bool {
	if dev, known := dmDevice(h, name); known {
		return dev != ""
	}
	return h.Exists("/dev/mapper/" + name)
}

// dropStaleNode removes a node in /dev/mapper whose mapping the kernel no
// longer has: udev did not remove it, or the command that would have was
// ended first.
func dropStaleNode(h Host, name string) {
	node := "/dev/mapper/" + name
	if dev, known := dmDevice(h, name); known && dev == "" && h.Exists(node) {
		_ = h.Remove(node)
	}
}

// udevCookies lists the udev cookies libdevmapper has outstanding on this
// machine, each with the id of its semaphore; nil when that cannot be read.
func udevCookies(h Host) map[string]string {
	if _, err := h.LookPath("dmsetup"); err != nil {
		return nil
	}
	out, err := h.RunLimited(dmLimit, nil, nil, "dmsetup", "udevcookies")
	if err != nil {
		return nil
	}
	cookies := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && strings.HasPrefix(f[0], "0x") {
			cookies[f[0]] = f[1]
		}
	}
	return cookies
}

// releaseCookies removes the cookies made since before was read: the one a
// command killed at its limit was waiting on, which nothing would ever remove.
// Only those. `dmsetup udevcomplete_all` would also end whatever the host's
// own tools (LVM, another cryptsetup) are waiting for at that moment.
func releaseCookies(h Host, before map[string]string) {
	if before == nil {
		return
	}
	for cookie, semid := range udevCookies(h) {
		if _, old := before[cookie]; !old {
			_, _ = h.RunLimited(dmLimit, nil, nil, "ipcrm", "-s", semid)
		}
	}
}
