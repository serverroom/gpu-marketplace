package vmrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/pcidev"
	"github.com/serverroom/gpu-marketplace/internal/stats"
)

// SelfTestTimeout bounds how long the host waits for the test VM's report.
var SelfTestTimeout = 15 * time.Minute

// GuestPCI is one display function the self-test VM saw on its PCI bus.
type GuestPCI struct {
	ID       string // vendor:device
	Driver   string // "" when no driver took it
	MemoryMB int    // 0 when the driver does not say
	BDF      string // its address inside the VM
	Unmapped int    // BARs the VM's kernel could not place
}

// NVSMIRow is one GPU as nvidia-smi inside the VM described it.
type NVSMIRow struct {
	BDF      string
	Name     string
	MemoryMB int // 0 for [N/A]: a GB10, whose memory is the machine's pool
}

// SerialReport is what a self-test VM printed.
type SerialReport struct {
	Begin     bool
	End       bool
	PCI       []GuestPCI
	NVSMI     []NVSMIRow
	NVSMIFail string
	Internet  string // "ok", "fail", or "" when never reported
	Blocked   []string
	Reached   []string
}

// HostGPU is a GPU this machine passes through, as the host knows it.
type HostGPU struct {
	pcidev.Device
	Name string // for people: "NVIDIA GeForce RTX 4090"
}

// HostGPUs describes the GPUs a rental gets.
func HostGPUs(h Host, bdfs []string) []HostGPU {
	out := make([]HostGPU, 0, len(bdfs))
	for _, bdf := range bdfs {
		d := pcidev.Read(h, bdf)
		out = append(out, HostGPU{Device: d, Name: pcidev.Name(h, d)})
	}
	return out
}

// GuestGPU is one of this machine's GPUs as a rental saw it: what the
// marketplace can truthfully say a renter gets.
type GuestGPU struct {
	HostBDF  string `json:"host_bdf"`
	ID       string `json:"pci_id"`
	Model    string `json:"model"`
	Driver   string `json:"driver,omitempty"`
	MemoryMB int    `json:"memory_mb,omitempty"`
	Unified  bool   `json:"unified_memory,omitempty"`
	// UnmappedBARs are memory windows the VM could not place: the GPU is on
	// the VM's bus but cannot work.
	UnmappedBARs int `json:"unmapped_bars,omitempty"`
}

func (g GuestGPU) String() string {
	s := g.Model + " (" + g.ID
	if g.MemoryMB > 0 {
		s += fmt.Sprintf(", %d MB", g.MemoryMB)
	} else if g.Unified {
		s += ", the machine's memory"
	}
	if g.Driver != "" {
		s += ", driver " + g.Driver
	} else {
		s += ", no driver"
	}
	return s + ")"
}

func parsePCI(rest string) (GuestPCI, bool) {
	f := strings.Fields(rest)
	if len(f) != 4 && len(f) != 5 {
		return GuestPCI{}, false
	}
	g := GuestPCI{ID: strings.ToLower(f[0]), Driver: f[1], BDF: f[3]}
	if g.Driver == "none" {
		g.Driver = ""
	}
	g.MemoryMB, _ = strconv.Atoi(f[2])
	if len(f) == 5 {
		n, err := strconv.Atoi(f[4])
		if err != nil {
			// A count the host cannot read proves nothing was placed.
			n = 1
		}
		g.Unmapped = n
	}
	return g, true
}

// parseNVSMI reads "<bus id>, <name>, <memory MiB>"; the name sits between the
// first and the last comma, so a comma inside it cannot shift the columns.
func parseNVSMI(rest string) (NVSMIRow, bool) {
	first, last := strings.Index(rest, ","), strings.LastIndex(rest, ",")
	if first < 0 || last <= first {
		return NVSMIRow{}, false
	}
	row := NVSMIRow{
		BDF:  pcidev.NormalizeBDF(rest[:first]),
		Name: strings.TrimSpace(rest[first+1 : last]),
	}
	row.MemoryMB, _ = strconv.Atoi(strings.TrimSpace(rest[last+1:]))
	return row, row.BDF != ""
}

// ParseSerial reads the self-test marker lines out of a serial log. The log
// also holds firmware and kernel output, so only marker lines count.
func ParseSerial(log string) SerialReport {
	var rep SerialReport
	for _, raw := range strings.Split(log, "\n") {
		i := strings.Index(raw, markSelfTest+" ")
		if i < 0 {
			continue
		}
		line := strings.TrimSpace(strings.TrimRight(raw[i+len(markSelfTest)+1:], "\r"))
		word, rest, _ := strings.Cut(line, " ")
		rest = strings.TrimSpace(rest)
		switch word {
		case "BEGIN":
			rep.Begin = true
		case "END":
			rep.End = true
		case "PCI":
			if g, ok := parsePCI(rest); ok {
				rep.PCI = append(rep.PCI, g)
			}
		case "NVSMI":
			if row, ok := parseNVSMI(rest); ok {
				rep.NVSMI = append(rep.NVSMI, row)
			}
		case "NVSMIFAIL":
			rep.NVSMIFail = rest
		case "INTERNET":
			rep.Internet = rest
		case "BLOCKED":
			rep.Blocked = append(rep.Blocked, rest)
		case "REACHED":
			target, _, _ := strings.Cut(rest, " ")
			rep.Reached = append(rep.Reached, target)
		}
	}
	return rep
}

// SelfTestResult is the outcome of one test boot, kept on disk. Preflight
// reports a machine ready only while its latest result passed, for this agent
// version and these GPUs.
type SelfTestResult struct {
	Passed       bool     `json:"passed"`
	AgentVersion string   `json:"agent_version"`
	HostGPUs     []string `json:"host_gpus"`
	// GuestGPUs describes, for people, what the VM saw; GPUs is the same record
	// for the marketplace.
	GuestGPUs []string   `json:"guest_gpus"`
	GPUs      []GuestGPU `json:"gpus,omitempty"`
	Internet  bool       `json:"internet"`
	Blocked   []string   `json:"blocked"`
	Problems  []string   `json:"problems"`
	// Notes are what a provider should know that does not fail the test: a GPU
	// no driver took inside the VM still reached the renter.
	Notes []string `json:"notes,omitempty"`
	At    int64    `json:"at"`
	// Stopped: the test was stopped before it finished and its VM was torn
	// down clean, so nothing was recorded -- the machine keeps the verdict of
	// its last finished test boot. Never written to selftest.json.
	Stopped bool `json:"-"`
}

// matchGPUs pairs each GPU the host passed through with a display function the
// guest saw with the same vendor:device ID -- the one thing that is the same on
// both sides of VFIO. Identical GPUs each need a device of their own. The
// guest's nvidia-smi, when it answered, names the GPU and its memory; otherwise
// the host's PCI name and the driver's own memory figure stand.
func matchGPUs(rep SerialReport, host []HostGPU) (gpus []GuestGPU, missing []HostGPU) {
	used := make([]bool, len(rep.PCI))
	nv := map[string]NVSMIRow{}
	for _, row := range rep.NVSMI {
		nv[row.BDF] = row
	}
	for _, hg := range host {
		found := -1
		for i, g := range rep.PCI {
			if !used[i] && g.ID == hg.ID() {
				found = i
				break
			}
		}
		if found < 0 {
			missing = append(missing, hg)
			continue
		}
		used[found] = true
		g := rep.PCI[found]
		gpu := GuestGPU{HostBDF: hg.BDF, ID: hg.ID(), Model: hg.Name, Driver: g.Driver, MemoryMB: g.MemoryMB, UnmappedBARs: g.Unmapped}
		if row, ok := nv[g.BDF]; ok {
			gpu.Model = row.Name
			if row.MemoryMB > 0 {
				gpu.MemoryMB = row.MemoryMB
			} else {
				gpu.Unified = stats.IsUnifiedMemoryModel(row.Name)
			}
		}
		gpus = append(gpus, gpu)
	}
	return gpus, missing
}

// Evaluate turns what the VM printed and what the teardown verified into a
// verdict. Every problem is listed, not just the first. A GPU passes when the
// VM sees it: whether a driver took it is the renter's to use, and is noted.
func Evaluate(rep SerialReport, host []HostGPU, probes []string, sshOpened bool, stop StopResult, version string, at int64) SelfTestResult {
	res := SelfTestResult{
		AgentVersion: version,
		Internet:     rep.Internet == "ok",
		Blocked:      rep.Blocked,
		At:           at,
	}
	for _, hg := range host {
		res.HostGPUs = append(res.HostGPUs, hg.BDF)
	}
	add := func(format string, a ...interface{}) { res.Problems = append(res.Problems, fmt.Sprintf(format, a...)) }

	if !rep.End {
		add("the test VM never finished its report (see last-serial.log)")
	}
	if !sshOpened {
		add("the test VM's SSH port never opened, so a renter could not have logged in")
	}
	gpus, missing := matchGPUs(rep, host)
	res.GPUs = gpus
	for _, g := range gpus {
		res.GuestGPUs = append(res.GuestGPUs, g.String())
		switch {
		case g.UnmappedBARs > 0:
			add("GPU %s (%s) reached the VM but %d of its memory windows could not be mapped there, so it cannot work in a rental", g.HostBDF, g.Model, g.UnmappedBARs)
		case g.Driver == "":
			res.Notes = append(res.Notes, fmt.Sprintf("GPU %s (%s) reached the VM but no driver took it there; a renter would have to install one", g.HostBDF, g.Model))
		}
	}
	if rep.End {
		for _, hg := range missing {
			add("the VM did not see GPU %s (%s, %s), so it was not passed through", hg.BDF, hg.Name, hg.ID())
		}
	}
	if rep.NVSMIFail != "" {
		res.Notes = append(res.Notes, "nvidia-smi failed inside the VM: "+rep.NVSMIFail)
	}
	if rep.End && rep.Internet != "ok" {
		add("the VM could not reach the internet")
	}
	for _, t := range rep.Reached {
		add("the VM reached %s, which the fence must block", t)
	}
	seen := map[string]bool{}
	for _, t := range append(append([]string(nil), rep.Blocked...), rep.Reached...) {
		seen[t] = true
	}
	for _, p := range probes {
		if rep.End && !seen[p] {
			add("the VM reported nothing for %s", p)
		}
	}
	if !stop.Clean() {
		add("the teardown did not verify: %s", strings.Join(stop.Detail, "; "))
	}
	res.Passed = len(res.Problems) == 0
	return res
}

// SelfTestPath is where the latest self-test result lives.
func SelfTestPath(dataDir string) string { return filepath.Join(dataDir, "selftest.json") }

// SaveSelfTest records a result.
func SaveSelfTest(h Host, dataDir string, res SelfTestResult) error {
	if err := h.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return h.WriteFile(SelfTestPath(dataDir), data, 0600)
}

// LoadSelfTest returns the latest result, or nil when there is none.
func LoadSelfTest(h Host, dataDir string) (*SelfTestResult, error) {
	data, err := h.ReadFile(SelfTestPath(dataDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var res SelfTestResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// SelfTestProblem is the reason a machine is not ready on account of its self-
// test, or "" when a passing test for this version and these GPUs is on disk.
// A machine without a GPU passes with a test that had none; a machine with
// GPUs needs a test that passed them through (a test from before it had them
// does not count).
func SelfTestProblem(res *SelfTestResult, version string, gpus []string) string {
	const run = "run 'sudo gpu-agent check --boot'"
	switch {
	case res == nil && len(gpus) == 0:
		return "this machine has not yet booted a test rental; " + run
	case res == nil:
		return "this machine has not yet booted a test rental with its GPU passed through; " + run
	case !res.Passed:
		return fmt.Sprintf("its last test boot failed (%s); fix that and %s", strings.Join(res.Problems, "; "), run)
	case res.AgentVersion != version:
		return fmt.Sprintf("its passing test boot was with agent %s, not %s; %s", res.AgentVersion, version, run)
	case len(res.HostGPUs) == 0 && len(gpus) > 0:
		return "this machine has a GPU now, and its last test boot had none; " + run
	case !sameSet(res.HostGPUs, gpus):
		return "its GPUs have changed since its last test boot; " + run
	}
	return ""
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// SelfTest boots a real rental VM with the GPUs passed through and a key nobody
// holds, lets it report what it sees, tears it down, and records the verdict.
// It exercises the whole path a renter's VM takes -- fence, network, encrypted
// disk, VFIO, boot, teardown and GPU turnover -- which unit tests cannot.
func (rt *Runtime) SelfTest(version string) SelfTestResult {
	return rt.SelfTestContext(context.Background(), version)
}

// SelfTestContext is SelfTest that ctx can stop early: the test VM is torn down
// exactly as at the end of a test, and the result records that it was stopped.
func (rt *Runtime) SelfTestContext(ctx context.Context, version string) SelfTestResult {
	now := time.Now().Unix()
	// Read before the VM takes the GPUs: once on vfio-pci their host driver is
	// gone, and the names are wanted in the verdict.
	host := HostGPUs(rt.h, rt.spec.GPUs)
	fail := func(problem string) SelfTestResult {
		res := SelfTestResult{AgentVersion: version, HostGPUs: rt.spec.GPUs, At: now, Problems: []string{problem}}
		_ = SaveSelfTest(rt.h, rt.spec.DataDir, res)
		return res
	}
	if ctx.Err() != nil {
		// Nothing ran, so there is nothing to record.
		return SelfTestResult{AgentVersion: version, HostGPUs: rt.spec.GPUs, At: now, Stopped: true,
			Problems: []string{"the test boot was stopped before it started"}}
	}
	// No GPU query of the agent's own runs while the GPU is being tested.
	resume := stats.PauseGPUQueries()
	defer resume()
	pub, err := ThrowawayPubkey()
	if err != nil {
		return fail("could not make a test key: " + err.Error())
	}
	probes := DefaultProbes(rt.h)
	id := fmt.Sprintf("%s%d", SelfTestPrefix, now)
	if err := rt.Start(StartOptions{ID: id, Pubkey: pub, Probes: probes, NoWait: true}); err != nil {
		problem := "the test VM did not start: " + err.Error()
		if st, _ := LoadState(rt.h, rt.spec.DataDir); st != nil && st.Dirty {
			problem += "; and its cleanup did not verify, so this machine refuses rentals until that is fixed"
		}
		return fail(problem)
	}

	r := NewRental(rt.spec.Storage(), id)
	var rep SerialReport
	sshOpened := false
	for waited := time.Duration(0); waited < SelfTestTimeout && ctx.Err() == nil; waited += pollInterval {
		if !sshOpened && rt.h.DialTCP(net.JoinHostPort(GuestIP, "22"), 3*time.Second) == nil {
			sshOpened = true
		}
		if data, err := rt.h.ReadFile(r.SerialLog); err == nil {
			rep = ParseSerial(string(data))
		}
		if (rep.End && sshOpened) || !rt.alive(r) {
			break
		}
		rt.h.Sleep(pollInterval)
	}

	stopped := ctx.Err() != nil && !(rep.End && sshOpened)
	stop := rt.Stop()
	res := Evaluate(rep, host, probes, sshOpened, stop, version, now)
	if stopped {
		res.Passed = false
		res.Problems = append([]string{"the test boot was stopped before it finished"}, res.Problems...)
		if stop.Clean() {
			// A test stopped half way proves nothing either way: the machine
			// keeps the verdict of its last finished test boot. A teardown that
			// did not verify clean is recorded, because that machine must not host.
			res.Stopped = true
			return res
		}
	}
	_ = SaveSelfTest(rt.h, rt.spec.DataDir, res)
	return res
}
