package vmrt

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

func cpuSpec() Spec {
	s := testSpec()
	s.GPUs = nil
	return s
}

// A rental on a machine without a GPU: no VFIO, no NVIDIA service touched, no
// GPU on the command line; the teardown has no GPU to check and is clean.
func TestARentalWithoutAGPU(t *testing.T) {
	h := newHost()
	h.SetFail("systemctl is-active --quiet nvidia-persistenced", nil)
	rt := New(h, cpuSpec(), &fakeFence{h: h}, func([]BoundDevice) bool { t.Error("a GPU check ran"); return false })
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"write /sys/bus/pci/drivers_probe", "run modprobe vfio-pci", "run systemctl stop nvidia", "run systemctl is-active --quiet nvidia"} {
		if h.Ran(c) {
			t.Errorf("%q ran on a machine without a GPU", c)
		}
	}
	if launch := h.Call("run systemd-run"); strings.Contains(launch, "vfio-pci") || !strings.Contains(launch, "-nodefaults") {
		t.Errorf("launch = %s", launch)
	}
	res := rt.Stop()
	if !res.Clean() || h.Ran("run systemctl start nvidia") {
		t.Errorf("teardown = %+v", res)
	}
}

// The test boot without a GPU looks for none, says nothing about one, and
// passes on boot, internet, fence and a clean teardown.
func TestSelfTestWithoutAGPU(t *testing.T) {
	k, _ := ThrowawayPubkey()
	ud, err := BuildUserData(SeedOptions{ID: "selftest", Pubkey: k, Probes: probes, NoGPU: true})
	if err != nil || strings.Contains(ud, "nvidia-smi") || !strings.Contains(ud, "INTERNET") {
		t.Fatalf("user-data: %v\n%s", err, ud)
	}

	h := newHost()
	h.Outputs["ip route get 1.1.1.1"] = "1.1.1.1 via 192.168.1.1 dev eno1 src 192.168.1.50\n"
	h.OnRun["systemd-run --unit="] = func(h *fakehost.Host, cmd string) {
		unit := strings.TrimPrefix(strings.Fields(cmd)[1], "--unit=")
		h.SetFail("systemctl is-active --quiet "+unit, nil)
		id := strings.TrimPrefix(unit, "gpu-rental-")
		h.SetFile(NewRental(dataDir, id).SerialLog, []byte("GPUAGENT-SELFTEST BEGIN\nGPUAGENT-SELFTEST INTERNET ok\n"+
			"GPUAGENT-SELFTEST BLOCKED 10.254.254.1:22\nGPUAGENT-SELFTEST BLOCKED 192.168.1.1:80\nGPUAGENT-SELFTEST BLOCKED 192.168.1.50:22\n"+
			"GPUAGENT-SELFTEST END\n"))
	}
	rt := New(h, cpuSpec(), &fakeFence{h: h}, nil)
	res := rt.SelfTest("v0.2.0-dev")
	if !res.Passed || len(res.HostGPUs) != 0 {
		t.Fatalf("self-test = %+v", res)
	}
	if p := SelfTestProblem(&res, Fingerprint{Version: "v0.2.0-dev"}); p != "" {
		t.Errorf("the pass does not count for the machine without a GPU: %s", p)
	}
}

func TestBakeWithoutTheDriver(t *testing.T) {
	ud, err := BakeUserData(NoDriver, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ud, "nvidia") || !strings.Contains(ud, "rdma-core") || !strings.Contains(ud, "GPUAGENT-BAKE DONE") {
		t.Errorf("bake user-data:\n%s", ud)
	}
	h := bakeHost(t)
	if err := Prepare(h, cpuSpec(), &fakeFence{h: h}, "v0.2.0-dev", PrepareOptions{Driver: NoDriver}); err != nil {
		t.Fatal(err)
	}
	var info GoldenInfo
	_ = json.Unmarshal(h.Files[dataDir+"/golden.img.json"], &info)
	if info.Driver != NoDriver || GoldenDriver(h, cpuSpec()) != NoDriver || GoldenProblem(h, cpuSpec()) != "" {
		t.Errorf("golden info = %+v", info)
	}
}

func TestNVIDIAPCIGPUs(t *testing.T) {
	h := fakehost.New()
	for bdf, v := range map[string][2]string{
		"0000:01:00.0": {"0x10de", "0x030200"}, // an NVIDIA 3D controller
		"0000:01:00.1": {"0x10de", "0x040300"}, // its audio function
		"0000:02:00.0": {"0x10de", "0x030000"}, // an NVIDIA VGA controller
		"0000:03:00.0": {"0x1a03", "0x030000"}, // a BMC's display
		"0000:04:00.0": {"0x1002", "0x030000"}, // an AMD iGPU
	} {
		h.PCI(bdf, "", v[1])
		h.Files["/sys/bus/pci/devices/"+bdf+"/vendor"] = []byte(v[0] + "\n")
	}
	if got := strings.Join(NVIDIAPCIGPUs(h), " "); got != "0000:01:00.0 0000:02:00.0" {
		t.Errorf("NVIDIA GPUs = %s", got)
	}
}
