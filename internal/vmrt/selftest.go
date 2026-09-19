package vmrt

import (
	"fmt"
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
// reports a machine ready only while its latest result passed, for these GPUs,
// this host driver and this base image (SelfTestProblem).
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
	// GPUVerified: the test passed with every GPU a rental gets passed through
	// to its VM (on a machine without a GPU, every passing test). A test run
	// while the host's own programs held the GPU passes without it: the
	// machine sells on it, and the GPU's handover is proven by a full test
	// boot before a rental starts.
	GPUVerified bool `json:"gpu_verified"`
	// WithoutGPU: the test left the GPUs with the host, which was using them.
	WithoutGPU bool `json:"without_gpu,omitempty"`
	// HostDriver and BaseImage are what the test ran with: the host's driver
	// for the GPUs (HostGPUDriver) and the rental base image's build. A
	// passing test stays valid across agent versions while they and the GPUs
	// are unchanged.
	HostDriver string `json:"host_driver"`
	BaseImage  string `json:"base_image"`
	// TestMemoryMB is the test VM's memory when it was sized below a rental's
	// because the host was using the machine's memory.
	TestMemoryMB int `json:"test_memory_mb,omitempty"`
	// LastFull is the last test that took the GPUs (on a machine without a
	// GPU, the last test), kept across the tests run without them.
	LastFull *TestVerdict `json:"last_full_test,omitempty"`
	// Stopped: the test was stopped before it finished and its VM was torn
	// down clean, so nothing was recorded -- the machine keeps the verdict of
	// its last finished test boot. Never written to selftest.json.
	Stopped bool `json:"-"`
	// legacy: written before v0.2.3, which recorded no driver or image (as
	// is any record without an image).
	legacy bool
}

// TestVerdict is one full test boot in brief: what it passed or failed with.
type TestVerdict struct {
	Passed       bool       `json:"passed"`
	AgentVersion string     `json:"agent_version"`
	At           int64      `json:"at"`
	HostGPUs     []string   `json:"host_gpus"`
	HostDriver   string     `json:"host_driver"`
	BaseImage    string     `json:"base_image"`
	GPUs         []GuestGPU `json:"gpus,omitempty"`
	Problems     []string   `json:"problems,omitempty"`
	legacy       bool
}

// verdict is the result in brief.
func (r SelfTestResult) verdict() *TestVerdict {
	return &TestVerdict{Passed: r.Passed, AgentVersion: r.AgentVersion, At: r.At, HostGPUs: r.HostGPUs,
		HostDriver: r.HostDriver, BaseImage: r.BaseImage, GPUs: r.GPUs, Problems: r.Problems, legacy: r.legacy}
}

// basis is what a verdict holds for.
type basis struct {
	version       string
	at            int64
	gpus          []string
	driver, image string
	legacy        bool
}

func (r SelfTestResult) basis() basis {
	return basis{version: r.AgentVersion, at: r.At, gpus: r.HostGPUs, driver: r.HostDriver, image: r.BaseImage, legacy: r.legacy || r.BaseImage == ""}
}

func (v TestVerdict) basis() basis {
	return basis{version: v.AgentVersion, at: v.At, gpus: v.HostGPUs, driver: v.HostDriver, image: v.BaseImage, legacy: v.legacy || v.BaseImage == ""}
}

// Fingerprint is the machine a test boot's verdict holds for: the agent
// version running, the GPUs a rental gets, the host's driver for them and the
// rental base image's build.
type Fingerprint struct {
	Version    string
	GPUs       []string
	HostDriver string
	BaseImage  string
	// ImageBuiltAt is when the base image was built (0: unknown).
	ImageBuiltAt int64
}

// MachineFingerprint reads the fingerprint of this machine for spec's GPUs.
func MachineFingerprint(h Host, spec Spec, version string) Fingerprint {
	fp := Fingerprint{Version: version, GPUs: append([]string(nil), spec.GPUs...), HostDriver: HostGPUDriver(h, spec.DataDir, spec.GPUs)}
	if info, err := LoadGoldenInfo(h, spec); err == nil {
		fp.BaseImage = imageBuild(info)
		fp.ImageBuiltAt = info.CreatedAt
	}
	return fp
}

// Fingerprint is MachineFingerprint for this runtime's GPUs.
func (rt *Runtime) Fingerprint(version string) Fingerprint {
	return MachineFingerprint(rt.h, rt.spec, version)
}

// imageBuild names one build of the base image.
func imageBuild(info GoldenInfo) string {
	drv := info.Driver
	if drv == "" {
		drv = "unrecorded"
	}
	built := "at an unrecorded time"
	if info.CreatedAt > 0 {
		built = time.Unix(info.CreatedAt, 0).UTC().Format("2006-01-02 15:04:05 UTC")
	}
	return fmt.Sprintf("%s with driver %s, built %s", info.Base, drv, built)
}

// HostGPUDriver is the host's driver for these GPUs, with its version where
// the module says ("nvidia 580.95.05", "amdgpu"), each once; "none" for a GPU
// without one. A GPU this agent's own rental has on vfio-pci counts with the
// driver it came from. "" without GPUs.
func HostGPUDriver(h Host, dataDir string, gpus []string) string {
	if len(gpus) == 0 {
		return ""
	}
	recorded := map[string]string{}
	if st, err := LoadState(h, dataDir); err == nil && st != nil {
		for _, d := range st.Devices {
			recorded[d.BDF] = d.Driver
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, gpu := range gpus {
		drv := driverOf(h, gpu)
		if orig, ok := recorded[gpu]; ok && (drv == "vfio-pci" || drv == "") {
			drv = orig
		}
		entry := "none"
		if drv != "" {
			entry = drv
			if v, err := h.ReadFile("/sys/module/" + drv + "/version"); err == nil && strings.TrimSpace(string(v)) != "" {
				entry += " " + strings.TrimSpace(string(v))
			}
		}
		if !seen[entry] {
			seen[entry] = true
			out = append(out, entry)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
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
	// Every GPU given was seen: SelfTestWith says otherwise for a test that
	// was given none of the machine's.
	res.GPUVerified = res.Passed
	return res
}
