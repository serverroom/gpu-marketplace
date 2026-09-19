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

// SelfTestPath is where the latest self-test result lives.
func SelfTestPath(dataDir string) string { return filepath.Join(dataDir, "selftest.json") }

// SaveSelfTest records a result. A test that took the GPUs is also the
// machine's last full test; one run without them keeps the last full test of
// the record before it.
func SaveSelfTest(h Host, dataDir string, res SelfTestResult) error {
	if res.WithoutGPU {
		res.LastFull = nil
		if prev, err := LoadSelfTest(h, dataDir); err == nil && prev != nil {
			res.LastFull = prev.LastFull
		}
	} else {
		res.LastFull = res.verdict()
	}
	if err := h.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return h.WriteFile(SelfTestPath(dataDir), data, 0600)
}

// LoadSelfTest returns the latest result, or nil when there is none. A record
// from before v0.2.3 is read as what it was: a full test, with no driver or
// image recorded.
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
	// Every test since v0.2.3 records the image it ran with; one that does
	// not was a full test by an older agent.
	if res.BaseImage == "" {
		res.legacy = true
		res.GPUVerified = res.Passed && !res.WithoutGPU
	}
	if res.LastFull != nil {
		res.LastFull.legacy = res.LastFull.BaseImage == ""
	} else if !res.WithoutGPU {
		res.LastFull = res.verdict()
	}
	return &res, nil
}

const runTestBoot = "run 'sudo gpu-agent check --boot'"

// problem says why a verdict does not hold for fp, or "". A verdict holds for
// the GPUs it was reached with, while the host's driver for them and the base
// image are the ones it was reached with -- across agent versions. A record
// from before v0.2.3 knows neither: it holds for its own agent version, as it
// always did, and for a later one while the base image is still the one it
// was tested with (not rebuilt after the test).
func (b basis) problem(fp Fingerprint) string {
	switch {
	case len(b.gpus) == 0 && len(fp.GPUs) > 0:
		return "this machine has a GPU now, and its last test boot had none; " + runTestBoot
	case !sameSet(b.gpus, fp.GPUs):
		return "its GPUs have changed since its last test boot; " + runTestBoot
	case b.legacy:
		if b.version == fp.Version || fp.ImageBuiltAt <= b.at {
			return ""
		}
		return "the rental base image was rebuilt after its last test boot; " + runTestBoot
	case b.driver != fp.HostDriver:
		return fmt.Sprintf("this machine's GPU driver has changed since its last test boot (%s then, %s now); %s",
			orUnknown(b.driver), orUnknown(fp.HostDriver), runTestBoot)
	case b.image != fp.BaseImage:
		return "the rental base image was rebuilt after its last test boot; " + runTestBoot
	}
	return ""
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// SelfTestProblem is the reason a machine is not ready on account of its test
// boot, or "" when its last test boot lets it be offered to renters: a pass
// for these GPUs, this host driver and this base image, from any agent
// version (a test run without the GPU, while the host was using it, counts),
// and no failed full test for them since. A machine without a GPU passes with
// a test that had none; a machine with GPUs needs a test that was for them.
func SelfTestProblem(res *SelfTestResult, fp Fingerprint) string {
	switch {
	case res == nil && len(fp.GPUs) == 0:
		return "this machine has not yet booted a test rental; " + runTestBoot
	case res == nil:
		return "this machine has not yet booted a test rental with its GPU passed through; " + runTestBoot
	case !res.Passed:
		return fmt.Sprintf("its last test boot failed (%s); fix that and %s", strings.Join(res.Problems, "; "), runTestBoot)
	}
	if p := res.basis().problem(fp); p != "" {
		return p
	}
	if GPUTestFailed(res, fp) {
		return fmt.Sprintf("its GPU could not be handed to its last full test rental (%s); the agent tries again when the GPU is free, or %s",
			strings.Join(res.LastFull.Problems, "; "), runTestBoot)
	}
	return ""
}

// GPUTestFailed reports whether the machine's last full test, for these GPUs,
// driver and image, failed: a test without the GPU cannot outweigh it.
func GPUTestFailed(res *SelfTestResult, fp Fingerprint) bool {
	return res != nil && res.LastFull != nil && !res.LastFull.Passed && res.LastFull.basis().problem(fp) == ""
}

// FullTestCurrent reports whether the running agent version has itself
// passed a full test boot -- the GPU handed over -- on this machine as it is
// now. While it has not, the machine sells on its earlier pass, and the full
// test runs when the GPU is free, and always before a rental starts.
func FullTestCurrent(res *SelfTestResult, fp Fingerprint) bool {
	if res == nil || SelfTestProblem(res, fp) != "" {
		return false
	}
	f := res.LastFull
	return f != nil && f.Passed && f.AgentVersion == fp.Version && f.basis().problem(fp) == ""
}

// VerifiedGPUs are the GPUs as the last test that took them saw them, for
// the machine's current GPUs: what the marketplace tells a renter they get.
func VerifiedGPUs(res *SelfTestResult, fp Fingerprint) []GuestGPU {
	switch {
	case res == nil:
		return nil
	case res.GPUVerified && sameSet(res.HostGPUs, fp.GPUs):
		return res.GPUs
	case res.LastFull != nil && res.LastFull.Passed && sameSet(res.LastFull.HostGPUs, fp.GPUs):
		return res.LastFull.GPUs
	}
	return nil
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
	return rt.SelfTestWith(ctx, version, TestOptions{})
}

// SelfTestWith is SelfTestContext shaped by o: without the GPUs (they stay
// with the host, which is using them; everything else a rental goes through
// is tested), or with a smaller VM (the host is using the memory).
func (rt *Runtime) SelfTestWith(ctx context.Context, version string, o TestOptions) SelfTestResult {
	now := time.Now().Unix()
	// Read before the VM takes the GPUs: once on vfio-pci their host driver is
	// gone, and the names are wanted in the verdict.
	var host []HostGPU
	if !o.NoGPU {
		host = HostGPUs(rt.h, rt.spec.GPUs)
	}
	fp := rt.Fingerprint(version)
	stamp := func(res SelfTestResult) SelfTestResult {
		res.AgentVersion, res.At = version, now
		res.HostGPUs = rt.spec.GPUs
		res.HostDriver, res.BaseImage = fp.HostDriver, fp.BaseImage
		res.WithoutGPU = o.NoGPU && len(rt.spec.GPUs) > 0
		res.TestMemoryMB = o.MemoryMB
		if res.WithoutGPU {
			res.GPUVerified = false
		}
		return res
	}
	fail := func(problem string) SelfTestResult {
		res := stamp(SelfTestResult{Problems: []string{problem}})
		_ = SaveSelfTest(rt.h, rt.spec.DataDir, res)
		return res
	}
	if ctx.Err() != nil {
		// Nothing ran, so there is nothing to record.
		res := stamp(SelfTestResult{Problems: []string{"the test boot was stopped before it started"}})
		res.Stopped = true
		return res
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
	if err := rt.Start(StartOptions{ID: id, Pubkey: pub, Probes: probes, NoWait: true, NoGPU: o.NoGPU, MemoryMB: o.MemoryMB}); err != nil {
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
	res := stamp(Evaluate(rep, host, probes, sshOpened, stop, version, now))
	if res.WithoutGPU && res.Passed {
		res.Notes = append(res.Notes, "the GPU was in use on this machine, so this test ran without it; "+
			"its handover to a rental is tested when the GPU is free, and always before a rental starts")
	}
	if stopped {
		res.Passed, res.GPUVerified = false, false
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
