package provisioner

import (
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// winMachine is a Windows 11 workstation with an RTX 4090 whose WSL 2
// distribution is set up: the container stack installed, the image built,
// and a container test boot passed for this agent version.
func winMachine(t *testing.T) (WinFacts, *fakehost.Host) {
	t.Helper()
	h := fakehost.New()
	h.Files["/etc/subuid"] = []byte("containers:2000000000:1000000\n")
	h.Outputs["nvidia-smi --query-gpu=uuid"] = "GPU-6b7c1f0e-aaaa-bbbb-cccc-0123456789ab\n"
	f := WinFacts{
		Build: 26100, Name: "Windows 11 Pro", TotalMemMB: 65536, CPUs: 16,
		WSLInstalled: true, Ready: true, Machine: h,
		StorageDrive: "C:", StorageFreeGB: 900,
		GPUs:  []WinGPU{{BusID: "0000:01:00.0", Name: "NVIDIA GeForce RTX 4090", DeviceID: "10de:2684", MemoryMB: 24564, Driver: "581.29"}},
		LANIP: "192.168.1.19", Gateway: "192.168.1.1",
	}
	spec := vmrt.Spec{DataDir: WSLDataDir, GPUs: []string{"0000:01:00.0"}}
	if err := vmrt.SaveContainerTest(h, WSLDataDir, vmrt.ContainerTest{Passed: true, GPUVerified: true, AgentVersion: version,
		Fingerprint: vmrt.ContainerFingerprint(spec, version)}); err != nil {
		t.Fatal(err)
	}
	return f, h
}

func detectWin(t *testing.T, f WinFacts) *Provisioner {
	t.Helper()
	old := ReadWindows
	ReadWindows = func() WinFacts { return f }
	t.Cleanup(func() { ReadWindows = old })
	return Detect(fakehost.New(), "windows", "amd64", `C:\ProgramData\gpu-agent\data`, version)
}

// A Windows machine set up in WSL 2 is ready, hosts as a container in WSL,
// shares its GPU through WSL (one CDI device for all of WSL's GPUs), and the
// setup counts as able to install what it lacks.
func TestWindowsMachineReady(t *testing.T) {
	f, _ := winMachine(t)
	p := detectWin(t, f)
	c := p.Capability()
	if !c.Ready || c.Kind != KindContainerWSL {
		t.Fatalf("capability = ready %v kind %s reasons %v", c.Ready, c.Kind, c.Reasons)
	}
	if c.GPUCount == nil || *c.GPUCount != 1 || len(c.GPUs) != 1 || c.GPUs[0].Model != "NVIDIA GeForce RTX 4090" || c.GPUs[0].PCIID != "10de:2684" {
		t.Errorf("gpus = %+v count %v", c.GPUs, c.GPUCount)
	}
	if c.SelfTest == nil || !c.SelfTest.Passed {
		t.Errorf("self test = %+v", c.SelfTest)
	}
	if !p.IsContainer() || !p.SelfInstalls() {
		t.Error("a Windows machine hosts as a container, and its setup installs its own Linux")
	}
	spec, ok := p.RuntimeSpec()
	if !ok || !spec.SharedGPU || spec.DataDir != WSLDataDir || spec.DiskGB != 900-WindowsKeepGB {
		t.Errorf("spec = %+v", spec)
	}
}

// A machine renting its CPUs only needs no NVIDIA stack in the distribution.
func TestWindowsWithoutAnNVIDIAGPU(t *testing.T) {
	f, h := winMachine(t)
	f.GPUs = nil
	spec := vmrt.Spec{DataDir: WSLDataDir}
	_ = vmrt.SaveContainerTest(h, WSLDataDir, vmrt.ContainerTest{Passed: true, AgentVersion: version, Fingerprint: vmrt.ContainerFingerprint(spec, version)})
	h.Tools = map[string]bool{"podman": true, "nsenter": true, "cryptsetup": true, "mkfs.ext4": true, "mount": true, "ip": true, "nft": true}
	c := detectWin(t, f).Capability()
	if !c.Ready || c.GPUCount == nil || *c.GPUCount != 0 {
		t.Errorf("a CPU-only Windows machine: ready %v count %v reasons %v", c.Ready, c.GPUCount, c.Reasons)
	}
}

// What stands between a Windows machine and hosting is said in words its
// person can act on, and only a person's own part holds the setup back.
func TestWindowsSaysWhatItLacks(t *testing.T) {
	for name, c := range map[string]struct {
		spoil func(f *WinFacts)
		want  string
		kind  ReasonKind
	}{
		"Windows 10 2004":      {func(f *WinFacts) { f.Build = 19041 }, "needs Windows 10 version 21H2 or later", ReasonHuman},
		"Windows Server 2019":  {func(f *WinFacts) { f.Server, f.Build = true, 17763 }, "needs Windows Server 2022 or later", ReasonHuman},
		"a restart pending":    {func(f *WinFacts) { f.RestartPending = true }, "Windows must restart to finish installing WSL 2", ReasonHuman},
		"an old NVIDIA driver": {func(f *WinFacts) { f.GPUs[0].Driver = "466.77" }, "install NVIDIA driver 470 or later", ReasonHuman},
		"a full drive":         {func(f *WinFacts) { f.StorageFreeGB = 30 }, "there is not 40 GB free on drive C:", ReasonHuman},
		"no WSL yet":           {func(f *WinFacts) { f.WSLInstalled, f.Ready, f.Machine = false, false, nil }, "WSL 2 is not installed; the automatic setup installs it", ReasonTools},
		"no distribution yet":  {func(f *WinFacts) { f.Ready, f.Machine = false, nil }, "the agent's Linux environment in WSL 2 is not set up yet", ReasonTools},
		"facts unreadable":     {func(f *WinFacts) { f.Problem = "this build of the agent cannot read Windows" }, "cannot read Windows", ReasonHuman},
	} {
		f, _ := winMachine(t)
		c.spoil(&f)
		p := detectWin(t, f)
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

// The checks shared with Linux name Linux's sudo; on Windows the same hint
// says to use an Administrator PowerShell.
func TestWindowsHintsNameTheAdministratorPowerShell(t *testing.T) {
	f, h := winMachine(t)
	h.DeleteFile(vmrt.ContainerSelfTestPath(WSLDataDir))
	c := detectWin(t, f).Capability()
	got := strings.Join(c.Reasons, " | ")
	if strings.Contains(got, "sudo") || !strings.Contains(got, "in an Administrator PowerShell") {
		t.Errorf("reasons = %s", got)
	}
}
