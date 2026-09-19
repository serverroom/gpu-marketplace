package autosetup

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// useGPU has the host's llama-server hold the GPU (or let go of it).
func useGPU(h *fakehost.Host, on bool) {
	if on {
		h.SetLink("/proc/11435/fd/9", "/dev/nvidia0")
		h.SetFile("/proc/11435/comm", []byte("llama-server\n"))
		return
	}
	h.DeleteLink("/proc/11435/fd/9")
}

func (r *rig) testRuns() []vmrt.TestOptions {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]vmrt.TestOptions(nil), r.tests...)
}

// The test a new machine needs runs at once, even with the host's programs on
// the GPU: without the GPU, which makes the machine sellable; the full test
// is owed from then on.
func TestAFirstTestBootLeavesAGPUInUseAlone(t *testing.T) {
	h := machine(t)
	noTestBoot(h)
	useGPU(h, true)
	r := newRig(t, h)
	r.run()
	if runs := r.testRuns(); len(runs) != 1 || !runs[0].NoGPU {
		t.Fatalf("test boots = %+v", runs)
	}
	c := r.d.Prov.Capability()
	if !c.Ready || c.SelfTest == nil || !c.SelfTest.Passed || c.SelfTest.GPUVerified || !c.RetestPending {
		t.Fatalf("capability ready=%v %v selftest=%+v retest=%v", c.Ready, c.Reasons, c.SelfTest, c.RetestPending)
	}
	if c.HostBusy == nil || strings.Join(c.HostBusy.Holders, ",") != "llama-server (pid 11435)" {
		t.Errorf("host_busy = %+v", c.HostBusy)
	}
	if h.Ran("run kill") || h.Ran("run systemctl stop") {
		t.Error("the setup touched a program of the host's")
	}
}

// After an update the machine sells on its earlier pass and runs the full
// test when the GPU is free -- never while the host uses it.
func TestKeepRetestsWhenTheGPUIsFree(t *testing.T) {
	h := machine(t)
	passed(h, "v0.1.9") // the last version's full pass, on the image still on disk
	useGPU(h, true)
	r := newRig(t, h)
	c := r.d.Prov.Capability()
	if !c.Ready || !c.RetestPending {
		t.Fatalf("after the update: ready=%v %v retest=%v", c.Ready, c.Reasons, c.RetestPending)
	}
	r.d.KeepOnce(context.Background(), true)
	if runs := r.testRuns(); len(runs) != 0 {
		t.Fatalf("a retest ran while the host used the GPU: %+v", runs)
	}
	useGPU(h, false)
	r.d.KeepOnce(context.Background(), false)
	if runs := r.testRuns(); len(runs) != 1 || runs[0].NoGPU {
		t.Fatalf("retests = %+v", runs)
	}
	c = r.d.Prov.Capability()
	if !c.Ready || c.RetestPending || !c.SelfTest.GPUVerified || c.SelfTest.AgentVersion != version {
		t.Errorf("after the retest: ready=%v retest=%v selftest=%+v", c.Ready, c.RetestPending, c.SelfTest)
	}
	rs := r.reasons()
	if len(rs) != 2 || !strings.HasPrefix(rs[0], "Checking this machine with a test rental (started 14:05 UTC)") || rs[1] != "" {
		t.Errorf("reported %q", rs)
	}
	// Nothing is owed any more: the next looks do nothing.
	r.d.KeepOnce(context.Background(), false)
	if len(r.testRuns()) != 1 {
		t.Error("a second retest ran")
	}
}

// A failed full test -- the one before a rental, say -- is not lifted by a
// test without the GPU: only a full test clears it, run when the GPU is free
// and, after a failure, not before RetryAfter (or the agent's next start).
func TestAFailedGPUTestIsRetriedOnlyWithTheGPU(t *testing.T) {
	h := machine(t)
	r := newRig(t, h)
	rt := r.d.Prov.Runtime()
	fp := rt.Fingerprint(version)
	failedAt := r.clock().Add(-time.Hour).Unix()
	_ = vmrt.SaveSelfTest(h, dataDir, vmrt.SelfTestResult{AgentVersion: version, HostGPUs: fp.GPUs, HostDriver: fp.HostDriver,
		BaseImage: fp.BaseImage, At: failedAt, Problems: []string{"the VM did not see GPU 0000:01:00.0"}})
	_ = vmrt.SaveSelfTest(h, dataDir, vmrt.SelfTestResult{Passed: true, WithoutGPU: true, AgentVersion: version, HostGPUs: fp.GPUs,
		HostDriver: fp.HostDriver, BaseImage: fp.BaseImage, At: failedAt + 60})
	// The setup itself passed for this machine long ago.
	a := r.d.Runner.Begin(Plan{Steps: []Step{StepTestBoot}}, fp.GPUs, ByAgent)
	a.Passed, a.FinishedAt = true, failedAt-3600
	_ = Save(h, dataDir, a)
	r = r.restart()
	if c := r.d.Prov.Capability(); c.Ready || !strings.Contains(strings.Join(c.Reasons, " "), "could not be handed to its last full test rental") {
		t.Fatalf("capability ready=%v %v", c.Ready, c.Reasons)
	}

	useGPU(h, true)
	r.d.KeepOnce(context.Background(), true)
	useGPU(h, false)
	r.d.KeepOnce(context.Background(), false)
	if runs := r.testRuns(); len(runs) != 0 {
		t.Fatalf("tests ran: with the GPU in use, or within the hour after the failure: %+v", runs)
	}
	r.mu.Lock()
	r.now = r.now.Add(RetryAfter)
	r.mu.Unlock()
	r.d.KeepOnce(context.Background(), false)
	if runs := r.testRuns(); len(runs) != 1 || runs[0].NoGPU {
		t.Fatalf("tests = %+v", runs)
	}
	if c := r.d.Prov.Capability(); !c.Ready || c.RetestPending {
		t.Errorf("after the full test: ready=%v %v retest=%v", c.Ready, c.Reasons, c.RetestPending)
	}
}

// A machine whose own setup record passed but which needs a test boot again
// (its image rebuilt by hand) gets it from Keep, as the setup will not run
// twice for one key.
func TestKeepRunsATestTheSetupWillNot(t *testing.T) {
	h := machine(t)
	r := newRig(t, h)
	fp := r.d.Prov.Runtime().Fingerprint(version)
	a := r.d.Runner.Begin(Plan{Steps: []Step{StepTestBoot}}, fp.GPUs, ByAgent)
	a.Passed, a.FinishedAt = true, r.clock().Add(-time.Hour).Unix()
	_ = Save(h, dataDir, a)
	imageBuiltAt(h, r.clock().Unix())
	passed(h, "v0.1.9")
	r = r.restart()
	if r.d.Prov.Capability().Ready {
		t.Fatal("a test from before the image was rebuilt still counts")
	}
	r.run()
	if len(r.testRuns()) != 0 {
		t.Fatal("the setup ran a key it had passed")
	}
	r.d.KeepOnce(context.Background(), false)
	if runs := r.testRuns(); len(runs) != 1 || runs[0].NoGPU {
		t.Fatalf("tests = %+v", runs)
	}
	if !r.d.Prov.Capability().Ready {
		t.Errorf("not ready after the test: %v", r.d.Prov.Capability().Reasons)
	}
}

// Keep does not run with the automatic setup off.
func TestKeepStaysOffWithTheSetupOff(t *testing.T) {
	h := machine(t)
	passed(h, "v0.1.9")
	r := newRig(t, h)
	if err := os.WriteFile(OptOutPath(r.d.ConfigDir), []byte("off\n"), 0644); err != nil {
		t.Fatal(err)
	}
	r.d.KeepOnce(context.Background(), true)
	if len(r.testRuns()) != 0 {
		t.Fatal("a retest ran with the automatic setup off")
	}
}

// With the automatic setup off, what it would have done is a problem for the
// marketplace, not only a line in the log; turned on, the problem goes.
func TestSetupOffIsAProblemWhileItHasWork(t *testing.T) {
	h := machine(t)
	noTestBoot(h)
	r := newRig(t, h)
	probs := &fakeProblems{}
	r.d.Errors = probs
	if err := os.WriteFile(OptOutPath(r.d.ConfigDir), []byte("off\n"), 0644); err != nil {
		t.Fatal(err)
	}
	r.run()
	if len(probs.raised) != 1 || probs.raised[0] != control.AreaSetup+": "+SetupOffProblem {
		t.Fatalf("raised = %q", probs.raised)
	}
	if err := os.Remove(OptOutPath(r.d.ConfigDir)); err != nil {
		t.Fatal(err)
	}
	r = r.restart()
	r.d.Errors = probs
	r.run()
	found := false
	for _, s := range probs.resolved {
		found = found || s == control.AreaSetup+": "+SetupOffProblem
	}
	if !found {
		t.Errorf("resolved = %q", probs.resolved)
	}
}

// A test the host cut in on (the GPU taken as the test came to it) is run
// again without the GPU, as if the host had been faster; nothing is recorded
// for the first try.
func TestATestTheHostCutInOnRunsWithoutTheGPU(t *testing.T) {
	h := machine(t)
	noTestBoot(h)
	r := newRig(t, h)
	fake := r.d.Runner.TestBoot
	tries := 0
	r.d.Runner.TestBoot = func(ctx context.Context, rt *vmrt.Runtime, o vmrt.TestOptions) vmrt.SelfTestResult {
		tries++
		if !o.NoGPU {
			return vmrt.SelfTestResult{InUse: "the GPU is in use on this machine by llama-server (pid 11500); stop them first"}
		}
		return fake(ctx, rt, o)
	}
	r.run()
	c := r.d.Prov.Capability()
	if tries != 2 || !c.Ready || c.SelfTest == nil || c.SelfTest.GPUVerified {
		t.Errorf("tries %d ready %v %v selftest %+v", tries, c.Ready, c.Reasons, c.SelfTest)
	}
}
