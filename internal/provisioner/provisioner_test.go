package provisioner

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

type fakeMachine struct {
	startErr error
	stopRes  vmrt.StopResult
	started  int
	stopped  int
	alive    bool
	present  bool
	dirty    bool
	lastID   string
	last     vmrt.StartOptions
}

func (m *fakeMachine) Start(o vmrt.StartOptions) error {
	m.started++
	m.lastID = o.ID
	m.last = o
	if m.startErr == nil {
		m.present, m.alive = true, true
	}
	return m.startErr
}

func (m *fakeMachine) Stop() vmrt.StopResult {
	m.stopped++
	m.alive = false
	if m.stopRes.Clean() {
		m.present = false
	} else {
		m.dirty = true
	}
	return m.stopRes
}

func (m *fakeMachine) Alive() bool   { return m.alive }
func (m *fakeMachine) Present() bool { return m.present }
func (m *fakeMachine) Dirty() bool   { return m.dirty }

type fakeForward struct{ stopped *int }

func (f fakeForward) Stop() { *f.stopped++ }

func withFakeForward(t *testing.T, fail error) *int {
	t.Helper()
	stops := 0
	orig := startForward
	startForward = func(listen, target string) (stopper, error) {
		if fail != nil {
			return nil, fail
		}
		return fakeForward{&stops}, nil
	}
	t.Cleanup(func() { startForward = orig })
	return &stops
}

func clean() vmrt.StopResult { return vmrt.StopResult{Wiped: true, GPUClean: true} }

func readyProv(m Machine) *Provisioner {
	p := New(m, VendorPCI, []string{"0000:01:00.0"}, false)
	p.capability = control.Capability{Ready: true, Kind: KindQEMUVFIO}
	p.async = false
	return p
}

func renterKey(t *testing.T) string {
	t.Helper()
	k, err := vmrt.ThrowawayPubkey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestProvisionRentsAndOpensTheSSHForward(t *testing.T) {
	stops := withFakeForward(t, nil)
	m := &fakeMachine{stopRes: clean()}
	p := readyProv(m)
	if err := p.Provision("R1", renterKey(t)); err != nil {
		t.Fatal(err)
	}
	if p.Status() != StatusRented || m.lastID != "R1" {
		t.Errorf("status = %q id = %q", p.Status(), m.lastID)
	}
	if err := p.Teardown("R1"); err != nil {
		t.Fatal(err)
	}
	if p.Status() != StatusFree || *stops != 1 {
		t.Errorf("status = %q, forward stopped %d times", p.Status(), *stops)
	}
}

// The v0.1.5 failure: a provision that "succeeds" on a machine that cannot
// host. An unchecked provisioner must refuse, and touch nothing on the way.
func TestUncheckedProvisionerRefuses(t *testing.T) {
	m := &fakeMachine{}
	p := New(m, VendorPCI, []string{"0000:01:00.0"}, false)
	if err := p.Provision("R1", renterKey(t)); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Provision = %v, want ErrNotReady", err)
	}
	if m.started != 0 || p.Status() != StatusFree {
		t.Errorf("a refused rental started (%d) or left status %q", m.started, p.Status())
	}
}

func TestAppleHostRefusesToProvision(t *testing.T) {
	p := New(&fakeMachine{}, VendorApple, nil, true)
	p.capability.Ready = true
	if err := p.Provision("r1", renterKey(t)); !errors.Is(err, ErrVendorCannotIsolate) {
		t.Fatalf("Provision on Apple = %v, want ErrVendorCannotIsolate", err)
	}
}

func TestProvisionRejectsBadInput(t *testing.T) {
	m := &fakeMachine{}
	p := readyProv(m)
	if err := p.Provision("../../etc/passwd", renterKey(t)); err == nil {
		t.Error("a path-traversal rental id was accepted")
	}
	if err := p.Provision("R1", "not a key"); err == nil {
		t.Error("a bad renter key was accepted")
	}
	if m.started != 0 {
		t.Errorf("bad input reached the machine")
	}
}

func TestFailedStartIsReportedAndLeavesTheMachineFree(t *testing.T) {
	withFakeForward(t, nil)
	m := &fakeMachine{startErr: errors.New("boot microVM: no IOMMU group")}
	p := readyProv(m)
	if err := p.Provision("R1", renterKey(t)); err == nil {
		t.Fatal("a failed start was reported as success")
	}
	if p.Status() != StatusFree || !strings.Contains(p.LastError(), "no IOMMU group") {
		t.Errorf("status = %q lastErr = %q", p.Status(), p.LastError())
	}
}

func TestFailedStartThatLeftADirtyMachineQuarantines(t *testing.T) {
	withFakeForward(t, nil)
	m := &fakeMachine{startErr: errors.New("boot failed"), dirty: true}
	p := readyProv(m)
	p.Provision("R1", renterKey(t))
	if p.Status() != StatusDirty {
		t.Errorf("status = %q, want dirty", p.Status())
	}
}

func TestForwardThatCannotOpenStopsTheVM(t *testing.T) {
	withFakeForward(t, errors.New("address in use"))
	m := &fakeMachine{stopRes: clean()}
	p := readyProv(m)
	if err := p.Provision("R1", renterKey(t)); err == nil {
		t.Fatal("a rental nobody can log in to was reported as up")
	}
	if m.stopped != 1 || p.Status() != StatusFree {
		t.Errorf("VM stopped %d times, status %q", m.stopped, p.Status())
	}
}

func TestSecondProvisionIsRefused(t *testing.T) {
	withFakeForward(t, nil)
	p := readyProv(&fakeMachine{stopRes: clean()})
	if err := p.Provision("R1", renterKey(t)); err != nil {
		t.Fatal(err)
	}
	if err := p.Provision("R2", renterKey(t)); err == nil {
		t.Fatal("a rented machine accepted a second rental")
	}
}

func TestUnverifiedTeardownQuarantines(t *testing.T) {
	withFakeForward(t, nil)
	m := &fakeMachine{stopRes: vmrt.StopResult{Wiped: false, GPUClean: true, Detail: []string{"mapping still open"}}}
	p := readyProv(m)
	p.Provision("R1", renterKey(t))
	err := p.Teardown("R1")
	if err == nil || p.Status() != StatusDirty || !strings.Contains(p.LastError(), "mapping still open") {
		t.Fatalf("Teardown = %v status = %q lastErr = %q", err, p.Status(), p.LastError())
	}
	if err := p.Provision("R2", renterKey(t)); err == nil {
		t.Errorf("a dirty machine accepted a rental")
	}
}

func TestTeardownWhileStartingIsRefused(t *testing.T) {
	p := readyProv(&fakeMachine{})
	p.status = StatusProvisioning
	if err := p.Teardown("R1"); err == nil {
		t.Fatal("teardown ran into a start in progress")
	}
}

func TestResume(t *testing.T) {
	withFakeForward(t, nil)

	running := readyProv(&fakeMachine{present: true, alive: true})
	running.Resume()
	if running.Status() != StatusRented || running.forward == nil {
		t.Errorf("a running VM was not picked back up: %q", running.Status())
	}

	gone := &fakeMachine{present: true, stopRes: clean()}
	rebooted := readyProv(gone)
	rebooted.Resume()
	if rebooted.Status() != StatusFree || gone.stopped != 1 {
		t.Errorf("a rental whose VM is gone was not cleaned up: %q stopped=%d", rebooted.Status(), gone.stopped)
	}

	dirty := readyProv(&fakeMachine{present: true, dirty: true})
	dirty.Resume()
	if dirty.Status() != StatusDirty {
		t.Errorf("a dirty leftover did not quarantine: %q", dirty.Status())
	}
}

type fakeRunner struct {
	outputs map[string]string
	fail    map[string]bool
	// missing tools answer LookPath with an error; every other tool is installed.
	missing map[string]bool
}

func (f *fakeRunner) Run(name string, args ...string) error { return nil }

func (f *fakeRunner) LookPath(name string) (string, error) {
	if f.missing[name] {
		return "", fmt.Errorf("%s: not found", name)
	}
	return "/usr/bin/" + name, nil
}

func (f *fakeRunner) Output(name string, args ...string) (string, error) {
	k := name + " " + strings.Join(args, " ")
	for prefix, fail := range f.fail {
		if fail && strings.HasPrefix(k, prefix) {
			return "", fmt.Errorf("simulated failure: %s", k)
		}
	}
	for prefix, out := range f.outputs {
		if strings.HasPrefix(k, prefix) {
			return out, nil
		}
	}
	return "", nil
}

func noSleep(t *testing.T) {
	orig := verifySleep
	verifySleep = func() {}
	t.Cleanup(func() { verifySleep = orig })
}

// returned is what a rental hands back: its GPU with the driver it came from,
// and the GPU's audio function.
func returned(bdf, driver string) []vmrt.BoundDevice {
	return []vmrt.BoundDevice{{BDF: bdf, Driver: driver}, {BDF: bdf[:len(bdf)-1] + "1", Driver: "snd_hda_intel"}}
}

func TestGPUVerifier(t *testing.T) {
	noSleep(t)
	rented := returned("0000:01:00.0", "nvidia")

	discrete := &fakeRunner{outputs: map[string]string{
		"nvidia-smi --query-gpu=pci.bus_id,name,memory.used": "00000000:01:00.0, NVIDIA L4, 3\n",
	}}
	if !gpuVerifier(discrete)(rented) {
		t.Error("a clear discrete GPU did not verify")
	}
	discrete.outputs["nvidia-smi --query-gpu=pci.bus_id,name,memory.used"] = "00000000:01:00.0, NVIDIA L4, 9000\n"
	if gpuVerifier(discrete)(rented) {
		t.Error("a discrete GPU with memory in use verified")
	}

	unified := &fakeRunner{outputs: map[string]string{
		"nvidia-smi --query-gpu=pci.bus_id,name,memory.used": "0000000F:01:00.0, NVIDIA GB10, [N/A]\n",
		"nvidia-smi --query-compute-apps":                    "",
	}}
	gb10 := returned("000f:01:00.0", "nvidia")
	if !gpuVerifier(unified)(gb10) {
		t.Error("an idle unified-memory GPU did not verify")
	}
	unified.outputs["nvidia-smi --query-compute-apps"] = "0000000F:01:00.0, 4242\n"
	if gpuVerifier(unified)(gb10) {
		t.Error("a unified-memory GPU still held by a process verified")
	}

	missing := &fakeRunner{outputs: map[string]string{"nvidia-smi --query-gpu=pci.bus_id,name,memory.used": "00000000:41:00.0, NVIDIA L4, 3\n"}}
	if gpuVerifier(missing)(rented) {
		t.Error("a GPU that never came back to its driver verified")
	}

	amd := returned("0000:41:00.0", "amdgpu")
	rocm := &fakeRunner{outputs: map[string]string{"rocm-smi --showbus --showmeminfo vram --csv": "device,PCI Bus,VRAM Total Used Memory (B)\ncard0,0000:41:00.0,4194304\n"}}
	if !gpuVerifier(rocm)(amd) {
		t.Error("a clear AMD GPU did not verify")
	}
	rocm.outputs["rocm-smi --showbus --showmeminfo vram --csv"] = "device,PCI Bus,VRAM Total Used Memory (B)\ncard0,0000:41:00.0,8589934592\n"
	if gpuVerifier(rocm)(amd) {
		t.Error("an AMD GPU with memory in use verified")
	}
}

// An NVIDIA GPU nvidia-smi gives no used-memory figure for, and that is not a
// known unified part, is checked by what holds it -- not failed for the figure.
func TestGPUVerifierChecksANoFigureGPUByItsHolders(t *testing.T) {
	noSleep(t)
	r := &fakeRunner{outputs: map[string]string{
		"nvidia-smi --query-gpu=pci.bus_id,name,memory.used": "00000000:01:00.0, NVIDIA Mystery, [N/A]\n",
		"nvidia-smi --query-compute-apps":                    "",
	}}
	if !gpuVerifier(r)(returned("0000:01:00.0", "nvidia")) {
		t.Error("an idle GPU without a memory figure did not verify")
	}
	r.outputs["nvidia-smi --query-compute-apps"] = "00000000:01:00.0, 4242\n"
	if gpuVerifier(r)(returned("0000:01:00.0", "nvidia")) {
		t.Error("a GPU still held by a process verified")
	}
}

// Only the rented GPUs are the rental's. A GPU the rental left out may be busy
// with the provider's own work, and that is no reason to quarantine the machine.
func TestGPUVerifierLooksOnlyAtTheRentedGPUs(t *testing.T) {
	noSleep(t)
	nv := &fakeRunner{outputs: map[string]string{
		"nvidia-smi --query-gpu=pci.bus_id,name,memory.used": "00000000:01:00.0, NVIDIA L4, 3\n00000000:02:00.0, NVIDIA L4, 20000\n",
		"nvidia-smi --query-compute-apps":                    "00000000:02:00.0, 777\n",
	}}
	if !gpuVerifier(nv)(returned("0000:01:00.0", "nvidia")) {
		t.Error("a busy GPU the rental did not have made the rented one fail")
	}
	amd := &fakeRunner{outputs: map[string]string{"rocm-smi --showbus --showmeminfo vram --csv": "device,PCI Bus,VRAM Total Used Memory (B)\ncard0,0000:41:00.0,4194304\ncard1,0000:c4:00.0,2147483648\n"}}
	if !gpuVerifier(amd)(returned("0000:41:00.0", "amdgpu")) {
		t.Error("an APU the desktop is using made the rented AMD card fail")
	}
	gone := &fakeRunner{outputs: map[string]string{"rocm-smi --showbus --showmeminfo vram --csv": "device,PCI Bus,VRAM Total Used Memory (B)\ncard1,0000:c4:00.0,4194304\n"}}
	if gpuVerifier(gone)(returned("0000:41:00.0", "amdgpu")) {
		t.Error("an AMD GPU that rocm-smi does not list again verified")
	}
}

// The vendor tools are an extra look where they are installed, never a
// requirement: the release itself verified the reset and each GPU's return to
// its own driver.
func TestGPUVerifierNeedsNoVendorTool(t *testing.T) {
	noSleep(t)
	none := &fakeRunner{missing: map[string]bool{"nvidia-smi": true, "rocm-smi": true}}
	for _, r := range [][]vmrt.BoundDevice{
		returned("0000:01:00.0", ""),
		returned("0000:01:00.0", "nvidia"),
		returned("0000:41:00.0", "amdgpu"),
		returned("0000:03:00.0", "xe"),
	} {
		if !gpuVerifier(none)(r) {
			t.Errorf("%+v did not verify on a host without vendor tools", r)
		}
	}
	// With nvidia-smi installed, an NVIDIA GPU it cannot see is a failure.
	broken := &fakeRunner{fail: map[string]bool{"nvidia-smi": true}, missing: map[string]bool{"rocm-smi": true}}
	if gpuVerifier(broken)(returned("0000:01:00.0", "nvidia")) {
		t.Error("an NVIDIA GPU verified with nvidia-smi failing")
	}
}

func TestVerifyVRAMClear(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"5\n10", true},
		{"5", true},
		{"600\n5", false},
		{"", false},
		{"abc", false},
		{"[N/A]", false},
	}
	for _, c := range cases {
		if got := VerifyVRAMClear(c.out); got != c.want {
			t.Errorf("VerifyVRAMClear(%q) = %v, want %v", c.out, got, c.want)
		}
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
