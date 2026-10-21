package vmrt

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// A host's bandwidth limit for the rentals on its machine (v0.3.19): a speed
// each way and a monthly data allowance, set in the marketplace and sent with
// the rental. The agent holds the rental to it on the rental's own network
// device on the host -- the tap a microVM is attached to, the host's end of a
// container's veth -- which every byte the renter moves crosses: the internet,
// the renter's SSH through the relay, and a direct connection alike. The
// renter cannot reach that device: a microVM's root is root inside the VM
// only, and a container has no network admin right at all. On Windows and
// macOS the device is in the agent's own Linux (WSL 2, or the agent's VM), one
// layer above the rental's container, and the same commands run there.
//
// To the rental (its download) is the device's egress, held by a token bucket
// (tbf), which queues to the rate. From the rental (its upload) is the
// device's ingress, where Linux can only drop what is over the rate (police);
// a burst of a quarter of a second of the rate is what keeps TCP at the rate
// rather than well under it. Measured between two network namespaces on Linux
// 7.0 (21 October 2026): tbf 48.2 of 50 and 481 of 500 Mbit/s; police 19.6 of
// 20 and 294 of 300 with that burst, against 9.6 and 146 with a tenth of a
// second.

// NetLimit is what a host set for its rentals. 0 is no limit. A gigabyte is
// 10^9 bytes, counted in both directions together.
type NetLimit struct {
	DownMbps  int `json:"down_mbps,omitempty"`
	UpMbps    int `json:"up_mbps,omitempty"`
	MonthlyGB int `json:"monthly_gb,omitempty"`
	// AfterMbps is the speed each way once the month's allowance is used,
	// until the next month of the rental starts (DefaultAfterMbps when 0).
	AfterMbps int `json:"after_mbps,omitempty"`
}

// The bounds a limit must be within.
const (
	MaxLimitMbps  = 100000   // 100 Gbit/s
	MaxMonthlyGB  = 10000000 // 10 PB
	MaxAfterMbps  = 10000
	NetMonth      = 30 * 24 * time.Hour
	gigabyte      = 1000 * 1000 * 1000
	NetLimitEvery = 30 * time.Second
)

// DefaultAfterMbps is the speed after the allowance when the marketplace sent
// none.
const DefaultAfterMbps = 10

// Empty reports whether the limit limits nothing.
func (l NetLimit) Empty() bool { return l.DownMbps == 0 && l.UpMbps == 0 && l.MonthlyGB == 0 }

// Validate says what is wrong with a limit, or nil.
func (l NetLimit) Validate() error {
	switch {
	case l.DownMbps < 0 || l.DownMbps > MaxLimitMbps, l.UpMbps < 0 || l.UpMbps > MaxLimitMbps:
		return fmt.Errorf("a speed limit is from 1 to %d Mbit/s, or 0 for none", MaxLimitMbps)
	case l.MonthlyGB < 0 || l.MonthlyGB > MaxMonthlyGB:
		return fmt.Errorf("a monthly allowance is from 1 to %d GB, or 0 for none", MaxMonthlyGB)
	case l.AfterMbps < 0 || l.AfterMbps > MaxAfterMbps:
		return fmt.Errorf("the speed after the allowance is from 1 to %d Mbit/s", MaxAfterMbps)
	}
	return nil
}

// after is the speed each way once the allowance is used: AfterMbps, or the
// limit itself where that is slower.
func (l NetLimit) after() (down, up int) {
	a := l.AfterMbps
	if a <= 0 {
		a = DefaultAfterMbps
	}
	down, up = a, a
	if l.DownMbps > 0 && l.DownMbps < down {
		down = l.DownMbps
	}
	if l.UpMbps > 0 && l.UpMbps < up {
		up = l.UpMbps
	}
	return down, up
}

// NetLimitStart is a limited rental's network as its start recorded it, in
// rental.json: the limit, the device it is held on, and the device's byte
// count when the rental's first month began.
type NetLimitStart struct {
	Limit      NetLimit `json:"limit"`
	Dev        string   `json:"dev"`
	Since      int64    `json:"since"`
	StartBytes int64    `json:"start_bytes"`
}

// startNetLimit holds a rental starting on dev to l: the speed limits now, the
// allowance from here on (AccountNet). An error means the rental must not
// start: the host asked for a limit this machine did not put in place.
func startNetLimit(h Host, dev string, l NetLimit, now time.Time) (*NetLimitStart, error) {
	if err := l.Validate(); err != nil {
		return nil, err
	}
	if err := ApplyRates(h, dev, l.DownMbps, l.UpMbps); err != nil {
		return nil, fmt.Errorf("hold the rental to the host's bandwidth limit: %w", err)
	}
	n, _ := devBytes(h, dev)
	return &NetLimitStart{Limit: l, Dev: dev, Since: now.Unix(), StartBytes: n}, nil
}

// ApplyRates sets dev's speed to the rental (down) and from it (up), in
// Mbit/s; 0 leaves that direction unlimited. It replaces whatever was set
// before, so it also changes a running rental's speed.
func ApplyRates(h Host, dev string, down, up int) error {
	if _, err := h.LookPath("tc"); err != nil {
		return errors.New("tc (iproute2) is not installed")
	}
	if down > 0 {
		if err := h.Run("tc", tbfArgs(dev, down)...); err != nil {
			return err
		}
	} else {
		_ = h.Run("tc", "qdisc", "del", "dev", dev, "root")
	}
	_ = h.Run("tc", "qdisc", "del", "dev", dev, "ingress")
	if up <= 0 {
		return nil
	}
	if err := h.Run("tc", "qdisc", "add", "dev", dev, "handle", "ffff:", "ingress"); err != nil {
		return err
	}
	// matchall is Linux 4.8 and later; u32 matching every packet is the same
	// filter on a kernel without it.
	if err := h.Run("tc", policeArgs(dev, up, "matchall")...); err != nil {
		if uerr := h.Run("tc", policeArgs(dev, up, "u32")...); uerr != nil {
			return fmt.Errorf("%v; and with u32: %v", err, uerr)
		}
	}
	return nil
}

// ClearRates takes dev's speed limits off.
func ClearRates(h Host, dev string) {
	_ = h.Run("tc", "qdisc", "del", "dev", dev, "root")
	_ = h.Run("tc", "qdisc", "del", "dev", dev, "ingress")
}

// tbfArgs holds dev's egress (to the rental) to mbps. The bucket holds 40 ms
// of the rate and never less than 64 KiB, so a segmentation-offloaded packet
// (up to 64 KiB) fits it whole.
func tbfArgs(dev string, mbps int) []string {
	burst := int64(mbps) * 1000 * 1000 / 8 / 25
	if burst < 64*1024 {
		burst = 64 * 1024
	}
	return []string{"qdisc", "replace", "dev", dev, "root", "tbf", "rate", fmt.Sprintf("%dmbit", mbps),
		"burst", fmt.Sprintf("%d", burst), "latency", "50ms"}
}

// policeArgs drops what dev receives from the rental over mbps. The burst is
// a quarter of a second of the rate (at least 64 KiB), and the packet size
// limit 64 KiB, so a packet the kernel has merged (GRO) is measured like any
// other rather than dropped for its size.
func policeArgs(dev string, mbps int, match string) []string {
	burst := int64(mbps) * 1000 * 1000 / 8 / 4
	if burst < 64*1024 {
		burst = 64 * 1024
	}
	args := []string{"filter", "add", "dev", dev, "parent", "ffff:", "protocol", "all", "prio", "1"}
	if match == "u32" {
		args = append(args, "u32", "match", "u32", "0", "0")
	} else {
		args = append(args, "matchall")
	}
	return append(args, "action", "police", "rate", fmt.Sprintf("%dmbit", mbps), "burst", fmt.Sprintf("%d", burst),
		"mtu", "64kb", "conform-exceed", "drop/ok")
}

// ratesInPlace says what of the speed limits tc does not show on dev, or "".
func ratesInPlace(h Host, dev string, down, up int) string {
	var missing []string
	if down > 0 {
		out, err := h.Output("tc", "qdisc", "show", "dev", dev)
		if err != nil || !strings.Contains(out, "tbf") {
			missing = append(missing, "the limit to the rental")
		}
	}
	if up > 0 {
		out, err := h.Output("tc", "filter", "show", "dev", dev, "ingress")
		if err != nil || !strings.Contains(out, "police") {
			missing = append(missing, "the limit from the rental")
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return strings.Join(missing, " and ") + " did not show on the rental's network device"
}

// devBytes is what dev has received and sent since it was made.
func devBytes(h Host, dev string) (int64, bool) {
	rx, err := readInt(h, "/sys/class/net/"+dev+"/statistics/rx_bytes")
	if err != nil {
		return 0, false
	}
	tx, err := readInt(h, "/sys/class/net/"+dev+"/statistics/tx_bytes")
	if err != nil {
		return 0, false
	}
	return rx + tx, true
}

// NetUsage is a limited rental's use of its allowance, in net-usage.json:
// written by AccountNet, never by Start, so that a write which loses a race
// with a teardown leaves a file the next rental ignores (RentalID), never a
// rental.json that says a rental is still here.
type NetUsage struct {
	RentalID    string `json:"rental_id"`
	PeriodStart int64  `json:"period_start"`
	// BaseBytes is the device's count when this month began; Carried is what
	// this month had used when the device's count last went back (a device
	// made again), counted on top.
	BaseBytes int64  `json:"base_bytes"`
	Carried   int64  `json:"carried,omitempty"`
	Used      int64  `json:"used"`
	Slowed    bool   `json:"slowed,omitempty"`
	At        int64  `json:"at"`
	Problem   string `json:"problem,omitempty"`
}

// NetUsagePath is where a rental's allowance is counted.
func NetUsagePath(dataDir string) string { return path.Join(dataDir, "net-usage.json") }

func loadNetUsage(h Host, dataDir, rentalID string) *NetUsage {
	data, err := h.ReadFile(NetUsagePath(dataDir))
	if err != nil {
		return nil
	}
	var u NetUsage
	if json.Unmarshal(data, &u) != nil || u.RentalID != rentalID {
		return nil
	}
	return &u
}

func saveNetUsage(h Host, dataDir string, u *NetUsage) error {
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	tmp := NetUsagePath(dataDir) + ".tmp"
	if err := h.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return h.Rename(tmp, NetUsagePath(dataDir))
}

// AccountNet is one look at the running rental's allowance, every
// NetLimitEvery: it counts what the rental moved this month, slows it to the
// speed after the allowance once the month's is used, and gives it its own
// limits back when the next month of the rental begins. Nothing to do (nil,
// nil) when no limited rental is running.
func AccountNet(h Host, dataDir string, now time.Time) (*NetUsage, error) {
	st, err := LoadState(h, dataDir)
	if err != nil || st == nil || st.NetLimit == nil || st.Dirty {
		return nil, err
	}
	nl := st.NetLimit
	u := loadNetUsage(h, dataDir, st.RentalID)
	if u == nil {
		u = &NetUsage{RentalID: st.RentalID, PeriodStart: nl.Since, BaseBytes: nl.StartBytes}
	}
	n, ok := devBytes(h, nl.Dev)
	if !ok {
		u.Problem = "the rental's network device could not be read"
		u.At = now.Unix()
		return u, saveNetUsage(h, dataDir, u)
	}
	if n < u.BaseBytes {
		u.Carried, u.BaseBytes = u.Used, 0
	}
	month := int64(NetMonth / time.Second)
	newMonth := false
	for now.Unix() >= u.PeriodStart+month {
		u.PeriodStart += month
		u.BaseBytes, u.Carried = n, 0
		newMonth = true
	}
	u.Used = u.Carried + n - u.BaseBytes
	u.At, u.Problem = now.Unix(), ""
	l := nl.Limit
	over := l.MonthlyGB > 0 && u.Used >= int64(l.MonthlyGB)*gigabyte
	var apply func() error
	switch {
	case over && !u.Slowed:
		down, up := l.after()
		apply = func() error { return ApplyRates(h, nl.Dev, down, up) }
	case !over && (u.Slowed || newMonth):
		apply = func() error { return ApplyRates(h, nl.Dev, l.DownMbps, l.UpMbps) }
	}
	if apply != nil {
		// The rental could have ended since the state was read; its device
		// name is the next rental's, so look once more.
		if again, _ := LoadState(h, dataDir); again == nil || again.RentalID != st.RentalID {
			return nil, nil
		}
		if err := apply(); err != nil {
			u.Problem = "change the rental's speed: " + err.Error()
		} else {
			u.Slowed = over
		}
	}
	return u, saveNetUsage(h, dataDir, u)
}

// NetLimitReport is a limited rental's network for /status (rental.net_limit):
// what the host set, how much of this month's allowance the rental used, when
// the month ends, and whether it runs at the speed after the allowance.
type NetLimitReport struct {
	NetLimit
	UsedBytes   int64  `json:"used_bytes"`
	PeriodStart int64  `json:"period_start"`
	PeriodEnd   int64  `json:"period_end"`
	Slowed      bool   `json:"slowed,omitempty"`
	Problem     string `json:"problem,omitempty"`
}

// netLimitReport is the report for the rental in st, or nil when it is not
// limited.
func netLimitReport(h Host, dataDir string, st *State) *NetLimitReport {
	if st == nil || st.NetLimit == nil {
		return nil
	}
	nl := st.NetLimit
	r := &NetLimitReport{NetLimit: nl.Limit, PeriodStart: nl.Since}
	if r.AfterMbps == 0 && r.MonthlyGB > 0 {
		r.AfterMbps = DefaultAfterMbps
	}
	u := loadNetUsage(h, dataDir, st.RentalID)
	if u != nil {
		r.PeriodStart, r.UsedBytes, r.Slowed, r.Problem = u.PeriodStart, u.Used, u.Slowed, u.Problem
		if n, ok := devBytes(h, nl.Dev); ok && n >= u.BaseBytes {
			r.UsedBytes = u.Carried + n - u.BaseBytes
		}
	} else if n, ok := devBytes(h, nl.Dev); ok && n >= nl.StartBytes {
		r.UsedBytes = n - nl.StartBytes
	}
	r.PeriodEnd = r.PeriodStart + int64(NetMonth/time.Second)
	return r
}

// NetLimitRecord is what a test boot found when it held its test rental to a
// bandwidth limit: whether this machine can hold a rental to one.
type NetLimitRecord struct {
	Usable       bool   `json:"usable"`
	Why          string `json:"why,omitempty"`
	Kernel       string `json:"kernel"`
	AgentVersion string `json:"agent_version"`
	At           int64  `json:"at"`
}

// NetLimitPath is where the record is kept.
func NetLimitPath(dataDir string) string { return path.Join(dataDir, "net-limit.json") }

// ProveNetLimit holds a test rental's device dev to a limit, reads it back and
// takes it off again, and records whether that worked: the record is what puts
// the host's bandwidth setting on offer (NetLimitVerdict). It never fails the
// test boot: a machine that cannot hold a rental to a limit rents without one.
func ProveNetLimit(h Host, dataDir, version, dev string) {
	why := ""
	const test = 900
	if err := ApplyRates(h, dev, test, test); err != nil {
		why = err.Error()
	} else if missing := ratesInPlace(h, dev, test, test); missing != "" {
		why = missing
	}
	ClearRates(h, dev)
	if len(why) > 300 {
		why = why[:300]
	}
	data, err := json.MarshalIndent(NetLimitRecord{Usable: why == "", Why: why, Kernel: KernelRelease(h),
		AgentVersion: version, At: time.Now().Unix()}, "", "  ")
	if err != nil {
		return
	}
	tmp := NetLimitPath(dataDir) + ".tmp"
	if h.WriteFile(tmp, data, 0600) == nil {
		_ = h.Rename(tmp, NetLimitPath(dataDir))
	}
}

// NetLimitVerdict is the record when it holds for this kernel and agent
// version; nil when this machine has not tried (its next test boot does).
func NetLimitVerdict(h Host, dataDir, version string) *NetLimitRecord {
	data, err := h.ReadFile(NetLimitPath(dataDir))
	if err != nil {
		return nil
	}
	var rec NetLimitRecord
	if json.Unmarshal(data, &rec) != nil || rec.AgentVersion != version || rec.Kernel != KernelRelease(h) {
		return nil
	}
	return &rec
}

// NetLimitReport is the running microVM rental's bandwidth limit and
// allowance, or nil when it has none.
func (rt *Runtime) NetLimitReport() *NetLimitReport {
	st, err := LoadState(rt.h, rt.spec.DataDir)
	if err != nil || st == nil || st.Dirty || st.Mode == ModeContainer {
		return nil
	}
	return netLimitReport(rt.h, rt.spec.DataDir, st)
}

// AccountNet is AccountNet for the microVM rental.
func (rt *Runtime) AccountNet(now time.Time) (*NetUsage, error) {
	return AccountNet(rt.h, rt.spec.DataDir, now)
}

// NetLimitReport is the running container rental's bandwidth limit and
// allowance, or nil when it has none.
func (rt *ContainerRuntime) NetLimitReport() *NetLimitReport {
	st, err := LoadState(rt.h, rt.spec.DataDir)
	if err != nil || st == nil || st.Dirty || st.Mode != ModeContainer {
		return nil
	}
	return netLimitReport(rt.h, rt.spec.DataDir, st)
}

// AccountNet is AccountNet for the container rental.
func (rt *ContainerRuntime) AccountNet(now time.Time) (*NetUsage, error) {
	return AccountNet(rt.h, rt.spec.DataDir, now)
}
