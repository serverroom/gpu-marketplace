package vmrt

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

const (
	oldKernel = "6.1.84-vendor-rk35xx"
	newKernel = "6.12.41-current-rockchip64"
	wideCPUs  = "4,5,6,7,2,3"
	wideName  = "4× Cortex-A76 + 2× Cortex-A55"
)

// rk3588Host is an RK3588 board (cpu0-3 Cortex-A55, cpu4-7 Cortex-A76) on a
// kernel, with everything a rental needs to come up (newHost).
func rk3588Host(t *testing.T, kernel string) *fakehost.Host {
	t.Helper()
	h := armHost(t, "cpuinfo-rk3588", freqs(0, 3, 1800000, freqs(4, 7, 2400000, nil)))
	h.Files["/proc/sys/kernel/osrelease"] = []byte(kernel + "\n")
	h.Files["/usr/share/AAVMF/AAVMF_VARS.fd"] = []byte("vars")
	return h
}

// rk3588Spec is the board's spec, without a GPU, in the layout the machine is
// read with.
func rk3588Spec(h *fakehost.Host, version string) Spec {
	s := testSpec()
	s.Arch, s.CPUs, s.TotalMemMB, s.GPUs = "arm64", 8, 15718, nil
	s.Firmware = Firmware{Code: "/usr/share/AAVMF/AAVMF_CODE.fd", Vars: "/usr/share/AAVMF/AAVMF_VARS.fd"}
	ChooseLayout(h, "arm64", dataDir, version).Apply(&s)
	return s
}

func TestTheWiderLayoutAddsTheSlowCoresTheHostDoesNotKeep(t *testing.T) {
	for _, c := range []struct {
		name, cpuinfo, arch string
		freqs               map[int]int
		wide, cpu           string
	}{
		// RK3588: the four Cortex-A76 first, then two of the four Cortex-A55;
		// the host keeps cpu0 and cpu1.
		{"RK3588", "cpuinfo-rk3588", "arm64", freqs(0, 3, 1800000, freqs(4, 7, 2400000, nil)), wideCPUs, wideName},
		{"RK3588 without cpufreq", "cpuinfo-rk3588", "arm64", nil, wideCPUs, wideName},
		// GB10: the ten Cortex-X925, then eight of the ten Cortex-A725.
		{"GB10", "cpuinfo-gb10", "arm64", freqs(0, 9, 3900000, freqs(10, 19, 2808000, nil)),
			"0,1,2,3,4,5,6,7,8,9,12,13,14,15,16,17,18,19", "10× Cortex-X925 + 8× Cortex-A725"},
		{"Ampere Altra: one core type", "cpuinfo-altra", "arm64", nil, "", ""},
		{"x86", "cpuinfo-x86", "amd64", nil, "", ""},
	} {
		ch := ChooseGuestCPUs(armHost(t, c.cpuinfo, c.freqs), c.arch)
		if cpuList(ch.WideCores) != c.wide || ch.WideName != c.cpu {
			t.Errorf("%s: wider cores %q name %q, want %q %q", c.name, cpuList(ch.WideCores), ch.WideName, c.wide, c.cpu)
		}
		// The first cores of the wider layout are the layout it falls back to.
		if len(ch.WideCores) > 0 && cpuList(ch.WideCores[:len(ch.Cores)]) != cpuList(ch.Cores) {
			t.Errorf("%s: the wider layout %v does not begin with %v", c.name, ch.WideCores, ch.Cores)
		}
	}
}

func TestKernelAtLeast(t *testing.T) {
	for release, want := range map[string]bool{
		"6.3.0":                      true,
		"6.12.41-current-rockchip64": true,
		"6.6.89":                     true,
		"7.0.0-38-generic":           true,
		"6.1.84-vendor-rk35xx":       false,
		"6.2.16":                     false,
		"5.10.160-legacy-rk35xx":     false,
		"6.3-rc1":                    true,
		"":                           false,
		"six.three":                  false,
		"6":                          false,
	} {
		if got := kernelAtLeast(release, mixedCoresKernel); got != want {
			t.Errorf("kernelAtLeast(%q) = %v, want %v", release, got, want)
		}
	}
}

// A machine rents in the wider layout only once its own test boot has proven
// it, with this agent version, on this kernel, for these cores.
func TestAMachineUsesTheWiderLayoutOnlyOnceItsTestBootProvedIt(t *testing.T) {
	const v = "v0.4.0"
	// A vendor kernel cannot: one core type, and the host is told which kernel.
	h := rk3588Host(t, oldKernel)
	l := ChooseLayout(h, "arm64", dataDir, v)
	if cpuList(l.Cores) != "4,5,6,7" || l.Name != "Cortex-A76" || l.Home != nil || l.Trial != nil ||
		!strings.Contains(l.Note, "Linux "+oldKernel) || !strings.Contains(l.Note, "Linux 6.3 or later") {
		t.Fatalf("on a vendor kernel: %+v", l)
	}
	// Even a record that says it worked does not outlive the kernel it was for.
	if err := MarkLayout(h, dataDir, v, []int{4, 5, 6, 7, 2, 3}, true, ""); err != nil {
		t.Fatal(err)
	}
	if l = ChooseLayout(h, "arm64", dataDir, v); l.Home != nil || l.Trial != nil {
		t.Fatalf("a vendor kernel with a record: %+v", l)
	}

	// A kernel that can, and nothing tried yet: still one core type, with the
	// wider layout on trial.
	h = rk3588Host(t, newKernel)
	l = ChooseLayout(h, "arm64", dataDir, v)
	if cpuList(l.Cores) != "4,5,6,7" || l.Home != nil || cpuList(l.Trial) != wideCPUs || l.TrialName != wideName ||
		!strings.Contains(l.Note, "next test boot tries "+wideName) {
		t.Fatalf("untried: %+v", l)
	}
	s := rk3588Spec(h, v)
	if s.EachOnOne() || s.GuestCPUs() != 4 || cpuList(s.TrialCores) != wideCPUs {
		t.Fatalf("untried spec: %+v", s)
	}

	// Proven: the wider layout, each vCPU on a core of its own, the fastest
	// cluster as QEMU's home.
	if err := MarkLayout(h, dataDir, v, l.Trial, true, ""); err != nil {
		t.Fatal(err)
	}
	l = ChooseLayout(h, "arm64", dataDir, v)
	if cpuList(l.Cores) != wideCPUs || l.Name != wideName || cpuList(l.Home) != "4,5,6,7" || l.HomeName != "Cortex-A76" || l.Trial != nil {
		t.Fatalf("proven: %+v", l)
	}
	s = rk3588Spec(h, v)
	if !s.EachOnOne() || s.GuestCPUs() != 6 || cpuList(s.LaunchCores()) != "4,5,6,7" || s.GuestCPUName != wideName {
		t.Fatalf("proven spec: %+v", s)
	}
	if one := s.OneCoreType(); one.EachOnOne() || cpuList(one.GuestCores) != "4,5,6,7" || one.GuestCPUName != "Cortex-A76" || one.GuestCPUs() != 4 {
		t.Fatalf("its way back: %+v", one)
	}

	// The proof holds for the agent version and the kernel it was reached
	// with: another of either, and the machine is one that has not tried.
	if l = ChooseLayout(h, "arm64", dataDir, "v0.4.1"); l.Home != nil || cpuList(l.Trial) != wideCPUs {
		t.Errorf("another agent version: %+v", l)
	}
	h.Files["/proc/sys/kernel/osrelease"] = []byte("6.15.2-edge-rockchip64\n")
	if l = ChooseLayout(h, "arm64", dataDir, v); l.Home != nil || cpuList(l.Trial) != wideCPUs {
		t.Errorf("another kernel: %+v", l)
	}

	// Tried, and it did not pass: one core type, no further trial, and why.
	h = rk3588Host(t, newKernel)
	if err := MarkLayout(h, dataDir, v, []int{4, 5, 6, 7, 2, 3}, false, "the VM did not take a reset"); err != nil {
		t.Fatal(err)
	}
	l = ChooseLayout(h, "arm64", dataDir, v)
	if cpuList(l.Cores) != "4,5,6,7" || l.Home != nil || l.Trial != nil || !strings.Contains(l.Note, "the VM did not take a reset") {
		t.Fatalf("refused: %+v", l)
	}

	// Without taskset there is nothing to pin with.
	h = rk3588Host(t, newKernel)
	h.Tools = map[string]bool{}
	if l = ChooseLayout(h, "arm64", dataDir, v); l.Trial != nil || !strings.Contains(l.Note, "taskset") {
		t.Errorf("without taskset: %+v", l)
	}

	// One core type, or x86: nothing to say.
	if l = ChooseLayout(armHost(t, "cpuinfo-altra", nil), "arm64", dataDir, v); l.Note != "" || l.Trial != nil || l.Cores != nil {
		t.Errorf("one core type: %+v", l)
	}
	if l = ChooseLayout(armHost(t, "cpuinfo-x86", nil), "amd64", dataDir, v); l.Note != "" || l.Trial != nil {
		t.Errorf("x86: %+v", l)
	}
}

// monitored makes h's VM answer its monitor as a QEMU that started paused:
// six vCPU threads, a status that follows cont, and a taskset that pins.
func monitored(h *fakehost.Host) {
	var cpus []string
	for i := 0; i < 6; i++ {
		cpus = append(cpus, fmt.Sprintf(`{"cpu-index":%d,"thread-id":%d,"qom-path":"/machine/unattached/device[%d]","target":"aarch64"}`, i, 9000+i, i))
	}
	h.Outputs["qmp query-cpus-fast"] = "[" + strings.Join(cpus, ",") + "]"
	h.Outputs["qmp query-status"] = `{"status":"prelaunch","running":false}`
	h.OnRun["qmp cont"] = func(h *fakehost.Host, _ string) {
		h.SetOutput("qmp query-status", `{"status":"running","running":true}`)
	}
	h.OnRun["taskset -pc"] = func(h *fakehost.Host, cmd string) {
		f := strings.Fields(cmd)
		h.SetFile("/proc/"+f[3]+"/status", []byte("Name:\tCPU 0/KVM\nCpus_allowed:\t04\nCpus_allowed_list:\t"+f[2]+"\n"))
	}
}

func widerRuntime(t *testing.T, h *fakehost.Host, version string) *Runtime {
	t.Helper()
	if err := MarkLayout(h, dataDir, version, []int{4, 5, 6, 7, 2, 3}, true, ""); err != nil {
		t.Fatal(err)
	}
	s := rk3588Spec(h, version)
	if !s.EachOnOne() {
		t.Fatalf("spec = %+v", s)
	}
	return New(h, s, &fakeFence{h: h}, func([]BoundDevice) bool { return true })
}

// In the wider layout QEMU starts paused on the fastest cluster, every vCPU is
// pinned to its own core and the pin read back, the paused VM takes a reset,
// and only then does the guest run.
func TestAVMInTheWiderLayoutRunsOnlyOnceEveryVCPUIsOnItsCore(t *testing.T) {
	h := rk3588Host(t, newKernel)
	monitored(h)
	rt := widerRuntime(t, h, "v0.4.0")
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	launch := h.Call("run systemd-run")
	for _, want := range []string{"--property=CPUAffinity=4,5,6,7 qemu-system-aarch64", " -S ", "-smp 6 ", "-cpu host"} {
		if !strings.Contains(launch, want) {
			t.Errorf("launch lacks %q: %s", want, launch)
		}
	}
	// vCPU i goes to the i-th core of the layout: the Cortex-A76 first.
	for i, core := range []int{4, 5, 6, 7, 2, 3} {
		if !h.Ran(fmt.Sprintf("run taskset -pc %d %d", core, 9000+i)) {
			t.Errorf("vCPU %d was not pinned to CPU %d: %v", i, core, h.Calls)
		}
	}
	before(t, h, "run systemd-run", "run qmp query-cpus-fast")
	before(t, h, "run qmp query-cpus-fast", "run taskset -pc 4 9000")
	before(t, h, "run taskset -pc 3 9005", "run qmp system_reset")
	before(t, h, "run qmp system_reset", "run qmp event:RESET")
	before(t, h, "run qmp event:RESET", "run qmp cont")
	if h.Count("run qmp cont") != 1 {
		t.Errorf("the guest was let run %d times", h.Count("run qmp cont"))
	}
	if rec := LoadLayout(h, dataDir); rec == nil || !rec.Usable {
		t.Errorf("record = %+v", rec)
	}
	rt.Stop()

	// The layout the machine always had is launched exactly as before: not
	// paused, nothing asked of a monitor, nothing pinned thread by thread.
	h = rk3588Host(t, oldKernel)
	monitored(h)
	narrow := New(h, rk3588Spec(h, "v0.4.0"), &fakeFence{h: h}, nil)
	if err := narrow.Start(StartOptions{ID: "R2", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	launch = h.Call("run systemd-run")
	if strings.Contains(launch, " -S ") || !strings.Contains(launch, "CPUAffinity=4,5,6,7 ") || !strings.Contains(launch, "-smp 4 ") ||
		h.Ran("run qmp") || h.Ran("run taskset") {
		t.Errorf("the one-core-type launch changed: %s\n%v", launch, h.Calls)
	}
	narrow.Stop()
}

// Whatever fails on the way, the guest never runs, the VM is torn down, and
// the machine is recorded as one that goes back to one core type.
func TestAVMThatCannotBePinnedNeverRuns(t *testing.T) {
	old := monitorWait
	monitorWait = 3 * time.Second
	defer func() { monitorWait = old }()
	for name, c := range map[string]struct {
		spoil func(h *fakehost.Host)
		want  string
	}{
		"the monitor never answers": {func(h *fakehost.Host) { h.Fail["qmp query-cpus-fast"] = errors.New("connect: no such file") },
			"monitor did not answer"},
		"QEMU exits at once": {func(h *fakehost.Host) {
			h.Fail["qmp query-cpus-fast"] = errors.New("connect: connection refused")
			h.OnRun["systemd-run --unit="] = func(*fakehost.Host, string) {}
			h.Outputs["journalctl -u gpu-rental-R1"] = "qemu-system-aarch64: Failed to put registers after init: Invalid argument\n"
		}, "Failed to put registers after init"},
		"fewer vCPU threads than cores": {func(h *fakehost.Host) {
			h.Outputs["qmp query-cpus-fast"] = `[{"cpu-index":0,"thread-id":9000}]`
		}, "has 1 vCPUs"},
		"taskset fails": {func(h *fakehost.Host) {
			h.Fail["taskset -pc 2 9004"] = errors.New("taskset: failed to set pid 9004's affinity")
		},
			"pin vCPU 4 to CPU 2"},
		"the pin did not take": {func(h *fakehost.Host) {
			h.OnRun["taskset -pc"] = func(h *fakehost.Host, cmd string) {
				h.SetFile("/proc/"+strings.Fields(cmd)[3]+"/status", []byte("Cpus_allowed_list:\t4-7\n"))
			}
		}, `allowed on CPUs "4-7"`},
		"the reset is refused": {func(h *fakehost.Host) {
			h.OnRun["qmp system_reset"] = func(h *fakehost.Host, _ string) {
				h.SetOutput("qmp query-status", `{"status":"internal-error","running":false}`)
			}
		}, `did not take a reset`},
		"it does not run after cont": {func(h *fakehost.Host) { h.OnRun["qmp cont"] = func(*fakehost.Host, string) {} },
			`"prelaunch" after it was let run`},
	} {
		h := rk3588Host(t, newKernel)
		monitored(h)
		c.spoil(h)
		rt := widerRuntime(t, h, "v0.4.0")
		err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)})
		if err == nil || !errors.Is(err, ErrLayout) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want a layout failure with %q", name, err, c.want)
			continue
		}
		if name != "it does not run after cont" && h.Ran("run qmp cont") {
			t.Errorf("%s: the guest was let run", name)
		}
		if st, _ := LoadState(h, dataDir); st != nil {
			t.Errorf("%s: the VM was not torn down: %+v", name, st)
		}
		// A renter's VM failed where a test boot had passed: the proof is gone,
		// for exactly the agent version that held it.
		rec := LoadLayout(h, dataDir)
		if rec == nil || rec.Usable || rec.AgentVersion != "v0.4.0" || !strings.Contains(rec.Why, "a rental did not start that way") {
			t.Errorf("%s: record = %+v", name, rec)
		}
		if l := ChooseLayout(h, "arm64", dataDir, "v0.4.0"); l.Home != nil || l.Trial != nil || cpuList(l.Cores) != "4,5,6,7" {
			t.Errorf("%s: the machine did not go back to one core type: %+v", name, l)
		}
	}
}

// widerTestHost is an RK3588 on a kernel that can run the wider layout, whose
// test VM answers its monitor and prints a good report. cpus says, by the
// launch command, what the VM reports of its CPUs ("" for no CPUS line), and
// whether it comes up at all.
func widerTestHost(t *testing.T, cpus func(cmd string) (string, bool)) *fakehost.Host {
	t.Helper()
	h := rk3588Host(t, newKernel)
	monitored(h)
	h.Outputs["ip route get 1.1.1.1"] = "1.1.1.1 via 192.168.1.1 dev eno1 src 192.168.1.50\n"
	h.OnRun["systemd-run --unit="] = func(h *fakehost.Host, cmd string) {
		// Every VM starts as one that has not been let run yet.
		h.SetOutput("qmp query-status", `{"status":"prelaunch","running":false}`)
		unit := strings.TrimPrefix(strings.Fields(cmd)[1], "--unit=")
		line, up := cpus(cmd)
		if !up {
			// A QEMU that exited has no monitor to answer.
			h.Fail["qmp query-cpus-fast"] = errors.New("connect: connection refused")
			return
		}
		delete(h.Fail, "qmp query-cpus-fast")
		h.SetFail("systemctl is-active --quiet "+unit, nil)
		id := strings.TrimPrefix(unit, "gpu-rental-")
		h.SetFile(NewRental(dataDir, id).SerialLog, []byte(
			"GPUAGENT-SELFTEST BEGIN\n"+line+
				"GPUAGENT-SELFTEST INTERNET ok\nGPUAGENT-SELFTEST BLOCKED 10.254.254.1:22\n"+
				"GPUAGENT-SELFTEST BLOCKED 192.168.1.1:80\nGPUAGENT-SELFTEST BLOCKED 192.168.1.50:22\nGPUAGENT-SELFTEST END\n"))
	}
	return h
}

const (
	sixCPUs  = "GPUAGENT-SELFTEST CPUS 6 11210 0x41/0xd0b,0x41/0xd0b,0x41/0xd0b,0x41/0xd0b,0x41/0xd05,0x41/0xd05\n"
	fourCPUs = "GPUAGENT-SELFTEST CPUS 4 11210 0x41/0xd0b,0x41/0xd0b,0x41/0xd0b,0x41/0xd0b\n"
)

// honest is a test VM that reports the CPUs it was launched with.
func honest(cmd string) (string, bool) {
	if strings.Contains(cmd, "-smp 6 ") {
		return sixCPUs, true
	}
	return fourCPUs, true
}

func untriedRuntime(h *fakehost.Host, version string) *Runtime {
	return New(h, rk3588Spec(h, version), &fakeFence{h: h}, func([]BoundDevice) bool { return true })
}

// The first test boot of a machine that has not tried the wider layout tries
// it; a pass is what puts it on offer.
func TestATestBootProvesTheWiderLayout(t *testing.T) {
	const v = "v0.4.0"
	h := widerTestHost(t, honest)
	rt := untriedRuntime(h, v)
	if rt.Spec().EachOnOne() {
		t.Fatal("an untried machine is already in the wider layout")
	}
	res := rt.SelfTest(v)
	if !res.Passed || res.Cores != wideCPUs || res.GuestCPUs != 6 || res.GuestMemoryMB != 11210 {
		t.Fatalf("result = %+v", res)
	}
	if h.Count("run systemd-run") != 1 || !strings.Contains(h.Call("run systemd-run"), " -S ") {
		t.Fatalf("booted %d times: %s", h.Count("run systemd-run"), h.Call("run systemd-run"))
	}
	rec := LoadLayout(h, dataDir)
	if rec == nil || !rec.Usable || rec.Cores != wideCPUs || rec.Kernel != newKernel || rec.AgentVersion != v {
		t.Fatalf("record = %+v", rec)
	}
	// Read again, the machine rents in the wider layout, and its test counts
	// for that -- not for the layout it had before.
	wide := rk3588Spec(h, v)
	saved, _ := LoadSelfTest(h, dataDir)
	if !wide.EachOnOne() || saved == nil || !saved.Passed || saved.LastFull == nil || saved.LastFull.Cores != wideCPUs ||
		!FullTestCurrent(saved, MachineFingerprint(h, wide, v)) {
		t.Fatalf("spec %+v, recorded %+v", wide, saved)
	}
	if FullTestCurrent(saved, MachineFingerprint(h, wide.OneCoreType(), v)) {
		t.Errorf("a pass in the wider layout counted for a rental on one core type")
	}
	if st, _ := LoadState(h, dataDir); st != nil {
		t.Errorf("state left behind")
	}
}

// A test boot that does not pass in the wider layout is run again as the
// machine always rented, and that pass is the machine's verdict: it keeps
// renting, on one core type, and says why.
func TestATestBootFallsBackToOneCoreType(t *testing.T) {
	const v = "v0.4.0"
	for name, c := range map[string]struct {
		spoil func(h *fakehost.Host)
		cpus  func(cmd string) (string, bool)
		why   string
	}{
		"the VM never comes up on two core types": {nil, func(cmd string) (string, bool) {
			return fourCPUs, !strings.Contains(cmd, "-smp 6 ")
		}, "exited as it started"},
		"the paused VM refuses its reset": {func(h *fakehost.Host) {
			h.OnRun["qmp system_reset"] = func(h *fakehost.Host, _ string) {
				h.SetOutput("qmp query-status", `{"status":"internal-error","running":false}`)
			}
		}, honest, "did not take a reset"},
		"a vCPU is not on the core type it was pinned to": {nil, func(cmd string) (string, bool) {
			if strings.Contains(cmd, "-smp 6 ") {
				return "GPUAGENT-SELFTEST CPUS 6 11210 0x41/0xd0b,0x41/0xd0b,0x41/0xd0b,0x41/0xd0b,0x41/0xd0b,0x41/0xd05\n", true
			}
			return fourCPUs, true
		}, "CPU 4 is a Cortex-A76, and it was pinned to a Cortex-A55 core"},
		"the VM comes up with fewer CPUs": {nil, func(cmd string) (string, bool) { return fourCPUs, true },
			"came up with 4 CPUs"},
		"the VM does not say what CPUs it has": {nil, func(cmd string) (string, bool) {
			if strings.Contains(cmd, "-smp 6 ") {
				return "", true
			}
			return fourCPUs, true
		}, "did not say which CPUs it has"},
	} {
		h := widerTestHost(t, c.cpus)
		if c.spoil != nil {
			c.spoil(h)
		}
		res := untriedRuntime(h, v).SelfTest(v)
		if !res.Passed || res.Cores != "" {
			t.Errorf("%s: result = %+v", name, res)
			continue
		}
		if h.Count("run systemd-run") != 2 {
			t.Errorf("%s: booted %d times, want twice", name, h.Count("run systemd-run"))
		}
		rec := LoadLayout(h, dataDir)
		if rec == nil || rec.Usable || !strings.Contains(rec.Why, c.why) {
			t.Errorf("%s: record = %+v, want why %q", name, rec, c.why)
		}
		if notes := strings.Join(res.Notes, "\n"); !strings.Contains(notes, "did not pass on this machine") || !strings.Contains(notes, "Cortex-A76 cores alone did") {
			t.Errorf("%s: notes = %v", name, res.Notes)
		}
		// From here the machine is what it was before: four Cortex-A76, no
		// further trial, and a test boot that is current for it.
		s := rk3588Spec(h, v)
		saved, _ := LoadSelfTest(h, dataDir)
		if s.EachOnOne() || len(s.TrialCores) != 0 || s.GuestCPUs() != 4 || saved == nil || !saved.Passed ||
			!FullTestCurrent(saved, MachineFingerprint(h, s, v)) {
			t.Errorf("%s: spec %+v, recorded %+v", name, s, saved)
		}
		if st, _ := LoadState(h, dataDir); st != nil {
			t.Errorf("%s: state left behind", name)
		}
	}

	// It passes neither way: the layout is not blamed, and the machine's own
	// failure is what is recorded.
	h := widerTestHost(t, func(string) (string, bool) { return "", false })
	res := untriedRuntime(h, v).SelfTest(v)
	if res.Passed || h.Count("run systemd-run") != 2 || LoadLayout(h, dataDir) != nil {
		t.Fatalf("result = %+v, booted %d times, record %+v", res, h.Count("run systemd-run"), LoadLayout(h, dataDir))
	}
	if saved, _ := LoadSelfTest(h, dataDir); saved == nil || saved.Passed || saved.Cores != "" {
		t.Errorf("recorded = %+v", saved)
	}
	if s := rk3588Spec(h, v); len(s.TrialCores) == 0 {
		t.Errorf("the wider layout is not tried again once the machine is mended: %+v", s)
	}
}

// The test right before a rental is of the machine as it rents now: it does
// not try a layout the rental would not get.
func TestTheTestBeforeARentalDoesNotTryTheWiderLayout(t *testing.T) {
	const v = "v0.4.0"
	h := widerTestHost(t, honest)
	res := untriedRuntime(h, v).SelfTestWith(t.Context(), v, TestOptions{Standing: true})
	if !res.Passed || res.Cores != "" || h.Count("run systemd-run") != 1 || strings.Contains(h.Call("run systemd-run"), " -S ") {
		t.Fatalf("result = %+v; launch %s", res, h.Call("run systemd-run"))
	}
	if LoadLayout(h, dataDir) != nil {
		t.Errorf("a standing test recorded a verdict on the wider layout")
	}
	if s := rk3588Spec(h, v); len(s.TrialCores) == 0 {
		t.Errorf("the wider layout is no longer on trial: %+v", s)
	}
}

// A machine that rents in the wider layout and no longer passes in it goes
// back to one core type, even from a standing test.
func TestAProvenWiderLayoutThatStopsPassingIsGivenUp(t *testing.T) {
	const v = "v0.4.0"
	h := widerTestHost(t, func(cmd string) (string, bool) { return fourCPUs, !strings.Contains(cmd, "-smp 6 ") })
	rt := widerRuntime(t, h, v)
	res := rt.SelfTestWith(t.Context(), v, TestOptions{Standing: true})
	if !res.Passed || res.Cores != "" || h.Count("run systemd-run") != 2 {
		t.Fatalf("result = %+v, booted %d times", res, h.Count("run systemd-run"))
	}
	if rec := LoadLayout(h, dataDir); rec == nil || rec.Usable {
		t.Fatalf("record = %+v", rec)
	}
	if s := rk3588Spec(h, v); s.EachOnOne() || s.GuestCPUs() != 4 {
		t.Errorf("spec = %+v", s)
	}
}

// A test stopped half way says nothing of the layout.
func TestAStoppedTestBootRecordsNothingOfTheLayout(t *testing.T) {
	const v = "v0.4.0"
	h := widerTestHost(t, honest)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := untriedRuntime(h, v).SelfTestWith(ctx, v, TestOptions{})
	if res.Passed || !res.Stopped || LoadLayout(h, dataDir) != nil || h.Ran("run systemd-run") {
		t.Fatalf("result = %+v, record %+v", res, LoadLayout(h, dataDir))
	}
	if saved, _ := LoadSelfTest(h, dataDir); saved != nil {
		t.Errorf("a stopped test was recorded: %+v", saved)
	}
}

// An attempt the host cut in on is no verdict on the layout: nothing is
// recorded, and nothing is run in its place that could be mistaken for one.
func TestAnAttemptTheHostCutInOnRecordsNothingOfTheLayout(t *testing.T) {
	const v = "v0.4.0"
	h := widerTestHost(t, honest)
	first := true
	h.OnRun["cryptsetup open"] = func(h *fakehost.Host, cmd string) {
		h.SetFile("/dev/mapper/"+fakehost.LastField(cmd), nil)
		if first {
			holdGPU(h, "11500", "llama-server") // started while the disk was made, and gone again after
			first = false
		} else {
			h.DeleteLink("/proc/11500/fd/3")
		}
	}
	s := rk3588Spec(h, v)
	s.GPUs = []string{testGPU}
	rt := New(h, s, &fakeFence{h: h}, func([]BoundDevice) bool { return true })
	res := rt.SelfTestWith(context.Background(), v, TestOptions{})
	if res.InUse == "" || res.Passed || h.Ran("run systemd-run") {
		t.Fatalf("result = %+v; launched: %s", res, h.Call("run systemd-run"))
	}
	if LoadLayout(h, dataDir) != nil || h.Exists(SelfTestPath(dataDir)) {
		t.Errorf("an attempt the host cut in on was recorded: layout %+v", LoadLayout(h, dataDir))
	}
}

// An attempt whose teardown does not verify is the machine's verdict whatever
// the layout: it is recorded, nothing is booted after it, and the layout is
// not what is blamed.
func TestAnAttemptWhoseTeardownDoesNotVerifyStands(t *testing.T) {
	const v = "v0.4.0"
	h := widerTestHost(t, func(string) (string, bool) { return fourCPUs, true })
	h.Fail["cryptsetup close"] = errors.New("device busy")
	res := untriedRuntime(h, v).SelfTest(v)
	if res.Passed || h.Count("run systemd-run") != 1 {
		t.Fatalf("result = %+v, booted %d times", res, h.Count("run systemd-run"))
	}
	saved, _ := LoadSelfTest(h, dataDir)
	if saved == nil || saved.Passed || !strings.Contains(strings.Join(saved.Problems, "\n"), "teardown did not verify") {
		t.Errorf("recorded = %+v", saved)
	}
	if st, _ := LoadState(h, dataDir); st == nil || !st.Dirty {
		t.Errorf("the machine is not held back: %+v", st)
	}
	if LoadLayout(h, dataDir) != nil {
		t.Errorf("record = %+v", LoadLayout(h, dataDir))
	}
}

// An attempt that failed as the agent stopped leaves the machine's last test
// boot as it was: the attempt is never the machine's verdict by itself.
func TestAFailedAttemptIsNotTheMachinesTestBoot(t *testing.T) {
	const v = "v0.4.0"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := widerTestHost(t, func(string) (string, bool) {
		cancel() // the agent is stopped as the VM fails to come up
		return "", false
	})
	earlier := SelfTestResult{Passed: true, AgentVersion: "v0.3.10", At: 1000}
	if err := SaveSelfTest(h, dataDir, earlier); err != nil {
		t.Fatal(err)
	}
	res := untriedRuntime(h, v).SelfTestWith(ctx, v, TestOptions{})
	if res.Passed || h.Count("run systemd-run") != 1 {
		t.Fatalf("result = %+v, booted %d times", res, h.Count("run systemd-run"))
	}
	saved, _ := LoadSelfTest(h, dataDir)
	if saved == nil || !saved.Passed || saved.AgentVersion != "v0.3.10" || saved.At != 1000 {
		t.Errorf("the machine's test boot was overwritten by the attempt: %+v", saved)
	}
	if LoadLayout(h, dataDir) != nil {
		t.Errorf("record = %+v", LoadLayout(h, dataDir))
	}
}

func TestTheSelfTestScriptSaysWhatCPUsAndMemoryTheVMHas(t *testing.T) {
	script, err := selfTestScript([]string{"10.254.254.1:22"}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"grep -c '^processor' /proc/cpuinfo", "/^CPU implementer/{i=$2} /^CPU part/", "/^MemTotal:/",
		"GPUAGENT-SELFTEST CPUS ${cpus:-0} ${mem:-0} ${types:--}"} {
		if !strings.Contains(script, want) {
			t.Errorf("missing %q in:\n%s", want, script)
		}
	}
	rep := ParseSerial("GPUAGENT-SELFTEST BEGIN\r\n" + strings.ReplaceAll(sixCPUs, "\n", "\r\n") + "GPUAGENT-SELFTEST END\r\n")
	if rep.CPUCount != 6 || rep.MemoryMB != 11210 || len(rep.CPUTypes) != 6 || rep.CPUTypes[0] != "0x41/0xd0b" || rep.CPUTypes[5] != "0x41/0xd05" {
		t.Errorf("report = %+v", rep)
	}
	// An x86 VM names no core types; a VM from before this said nothing.
	if rep = ParseSerial("GPUAGENT-SELFTEST CPUS 7 28100 -\n"); rep.CPUCount != 7 || rep.MemoryMB != 28100 || rep.CPUTypes != nil {
		t.Errorf("x86 report = %+v", rep)
	}
	if rep = ParseSerial("GPUAGENT-SELFTEST BEGIN\nGPUAGENT-SELFTEST END\n"); rep.CPUCount != 0 {
		t.Errorf("silent report = %+v", rep)
	}
}

func TestWiderProblem(t *testing.T) {
	want := []string{"0x41/0xd0b", "0x41/0xd0b", "0x41/0xd05"}
	for name, c := range map[string]struct {
		rep  SerialReport
		want string
	}{
		"as given":        {SerialReport{CPUCount: 3, CPUTypes: []string{"0x41/0xd0b", "0x41/0xd0b", "0x41/0xd05"}}, ""},
		"silent":          {SerialReport{}, "did not say"},
		"a CPU short":     {SerialReport{CPUCount: 2, CPUTypes: []string{"0x41/0xd0b", "0x41/0xd0b"}}, "came up with 2 CPUs"},
		"no types named":  {SerialReport{CPUCount: 3}, "0 named by type"},
		"the wrong order": {SerialReport{CPUCount: 3, CPUTypes: []string{"0x41/0xd0b", "0x41/0xd05", "0x41/0xd0b"}}, "CPU 1 is a Cortex-A55, and it was pinned to a Cortex-A76 core"},
	} {
		if got := widerProblem(c.rep, want); (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
	// A core the host itself could not name proves nothing.
	if got := widerProblem(SerialReport{CPUCount: 1, CPUTypes: []string{"0x41/0xd0b"}}, []string{""}); got == "" {
		t.Errorf("an unnamed host core passed")
	}
}
