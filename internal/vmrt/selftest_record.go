package vmrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/stats"
)

// SelfTestPath is where the latest self-test result lives.
func SelfTestPath(dataDir string) string { return path.Join(dataDir, "selftest.json") }

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
	} else if !res.WithoutGPU && !(res.legacy && !res.Passed && neverHandedOver(res.Problems)) {
		// A test by an agent up to v0.2.2 that failed because the host held
		// the GPU never handed it to a VM: it is no verdict on the GPU.
		res.LastFull = res.verdict()
	}
	return &res, nil
}

// neverHandedOver: a failed test whose VM never got the GPU, because the host
// held it (the refusals of agents up to v0.2.2).
func neverHandedOver(problems []string) bool {
	for _, p := range problems {
		for _, refusal := range []string{"the GPU is in use on this machine by", "this machine's desktop is running on the GPU",
			"is still in use on this machine by"} {
			if strings.Contains(p, refusal) {
				return true
			}
		}
	}
	return false
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
	// And with the firmware rentals boot with now: a pass without Secure Boot
	// says nothing of a rental that boots with it, nor the other way round.
	// The same for the cores they run on: a pass on one core type says nothing
	// of a rental with a vCPU on each of two.
	return f != nil && f.Passed && f.AgentVersion == fp.Version && f.LockedDown == fp.Secure && f.Cores == fp.Cores &&
		f.basis().problem(fp) == ""
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
//
// A machine whose rentals boot with Secure Boot is tested that way, and the
// test passes only when the VM says it is locked down. When the VM never came
// up at all with it, the test is run once more with the plain firmware: a pass
// there is this machine saying it cannot boot a VM with Secure Boot, which is
// recorded (SecureBootRecord) so that it keeps renting as it did, not locked
// down, rather than not at all.
//
// A machine with two core types is tested in the wider layout (layout.go) when
// it rents in it, and also when it has not tried it yet -- unless o.Standing.
// The wider layout is only ever an attempt: a test that does not pass in it is
// run again on the fastest cores alone, as the machine always rented, and a
// pass there is what is recorded, with the wider layout marked as one this
// machine cannot use (LayoutRecord).
func (rt *Runtime) SelfTestWith(ctx context.Context, version string, o TestOptions) SelfTestResult {
	switch {
	case rt.spec.EachOnOne():
		return rt.selfTestWider(ctx, version, o, rt.spec)
	case len(rt.spec.TrialCores) > 0 && !o.Standing:
		return rt.selfTestWider(ctx, version, o, rt.spec.Wider())
	}
	res := rt.selfTestOnce(ctx, version, o, true)
	if !rt.spec.Firmware.Secure || res.Passed || !res.neverBooted || res.Stopped || res.InUse != "" || ctx.Err() != nil {
		return res
	}
	plain, ok := FindFirmware(rt.h, rt.spec.Arch)
	if !ok || rt.Present() {
		return res
	}
	spec := rt.spec
	spec.Firmware = plain
	again := New(rt.h, spec, rt.fence, rt.verifyGPU).selfTestOnce(ctx, version, o, true)
	if !again.Passed {
		// It does not boot either way: the first failure stands.
		if !res.Stopped && res.InUse == "" {
			_ = SaveSelfTest(rt.h, rt.spec.DataDir, res)
		}
		return res
	}
	why := "the test rental never came up"
	if len(res.Problems) > 0 {
		why = res.Problems[0]
	}
	_ = MarkSecureBootUnusable(rt.h, rt.spec.DataDir, version, why)
	again.Notes = append(again.Notes, "a test rental did not come up with Secure Boot on this machine ("+why+
		"), and did without it: this machine's rentals are not locked down")
	_ = SaveSelfTest(rt.h, rt.spec.DataDir, again)
	return again
}

// selfTestWider is a test boot in the wider layout, wide, and the way back from
// it. The attempt itself is not recorded as the machine's test boot unless it
// is the verdict: a pass, or a teardown that did not verify.
func (rt *Runtime) selfTestWider(ctx context.Context, version string, o TestOptions, wide Spec) SelfTestResult {
	dir := rt.spec.DataDir
	res := New(rt.h, wide, rt.fence, rt.verifyGPU).selfTestOnce(ctx, version, o, false)
	switch {
	case res.Passed:
		_ = MarkLayout(rt.h, dir, version, wide.GuestCores, true, "")
		_ = SaveSelfTest(rt.h, dir, res)
		return res
	case rt.Present():
		// Its teardown did not verify: that keeps the machine from renting in
		// any layout, and is what stands, however the test ended.
		_ = SaveSelfTest(rt.h, dir, res)
		return res
	case res.Stopped || res.InUse != "" || ctx.Err() != nil:
		// Nothing was learned of the layout, and nothing is recorded.
		return res
	}
	why := "the test rental never came up"
	if len(res.Problems) > 0 {
		why = res.Problems[0]
	}
	narrow := wide.OneCoreType()
	again := New(rt.h, narrow, rt.fence, rt.verifyGPU).SelfTestWith(ctx, version, o)
	if !again.Passed {
		// It passes neither way, so the layout is not what is wrong: the
		// machine's own failure stands, and the wider layout is tried again
		// once that is mended.
		return again
	}
	_ = MarkLayout(rt.h, dir, version, wide.GuestCores, false, why)
	again.Notes = append(again.Notes, fmt.Sprintf("a test rental on %s, each vCPU on a core of its own, did not pass on this machine (%s), "+
		"and one on its %s cores alone did: this machine's rentals run on those", wide.GuestCPUName, why, narrow.GuestCPUName))
	_ = SaveSelfTest(rt.h, dir, again)
	return again
}

// selfTestOnce is one test boot with the runtime's firmware and cores. record
// false leaves selftest.json alone: the caller decides what the machine's
// verdict is.
func (rt *Runtime) selfTestOnce(ctx context.Context, version string, o TestOptions, record bool) SelfTestResult {
	save := func(res SelfTestResult) {
		if record {
			_ = SaveSelfTest(rt.h, rt.spec.DataDir, res)
		}
	}
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
		res.Cores = rt.spec.widerCores()
		if res.WithoutGPU {
			res.GPUVerified = false
		}
		return res
	}
	fail := func(problem string) SelfTestResult {
		res := stamp(SelfTestResult{Problems: []string{problem}})
		save(res)
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
		if st, _ := LoadState(rt.h, rt.spec.DataDir); st == nil && errors.Is(err, ErrGPUInUse) {
			// The host took the GPU back between the plan and the handover:
			// no verdict on the GPU, nothing recorded.
			res := stamp(SelfTestResult{Problems: []string{"the test VM did not start: " + err.Error()}})
			res.InUse = err.Error()
			return res
		}
		problem := "the test VM did not start: " + err.Error()
		dirty := false
		if st, _ := LoadState(rt.h, rt.spec.DataDir); st != nil && st.Dirty {
			dirty = true
			problem += "; and its cleanup did not verify, so this machine refuses rentals until that is fixed"
		}
		res := fail(problem)
		res.neverBooted = !dirty
		return res
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
	res.neverBooted = !rep.Begin
	res.GuestCPUs, res.GuestMemoryMB = rep.CPUCount, rep.MemoryMB
	// A VM in the wider layout has every CPU it was given, each the type of
	// the core it was pinned to, or the layout did not hold.
	if rt.spec.EachOnOne() && rep.End {
		if problem := widerProblem(rep, coreTypes(rt.h, rt.spec.GuestCores)); problem != "" {
			res.Passed, res.GPUVerified = false, false
			res.Problems = append(res.Problems, problem)
		}
	}
	if rep.SecureBoot != "" {
		res.Lockdown = rep.SecureBoot + " " + rep.Lockdown + " " + rep.RawMemory
	}
	// A rental that boots with Secure Boot is locked down, or the test fails:
	// the lock is the point, and only the VM can say it holds.
	if rt.spec.Firmware.Secure && rep.End {
		if rep.LockedDown() {
			res.LockedDown = true
		} else {
			res.Passed, res.GPUVerified = false, false
			res.Problems = append(res.Problems, fmt.Sprintf("the test VM booted with the Secure Boot firmware but is not locked down "+
				"(it says Secure Boot %s, kernel lockdown %s, the machine's memory %s to root)",
				orUnknown(rep.SecureBoot), orUnknown(rep.Lockdown), orUnknown(rep.RawMemory)))
		}
	}
	if note := DMUdevNote(rt.h, rt.spec.DataDir, rt.spec.AgentVersion); note != "" && res.Passed {
		res.Notes = append(res.Notes, note)
	}
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
	save(res)
	return res
}

// widerProblem says what a test VM in the wider layout reported of its CPUs
// that is not what it was given, or "": want is the core type of the host CPU
// each vCPU was pinned to, in order.
func widerProblem(rep SerialReport, want []string) string {
	switch {
	case rep.CPUCount == 0:
		return "the test VM did not say which CPUs it has"
	case rep.CPUCount != len(want) || len(rep.CPUTypes) != len(want):
		return fmt.Sprintf("the test VM came up with %d CPUs (%d named by type), and was given %d", rep.CPUCount, len(rep.CPUTypes), len(want))
	}
	for i, t := range want {
		if t == "" || rep.CPUTypes[i] != t {
			return fmt.Sprintf("the test VM's CPU %d is a %s, and it was pinned to a %s core", i, coreTypeName(rep.CPUTypes[i]), coreTypeName(t))
		}
	}
	return ""
}

// coreTypeName is a core type ("0x41/0xd0b") in words ("Cortex-A76").
func coreTypeName(t string) string {
	impl, part, ok := strings.Cut(t, "/")
	if !ok {
		return "core of an unknown type"
	}
	return stats.CoreName(impl, part)
}
