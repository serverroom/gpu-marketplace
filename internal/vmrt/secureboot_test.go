package vmrt

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

const (
	secureCode = "/usr/share/OVMF/OVMF_CODE_4M.secboot.fd"
	secureVars = "/usr/share/OVMF/OVMF_VARS_4M.ms.fd"
	plainCode  = "/usr/share/OVMF/OVMF_CODE_4M.fd"
	plainVars  = "/usr/share/OVMF/OVMF_VARS_4M.fd"
)

// withSecureFirmware installs both firmware builds, as the ovmf package does.
func withSecureFirmware(h *fakehost.Host) *fakehost.Host {
	h.Files[plainCode] = []byte("code")
	h.Files[plainVars] = []byte("vars")
	h.Files[secureCode] = []byte("secure code")
	h.Files[secureVars] = []byte("vars with the keys enrolled")
	return h
}

func secureSpec() Spec {
	spec := testSpec()
	spec.Firmware = Firmware{Code: secureCode, Vars: secureVars, Secure: true}
	return spec
}

func TestTheSecureBootFirmwareIsChosenWhereItIsInstalledAndUsable(t *testing.T) {
	h := fakehost.New()
	h.Files[plainCode], h.Files[plainVars] = []byte("code"), []byte("vars")
	if fw, ok := ChooseFirmware(h, "amd64", dataDir, "v0.3.10"); !ok || fw.Secure || fw.Code != plainCode {
		t.Errorf("a machine with the plain build only: %+v %v", fw, ok)
	}
	withSecureFirmware(h)
	fw, ok := ChooseFirmware(h, "amd64", dataDir, "v0.3.10")
	if !ok || !fw.Secure || fw.Code != secureCode || fw.Vars != secureVars {
		t.Fatalf("the Secure Boot build was not chosen: %+v %v", fw, ok)
	}
	// The plain lookup is still the plain build: the fallback boots with it.
	if plain, _ := FindFirmware(h, "amd64"); plain.Secure || plain.Code != plainCode {
		t.Errorf("FindFirmware = %+v", plain)
	}

	// This agent version found it cannot boot a VM that way here: plain, and why.
	if err := MarkSecureBootUnusable(h, dataDir, "v0.3.10", "the test VM did not start: kvm has no SMM"); err != nil {
		t.Fatal(err)
	}
	if fw, _ := ChooseFirmware(h, "amd64", dataDir, "v0.3.10"); fw.Secure {
		t.Errorf("chosen although it was found unusable: %+v", fw)
	}
	spec := testSpec()
	spec.Firmware, _ = ChooseFirmware(h, "amd64", dataDir, "v0.3.10")
	if why := NotLockedDown(h, spec, "v0.3.10"); !strings.Contains(why, "did not boot with Secure Boot") || !strings.Contains(why, "kvm has no SMM") {
		t.Errorf("why = %q", why)
	}
	// The next version tries again.
	if fw, _ := ChooseFirmware(h, "amd64", dataDir, "v0.3.11"); !fw.Secure {
		t.Errorf("a new agent version did not try again: %+v", fw)
	}

	// Arm has no SMM under KVM: never, and nothing to explain.
	arm := fakehost.New()
	arm.Files["/usr/share/AAVMF/AAVMF_CODE.fd"], arm.Files["/usr/share/AAVMF/AAVMF_VARS.fd"] = []byte("c"), []byte("v")
	arm.Files["/usr/share/AAVMF/AAVMF_CODE.ms.fd"], arm.Files["/usr/share/AAVMF/AAVMF_VARS.ms.fd"] = []byte("c"), []byte("v")
	fw, ok = ChooseFirmware(arm, "arm64", dataDir, "v0.3.10")
	if !ok || fw.Secure {
		t.Errorf("arm64: %+v %v", fw, ok)
	}
	if why := NotLockedDown(arm, Spec{Arch: "arm64", Firmware: fw}, "v0.3.10"); why != "" {
		t.Errorf("arm64 why = %q", why)
	}
	// A machine whose package has no Secure Boot build says that.
	bare := fakehost.New()
	bare.Files[plainCode], bare.Files[plainVars] = []byte("code"), []byte("vars")
	if why := NotLockedDown(bare, testSpec(), "v0.3.10"); !strings.Contains(why, "no Secure Boot build") {
		t.Errorf("why = %q", why)
	}
	if why := NotLockedDown(h, secureSpec(), "v0.3.10"); why != "" {
		t.Errorf("a locked-down machine explains itself: %q", why)
	}
}

func TestARentalWithSecureBootRunsWithSMMAndAProtectedVariableStore(t *testing.T) {
	r := NewRental(dataDir, "R1")
	secure := strings.Join(QEMUArgs(secureSpec(), r), " ")
	for _, want := range []string{"-machine q35,accel=kvm,smm=on", "-global driver=cfi.pflash01,property=secure,value=on",
		"if=pflash,format=raw,readonly=on,file=" + secureCode, "X-PciMmio64Mb"} {
		if !strings.Contains(secure, want) {
			t.Errorf("missing %q in:\n%s", want, secure)
		}
	}
	plain := strings.Join(QEMUArgs(testSpec(), r), " ")
	if strings.Contains(plain, "smm") || strings.Contains(plain, "cfi.pflash01") || !strings.Contains(plain, "-machine q35,accel=kvm ") {
		t.Errorf("a plain rental's command line changed:\n%s", plain)
	}
	// The per-rental variable store is a copy of the one with the keys enrolled.
	h := withSecureFirmware(newHost())
	rt := New(h, secureSpec(), &fakeFence{h: h}, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := string(h.Files[r.Vars]); got != "vars with the keys enrolled" {
		t.Errorf("the rental's variable store = %q", got)
	}
	if launch := h.Call("run systemd-run"); !strings.Contains(launch, "smm=on") {
		t.Errorf("launch:\n%s", launch)
	}
}

func TestALockedDownImageCarriesSignedModulesAndNothingBuiltInside(t *testing.T) {
	cases := map[string][]string{
		"580-server-open": {"linux-modules-nvidia-580-server-open-generic", "nvidia-headless-no-dkms-580-server-open",
			"libnvidia-gl-580-server", "libnvidia-decode-580-server", "libnvidia-encode-580-server", "libnvidia-extra-580-server", "nvidia-utils-580-server"},
		"580-server": {"linux-modules-nvidia-580-server-generic", "nvidia-headless-no-dkms-580-server",
			"libnvidia-gl-580-server", "libnvidia-decode-580-server", "libnvidia-encode-580-server", "libnvidia-extra-580-server", "nvidia-utils-580-server"},
		"570-open": {"linux-modules-nvidia-570-open-generic", "nvidia-headless-no-dkms-570-open",
			"libnvidia-gl-570", "libnvidia-decode-570", "libnvidia-encode-570", "libnvidia-extra-570", "nvidia-utils-570"},
		"550": {"linux-modules-nvidia-550-generic", "nvidia-headless-no-dkms-550",
			"libnvidia-gl-550", "libnvidia-decode-550", "libnvidia-encode-550", "libnvidia-extra-550", "nvidia-utils-550"},
	}
	for driver, want := range cases {
		got := GPUPackages(driver, []string{"10de"}, true)
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s signed:\n got %v\nwant %v", driver, got, want)
		}
	}
	// Built inside the image, as it always was, where rentals boot without Secure Boot.
	if got := strings.Join(GPUPackages("580-server-open", []string{"10de"}, false), " "); got != "linux-headers-generic nvidia-driver-580-server-open nvidia-utils-580-server" {
		t.Errorf("unsigned = %q", got)
	}
	if got := strings.Join(GPUPackages("570", nil, false), " "); got != "linux-headers-generic nvidia-driver-570 nvidia-utils-570" {
		t.Errorf("unsigned = %q", got)
	}
	// The other makes' drivers are the kernel's own, signed already: only their firmware, either way.
	if got := strings.Join(GPUPackages(NoDriver, []string{"1002"}, true), " "); got != "linux-firmware-amd-graphics" {
		t.Errorf("amd signed = %q", got)
	}

	ud, err := BakeUserData("580-server-open", []string{"10de"}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"dkms", "linux-headers", "nvidia-driver-"} {
		if strings.Contains(strings.ReplaceAll(ud, "no-dkms", ""), banned) {
			t.Errorf("a locked-down image's recipe has %q:\n%s", banned, ud)
		}
	}
	// It says it is up before it installs anything.
	if begin, apt := strings.Index(ud, "GPUAGENT-BAKE BEGIN"), strings.Index(ud, "apt-get update"); begin < 0 || apt < 0 || begin > apt {
		t.Errorf("BEGIN at %d, apt-get at %d", begin, apt)
	}
}

func goldenInfo(h *fakehost.Host, info GoldenInfo) {
	data, _ := json.Marshal(info)
	h.Files[dataDir+"/golden.img.json"] = data
}

func TestAnImageWithADriverBuiltInsideIsRebuiltForSecureBoot(t *testing.T) {
	h := withSecureFirmware(newHost())
	base := cloudImageName("amd64")
	goldenInfo(h, GoldenInfo{Base: base, Driver: "580-server-open"})
	if p := GoldenProblem(h, secureSpec()); !strings.Contains(p, "built inside it") || !strings.Contains(p, "runtime prepare") {
		t.Errorf("problem = %q", p)
	}
	// Without Secure Boot it is the image it always was.
	if p := GoldenProblem(h, testSpec()); p != "" {
		t.Errorf("a plain machine's image: %q", p)
	}
	goldenInfo(h, GoldenInfo{Base: base, Driver: "580-server-open", SignedModules: true})
	if p := GoldenProblem(h, secureSpec()); p != "" {
		t.Errorf("a signed image: %q", p)
	}
	// And it serves a rental without Secure Boot just as well.
	if p := GoldenProblem(h, testSpec()); p != "" {
		t.Errorf("a signed image on a plain machine: %q", p)
	}
	// No NVIDIA driver, nothing built: nothing to rebuild.
	goldenInfo(h, GoldenInfo{Base: base, Driver: NoDriver})
	spec := secureSpec()
	spec.GPUs = nil
	if p := GoldenProblem(h, spec); p != "" {
		t.Errorf("an image with no NVIDIA driver: %q", p)
	}
}

func TestPrepareBuildsALockedDownImageWithSecureBoot(t *testing.T) {
	h := withSecureFirmware(prepareHost(t))
	if err := Prepare(h, testSpec(), &fakeFence{h: h}, "v0.3.10", PrepareOptions{}); err != nil {
		t.Fatalf("Prepare = %v", err)
	}
	var info GoldenInfo
	if err := json.Unmarshal(h.Files[dataDir+"/golden.img.json"], &info); err != nil || !info.SignedModules {
		t.Fatalf("golden info = %+v %v", info, err)
	}
	if launch := h.Call("run systemd-run"); !strings.Contains(launch, "smm=on") || !strings.Contains(launch, secureCode) {
		t.Errorf("the image was not built with Secure Boot:\n%s", launch)
	}
	// The recipe the build VM was given (its directory is gone with the VM).
	userData := h.Call("write " + NewRental(dataDir, BakeID).Dir + "/user-data")
	if !strings.Contains(userData, "linux-modules-nvidia-"+DefaultDriver+"-generic") || strings.Contains(userData, "nvidia-driver-") {
		t.Errorf("recipe:\n%s", userData)
	}
	if LoadSecureBoot(h, dataDir) != nil {
		t.Error("a build that worked left a record that it could not")
	}
	if p := GoldenProblem(h, secureSpec()); p != "" {
		t.Errorf("the new image is not accepted: %q", p)
	}
}

// A machine whose VM never comes up with Secure Boot builds its image, and
// rents, without it -- and says so -- instead of not at all.
func TestPrepareDoesWithoutSecureBootWhereAVMNeverComesUpWithIt(t *testing.T) {
	h := withSecureFirmware(prepareHost(t))
	h.OnRun["systemd-run --unit=gpu-rental-bake"] = func(h *fakehost.Host, cmd string) {
		if strings.Contains(cmd, "smm=on") {
			return // qemu exits at once: this KVM has no SMM
		}
		h.SetFile(NewRental(dataDir, BakeID).SerialLog, []byte("GPUAGENT-BAKE BEGIN\nGPUAGENT-BAKE DONE\n"))
	}
	var said []string
	log := func(format string, args ...interface{}) { said = append(said, format) }
	if err := Prepare(h, testSpec(), &fakeFence{h: h}, "v0.3.10", PrepareOptions{Log: log}); err != nil {
		t.Fatalf("Prepare = %v", err)
	}
	if n := h.Count("run systemd-run"); n != 2 {
		t.Fatalf("booted %d times, want twice", n)
	}
	rec := LoadSecureBoot(h, dataDir)
	if rec == nil || rec.Usable || rec.AgentVersion != "v0.3.10" || !strings.Contains(rec.Why, "never came up") {
		t.Fatalf("record = %+v", rec)
	}
	if fw, _ := ChooseFirmware(h, "amd64", dataDir, "v0.3.10"); fw.Secure {
		t.Error("Secure Boot is still chosen after it was found unusable")
	}
	if !strings.Contains(strings.Join(said, "\n"), "never came up with Secure Boot") {
		t.Errorf("the host was not told:\n%s", strings.Join(said, "\n"))
	}
	if !h.Exists(dataDir + "/golden.img") {
		t.Error("no image was built")
	}

	// A build that came up and then failed is not the firmware's doing: no retry, no record.
	h = withSecureFirmware(prepareHost(t))
	h.OnRun["systemd-run --unit=gpu-rental-bake"] = func(h *fakehost.Host, _ string) {
		h.SetFile(NewRental(dataDir, BakeID).SerialLog, []byte("GPUAGENT-BAKE BEGIN\nGPUAGENT-BAKE FAIL\n"))
	}
	err := Prepare(h, testSpec(), &fakeFence{h: h}, "v0.3.10", PrepareOptions{})
	if err == nil || !strings.Contains(err.Error(), "did not succeed") {
		t.Fatalf("Prepare = %v", err)
	}
	if h.Count("run systemd-run") != 1 || LoadSecureBoot(h, dataDir) != nil {
		t.Errorf("a failed install was taken for a firmware that cannot boot")
	}
}

const (
	lockedReport   = "GPUAGENT-SELFTEST LOCKDOWN on integrity denied\n"
	unlockedReport = "GPUAGENT-SELFTEST LOCKDOWN on none open\n"
)

// selfTestHost is a machine whose test VM prints a good report, with body
// between its first line and the rest; body may decide by the launch command.
func selfTestHost(body func(cmd string) (string, bool)) *fakehost.Host {
	h := withSecureFirmware(newHost())
	h.Outputs["ip route get 1.1.1.1"] = "1.1.1.1 via 192.168.1.1 dev eno1 src 192.168.1.50\n"
	h.OnRun["systemd-run --unit="] = func(h *fakehost.Host, cmd string) {
		unit := strings.TrimPrefix(strings.Fields(cmd)[1], "--unit=")
		extra, up := body(cmd)
		if !up {
			return // the VM exits without a word
		}
		h.SetFail("systemctl is-active --quiet "+unit, nil)
		id := strings.TrimPrefix(unit, "gpu-rental-")
		h.SetFile(NewRental(dataDir, id).SerialLog, []byte(
			"GPUAGENT-SELFTEST BEGIN\nGPUAGENT-SELFTEST PCI 10de:20b5 nvidia 0 0000:06:00.0\n"+extra+
				"GPUAGENT-SELFTEST INTERNET ok\nGPUAGENT-SELFTEST BLOCKED 10.254.254.1:22\n"+
				"GPUAGENT-SELFTEST BLOCKED 192.168.1.1:80\nGPUAGENT-SELFTEST BLOCKED 192.168.1.50:22\nGPUAGENT-SELFTEST END\n"))
	}
	return h
}

func TestATestBootWithSecureBootPassesOnlyWhenTheVMSaysItIsLockedDown(t *testing.T) {
	h := selfTestHost(func(string) (string, bool) { return lockedReport, true })
	rt := New(h, secureSpec(), &fakeFence{h: h}, func([]BoundDevice) bool { return true })
	res := rt.SelfTest("v0.3.10")
	if !res.Passed || !res.LockedDown || res.Lockdown != "on integrity denied" {
		t.Fatalf("result = %+v", res)
	}
	saved, _ := LoadSelfTest(h, dataDir)
	if saved == nil || !saved.LockedDown || saved.LastFull == nil || !saved.LastFull.LockedDown {
		t.Fatalf("recorded = %+v", saved)
	}
	fp := rt.Fingerprint("v0.3.10")
	if !fp.Secure || !FullTestCurrent(saved, fp) {
		t.Errorf("the locked-down pass does not count for a machine that boots locked down")
	}
	// The same pass says nothing of a rental that boots without Secure Boot.
	fp.Secure = false
	if FullTestCurrent(saved, fp) {
		t.Errorf("a pass with Secure Boot counted for a rental without it")
	}

	// Booted, and not locked down: a failure, and not one a boot without Secure Boot would mend.
	h = selfTestHost(func(string) (string, bool) { return unlockedReport, true })
	rt = New(h, secureSpec(), &fakeFence{h: h}, func([]BoundDevice) bool { return true })
	res = rt.SelfTest("v0.3.10")
	if res.Passed || res.LockedDown || len(res.Problems) != 1 || !strings.Contains(res.Problems[0], "is not locked down") ||
		!strings.Contains(res.Problems[0], "kernel lockdown none") {
		t.Fatalf("result = %+v", res)
	}
	if h.Count("run systemd-run") != 1 || LoadSecureBoot(h, dataDir) != nil {
		t.Errorf("a VM that booted unlocked was tried again without Secure Boot")
	}

	// An older image's VM says nothing of its lockdown: not locked down, then.
	h = selfTestHost(func(string) (string, bool) { return "", true })
	rt = New(h, secureSpec(), &fakeFence{h: h}, func([]BoundDevice) bool { return true })
	if res = rt.SelfTest("v0.3.10"); res.Passed || !strings.Contains(res.Problems[0], "Secure Boot unknown") {
		t.Fatalf("result = %+v", res)
	}

	// A machine that boots its rentals without Secure Boot is not asked for a lock.
	h = selfTestHost(func(string) (string, bool) { return unlockedReport, true })
	plain, _ := newRuntime(h, func([]BoundDevice) bool { return true })
	if res = plain.SelfTest("v0.3.10"); !res.Passed || res.LockedDown {
		t.Fatalf("a plain machine's test: %+v", res)
	}
}

func TestATestBootThatNeverComesUpWithSecureBootIsRunAgainWithoutIt(t *testing.T) {
	h := selfTestHost(func(cmd string) (string, bool) { return unlockedReport, !strings.Contains(cmd, "smm=on") })
	rt := New(h, secureSpec(), &fakeFence{h: h}, func([]BoundDevice) bool { return true })
	res := rt.SelfTest("v0.3.10")
	if !res.Passed || res.LockedDown {
		t.Fatalf("result = %+v", res)
	}
	if h.Count("run systemd-run") != 2 {
		t.Fatalf("booted %d times, want twice", h.Count("run systemd-run"))
	}
	rec := LoadSecureBoot(h, dataDir)
	if rec == nil || rec.Usable || !strings.Contains(rec.Why, "never finished its report") {
		t.Fatalf("record = %+v", rec)
	}
	if len(res.Notes) == 0 || !strings.Contains(strings.Join(res.Notes, "\n"), "not locked down") {
		t.Errorf("notes = %v", res.Notes)
	}
	saved, _ := LoadSelfTest(h, dataDir)
	if saved == nil || !saved.Passed || saved.LockedDown {
		t.Fatalf("recorded = %+v", saved)
	}
	// From here the machine boots its rentals as it did before, and its pass counts for that.
	fw, _ := ChooseFirmware(h, "amd64", dataDir, "v0.3.10")
	spec := testSpec()
	spec.Firmware = fw
	if fw.Secure || !FullTestCurrent(saved, MachineFingerprint(h, spec, "v0.3.10")) {
		t.Errorf("firmware %+v; the fallback's pass is not current", fw)
	}
	if st, _ := LoadState(h, dataDir); st != nil {
		t.Errorf("state left behind")
	}

	// It comes up neither way: the first failure stands, and Secure Boot is not blamed.
	h = selfTestHost(func(string) (string, bool) { return "", false })
	rt = New(h, secureSpec(), &fakeFence{h: h}, func([]BoundDevice) bool { return true })
	res = rt.SelfTest("v0.3.10")
	if res.Passed || h.Count("run systemd-run") != 2 || LoadSecureBoot(h, dataDir) != nil {
		t.Fatalf("result = %+v, booted %d times, record %+v", res, h.Count("run systemd-run"), LoadSecureBoot(h, dataDir))
	}
	if saved, _ := LoadSelfTest(h, dataDir); saved == nil || saved.Passed {
		t.Errorf("the failure is not what is recorded: %+v", saved)
	}
}

func TestTheSelfTestScriptAsksTheVMWhetherItIsLockedDown(t *testing.T) {
	script, err := selfTestScript([]string{"10.254.254.1:22"}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/sys/firmware/efi/efivars/SecureBoot-", "/sys/kernel/security/lockdown", "head -c 1 /dev/mem", "GPUAGENT-SELFTEST LOCKDOWN $sb $ld $raw"} {
		if !strings.Contains(script, want) {
			t.Errorf("missing %q in:\n%s", want, script)
		}
	}
	rep := ParseSerial("GPUAGENT-SELFTEST BEGIN\r\nGPUAGENT-SELFTEST LOCKDOWN on confidentiality denied\r\nGPUAGENT-SELFTEST END\r\n")
	if !rep.LockedDown() || rep.Lockdown != "confidentiality" {
		t.Errorf("report = %+v", rep)
	}
	for _, line := range []string{"off integrity denied", "on none denied", "on integrity open", "on integrity"} {
		if rep := ParseSerial("GPUAGENT-SELFTEST LOCKDOWN " + line + "\n"); rep.LockedDown() {
			t.Errorf("%q read as locked down", line)
		}
	}
}
