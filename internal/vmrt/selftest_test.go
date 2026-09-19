package vmrt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/pcidev"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

const goodSerial = "BdsDxe: loading Boot0001\r\n[  3.2] cloud-init: ...\r\n" +
	"GPUAGENT-SELFTEST BEGIN\r\n" +
	"GPUAGENT-SELFTEST PCI 10de:20b5 nvidia 0 0000:06:00.0\r\n" +
	"GPUAGENT-SELFTEST NVSMI 00000000:06:00.0, NVIDIA A100 80GB PCIe, 81920\r\n" +
	"GPUAGENT-SELFTEST INTERNET ok\r\n" +
	"GPUAGENT-SELFTEST BLOCKED 10.254.254.1:22\r\n" +
	"GPUAGENT-SELFTEST BLOCKED 192.168.1.1:80\r\n" +
	"GPUAGENT-SELFTEST END\r\n"

var probes = []string{"10.254.254.1:22", "192.168.1.1:80"}

var clean = StopResult{Wiped: true, GPUClean: true}

func hostGPU(bdf, vendor, device, name string) HostGPU {
	return HostGPU{Device: pcidev.Device{BDF: bdf, Vendor: vendor, Device: device}, Name: name}
}

var a100 = []HostGPU{hostGPU(testGPU, "10de", "20b5", "NVIDIA A100 PCIe 80GB")}

// report is a finished self-test report around the given GPU lines, with the
// internet reached and both probes blocked.
func report(gpuLines ...string) SerialReport {
	s := "GPUAGENT-SELFTEST BEGIN\n"
	for _, l := range gpuLines {
		s += "GPUAGENT-SELFTEST " + l + "\n"
	}
	s += "GPUAGENT-SELFTEST INTERNET ok\nGPUAGENT-SELFTEST BLOCKED 10.254.254.1:22\n" +
		"GPUAGENT-SELFTEST BLOCKED 192.168.1.1:80\nGPUAGENT-SELFTEST END\n"
	return ParseSerial(s)
}

func TestParseSerialIgnoresConsoleNoise(t *testing.T) {
	rep := ParseSerial(goodSerial)
	if !rep.Begin || !rep.End || rep.Internet != "ok" || len(rep.Blocked) != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if len(rep.PCI) != 1 || rep.PCI[0] != (GuestPCI{ID: "10de:20b5", Driver: "nvidia", BDF: "0000:06:00.0"}) {
		t.Errorf("PCI = %+v", rep.PCI)
	}
	if len(rep.NVSMI) != 1 || rep.NVSMI[0] != (NVSMIRow{BDF: "0000:06:00.0", Name: "NVIDIA A100 80GB PCIe", MemoryMB: 81920}) {
		t.Errorf("NVSMI = %+v", rep.NVSMI)
	}
}

func TestEvaluatePassesAGoodBoot(t *testing.T) {
	res := Evaluate(ParseSerial(goodSerial), a100, probes, true, clean, "v0.1.7", 1)
	if !res.Passed {
		t.Fatalf("a good boot failed: %v", res.Problems)
	}
	want := GuestGPU{HostBDF: testGPU, ID: "10de:20b5", Model: "NVIDIA A100 80GB PCIe", Driver: "nvidia", MemoryMB: 81920}
	if len(res.GPUs) != 1 || res.GPUs[0] != want {
		t.Errorf("GPUs = %+v, want %+v", res.GPUs, want)
	}
	if len(res.GuestGPUs) != 1 || !strings.Contains(res.GuestGPUs[0], "NVIDIA A100 80GB PCIe") {
		t.Errorf("GuestGPUs = %v", res.GuestGPUs)
	}
}

func TestEvaluateListsEveryProblem(t *testing.T) {
	serial := "GPUAGENT-SELFTEST BEGIN\nGPUAGENT-SELFTEST NVSMIFAIL NVIDIA-SMI has failed\n" +
		"GPUAGENT-SELFTEST INTERNET fail\nGPUAGENT-SELFTEST REACHED 192.168.1.1:80 rc=1\nGPUAGENT-SELFTEST END\n"
	res := Evaluate(ParseSerial(serial), a100, probes, false,
		StopResult{Wiped: false, GPUClean: true, Detail: []string{"mapping still open"}}, "v0.1.7", 1)
	if res.Passed {
		t.Fatal("a failed boot passed")
	}
	all := strings.Join(res.Problems, " | ")
	for _, want := range []string{"SSH port", "did not see GPU 0000:01:00.0 (NVIDIA A100 PCIe 80GB, 10de:20b5)", "internet", "reached 192.168.1.1:80", "nothing for 10.254.254.1:22", "mapping still open"} {
		if !strings.Contains(all, want) {
			t.Errorf("problems missing %q: %s", want, all)
		}
	}
	if !strings.Contains(strings.Join(res.Notes, " | "), "nvidia-smi failed inside the VM: NVIDIA-SMI has failed") {
		t.Errorf("notes = %v", res.Notes)
	}
}

// The rule: the VM only has to show that each GPU really was passed through. A
// GPU of any make counts when the guest sees its vendor:device ID, whether or
// not a driver took it.
func TestEvaluatePassesAnyMakeByPCIID(t *testing.T) {
	mi300 := []HostGPU{
		hostGPU("0000:41:00.0", "1002", "74a1", "AMD Instinct MI300X"),
		hostGPU("0000:c1:00.0", "1002", "74a1", "AMD Instinct MI300X"),
	}
	res := Evaluate(report("PCI 1002:74a1 amdgpu 196608 0000:06:00.0", "PCI 1002:74a1 amdgpu 196608 0000:07:00.0"), mi300, probes, true, clean, "v0.1.9", 1)
	if !res.Passed || len(res.GPUs) != 2 {
		t.Fatalf("two AMD GPUs seen in the VM failed: %v %+v", res.Problems, res.GPUs)
	}
	if g := res.GPUs[1]; g.HostBDF != "0000:c1:00.0" || g.Model != "AMD Instinct MI300X" || g.MemoryMB != 196608 || g.Driver != "amdgpu" {
		t.Errorf("second GPU = %+v", g)
	}

	res = Evaluate(report("PCI 1002:74a1 amdgpu 196608 0000:06:00.0"), mi300, probes, true, clean, "v0.1.9", 1)
	if res.Passed || !strings.Contains(strings.Join(res.Problems, " "), "did not see GPU 0000:c1:00.0") {
		t.Errorf("one guest device counted for two GPUs: %v", res.Problems)
	}

	arc := []HostGPU{hostGPU("0000:03:00.0", "8086", "56a0", "Intel Arc A770")}
	res = Evaluate(report("PCI 8086:56a0 none 0 0000:06:00.0"), arc, probes, true, clean, "v0.1.9", 1)
	if !res.Passed || res.GPUs[0].Driver != "" {
		t.Fatalf("a GPU passed through without a driver failed: %v", res.Problems)
	}
	if !strings.Contains(strings.Join(res.Notes, " "), "no driver took it") {
		t.Errorf("notes = %v", res.Notes)
	}

	res = Evaluate(report("PCI 10de:2684 nvidia 0 0000:06:00.0"), arc, probes, true, clean, "v0.1.9", 1)
	if res.Passed {
		t.Errorf("another make's device stood in for the GPU")
	}
}

// Linux lists a device whose memory windows it could not place, with the right
// class and IDs. Such a GPU is on the VM's bus and cannot work: two H100s whose
// 128 GB windows overflow the firmware's space are the case that makes it real.
func TestEvaluateFailsAGPUWhoseMemoryWindowsAreUnmapped(t *testing.T) {
	h100 := []HostGPU{hostGPU("0000:41:00.0", "10de", "2330", "NVIDIA H100 SXM5 80GB")}
	res := Evaluate(report("PCI 10de:2330 none 0 0000:06:00.0 1"), h100, probes, true, clean, "v0.1.9", 1)
	all := strings.Join(res.Problems, " ")
	if res.Passed || !strings.Contains(all, "0000:41:00.0") || !strings.Contains(all, "memory windows") {
		t.Errorf("a GPU with an unmapped BAR passed: %v", res.Problems)
	}
	if strings.Contains(strings.Join(res.Notes, " "), "install one") {
		t.Errorf("a renter was told to install a driver for a GPU that cannot work: %v", res.Notes)
	}
	if res := Evaluate(report("PCI 10de:2330 nvidia 0 0000:06:00.0 0"), h100, probes, true, clean, "v0.1.9", 1); !res.Passed {
		t.Errorf("a fully mapped GPU failed: %v", res.Problems)
	}
}

// A DGX Spark: nvidia-smi says [N/A] for a GB10's memory inside the VM too.
func TestEvaluateMarksAGB10Unified(t *testing.T) {
	gb10 := []HostGPU{hostGPU("000f:01:00.0", "10de", "2e12", "NVIDIA GPU 10de:2e12")}
	res := Evaluate(report("PCI 10de:2e12 nvidia 0 0000:06:00.0", "NVSMI 00000000:06:00.0, NVIDIA GB10, [N/A]"), gb10, probes, true, clean, "v0.1.9", 1)
	if !res.Passed || !res.GPUs[0].Unified || res.GPUs[0].Model != "NVIDIA GB10" || res.GPUs[0].MemoryMB != 0 {
		t.Errorf("GB10 = %+v %v", res.GPUs, res.Problems)
	}
}

func TestSelfTestProblem(t *testing.T) {
	fp := Fingerprint{Version: "v0.1.7", GPUs: []string{testGPU}, HostDriver: "nvidia 580.95.05", BaseImage: "image A"}
	pass := &SelfTestResult{Passed: true, AgentVersion: "v0.1.7", HostGPUs: []string{testGPU}, HostDriver: "nvidia 580.95.05", BaseImage: "image A"}
	if p := SelfTestProblem(pass, fp); p != "" {
		t.Errorf("a current pass reported %q", p)
	}
	// Another agent version's pass sells as long as the machine is the same.
	older := *pass
	older.AgentVersion = "v0.1.6"
	if p := SelfTestProblem(&older, fp); p != "" {
		t.Errorf("a pass by another agent version on the same machine reported %q", p)
	}
	with := func(f func(r *SelfTestResult)) *SelfTestResult {
		r := *pass
		f(&r)
		return &r
	}
	for name, c := range map[string]struct {
		res  *SelfTestResult
		want string
	}{
		"never":      {nil, "has not yet booted"},
		"failed":     {with(func(r *SelfTestResult) { r.Passed, r.Problems = false, []string{"no GPU"} }), "failed (no GPU)"},
		"new GPU":    {with(func(r *SelfTestResult) { r.HostGPUs = []string{"0000:41:00.0"} }), "GPUs have changed"},
		"new driver": {with(func(r *SelfTestResult) { r.HostDriver = "nvidia 570.86.10" }), "driver has changed since its last test boot (nvidia 570.86.10 then, nvidia 580.95.05 now)"},
		"new image":  {with(func(r *SelfTestResult) { r.BaseImage = "image B" }), "base image was rebuilt"},
	} {
		if p := SelfTestProblem(c.res, fp); !strings.Contains(p, c.want) {
			t.Errorf("%s: %q, want it to contain %q", name, p, c.want)
		}
	}
}

// The whole self-test against the fake machine: the VM "prints" a good report
// when it starts, and the result is recorded as a pass.
func TestSelfTestEndToEnd(t *testing.T) {
	h := newHost()
	h.Outputs["ip route get 1.1.1.1"] = "1.1.1.1 via 192.168.1.1 dev eno1 src 192.168.1.50\n"
	h.OnRun["systemd-run --unit="] = func(h *fakehost.Host, cmd string) {
		unit := strings.TrimPrefix(strings.Fields(cmd)[1], "--unit=")
		h.SetFail("systemctl is-active --quiet "+unit, nil)
		id := strings.TrimPrefix(unit, "gpu-rental-")
		h.SetFile(NewRental(dataDir, id).SerialLog, []byte(
			"GPUAGENT-SELFTEST BEGIN\nGPUAGENT-SELFTEST PCI 10de:20b5 nvidia 0 0000:06:00.0\n"+
				"GPUAGENT-SELFTEST INTERNET ok\nGPUAGENT-SELFTEST BLOCKED 10.254.254.1:22\n"+
				"GPUAGENT-SELFTEST BLOCKED 192.168.1.1:80\nGPUAGENT-SELFTEST BLOCKED 192.168.1.50:22\nGPUAGENT-SELFTEST END\n"))
	}
	rt, _ := newRuntime(h, func([]BoundDevice) bool { return true })
	res := rt.SelfTest("v0.1.7")
	if !res.Passed {
		t.Fatalf("self-test failed: %v", res.Problems)
	}
	saved, err := LoadSelfTest(h, dataDir)
	if err != nil || saved == nil || !saved.Passed || saved.AgentVersion != "v0.1.7" {
		t.Fatalf("result not recorded: %+v %v", saved, err)
	}
	if len(saved.GPUs) != 1 || saved.GPUs[0].Model != "NVIDIA GPU 10de:20b5" || saved.GPUs[0].HostBDF != testGPU {
		t.Errorf("recorded GPUs = %+v; with no nvidia-smi row the host's PCI name stands", saved.GPUs)
	}
	if h.Driver(testGPU) != "nvidia" {
		t.Errorf("GPU not given back after the self-test")
	}
	if st, _ := LoadState(h, dataDir); st != nil {
		t.Errorf("the self-test left rental state behind")
	}
}

func TestSelfTestThatCannotStartIsRecordedAsAFailure(t *testing.T) {
	h := newHost()
	h.Fail["cloud-localds"] = errors.New("cloud-localds: not found")
	rt, _ := newRuntime(h, nil)
	res := rt.SelfTest("v0.1.7")
	if res.Passed || len(res.Problems) == 0 || !strings.Contains(res.Problems[0], "did not start") {
		t.Fatalf("result = %+v", res)
	}
	if saved, _ := LoadSelfTest(h, dataDir); saved == nil || saved.Passed {
		t.Errorf("failure not recorded: %+v", saved)
	}
}

func TestPrepareRefusesABaseImageThatFailsItsChecksum(t *testing.T) {
	h := fakehost.New()
	h.Files["/usr/share/OVMF/OVMF_CODE_4M.fd"] = []byte("code")
	h.Files["/usr/share/OVMF/OVMF_VARS_4M.fd"] = []byte("vars")
	base := cloudImageBase()
	name := cloudImageName("amd64")
	h.Downloads[base+"SHA256SUMS"] = []byte("0000000000000000000000000000000000000000000000000000000000000000 *" + name + "\n")
	h.Downloads[base+name] = []byte("a tampered image")
	err := Prepare(h, testSpec(), &fakeFence{h: h}, "v0.1.7", PrepareOptions{})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Prepare = %v, want a checksum refusal", err)
	}
	if h.Ran("run qemu-img create") || h.Ran("run systemd-run") {
		t.Errorf("an unverified image was used")
	}
}

// An image baked for one make cannot serve another: a machine that gains an AMD
// card must rebuild before it rents the card out, because the image lacks the
// firmware the card loads. NVIDIA's driver is judged by GoldenDriver and
// GoldenDriverProblem, not here, so an image from before makes were recorded
// still serves an NVIDIA machine.
func TestGoldenProblemNamesAMakeTheImageLacks(t *testing.T) {
	h := newHost()
	base := cloudImageName("amd64")
	writeGolden(h, GoldenInfo{Base: base, Driver: "580-server-open"})
	if p := GoldenProblem(h, testSpec()); p != "" {
		t.Errorf("an NVIDIA image was refused for an NVIDIA GPU: %s", p)
	}

	amd := "0000:41:00.0"
	h.PCI(amd, "amdgpu", "0x030000", amd)
	h.PCIID(amd, "1002", "744c")
	spec := testSpec()
	spec.GPUs = []string{amd}
	writeGolden(h, GoldenInfo{Base: base, Driver: NoDriver})
	if p := GoldenProblem(h, spec); !strings.Contains(p, "AMD") || !strings.Contains(p, "runtime prepare") {
		t.Errorf("an image without AMD firmware served an AMD GPU: %q", p)
	}
	writeGolden(h, GoldenInfo{Base: base, Driver: NoDriver, Vendors: []string{"1002"}})
	if p := GoldenProblem(h, spec); p != "" {
		t.Errorf("an image built for AMD was refused: %s", p)
	}
}

func TestPrepareBakesForEveryMakeOnTheMachine(t *testing.T) {
	h := newHost()
	h.PCI("0000:04:00.0", "xe", "0x030000")
	h.PCIID("0000:04:00.0", "8086", "56a0")
	h.PCI("0000:00:02.0", "i915", "0x030000")
	h.PCIID("0000:00:02.0", "1002", "46a6")
	h.Links["/sys/bus/pci/devices/0000:00:02.0"] = "../../../devices/pci0000:00/0000:00:08.1/0000:00:02.0"
	h.PCI("0000:03:00.0", "ast", "0x030000")
	h.PCIID("0000:03:00.0", "1a03", "2000")
	h.Files["/usr/share/OVMF/OVMF_CODE_4M.fd"] = []byte("code")
	base, name := cloudImageBase(), cloudImageName("amd64")
	image := []byte("the cloud image")
	sum := sha256.Sum256(image)
	h.Downloads[base+"SHA256SUMS"] = []byte(hex.EncodeToString(sum[:]) + " *" + name + "\n")
	h.Downloads[base+name] = image
	var seed string
	h.OnRun["cloud-localds"] = func(h *fakehost.Host, cmd string) {
		for _, f := range strings.Fields(cmd) {
			if strings.HasSuffix(f, "user-data") {
				data, _ := h.ReadFile(f)
				seed = string(data)
			}
		}
	}
	h.OnRun["systemd-run --unit="] = func(h *fakehost.Host, cmd string) {
		h.SetFile(NewRental(dataDir, BakeID).SerialLog, []byte("GPUAGENT-BAKE DONE\n"))
	}
	h.OnRun["qemu-img convert"] = func(h *fakehost.Host, cmd string) { h.SetFile(fakehost.LastField(cmd), []byte("golden")) }
	if err := Prepare(h, testSpec(), &fakeFence{h: h}, "v0.1.9", PrepareOptions{}); err != nil {
		t.Fatalf("Prepare = %v", err)
	}
	for _, want := range []string{"nvidia-driver-580-server-open", "linux-firmware-intel-graphics"} {
		if !strings.Contains(seed, want) {
			t.Errorf("bake user-data missing %q:\n%s", want, seed)
		}
	}
	var info GoldenInfo
	if err := json.Unmarshal(h.Files[dataDir+"/golden.img.json"], &info); err != nil {
		t.Fatal(err)
	}
	if strings.Join(info.Vendors, ",") != "10de,8086" || info.Driver != "580-server-open" {
		t.Errorf("golden info = %+v; the BMC display and the processor's own GPU must not count as makes", info)
	}
}

func TestSumFor(t *testing.T) {
	sums := "abc *noble-server-cloudimg-amd64.img\ndef *noble-server-cloudimg-arm64.img\n"
	if sumFor(sums, "noble-server-cloudimg-arm64.img") != "def" || sumFor(sums, "missing.img") != "" {
		t.Errorf("sumFor wrong")
	}
}
