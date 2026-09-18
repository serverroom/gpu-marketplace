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
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/stats"
)

// SelfTestTimeout bounds how long the host waits for the test VM's report.
var SelfTestTimeout = 15 * time.Minute

// SerialReport is what a self-test VM printed.
type SerialReport struct {
	Begin    bool
	End      bool
	GPUs     []string
	GPUFail  string
	Internet string // "ok", "fail", or "" when never reported
	Blocked  []string
	Reached  []string
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
		case "GPU":
			if rest != "" {
				rep.GPUs = append(rep.GPUs, rest)
			}
		case "GPUFAIL":
			rep.GPUFail = rest
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
	GuestGPUs    []string `json:"guest_gpus"`
	Internet     bool     `json:"internet"`
	Blocked      []string `json:"blocked"`
	Problems     []string `json:"problems"`
	At           int64    `json:"at"`
}

// Evaluate turns what the VM printed and what the teardown verified into a
// verdict. Every problem is listed, not just the first.
func Evaluate(rep SerialReport, hostGPUs []string, probes []string, sshOpened bool, stop StopResult, version string, at int64) SelfTestResult {
	res := SelfTestResult{
		AgentVersion: version,
		HostGPUs:     append([]string(nil), hostGPUs...),
		GuestGPUs:    rep.GPUs,
		Internet:     rep.Internet == "ok",
		Blocked:      rep.Blocked,
		At:           at,
	}
	add := func(format string, a ...interface{}) { res.Problems = append(res.Problems, fmt.Sprintf(format, a...)) }

	if !rep.End {
		add("the test VM never finished its report (see last-serial.log)")
	}
	if !sshOpened {
		add("the test VM's SSH port never opened, so a renter could not have logged in")
	}
	if rep.GPUFail != "" {
		add("nvidia-smi failed inside the VM: %s", rep.GPUFail)
	}
	if rep.End && len(rep.GPUs) < len(hostGPUs) {
		add("the VM saw %d GPU(s), and this machine passed through %d", len(rep.GPUs), len(hostGPUs))
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
	fail := func(problem string) SelfTestResult {
		res := SelfTestResult{AgentVersion: version, HostGPUs: rt.spec.GPUs, At: now, Problems: []string{problem}}
		_ = SaveSelfTest(rt.h, rt.spec.DataDir, res)
		return res
	}
	if ctx.Err() != nil {
		return fail("the test boot was stopped before it started")
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

	r := NewRental(rt.spec.DataDir, id)
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
	res := Evaluate(rep, rt.spec.GPUs, probes, sshOpened, stop, version, now)
	if stopped {
		res.Passed = false
		res.Problems = append([]string{"the test boot was stopped before it finished"}, res.Problems...)
	}
	_ = SaveSelfTest(rt.h, rt.spec.DataDir, res)
	return res
}
