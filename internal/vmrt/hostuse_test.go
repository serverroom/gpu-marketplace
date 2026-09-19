package vmrt

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// busyHost is newHost with the host's own llama-server holding the GPU, the
// desktop's Xorg, nvidia-persistenced and a monitoring nvidia-smi beside it.
func busyHost() *fakehost.Host {
	h := newHost()
	for _, p := range []struct{ pid, comm string }{
		{"11435", "llama-server"}, {"2558", "Xorg"}, {"2411", "nvidia-persiste"}, {"7070", "nvidia-smi"},
	} {
		h.Links["/proc/"+p.pid+"/fd/5"] = "/dev/nvidia0"
		h.Files["/proc/"+p.pid+"/comm"] = []byte(p.comm + "\n")
	}
	return h
}

func meminfo(h *fakehost.Host, totalMB, availMB int) {
	h.Files["/proc/meminfo"] = []byte("MemTotal:       " + itoa(totalMB*1024) + " kB\nMemFree:        1000 kB\nMemAvailable:   " + itoa(availMB*1024) + " kB\n")
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// The host's own programs are its use of the GPU: not a DGX Spark's desktop
// (it closes for a rental), not NVIDIA's services, not a short-lived tool. On
// any other machine a desktop on a rented GPU does not close: the host's too.
func TestReadHostUseNamesTheHostsPrograms(t *testing.T) {
	h := busyHost()
	spark := testSpec()
	spark.DesktopOnDemand = true
	u := ReadHostUse(h, spark)
	if strings.Join(u.Holders, ", ") != "llama-server (pid 11435)" || u.MemoryShortMB != 0 || !u.Busy() {
		t.Errorf("host use = %+v", u)
	}
	if got := u.Describe(); got != "llama-server" {
		t.Errorf("described as %q", got)
	}
	if u := ReadHostUse(h, testSpec()); strings.Join(u.Holders, ", ") != "Xorg (pid 2558), llama-server (pid 11435)" {
		t.Errorf("a workstation's desktop on a rented GPU: %+v", u)
	}
	delete(h.Links, "/proc/11435/fd/5")
	if u := ReadHostUse(h, spark); u.Busy() {
		t.Errorf("a Spark with only its desktop, NVIDIA's services and an nvidia-smi on the GPU is busy: %+v", u)
	}
}

// Memory the rental's VM needs and the host is using counts too, on any
// machine -- one without a GPU included; an unreadable figure counts as free.
func TestReadHostUseCountsTheMemoryARentalNeeds(t *testing.T) {
	h := newHost()
	spec := cpuSpec() // 32 GB: a rental gets 28 GB
	meminfo(h, 32768, 20000)
	u := ReadHostUse(h, spec)
	if len(u.Holders) != 0 || u.MemoryShortMB != spec.GuestMemoryMB()-20000 || u.MemoryShortGB() != 9 {
		t.Errorf("host use = %+v (%d GB short)", u, u.MemoryShortGB())
	}
	if got := u.Describe(); got != "9 GB of the memory a rental needs" {
		t.Errorf("described as %q", got)
	}
	meminfo(h, 32768, 30000)
	if u := ReadHostUse(h, spec); u.Busy() {
		t.Errorf("enough free memory counted as busy: %+v", u)
	}
	h.Files["/proc/meminfo"] = []byte("MemTotal:       33554432 kB\n")
	if u := ReadHostUse(h, spec); u.Busy() {
		t.Errorf("an unreadable figure counted as busy: %+v", u)
	}
}

// A test boot never interrupts the host: a GPU its programs hold is left out,
// a machine whose memory is in use gets a smaller test VM, and too little
// memory for any test waits.
func TestPlanTest(t *testing.T) {
	h := busyHost()
	// A desktop on the GPU of a machine that is not a DGX Spark leaves that
	// GPU out of rentals altogether (preflight); here the GPU has none.
	delete(h.Links, "/proc/2558/fd/5")
	plan := PlanTest(h, testSpec(), false)
	if !plan.NoGPU || strings.Join(plan.InUse, ",") != "llama-server (pid 11435)" || plan.MemoryMB != 0 || plan.Wait != "" {
		t.Errorf("plan with the GPU in use = %+v", plan)
	}
	delete(h.Links, "/proc/11435/fd/5")
	delete(h.Links, "/proc/7070/fd/5")
	if plan := PlanTest(h, testSpec(), false); plan.NoGPU || plan.MemoryMB != 0 {
		t.Errorf("plan with the GPU free = %+v", plan)
	}

	meminfo(h, 32768, 10000)
	if plan := PlanTest(h, testSpec(), false); plan.MemoryMB != (10000-1024)/256*256 || plan.Wait != "" {
		t.Errorf("plan with 10 GB free = %+v", plan)
	}
	meminfo(h, 32768, 2500)
	if plan := PlanTest(h, testSpec(), false); plan.Wait == "" || !strings.Contains(plan.Wait, "needs 3 GB free") {
		t.Errorf("plan with 2.5 GB free = %+v", plan)
	}
}

// On a DGX Spark the desktop closes for a rental, but the agent's own test
// boots leave a desktop someone is logged in to alone; a person's test (or
// the one right before a rental) may close it.
func TestPlanTestLeavesALoggedInSparkDesktopAlone(t *testing.T) {
	h := sparkHost()
	spec := testSpec()
	spec.DesktopOnDemand = true
	if plan := PlanTest(h, spec, false); plan.NoGPU {
		t.Errorf("the login screen alone kept the GPU from the test: %+v", plan)
	}
	loggedIn(h)
	plan := PlanTest(h, spec, false)
	if !plan.NoGPU || strings.Join(plan.InUse, ",") != "the desktop, where ana is logged in" {
		t.Errorf("plan with ana logged in = %+v", plan)
	}
	if plan := PlanTest(h, spec, true); plan.NoGPU {
		t.Errorf("a test that may close the desktop left the GPU out: %+v", plan)
	}
}

// loggedIn puts ana on the desktop (session 3), beside gdm's login screen
// (session c1).
func loggedIn(h *fakehost.Host) {
	h.Outputs["loginctl list-sessions --no-legend"] = "     3 1000 ana  seat0 tty2\n    c1  120 gdm  seat0 tty1\n     5 1000 ana        pts/0\n"
	h.Outputs["loginctl show-session 3 -p Type"] = "Type=wayland\nClass=user\nName=ana\nUser=1000\nState=active\n"
	h.Outputs["loginctl show-session c1 -p Type"] = "Type=wayland\nClass=greeter\nName=gdm\nUser=120\nState=online\n"
	h.Outputs["loginctl show-session 5 -p Type"] = "Type=tty\nClass=user\nName=ana\nUser=1000\nState=active\n"
}

// The people on the machine are told on every terminal (wall) and on every
// desktop someone is logged in to, through that person's own session bus.
func TestNotifyHost(t *testing.T) {
	h := newHost()
	loggedIn(h)
	reached, problems := NotifyHost(h, "GPU marketplace: this machine has been rented", "Please stop llama-server.")
	if len(problems) != 0 || strings.Join(reached, ", ") != "every terminal, the desktop of ana" {
		t.Fatalf("reached %v, problems %v", reached, problems)
	}
	if in := h.Input("timeout 20 wall"); in != "GPU marketplace: this machine has been rented\n\nPlease stop llama-server.\n" {
		t.Errorf("wall got %q", in)
	}
	want := "run timeout 20 runuser -u ana -- env DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus notify-send -u critical -a GPU marketplace " +
		"GPU marketplace: this machine has been rented Please stop llama-server."
	if got := h.Call("run timeout 20 runuser"); got != want {
		t.Errorf("notification = %q\nwant %q", got, want)
	}
	if h.Count("run timeout 20 runuser") != 1 {
		t.Errorf("ana was notified %d times, and the login screen must not be", h.Count("run timeout 20 runuser"))
	}
	if h.Ran("run kill") || h.Ran("run systemctl stop") {
		t.Error("telling the host touched a program")
	}

	h.Tools = map[string]bool{"wall": true}
	if _, problems := NotifyHost(h, "t", "b"); len(problems) != 1 || !strings.Contains(problems[0], "notify-send") {
		t.Errorf("without notify-send: %v", problems)
	}
}

// A test boot while the host uses the GPU runs without it: nothing is taken
// from the host, the VM is still fenced, booted, reached over SSH and torn
// down, and the record says the GPU was not handed over -- keeping the last
// full test.
func TestSelfTestWithoutTheGPU(t *testing.T) {
	h := busyHost()
	h.Outputs["ip route get 1.1.1.1"] = "1.1.1.1 via 192.168.1.1 dev eno1 src 192.168.1.50\n"
	var launch string
	h.OnRun["systemd-run --unit="] = func(h *fakehost.Host, cmd string) {
		launch = cmd
		unit := strings.TrimPrefix(strings.Fields(cmd)[1], "--unit=")
		h.SetFail("systemctl is-active --quiet "+unit, nil)
		id := strings.TrimPrefix(unit, "gpu-rental-")
		h.SetFile(NewRental(dataDir, id).SerialLog, []byte("GPUAGENT-SELFTEST BEGIN\nGPUAGENT-SELFTEST INTERNET ok\n"+
			"GPUAGENT-SELFTEST BLOCKED 10.254.254.1:22\nGPUAGENT-SELFTEST BLOCKED 192.168.1.1:80\nGPUAGENT-SELFTEST BLOCKED 192.168.1.50:22\n"+
			"GPUAGENT-SELFTEST END\n"))
	}
	writeGolden(h, GoldenInfo{Base: cloudImageName("amd64"), Driver: "580-server", CreatedAt: 100})
	h.Files["/sys/module/nvidia/version"] = []byte("580.95.05\n")
	rt, _ := newRuntime(h, func([]BoundDevice) bool {
		t.Error("the GPU was checked after a test that did not take it")
		return true
	})
	// The last full test, by an earlier version.
	full := SelfTestResult{Passed: true, AgentVersion: "v0.2.2", HostGPUs: []string{testGPU}, GPUVerified: true,
		HostDriver: "nvidia 580.95.05", BaseImage: rt.Fingerprint("v0.2.2").BaseImage, GPUs: []GuestGPU{{HostBDF: testGPU, ID: "10de:20b5", Model: "A100"}}, At: 50}
	if err := SaveSelfTest(h, dataDir, full); err != nil {
		t.Fatal(err)
	}

	res := rt.SelfTestWith(context.Background(), "v0.2.3", TestOptions{NoGPU: true, MemoryMB: 8192})
	if !res.Passed || res.GPUVerified || !res.WithoutGPU || res.TestMemoryMB != 8192 {
		t.Fatalf("result = %+v", res)
	}
	for _, c := range []string{"write /sys/bus/pci/drivers_probe", "run modprobe vfio-pci", "run systemctl stop nvidia", "run kill"} {
		if h.Ran(c) {
			t.Errorf("%q ran: a test without the GPU took something from the host", c)
		}
	}
	if strings.Contains(launch, "vfio-pci") || !strings.Contains(launch, "-m 8192") {
		t.Errorf("launch = %s", launch)
	}
	if h.Driver(testGPU) != "nvidia" {
		t.Error("the GPU left its driver")
	}
	saved, err := LoadSelfTest(h, dataDir)
	if err != nil || saved == nil || saved.GPUVerified || saved.LastFull == nil || saved.LastFull.AgentVersion != "v0.2.2" || !saved.LastFull.Passed {
		t.Fatalf("saved = %+v %v", saved, err)
	}
	if saved.HostDriver != "nvidia 580.95.05" || !strings.Contains(saved.BaseImage, "580-server") {
		t.Errorf("fingerprint recorded = %q / %q", saved.HostDriver, saved.BaseImage)
	}
	fp := rt.Fingerprint("v0.2.3")
	if p := SelfTestProblem(saved, fp); p != "" {
		t.Errorf("a pass without the GPU does not sell: %s", p)
	}
	if FullTestCurrent(saved, fp) {
		t.Error("a pass without the GPU counted as this version's full test")
	}
	if g := VerifiedGPUs(saved, fp); len(g) != 1 || g[0].Model != "A100" {
		t.Errorf("the GPUs the last full test saw were lost: %+v", g)
	}
}

// A failed full test is not outweighed by a pass without the GPU: the GPU
// could not be handed over, and the machine is held back until a full test
// passes.
func TestAFailedFullTestOutweighsAPassWithoutTheGPU(t *testing.T) {
	h := newHost()
	fp := Fingerprint{Version: "v0.2.3", GPUs: []string{testGPU}, HostDriver: "nvidia 580.95.05", BaseImage: "image A"}
	failed := SelfTestResult{AgentVersion: "v0.2.3", HostGPUs: []string{testGPU}, HostDriver: fp.HostDriver, BaseImage: fp.BaseImage,
		Problems: []string{"the VM did not see GPU 0000:01:00.0"}, At: 10}
	if err := SaveSelfTest(h, dataDir, failed); err != nil {
		t.Fatal(err)
	}
	noGPU := SelfTestResult{Passed: true, WithoutGPU: true, AgentVersion: "v0.2.3", HostGPUs: []string{testGPU}, HostDriver: fp.HostDriver,
		BaseImage: fp.BaseImage, At: 20}
	if err := SaveSelfTest(h, dataDir, noGPU); err != nil {
		t.Fatal(err)
	}
	saved, _ := LoadSelfTest(h, dataDir)
	if p := SelfTestProblem(saved, fp); !strings.Contains(p, "could not be handed to its last full test rental (the VM did not see GPU") {
		t.Errorf("problem = %q", p)
	}
	if !GPUTestFailed(saved, fp) {
		t.Error("the failed full test was forgotten")
	}
	// A new image is another machine: the failed test no longer speaks for it.
	fp.BaseImage = "image B"
	if GPUTestFailed(saved, fp) {
		t.Error("a failed test counted against a rebuilt image")
	}
}

// A record written before v0.2.3 is read as the full test it was, and holds
// across versions until the base image is rebuilt after it.
func TestALegacyRecordHoldsUntilTheImageIsRebuilt(t *testing.T) {
	h := newHost()
	h.Files[SelfTestPath(dataDir)] = []byte(`{"passed":true,"agent_version":"v0.2.2","host_gpus":["` + testGPU + `"],"at":1000}`)
	res, err := LoadSelfTest(h, dataDir)
	if err != nil || !res.GPUVerified || res.LastFull == nil {
		t.Fatalf("legacy record = %+v %v", res, err)
	}
	fp := Fingerprint{Version: "v0.2.2", GPUs: []string{testGPU}, HostDriver: "nvidia 580.95.05", BaseImage: "x", ImageBuiltAt: 900}
	if SelfTestProblem(res, fp) != "" || !FullTestCurrent(res, fp) {
		t.Errorf("the version's own legacy pass: %q current=%v", SelfTestProblem(res, fp), FullTestCurrent(res, fp))
	}
	fp.Version = "v0.2.3"
	if SelfTestProblem(res, fp) != "" || FullTestCurrent(res, fp) {
		t.Errorf("another version on the same image: %q current=%v", SelfTestProblem(res, fp), FullTestCurrent(res, fp))
	}
	fp.ImageBuiltAt = 2000
	if p := SelfTestProblem(res, fp); !strings.Contains(p, "rebuilt after its last test boot") {
		t.Errorf("an image rebuilt after the test: %q", p)
	}
}

// A host program that takes the GPU between the test's plan and its handover
// makes no verdict: the start is refused as "in use", nothing is recorded,
// and the result says so.
func TestATestTheHostCutInOnRecordsNothing(t *testing.T) {
	h := newHost()
	h.OnRun["cryptsetup open"] = func(h *fakehost.Host, cmd string) {
		h.SetFile("/dev/mapper/"+fakehost.LastField(cmd), nil)
		holdGPU(h, "11500", "llama-server") // started while the disk was made
	}
	rt, _ := newRuntime(h, nil)
	res := rt.SelfTestWith(context.Background(), "v0.2.3", TestOptions{})
	if res.InUse == "" || res.Passed || !strings.Contains(res.InUse, "llama-server (pid 11500)") {
		t.Fatalf("result = %+v", res)
	}
	if h.Exists(SelfTestPath(dataDir)) {
		t.Error("a test the host cut in on was recorded")
	}
	if st, _ := LoadState(h, dataDir); st != nil {
		t.Errorf("state left behind: %+v", st)
	}
}

// A failed test by an agent up to v0.2.2 whose VM never got the GPU -- the
// host held it -- is no verdict on the GPU: a pass without the GPU clears it.
func TestALegacyInUseFailureIsNoGPUVerdict(t *testing.T) {
	h := newHost()
	h.Files[SelfTestPath(dataDir)] = []byte(`{"passed":false,"agent_version":"v0.2.2","host_gpus":["` + testGPU + `"],"at":1000,` +
		`"problems":["the test VM did not start: the GPU is in use on this machine by llama-server (pid 11435); stop them first"]}`)
	fp := Fingerprint{Version: "v0.2.3", GPUs: []string{testGPU}, HostDriver: "nvidia", BaseImage: "image A"}
	res, _ := LoadSelfTest(h, dataDir)
	if GPUTestFailed(res, fp) || SelfTestProblem(res, fp) == "" {
		t.Fatalf("GPUTestFailed=%v problem=%q", GPUTestFailed(res, fp), SelfTestProblem(res, fp))
	}
	_ = SaveSelfTest(h, dataDir, SelfTestResult{Passed: true, WithoutGPU: true, AgentVersion: "v0.2.3", HostGPUs: fp.GPUs,
		HostDriver: fp.HostDriver, BaseImage: fp.BaseImage, At: 2000})
	res, _ = LoadSelfTest(h, dataDir)
	if p := SelfTestProblem(res, fp); p != "" {
		t.Errorf("a pass without the GPU did not clear an in-use failure: %q", p)
	}
	// A real GPU failure of that age still counts.
	h.Files[SelfTestPath(dataDir)] = []byte(`{"passed":false,"agent_version":"v0.2.2","host_gpus":["` + testGPU + `"],"at":1000,` +
		`"problems":["the VM did not see GPU 0000:01:00.0"]}`)
	fp.ImageBuiltAt = 900
	if res, _ := LoadSelfTest(h, dataDir); !GPUTestFailed(res, fp) {
		t.Error("a legacy GPU failure was forgotten")
	}
}
