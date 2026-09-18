package vmrt

import (
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// pairTestID is a pair test boot's id: the rental name gpu-1a2b3c4d, and the
// suffix that marks it as the runtime's own VM.
const pairTestID = "1a2b3c4d" + PairTestSuffix

const goodPairSerial = "GPUAGENT-SELFTEST BEGIN\n" +
	"GPUAGENT-SELFTEST GPU 00000000:06:00.0, NVIDIA GB10, [N/A]\n" +
	"GPUAGENT-SELFTEST INTERNET ok\n" +
	"GPUAGENT-SELFTEST BLOCKED 10.254.254.1:22\n" +
	"GPUAGENT-SELFTEST BLOCKED 192.168.1.1:80\n" +
	"GPUAGENT-SELFTEST END\n" +
	"GPUAGENT-LINK ok 0\r\nGPUAGENT-LINK ok 1\r\n" +
	"GPUAGENT-SELFTEST PAIR BEGIN\n" +
	"GPUAGENT-SELFTEST PAIR PING ok 0\nGPUAGENT-SELFTEST RDMA 0 112.43\n" +
	"GPUAGENT-SELFTEST PAIR PING ok 1\nGPUAGENT-SELFTEST RDMA 1 110.90\n" +
	"GPUAGENT-SELFTEST NCCL skipped\nGPUAGENT-SELFTEST PAIR END\n"

func testPair() *PairOptions {
	p := &PairOptions{Node: "a", PeerHostname: "gpu-1a2b3c4d-b", MTU: 9000, Functions: []string{nic0, nic1},
		Links: []GuestLink{{LocalMAC: nicMAC0, CIDR: "10.200.0.1/30", PeerIP: "10.200.0.2"}, {LocalMAC: nicMAC1, CIDR: "10.200.1.1/30", PeerIP: "10.200.1.2"}}}
	if err := ValidatePair(pairID, p); err != nil {
		panic(err)
	}
	return p
}

func TestParsePairSerial(t *testing.T) {
	rep := ParsePairSerial(goodPairSerial + "GPUAGENT-SELFTEST RDMA 9 1\nGPUAGENT-SELFTEST RDMA 0x1 5\nGPUAGENT-SELFTEST RDMAFAIL 1 Couldn't connect to 10.200.1.1:18516\n")
	if !rep.Begin || !rep.End || rep.Ping[0] != "ok" || rep.Ping[1] != "ok" || rep.RDMA[0] != 112.43 || rep.NCCL != "skipped" {
		t.Fatalf("report = %+v", rep)
	}
	if _, ok := rep.RDMA[9]; ok || rep.RDMAFail[1] != "Couldn't connect to 10.200.1.1:18516" {
		t.Errorf("report = %+v", rep)
	}
}

func TestEvaluatePair(t *testing.T) {
	clean := StopResult{Wiped: true, GPUClean: true}
	all := map[int]bool{0: true, 1: true}
	good := EvaluatePair(ParseSerial(goodPairSerial), ParsePairSerial(goodPairSerial), all, []string{testGPU}, probes, true,
		clean, testPair(), []string{"58:a2:e1:00:01:01", "58:a2:e1:00:01:02"}, 100, "v0.2.0-dev", 1)
	if !good.Passed || good.Node != "a" || strings.Join(good.LocalMACs, ",") != nicMAC0+","+nicMAC1 || len(good.RDMAGbps) != 2 || good.RDMAGbps[1] != 110.9 {
		t.Fatalf("a good pair test = %+v", good)
	}

	bad := strings.NewReplacer("RDMA 1 110.90", "RDMA 1 61.20", "PAIR PING ok 0", "PAIR PING fail 0", "RDMA 0 112.43", "RDMAFAIL 0 no RDMA device behind cx7p0 (mlx5_ib)").Replace(goodPairSerial)
	res := EvaluatePair(ParseSerial(bad), ParsePairSerial(bad), map[int]bool{1: true}, []string{testGPU}, probes, true,
		StopResult{Wiped: true, GPUClean: true, NICDirty: true, Detail: []string{"the firmware of the ConnectX function 0001:01:00.0 changed"}},
		testPair(), nil, 100, "v0.2.0-dev", 1)
	if res.Passed {
		t.Fatal("a failed pair test passed")
	}
	got := strings.Join(res.Problems, " | ")
	for _, want := range []string{"link 0 (cx7p0, 58:a2:e1:00:00:01) did not carry 9000-byte frames", "own link check did not report link 0",
		"RDMA did not run: no RDMA device behind cx7p0", "link 1 (cx7p1, 58:a2:e1:00:00:02) measured 61.2 Gb/s over RDMA, below the 100 Gb/s",
		"firmware of the ConnectX function 0001:01:00.0 changed"} {
		if !strings.Contains(got, want) {
			t.Errorf("problems missing %q: %s", want, got)
		}
	}

	unfinished := EvaluatePair(ParseSerial(goodPairSerial), PairSerialReport{}, all, []string{testGPU}, probes, true, clean, testPair(), nil, 100, "v", 1)
	if unfinished.Passed || !strings.Contains(strings.Join(unfinished.Problems, " "), "never finished its pair report") {
		t.Errorf("an unfinished report passed: %+v", unfinished)
	}
}

// Machine a serves each link's RDMA measurement and machine b connects to it,
// over RDMA CM, one link at a time on its own port.
func TestPairTestScriptRoles(t *testing.T) {
	a := testPair()
	sa := pairTestScript(a, DefaultPairTestPlan)
	b := testPair()
	b.Node, b.PeerHostname = "b", "gpu-1a2b3c4d-a"
	b.Links[0].CIDR, b.Links[0].PeerIP = "10.200.0.2/30", "10.200.0.1"
	sb := pairTestScript(b, DefaultPairTestPlan)
	for _, want := range []string{"ping -c3 -W1 -M do -s 8972 -I \"$1\" \"$2\"", "waitping cx7p0 10.200.0.2", "ib_write_bw -R -F --report_gbits -D 5 -p 18515 2>&1",
		"-p 18516 2>&1", "RDMA 0 $v", "PAIR PING fail 1", "NCCL skipped", "PAIR END"} {
		if !strings.Contains(sa, want) {
			t.Errorf("node a script missing %q:\n%s", want, sa)
		}
	}
	if !strings.Contains(sb, "ib_write_bw -R -F --report_gbits -D 5 -p 18515 10.200.0.1 2>&1") || strings.Contains(sb, "-p 18515 2>&1") {
		t.Errorf("node b does not connect to node a:\n%s", sb)
	}
	k, _ := ThrowawayPubkey()
	ud, err := BuildUserData(SeedOptions{ID: pairID, Pubkey: k, Probes: probes, Pair: a, PairTest: &DefaultPairTestPlan})
	if err != nil || !strings.Contains(ud, "[bash, /usr/local/sbin/gpuagent-pairtest]") || !strings.Contains(ud, "[bash, /usr/local/sbin/gpuagent-selftest]") {
		t.Errorf("pair test user-data: %v\n%s", err, ud)
	}
	if _, err := BuildUserData(SeedOptions{ID: pairID, Pubkey: k, PairTest: &DefaultPairTestPlan}); err == nil {
		t.Errorf("a pair test without a pair was built")
	}
}

func TestThrowawayIntraKeyValidates(t *testing.T) {
	k, err := ThrowawayIntraKey()
	if err != nil {
		t.Fatal(err)
	}
	p := testPair()
	p.IntraKey = k
	if err := ValidatePair(pairID, p); err != nil {
		t.Fatal(err)
	}
}

// The whole pair test against the fake machine: the VM "prints" a good report
// as it starts; the result is recorded, passes, and names the links.
func TestPairSelfTestEndToEnd(t *testing.T) {
	h := pairHost()
	h.Outputs["ip route get 1.1.1.1"] = "1.1.1.1 via 192.168.1.1 dev eno1 src 192.168.1.50\n"
	h.OnRun["systemd-run --unit="] = func(h *fakehost.Host, cmd string) {
		unit := strings.TrimPrefix(strings.Fields(cmd)[1], "--unit=")
		h.SetFail("systemctl is-active --quiet "+unit, nil)
		id := strings.TrimPrefix(unit, "gpu-rental-")
		h.SetFile(NewRental(dataDir, id).SerialLog, []byte(goodPairSerial+"GPUAGENT-SELFTEST BLOCKED 192.168.1.50:22\n"))
	}
	rt, _ := newRuntime(h, func() bool { return true })
	res := rt.PairSelfTest("v0.2.0-dev", PairSelfTestOptions{ID: pairTestID, Pair: testPair(), MinRDMAGbps: 100,
		PeerMACs: []string{"58:a2:e1:00:01:01", "58:a2:e1:00:01:02"}, PeerListing: "B"})
	if !res.Passed {
		t.Fatalf("pair test failed: %v", res.Problems)
	}
	saved, err := LoadPairTest(h, dataDir)
	if err != nil || saved == nil || !saved.Passed || saved.PeerListing != "B" || len(saved.RDMAGbps) != 2 {
		t.Fatalf("not recorded: %+v %v", saved, err)
	}
	if p := PairTestProblem(saved, "v0.2.0-dev", []string{nicMAC0, nicMAC1}); p != "" {
		t.Errorf("the recorded pass does not count: %s", p)
	}
	if h.Driver(nic0) != "mlx5_core" || h.Driver(testGPU) != "nvidia" {
		t.Errorf("not given back: nic %s gpu %s", h.Driver(nic0), h.Driver(testGPU))
	}
	ud := string(h.Files[NewRental(dataDir, pairTestID).Dir+"/user-data"])
	if ud != "" {
		t.Errorf("the rental directory outlived the test")
	}
}

func TestPairSelfTestThatCannotStartIsRecorded(t *testing.T) {
	h := pairHost()
	h.Fail["cloud-localds"] = fakehostErr("cloud-localds: not found")
	rt, _ := newRuntime(h, nil)
	res := rt.PairSelfTest("v0.2.0-dev", PairSelfTestOptions{ID: pairTestID, Pair: testPair()})
	if res.Passed || !strings.Contains(strings.Join(res.Problems, " "), "did not start") || len(res.LocalMACs) != 2 {
		t.Fatalf("result = %+v", res)
	}
	if saved, _ := LoadPairTest(h, dataDir); saved == nil || saved.Passed {
		t.Errorf("failure not recorded: %+v", saved)
	}
}

type fakehostErr string

func (e fakehostErr) Error() string { return string(e) }
