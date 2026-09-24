package provisioner

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// cpuHost is a Linux KVM machine with no GPU at all and no IOMMU, that has
// everything else: tools, firmware, a base image baked without the NVIDIA
// driver, and a passing test boot (with no GPU) for this version.
func cpuHost(t *testing.T) *fakehost.Host {
	t.Helper()
	h := fakehost.New()
	h.Files["/dev/kvm"] = nil
	h.Files["/usr/share/OVMF/OVMF_CODE_4M.fd"] = nil
	h.Files["/usr/share/OVMF/OVMF_VARS_4M.fd"] = nil
	h.Files[dataDir+"/golden.img"] = nil
	h.Files[filepath.Join(dataDir, "golden.img")] = nil
	golden, _ := json.Marshal(vmrt.GoldenInfo{Base: "resolute-server-cloudimg-amd64.img", Driver: vmrt.NoDriver, Extras: []string{"rdma"}})
	h.Files[dataDir+"/golden.img.json"] = golden
	h.Files[filepath.Join(dataDir, "golden.img")+".json"] = golden
	pass, _ := json.Marshal(vmrt.SelfTestResult{Passed: true, AgentVersion: version, HostGPUs: nil})
	h.Files[vmrt.SelfTestPath(dataDir)] = pass
	h.Files["/proc/meminfo"] = []byte("MemTotal:       65536000 kB\n")
	h.Files[dataDir] = nil
	h.Outputs["df --output=avail"] = " Avail\n  900G\n"
	// An onboard BMC display controller is not a GPU anyone rents.
	h.PCI("0000:03:00.0", "ast", "0x030000")
	h.Files["/sys/bus/pci/devices/0000:03:00.0/vendor"] = []byte("0x1a03\n")
	return h
}

func TestAMachineWithoutAGPUHosts(t *testing.T) {
	h := cpuHost(t)
	rep := Preflight(h, "linux", spec(), version)
	if len(rep.Reasons) != 0 || rep.Vendor != VendorNone || rep.GPUCount != 0 || len(rep.BDFs) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	noRelay(t)
	p := Detect(h, "linux", "amd64", dataDir, version)
	c := p.Capability()
	if !c.Ready || c.Kind != KindQEMU || c.GPUCount == nil || *c.GPUCount != 0 {
		t.Fatalf("capability = %+v", c)
	}
	data, _ := json.Marshal(c)
	if !strings.Contains(string(data), `"gpu_count":0`) || !strings.Contains(string(data), `"kind":"qemu"`) {
		t.Errorf("capability JSON = %s", data)
	}
	// It rents: nothing about a missing GPU stops a rental.
	m := &fakeMachine{stopRes: clean()}
	p.machine, p.async = m, false
	withFakeForward(t, nil)
	if err := p.Provision("R1", renterKey(t)); err != nil || p.Status() != StatusRented {
		t.Fatalf("Provision = %v, status %s", err, p.Status())
	}
}

func TestTheIOMMUAndTheTestBootWithoutAGPU(t *testing.T) {
	h := cpuHost(t)
	delete(h.Files, vmrt.SelfTestPath(dataDir))
	got := reasons(Preflight(h, "linux", spec(), version))
	if !strings.Contains(got, "has not yet booted a test rental; run 'sudo gpu-agent check --boot'") || strings.Contains(got, "GPU") {
		t.Errorf("reasons = %s", got)
	}
}

// An NVIDIA GPU the host has no driver for is still a GPU: the host needs no
// GPU driver (the rental's VM is where it has to work), so it is rented, and
// never passed off as a machine without one.
func TestAnNVIDIAGPUWithoutItsDriverIsStillAGPU(t *testing.T) {
	h := cpuHost(t)
	h.PCI("0000:41:00.0", "", "0x030200")
	h.PCIID("0000:41:00.0", "10de", "27b8")
	rep := Preflight(h, "linux", spec(), version)
	got := reasons(rep)
	if rep.Vendor != VendorPCI || rep.GPUCount != 1 || strings.Join(rep.BDFs, " ") != "0000:41:00.0" ||
		strings.Contains(got, "nvidia-smi does not see it") || !strings.Contains(got, "IOMMU") {
		t.Errorf("vendor %s count %d bdfs %v reasons %s", rep.Vendor, rep.GPUCount, rep.BDFs, got)
	}
	if !HasNVIDIAGPU(h) {
		t.Errorf("runtime prepare would bake no driver for a machine with an NVIDIA GPU")
	}
	noRelay(t)
	c := Detect(h, "linux", "amd64", dataDir, version).Capability()
	if c.Ready || c.Kind != KindQEMUVFIO || c.GPUCount == nil || *c.GPUCount != 1 {
		t.Errorf("capability = %+v", c)
	}
}

// A machine that gains a GPU after its image was baked without the driver is
// told to rebuild -- and its test boot without a GPU no longer counts.
func TestAMachineThatGainedAGPURebuilds(t *testing.T) {
	h := goodHost(t, "00000000:01:00.0, NVIDIA L4, 24564\n")
	golden, _ := json.Marshal(vmrt.GoldenInfo{Base: "resolute-server-cloudimg-amd64.img", Driver: vmrt.NoDriver})
	h.Files[dataDir+"/golden.img.json"] = golden
	got := reasons(Preflight(h, "linux", spec(), version))
	if !strings.Contains(got, "built without the NVIDIA driver") || !strings.Contains(got, "runtime prepare") {
		t.Errorf("reasons = %s", got)
	}
	cpuPass := &vmrt.SelfTestResult{Passed: true, AgentVersion: version}
	if p := vmrt.SelfTestProblem(cpuPass, vmrt.Fingerprint{Version: version, GPUs: []string{gpu}}); !strings.Contains(p, "has a GPU now") {
		t.Errorf("a test boot without the GPU counted: %q", p)
	}
}

func TestGPUCountIsOnlyReportedWhereTheAgentHosts(t *testing.T) {
	for goos, want := range map[string]string{"darwin": "container-vm", "windows": "container-wsl"} {
		c := Detect(fakehost.New(), goos, "arm64", dataDir, version).Capability()
		if c.GPUCount != nil || c.Kind != want {
			t.Errorf("%s: gpu_count %v kind %s", goos, c.GPUCount, c.Kind)
		}
	}
	noRelay(t)
	h := goodHost(t, "00000000:01:00.0, NVIDIA L4, 24564\n")
	h.Files["/proc/meminfo"] = []byte("MemTotal:       65536000 kB\n")
	h.Files[dataDir] = nil
	h.Outputs["df --output=avail"] = " Avail\n  900G\n"
	c := Detect(h, "linux", "amd64", dataDir, version).Capability()
	if c.Kind != KindQEMUVFIO || c.GPUCount == nil || *c.GPUCount != 1 || !c.Ready {
		t.Errorf("GPU machine: %+v", c)
	}
}

func TestCheckBakeDriver(t *testing.T) {
	for flag, ok := range map[string]bool{"": true, "580-server": true, "none": true, "latest": false, "580; reboot": false} {
		if err := CheckBakeDriver(flag); (err == nil) != ok {
			t.Errorf("CheckBakeDriver(%q) = %v", flag, err)
		}
	}
}

// An agent that starts while a rental holds the GPU cannot see the GPU: it
// never calls the machine GPU-less, leaves the count out, and reads the
// machine again once the rental has gone -- and says so.
func TestAMachineReadMidRentalIsReadAgainAfterIt(t *testing.T) {
	noRelay(t)
	h := cpuHost(t)
	h.Files[vmrt.StatePath(dataDir)] = []byte(`{"rental_id":"R1","devices":[{"bdf":"0000:05:00.0","driver":"amdgpu"}]}`)
	p := Detect(h, "linux", "amd64", dataDir, version)
	c := p.Capability()
	if c.Ready || c.Kind != KindQEMUVFIO || c.GPUCount != nil || !strings.Contains(strings.Join(c.Reasons, ";"), "a rental holds this machine's GPU") {
		t.Fatalf("mid-rental capability = %+v", c)
	}

	changed := 0
	p.OnCapabilityChange(func() { changed++ })
	m := &fakeMachine{stopRes: clean(), present: true}
	p.machine, p.status = m, StatusRented
	delete(h.Files, vmrt.StatePath(dataDir)) // the teardown removes the state
	if err := p.Teardown("R1"); err != nil {
		t.Fatal(err)
	}
	c = p.Capability()
	if !c.Ready || c.Kind != KindQEMU || c.GPUCount == nil || changed != 1 {
		t.Errorf("after the rental: %+v (reported %d)", c, changed)
	}
}

// Ids the agent keeps for its own VMs are never a renter's.
func TestSetupIDsAreNeverRentals(t *testing.T) {
	noRelay(t)
	p := Detect(cpuHost(t), "linux", "amd64", dataDir, version)
	m := &fakeMachine{stopRes: clean()}
	p.machine, p.async = m, false
	for _, id := range []string{vmrt.SelfTestPrefix + "1", "abcd1234" + vmrt.PairTestSuffix, vmrt.BakeID} {
		if !control.ValidRentalID(id) {
			continue
		}
		if err := p.Provision(id, renterKey(t)); err == nil || !strings.Contains(err.Error(), "test boots") {
			t.Errorf("%s: %v", id, err)
		}
	}
	if m.started != 0 {
		t.Errorf("started %d", m.started)
	}
}

type recordedProblems struct {
	mu                    sync.Mutex
	raised, noted, solved []string
}

func (r *recordedProblems) Raise(area, message, detail string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.raised = append(r.raised, area+": "+message)
	return true
}
func (r *recordedProblems) Note(area, message, detail string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.noted = append(r.noted, area+": "+message)
	return true
}
func (r *recordedProblems) Resolve(area, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.solved = append(r.solved, area+": "+message)
}
func (r *recordedProblems) count() (int, int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.raised), len(r.noted), len(r.solved)
}

// A rental that did not start, and a cleanup that did not verify, are
// problems for the marketplace; a clean teardown resolves the second.
func TestRentalFailuresAreProblems(t *testing.T) {
	noRelay(t)
	p := Detect(cpuHost(t), "linux", "amd64", dataDir, version)
	pr := &recordedProblems{}
	p.SetProblems(pr)
	withFakeForward(t, nil)
	m := &fakeMachine{stopRes: clean(), startErr: errors.New("boot microVM: qemu exited")}
	p.machine, p.async = m, false
	_ = p.Provision("R1", renterKey(t))
	m.startErr = nil
	if err := p.Provision("R2", renterKey(t)); err != nil {
		t.Fatal(err)
	}
	m.stopRes = vmrt.StopResult{Wiped: false, GPUClean: true, Detail: []string{"the disk was not wiped"}}
	_ = p.Teardown("R2")
	for i := 0; i < 200; i++ {
		if r, n, _ := pr.count(); r >= 1 && n >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if len(pr.noted) != 1 || !strings.Contains(pr.noted[0], "rental R1 did not start: boot microVM: qemu exited") {
		t.Errorf("noted = %q", pr.noted)
	}
	if len(pr.raised) != 1 || pr.raised[0] != "rental: "+DirtyProblem {
		t.Errorf("raised = %q", pr.raised)
	}
}
