package vmrt

import (
	"errors"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

const goodSerial = "BdsDxe: loading Boot0001\r\n[  3.2] cloud-init: ...\r\n" +
	"GPUAGENT-SELFTEST BEGIN\r\n" +
	"GPUAGENT-SELFTEST GPU 00000000:06:00.0, NVIDIA CMP 170HX, 8192 MiB\r\n" +
	"GPUAGENT-SELFTEST INTERNET ok\r\n" +
	"GPUAGENT-SELFTEST BLOCKED 10.254.254.1:22\r\n" +
	"GPUAGENT-SELFTEST BLOCKED 192.168.1.1:80\r\n" +
	"GPUAGENT-SELFTEST END\r\n"

var probes = []string{"10.254.254.1:22", "192.168.1.1:80"}

func TestParseSerialIgnoresConsoleNoise(t *testing.T) {
	rep := ParseSerial(goodSerial)
	if !rep.Begin || !rep.End || len(rep.GPUs) != 1 || rep.Internet != "ok" || len(rep.Blocked) != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if rep.GPUs[0] != "00000000:06:00.0, NVIDIA CMP 170HX, 8192 MiB" {
		t.Errorf("gpu line = %q", rep.GPUs[0])
	}
}

func TestEvaluatePassesAGoodBoot(t *testing.T) {
	res := Evaluate(ParseSerial(goodSerial), []string{testGPU}, probes, true, StopResult{Wiped: true, GPUClean: true}, "v0.1.7", 1)
	if !res.Passed {
		t.Fatalf("a good boot failed: %v", res.Problems)
	}
}

func TestEvaluateListsEveryProblem(t *testing.T) {
	serial := "GPUAGENT-SELFTEST BEGIN\nGPUAGENT-SELFTEST GPUFAIL NVIDIA-SMI has failed\n" +
		"GPUAGENT-SELFTEST INTERNET fail\nGPUAGENT-SELFTEST REACHED 192.168.1.1:80 rc=1\nGPUAGENT-SELFTEST END\n"
	res := Evaluate(ParseSerial(serial), []string{testGPU}, probes, false,
		StopResult{Wiped: false, GPUClean: true, Detail: []string{"mapping still open"}}, "v0.1.7", 1)
	if res.Passed {
		t.Fatal("a failed boot passed")
	}
	all := strings.Join(res.Problems, " | ")
	for _, want := range []string{"SSH port", "nvidia-smi failed", "saw 0 GPU", "internet", "reached 192.168.1.1:80", "nothing for 10.254.254.1:22", "mapping still open"} {
		if !strings.Contains(all, want) {
			t.Errorf("problems missing %q: %s", want, all)
		}
	}
}

func TestSelfTestProblem(t *testing.T) {
	pass := &SelfTestResult{Passed: true, AgentVersion: "v0.1.7", HostGPUs: []string{testGPU}}
	if p := SelfTestProblem(pass, "v0.1.7", []string{testGPU}); p != "" {
		t.Errorf("a current pass reported %q", p)
	}
	for name, c := range map[string]struct {
		res  *SelfTestResult
		want string
	}{
		"never":   {nil, "has not yet booted"},
		"failed":  {&SelfTestResult{Problems: []string{"no GPU"}, AgentVersion: "v0.1.7", HostGPUs: []string{testGPU}}, "failed (no GPU)"},
		"old":     {&SelfTestResult{Passed: true, AgentVersion: "v0.1.6", HostGPUs: []string{testGPU}}, "agent v0.1.6"},
		"new GPU": {&SelfTestResult{Passed: true, AgentVersion: "v0.1.7", HostGPUs: []string{"0000:41:00.0"}}, "GPUs have changed"},
	} {
		if p := SelfTestProblem(c.res, "v0.1.7", []string{testGPU}); !strings.Contains(p, c.want) {
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
			"GPUAGENT-SELFTEST BEGIN\nGPUAGENT-SELFTEST GPU 00000000:06:00.0, NVIDIA CMP 170HX, 8192\n"+
				"GPUAGENT-SELFTEST INTERNET ok\nGPUAGENT-SELFTEST BLOCKED 10.254.254.1:22\n"+
				"GPUAGENT-SELFTEST BLOCKED 192.168.1.1:80\nGPUAGENT-SELFTEST BLOCKED 192.168.1.50:22\nGPUAGENT-SELFTEST END\n"))
	}
	rt, _ := newRuntime(h, func() bool { return true })
	res := rt.SelfTest("v0.1.7")
	if !res.Passed {
		t.Fatalf("self-test failed: %v", res.Problems)
	}
	saved, err := LoadSelfTest(h, dataDir)
	if err != nil || saved == nil || !saved.Passed || saved.AgentVersion != "v0.1.7" {
		t.Fatalf("result not recorded: %+v %v", saved, err)
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

func TestSumFor(t *testing.T) {
	sums := "abc *noble-server-cloudimg-amd64.img\ndef *noble-server-cloudimg-arm64.img\n"
	if sumFor(sums, "noble-server-cloudimg-arm64.img") != "def" || sumFor(sums, "missing.img") != "" {
		t.Errorf("sumFor wrong")
	}
}
