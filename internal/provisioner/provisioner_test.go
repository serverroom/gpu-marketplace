package provisioner

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/netguard"
)

type fakeRunner struct {
	failCmd map[string]bool   // command name -> should error
	output  map[string]string // command name -> Output result
	calls   []string          // every command name invoked, in order
	args    [][]string        // the arguments of each call, in the same order
}

func (f *fakeRunner) Run(name string, args ...string) error {
	f.calls = append(f.calls, name)
	f.args = append(f.args, args)
	if f.failCmd[name] {
		return fmt.Errorf("simulated failure: %s", name)
	}
	return nil
}

func (f *fakeRunner) Output(name string, args ...string) (string, error) {
	f.calls = append(f.calls, name)
	f.args = append(f.args, args)
	if f.failCmd[name] {
		return "", fmt.Errorf("simulated failure: %s", name)
	}
	return f.output[name], nil
}

type fakeFence struct {
	applyErr error
	applied  int
	removed  int
}

func (f *fakeFence) Apply() error  { f.applied++; return f.applyErr }
func (f *fakeFence) Remove() error { f.removed++; return nil }

// ready marks a provisioner as having passed preflight, with a fence that
// always verifies, so each test exercises the part it is about.
func ready(p *Provisioner) (*Provisioner, *fakeFence) {
	fence := &fakeFence{}
	p.fence = fence
	p.capability = control.Capability{Ready: true, Kind: KindKataVFIO}
	return p, fence
}

func newProv(r Runner) *Provisioner {
	p, _ := ready(New(r, "/img/ubuntu.img", "/disks", []string{"0000:01:00.0"}, VendorNVIDIA))
	return p
}

func TestProvisionSetsRented(t *testing.T) {
	p := newProv(&fakeRunner{})
	if err := p.Provision("R1", "ssh-ed25519 K"); err != nil {
		t.Fatal(err)
	}
	if p.Status() != StatusRented {
		t.Errorf("status = %q, want rented", p.Status())
	}
}

// The v0.1.5 failure: a provision that "succeeds" on a machine that cannot
// host. An unchecked provisioner must refuse, and touch nothing on the way.
func TestUncheckedProvisionerRefuses(t *testing.T) {
	r := &fakeRunner{}
	p := New(r, "/img/ubuntu.img", "/disks", []string{"0000:01:00.0"}, VendorNVIDIA)
	err := p.Provision("R1", "ssh-ed25519 K")
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("Provision = %v, want ErrNotReady", err)
	}
	if len(r.calls) != 0 || p.Status() != StatusFree {
		t.Errorf("a refused rental ran %v and left status %q", r.calls, p.Status())
	}
}

// The fence goes up before anything boots, and if it does not verify nothing
// boots at all: a tenant must never get a machine that can see the host's LAN.
func TestNoBootWithoutAVerifiedFence(t *testing.T) {
	r := &fakeRunner{}
	p := newProv(r)
	p.fence = &fakeFence{applyErr: netguard.ErrNotVerified}
	if err := p.Provision("R1", "ssh-ed25519 K"); err == nil {
		t.Fatal("Provision succeeded without a verified network fence")
	}
	for _, c := range r.calls {
		if c == "gpu-agent-kata" || c == "gpu-agent-mkdisk" {
			t.Errorf("%s ran before the fence verified", c)
		}
	}
	if p.Status() != StatusFree {
		t.Errorf("status = %q, want free", p.Status())
	}
}

func TestMicroVMIsAttachedOnlyToTheFencedBridge(t *testing.T) {
	r := &fakeRunner{}
	p := newProv(r)
	if err := p.Provision("R1", "ssh-ed25519 K"); err != nil {
		t.Fatal(err)
	}
	for i, c := range r.calls {
		if c == "gpu-agent-kata" {
			joined := strings.Join(r.args[i], " ")
			if !strings.Contains(joined, "--bridge "+netguard.Bridge) {
				t.Errorf("microVM booted without --bridge %s: %s", netguard.Bridge, joined)
			}
			return
		}
	}
	t.Fatal("gpu-agent-kata was never called")
}

func TestProvisionRejectsUnsafeRentalID(t *testing.T) {
	r := &fakeRunner{}
	p := newProv(r)
	if err := p.Provision("../../etc/passwd", "ssh-ed25519 K"); err == nil {
		t.Fatal("Provision accepted a path-traversal rental id")
	}
	if len(r.calls) != 0 {
		t.Errorf("an invalid id reached the host: %v", r.calls)
	}
}

func TestSecondProvisionIsRefused(t *testing.T) {
	p := newProv(&fakeRunner{})
	if err := p.Provision("R1", "ssh-ed25519 K"); err != nil {
		t.Fatal(err)
	}
	if err := p.Provision("R2", "ssh-ed25519 K"); err == nil {
		t.Fatal("a rented machine accepted a second rental")
	}
}

func TestTeardownFreeWhenWipeAndResetVerify(t *testing.T) {
	r := &fakeRunner{output: map[string]string{"nvidia-smi": "12\n8"}} // VRAM clear
	p := newProv(r)
	fence := p.fence.(*fakeFence)
	if err := p.Teardown("R1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Status() != StatusFree {
		t.Errorf("status = %q, want free", p.Status())
	}
	if fence.removed != 1 {
		t.Errorf("fence removed %d times, want 1", fence.removed)
	}
}

func TestTeardownDirtyWhenGPUResetFails(t *testing.T) {
	r := &fakeRunner{failCmd: map[string]bool{"nvidia-smi": true}}
	p := newProv(r)
	if err := p.Teardown("R1"); err == nil {
		t.Fatal("expected an error when GPU reset fails")
	}
	if p.Status() != StatusDirty {
		t.Errorf("status = %q, want dirty (fail closed)", p.Status())
	}
}

func TestTeardownDirtyWhenVRAMNotClear(t *testing.T) {
	r := &fakeRunner{output: map[string]string{"nvidia-smi": "9000\n8"}} // a GPU still dirty
	p := newProv(r)
	if err := p.Teardown("R1"); err == nil {
		t.Fatal("expected an error when VRAM is not clear")
	}
	if p.Status() != StatusDirty {
		t.Errorf("status = %q, want dirty", p.Status())
	}
}

func TestTeardownDirtyWhenDiskWipeFails(t *testing.T) {
	r := &fakeRunner{
		failCmd: map[string]bool{"rm": true}, // overlay delete fails
		output:  map[string]string{"nvidia-smi": "5\n5"},
	}
	p := newProv(r)
	if err := p.Teardown("R1"); err == nil {
		t.Fatal("expected an error when the disk wipe fails")
	}
	if p.Status() != StatusDirty {
		t.Errorf("status = %q, want dirty", p.Status())
	}
}

func TestVerifyVRAMClear(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"5\n10", true},
		{"5", true},
		{"600\n5", false}, // one GPU over threshold
		{"", false},       // no output -> not clear
		{"abc", false},    // unparseable -> not clear
		{"[N/A]", false},  // unified memory reports no figure -> not clear
	}
	for _, c := range cases {
		if got := VerifyVRAMClear(c.out); got != c.want {
			t.Errorf("VerifyVRAMClear(%q) = %v, want %v", c.out, got, c.want)
		}
	}
}

// An AMD host used to shell out to nvidia-smi, which is not installed there, so
// the reset failed, the turnover could not be verified, and the box quarantined
// itself dirty on its FIRST teardown - permanently unrentable.
func TestAMDHostResetsWithRocmSMI(t *testing.T) {
	r := &fakeRunner{}
	r.output = map[string]string{
		"rocm-smi": "device,VRAM Total Memory (B),VRAM Total Used Memory (B)\ncard0,17163091968,4194304\n",
	}
	p, _ := ready(New(r, "/img/ubuntu.img", "/disks", []string{"0"}, VendorAMD))
	if err := p.Provision("r1", "ssh-ed25519 KEY"); err != nil {
		t.Fatalf("Provision on an AMD host: %v", err)
	}
	if err := p.Teardown("r1"); err != nil {
		t.Fatalf("Teardown on an AMD host: %v", err)
	}
	if p.Status() != StatusFree {
		t.Errorf("status = %q, want %q - an AMD host must return to the pool", p.Status(), StatusFree)
	}
	if strings.Contains(strings.Join(r.calls, " "), "nvidia-smi") {
		t.Errorf("an AMD host shelled out to nvidia-smi: %v", r.calls)
	}
}

// Apple Silicon has no passthrough path at all. Refuse the rental up front
// rather than accepting it and bricking the host at teardown.
func TestAppleHostRefusesToProvision(t *testing.T) {
	p := New(&fakeRunner{}, "/img/ubuntu.img", "/disks", nil, VendorApple)
	err := p.Provision("r1", "ssh-ed25519 KEY")
	if !errors.Is(err, ErrVendorCannotIsolate) {
		t.Fatalf("Provision on Apple = %v, want ErrVendorCannotIsolate", err)
	}
	if p.Status() != StatusFree {
		t.Errorf("a refused rental left status %q", p.Status())
	}
}

func TestVerifyAMDVRAMClear(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"clear", "device,VRAM Total Used Memory (B)\ncard0,4194304\n", true},
		{"in use", "device,VRAM Total Used Memory (B)\ncard0,8589934592\n", false},
		{"one dirty of two", "device,VRAM Total Used Memory (B)\ncard0,4194304\ncard1,8589934592\n", false},
		{"no rows", "device,VRAM Total Used Memory (B)\n", false},
		{"unparseable", "device,VRAM Total Used Memory (B)\ncard0,n/a\n", false},
		{"no used column", "device,VRAM Total Memory (B)\ncard0,17163091968\n", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		if got := VerifyAMDVRAMClear(tt.in); got != tt.want {
			t.Errorf("%s: VerifyAMDVRAMClear = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// ---- preflight ----

type fakeProbe struct {
	exists  map[string]bool
	entries map[string]int
	missing map[string]bool
}

func (f fakeProbe) LookPath(name string) (string, error) {
	if f.missing[name] {
		return "", errors.New("not found")
	}
	return "/usr/bin/" + name, nil
}
func (f fakeProbe) Exists(path string) bool     { return f.exists[path] }
func (f fakeProbe) CountEntries(dir string) int { return f.entries[dir] }

func goodHost() fakeProbe {
	return fakeProbe{
		exists:  map[string]bool{"/dev/kvm": true, "/img/golden.img": true},
		entries: map[string]int{"/sys/kernel/iommu_groups": 42},
		missing: map[string]bool{},
	}
}

func TestPreflightReadyHost(t *testing.T) {
	r := &fakeRunner{output: map[string]string{"nvidia-smi": "00000000:01:00.0, 81559\n00000000:41:00.0, 81559\n"}}
	p := Detect(r, goodHost(), "linux", "/img/golden.img", "/disks", "v0.1.6")
	c := p.Capability()
	if !c.Ready {
		t.Fatalf("a good host is not ready: %v", c.Reasons)
	}
	if c.AgentVersion != "v0.1.6" || c.Kind != KindKataVFIO {
		t.Errorf("capability = %+v", c)
	}
	if strings.Join(p.gpuBDFs, " ") != "0000:01:00.0 0000:41:00.0" {
		t.Errorf("BDFs = %v", p.gpuBDFs)
	}
}

// A DGX Spark: one GB10 whose memory is the host's. It has no memory of its own
// to hand a tenant or to prove clean, so it must be reported as not ready.
func TestPreflightRefusesUnifiedMemoryGPU(t *testing.T) {
	r := &fakeRunner{output: map[string]string{"nvidia-smi": "0000000F:01:00.0, [N/A]\n"}}
	rep := Preflight(r, goodHost(), "linux", "/img/golden.img")
	if len(rep.Reasons) != 1 || !strings.Contains(rep.Reasons[0], "shares its memory with the host") {
		t.Fatalf("reasons = %v, want the unified-memory refusal", rep.Reasons)
	}
	if !strings.Contains(rep.Reasons[0], "000f:01:00.0") {
		t.Errorf("reason does not name the GPU: %q", rep.Reasons[0])
	}
}

// The stock install: no runtime helpers. It must say so, not claim readiness.
func TestPreflightReportsMissingRuntime(t *testing.T) {
	probe := goodHost()
	probe.missing = map[string]bool{"gpu-agent-kata": true, "gpu-agent-mkdisk": true, "gpu-agent-injectkey": true}
	r := &fakeRunner{output: map[string]string{"nvidia-smi": "00000000:01:00.0, 24564\n"}}
	rep := Preflight(r, probe, "linux", "/img/golden.img")
	if len(rep.Reasons) != 1 || !strings.Contains(rep.Reasons[0], "gpu-agent-kata, gpu-agent-mkdisk, gpu-agent-injectkey") {
		t.Fatalf("reasons = %v", rep.Reasons)
	}
}

func TestPreflightReportsEveryFailingCheck(t *testing.T) {
	probe := fakeProbe{missing: map[string]bool{"nft": true}}
	r := &fakeRunner{failCmd: map[string]bool{"nvidia-smi": true, "rocm-smi": true}}
	rep := Preflight(r, probe, "linux", "/img/golden.img")
	want := []string{"/dev/kvm", "IOMMU", "no NVIDIA or AMD GPU", "missing: nft"}
	if len(rep.Reasons) != len(want) {
		t.Fatalf("reasons = %v", rep.Reasons)
	}
	for i, w := range want {
		if !strings.Contains(rep.Reasons[i], w) {
			t.Errorf("reason %d = %q, want it to mention %q", i, rep.Reasons[i], w)
		}
	}
}

func TestPreflightNonLinux(t *testing.T) {
	for goos, want := range map[string]string{"windows": "runs windows", "darwin": "Apple Silicon"} {
		rep := Preflight(&fakeRunner{}, goodHost(), goos, "/img/golden.img")
		if len(rep.Reasons) != 1 || !strings.Contains(rep.Reasons[0], want) {
			t.Errorf("%s: reasons = %v", goos, rep.Reasons)
		}
	}
}

func TestNormalizeBDF(t *testing.T) {
	cases := map[string]string{
		"00000000:01:00.0": "0000:01:00.0",
		"0000000F:01:00.0": "000f:01:00.0",
		"0000:03:00.0":     "0000:03:00.0",
		"card0":            "",
		"":                 "",
	}
	for in, want := range cases {
		if got := NormalizeBDF(in); got != want {
			t.Errorf("NormalizeBDF(%q) = %q, want %q", in, got, want)
		}
	}
}
