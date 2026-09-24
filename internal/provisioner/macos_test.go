package provisioner

import (
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// macMachine is an Apple Silicon Mac whose agent VM is set up: the container
// stack installed and a container test boot passed for this agent version.
func macMachine(t *testing.T) (MacFacts, *fakehost.Host) {
	t.Helper()
	h := fakehost.New()
	h.Files["/etc/subuid"] = []byte("containers:2000000000:1000000\n")
	f := MacFacts{
		MacOS: 15, Name: "macOS 15.6.1", Arch: "arm64", TotalMemMB: 65536, CPUs: 12,
		HVF: true, QEMU: true, Brew: true, Ready: true, Machine: h, StorageFreeGB: 800,
	}
	spec := vmrt.Spec{DataDir: MacVMDataDir}
	if err := vmrt.SaveContainerTest(h, MacVMDataDir, vmrt.ContainerTest{Passed: true, AgentVersion: version,
		Fingerprint: vmrt.ContainerFingerprint(spec, version)}); err != nil {
		t.Fatal(err)
	}
	return f, h
}

func detectMac(t *testing.T, f MacFacts) *Provisioner {
	t.Helper()
	old := ReadMac
	ReadMac = func() MacFacts { return f }
	t.Cleanup(func() { ReadMac = old })
	return Detect(fakehost.New(), "darwin", "arm64", "/Users/x/Library/Application Support/gpu-agent/data", version)
}

// A Mac with its VM set up is ready, hosts CPU-only (no GPU passthrough on a
// Mac) in a container inside the VM, and self-installs.
func TestMacMachineReady(t *testing.T) {
	f, _ := macMachine(t)
	c := detectMac(t, f).Capability()
	if !c.Ready || c.Kind != KindContainerVM {
		t.Fatalf("capability = ready %v kind %s reasons %v", c.Ready, c.Kind, c.Reasons)
	}
	if c.GPUCount == nil || *c.GPUCount != 0 {
		t.Errorf("a Mac rents CPU-only: gpu_count %v", c.GPUCount)
	}
	if c.SelfTest == nil || !c.SelfTest.Passed {
		t.Errorf("self test = %+v", c.SelfTest)
	}
	p := detectMac(t, f)
	if !p.IsContainer() || !p.SelfInstalls() {
		t.Error("a Mac hosts as a container and its setup installs its own Linux")
	}
	spec, ok := p.RuntimeSpec()
	if !ok || spec.DataDir != MacVMDataDir || spec.DiskGB != 800-MacKeepGB {
		t.Errorf("spec = %+v", spec)
	}
}

// What stands between a Mac and hosting is said in words its person can act on,
// and only a person's own part holds the setup back.
func TestMacSaysWhatItLacks(t *testing.T) {
	for name, c := range map[string]struct {
		spoil func(f *MacFacts)
		want  string
		kind  ReasonKind
	}{
		"macOS 11":         {func(f *MacFacts) { f.MacOS = 11 }, "needs macOS 12 (Monterey) or later", ReasonHuman},
		"no hypervisor":    {func(f *MacFacts) { f.HVF = false }, "hypervisor is not available", ReasonHuman},
		"no QEMU, no brew": {func(f *MacFacts) { f.QEMU, f.Brew = false, false }, "install Homebrew from https://brew.sh", ReasonHuman},
		"no QEMU":          {func(f *MacFacts) { f.QEMU = false }, "QEMU is not installed; the automatic setup installs it", ReasonTools},
		"a full disk":      {func(f *MacFacts) { f.StorageFreeGB = 25 }, "there is not 40 GB free for a rental's disk", ReasonHuman},
		"no VM yet":        {func(f *MacFacts) { f.Ready, f.Machine = false, nil }, "the agent's Linux VM is not set up yet", ReasonTools},
		"facts unreadable": {func(f *MacFacts) { f.Problem = "this build of the agent cannot read macOS" }, "cannot read macOS", ReasonHuman},
	} {
		f, _ := macMachine(t)
		c.spoil(&f)
		p := detectMac(t, f)
		if p.Capability().Ready {
			t.Errorf("%s: ready", name)
			continue
		}
		found := false
		for _, fd := range p.Findings() {
			if strings.Contains(fd.Text, c.want) {
				found = true
				if fd.Kind != c.kind {
					t.Errorf("%s: kind %s, want %s", name, fd.Kind, c.kind)
				}
			}
		}
		if !found {
			t.Errorf("%s: findings %+v, want %q", name, p.Findings(), c.want)
		}
	}
}

// The checks shared with Linux name sudo; on a Mac the hint says Terminal.
func TestMacHintsNameTerminal(t *testing.T) {
	f, h := macMachine(t)
	h.DeleteFile(vmrt.ContainerSelfTestPath(MacVMDataDir))
	c := detectMac(t, f).Capability()
	got := strings.Join(c.Reasons, " | ")
	if strings.Contains(got, "sudo") || !strings.Contains(got, "in Terminal") {
		t.Errorf("reasons = %s", got)
	}
}
