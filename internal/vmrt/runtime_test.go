package vmrt

import (
	"errors"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

const (
	testGPU   = "0000:01:00.0"
	testAudio = "0000:01:00.1"
	dataDir   = "/var/lib/gpu-agent"
)

type fakeFence struct {
	h        *fakehost.Host
	applyErr error
	applied  int
	removed  int
}

func (f *fakeFence) Apply() error {
	f.applied++
	f.h.Run("fence", "apply")
	return f.applyErr
}

func (f *fakeFence) Remove() error {
	f.removed++
	f.h.Run("fence", "remove")
	return nil
}

// newHost is a machine with one GPU (and its audio function, in the same IOMMU
// group), forwarding off, no Docker, and just enough behaviour for a rental to
// come up: losetup answers, cryptsetup makes its mapping, systemd-run starts a
// unit that `systemctl is-active` then sees, and the guest answers on 22.
func newHost() *fakehost.Host {
	h := fakehost.New()
	h.PCI(testGPU, "nvidia", "0x030200", testGPU, testAudio)
	h.PCIID(testGPU, "10de", "20b5")
	h.PCI(testAudio, "snd_hda_intel", "0x040300", testGPU, testAudio)
	h.PCIID(testAudio, "10de", "1aef")
	h.Files["/proc/sys/net/ipv4/ip_forward"] = []byte("0\n")
	h.Files["/usr/share/OVMF/OVMF_VARS_4M.fd"] = []byte("vars")
	h.Outputs["losetup --find --show"] = "/dev/loop7\n"
	h.Fail["systemctl is-active --quiet"] = errors.New("inactive")
	h.Fail["iptables -S DOCKER-USER"] = errors.New("no such chain")
	h.Outputs["iptables -S FORWARD"] = "-P FORWARD ACCEPT\n"
	h.OnRun["truncate -s"] = func(h *fakehost.Host, cmd string) { h.SetFile(fakehost.LastField(cmd), nil) }
	h.OnRun["cryptsetup open"] = func(h *fakehost.Host, cmd string) {
		h.SetFile("/dev/mapper/"+fakehost.LastField(cmd), nil)
	}
	h.OnRun["cryptsetup close"] = func(h *fakehost.Host, cmd string) {
		h.DeleteFile("/dev/mapper/" + fakehost.LastField(cmd))
	}
	h.OnRun["systemd-run --unit="] = func(h *fakehost.Host, cmd string) {
		unit := strings.TrimPrefix(strings.Fields(cmd)[1], "--unit=")
		h.SetFail("systemctl is-active --quiet "+unit, nil)
	}
	h.OnRun["systemctl stop gpu-rental-"] = func(h *fakehost.Host, cmd string) {
		h.SetFail("systemctl is-active --quiet "+fakehost.LastField(cmd), errors.New("inactive"))
	}
	h.Dial[GuestIP+":22"] = true
	return h
}

func testSpec() Spec {
	return Spec{
		Arch:        "amd64",
		DataDir:     dataDir,
		GoldenImage: dataDir + "/golden.img",
		Firmware:    Firmware{Code: "/usr/share/OVMF/OVMF_CODE_4M.fd", Vars: "/usr/share/OVMF/OVMF_VARS_4M.fd"},
		GPUs:        []string{testGPU},
		TotalMemMB:  32768,
		CPUs:        12,
		DiskGB:      100,
	}
}

func newRuntime(h *fakehost.Host, verify func([]BoundDevice) bool) (*Runtime, *fakeFence) {
	fence := &fakeFence{h: h}
	return New(h, testSpec(), fence, verify), fence
}

func key(t *testing.T) string {
	t.Helper()
	k, err := ThrowawayPubkey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func before(t *testing.T, h *fakehost.Host, first, second string) {
	t.Helper()
	a, b := h.Index(first), h.Index(second)
	if a < 0 || b < 0 || a > b {
		t.Errorf("%q (at %d) must happen before %q (at %d)", first, a, second, b)
	}
}

func TestStartBringsARentalUpInASafeOrder(t *testing.T) {
	h := newHost()
	rt, _ := newRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Nothing exists unfenced; the GPUs leave the host last, right before boot.
	before(t, h, "run fence apply", "run ip link add gpurent0")
	before(t, h, "run ip link add gpurent0", "run cryptsetup open")
	before(t, h, "run cryptsetup open", "run cloud-localds")
	before(t, h, "run cloud-localds", "write /sys/bus/pci/drivers_probe")
	before(t, h, "write /sys/bus/pci/drivers_probe", "run systemd-run")

	if h.Driver(testGPU) != "vfio-pci" || h.Driver(testAudio) != "vfio-pci" {
		t.Errorf("GPU group not on vfio-pci: gpu=%s audio=%s", h.Driver(testGPU), h.Driver(testAudio))
	}
	launch := h.Call("run systemd-run")
	for _, want := range []string{"vfio-pci,host=" + testGPU, "vfio-pci,host=" + testAudio, "-nodefaults", "ifname=" + Tap} {
		if !strings.Contains(launch, want) {
			t.Errorf("launch is missing %q:\n%s", want, launch)
		}
	}
	if string(h.Files["/proc/sys/net/ipv4/ip_forward"]) != "1" {
		t.Errorf("forwarding not enabled for the rental")
	}
	st, err := LoadState(h, dataDir)
	if err != nil || st == nil || st.RentalID != "R1" || len(st.Devices) != 2 || st.Disk.Mapper == "" {
		t.Fatalf("state not recorded: %+v %v", st, err)
	}
}

// A boot that fails must leave the machine exactly as it was: GPUs back on
// their drivers, no mapping, no fence, forwarding restored, no state.
func TestFailedStartTearsDownEverything(t *testing.T) {
	h := newHost()
	h.Fail["systemd-run"] = errors.New("qemu: could not open vfio device")
	rt, fence := newRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err == nil {
		t.Fatal("Start succeeded although the VM did not boot")
	}
	if h.Driver(testGPU) != "nvidia" || h.Driver(testAudio) != "snd_hda_intel" {
		t.Errorf("GPU not given back: gpu=%s audio=%s", h.Driver(testGPU), h.Driver(testAudio))
	}
	if h.Exists("/dev/mapper/" + MapperName("R1")) {
		t.Errorf("encrypted mapping left open")
	}
	if fence.removed != 1 {
		t.Errorf("fence removed %d times, want 1", fence.removed)
	}
	if string(h.Files["/proc/sys/net/ipv4/ip_forward"]) != "0" {
		t.Errorf("forwarding not restored: %q", h.Files["/proc/sys/net/ipv4/ip_forward"])
	}
	if st, _ := LoadState(h, dataDir); st != nil {
		t.Errorf("state left behind: %+v", st)
	}
}

func TestNothingIsBuiltWithoutTheFence(t *testing.T) {
	h := newHost()
	rt, fence := newRuntime(h, nil)
	fence.applyErr = errors.New("nft: rules did not verify")
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err == nil {
		t.Fatal("Start succeeded without a fence")
	}
	for _, c := range []string{"run ip link add", "run cryptsetup open", "run systemd-run", "write /sys/bus/pci/drivers_probe"} {
		if h.Ran(c) {
			t.Errorf("%q ran although the fence failed", c)
		}
	}
}

func TestGPUInUseOnTheHostIsRefused(t *testing.T) {
	h := newHost()
	h.Links["/proc/4242/fd/9"] = "/dev/nvidia0"
	h.Files["/proc/4242/comm"] = []byte("python3\n")
	rt, _ := newRuntime(h, nil)
	err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)})
	if err == nil || !strings.Contains(err.Error(), "python3 (pid 4242)") {
		t.Fatalf("Start = %v, want a refusal naming the holder", err)
	}
	if h.Driver(testGPU) != "nvidia" || h.Ran("run systemd-run") {
		t.Errorf("a held GPU was taken or a VM booted")
	}
}

func TestPersistencedIsStoppedAndRestarted(t *testing.T) {
	h := newHost()
	h.SetFail("systemctl is-active --quiet nvidia-persistenced", nil)
	rt, _ := newRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatal(err)
	}
	if !h.Ran("run systemctl stop nvidia-persistenced") {
		t.Errorf("nvidia-persistenced was not stopped")
	}
	rt.Stop()
	if !h.Ran("run systemctl start nvidia-persistenced") {
		t.Errorf("nvidia-persistenced was not restarted")
	}
}

func TestStopVerifiesAndClears(t *testing.T) {
	h := newHost()
	rt, fence := newRuntime(h, func([]BoundDevice) bool { return true })
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatal(err)
	}
	res := rt.Stop()
	if !res.Clean() {
		t.Fatalf("Stop not clean: %+v", res)
	}
	if !h.Ran("run systemctl stop gpu-rental-R1") || fence.removed != 1 {
		t.Errorf("VM not stopped or fence not removed")
	}
	if h.Driver(testGPU) != "nvidia" || h.Driver(testAudio) != "snd_hda_intel" {
		t.Errorf("GPU not given back")
	}
	if st, _ := LoadState(h, dataDir); st != nil {
		t.Errorf("state left behind after a clean stop")
	}
}

// A disk that will not close is not a wiped disk. The machine stays quarantined
// and refuses the next rental.
func TestUnverifiedWipeQuarantinesTheMachine(t *testing.T) {
	h := newHost()
	rt, _ := newRuntime(h, func([]BoundDevice) bool { return true })
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatal(err)
	}
	h.SetFail("cryptsetup close", errors.New("device busy"))
	res := rt.Stop()
	if res.Wiped || res.Clean() {
		t.Fatalf("an open mapping counted as wiped: %+v", res)
	}
	st, _ := LoadState(h, dataDir)
	if st == nil || !st.Dirty {
		t.Fatalf("machine not quarantined: %+v", st)
	}
	if err := rt.Start(StartOptions{ID: "R2", Pubkey: key(t)}); !errors.Is(err, ErrRentalPresent) {
		t.Errorf("a quarantined machine accepted a rental: %v", err)
	}
}

func TestGPUThatDoesNotVerifyIsDirty(t *testing.T) {
	h := newHost()
	rt, _ := newRuntime(h, func([]BoundDevice) bool { return false })
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatal(err)
	}
	if res := rt.Stop(); res.GPUClean {
		t.Fatalf("an unverified GPU counted as clean: %+v", res)
	}
}

// The agent can restart while a rental runs (an upgrade, a crash). Its new
// runtime then sees the GPUs on vfio-pci, so the drivers they go back to must
// come from what the rental recorded, or the vendor's checks would be skipped.
func TestTheVerifierIsToldTheDriversTheRentalRecorded(t *testing.T) {
	h := newHost()
	first, _ := newRuntime(h, nil)
	if err := first.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatal(err)
	}
	var got []BoundDevice
	restarted, _ := newRuntime(h, func(returned []BoundDevice) bool { got = returned; return true })
	if res := restarted.Stop(); !res.Clean() {
		t.Fatalf("Stop = %+v", res)
	}
	want := []BoundDevice{{BDF: testGPU, Driver: "nvidia"}, {BDF: testAudio, Driver: "snd_hda_intel"}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("verifier got %+v, want %+v", got, want)
	}
}

func TestVMThatExitsWhileBootingFails(t *testing.T) {
	h := newHost()
	delete(h.OnRun, "systemd-run --unit=") // the unit never becomes active
	h.Outputs["journalctl -u gpu-rental-R1"] = "qemu-system-x86_64: vfio 0000:01:00.0: failed to setup container"
	rt, _ := newRuntime(h, nil)
	err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)})
	if err == nil || !strings.Contains(err.Error(), "failed to setup container") {
		t.Fatalf("Start = %v, want the VM's own exit reason", err)
	}
	if h.Driver(testGPU) != "nvidia" {
		t.Errorf("GPU not given back after a failed boot")
	}
}

func TestDockerForwardPolicyIsOpenedForTheRentalOnlyWhileRented(t *testing.T) {
	h := newHost()
	h.SetFail("iptables -S DOCKER-USER", nil)
	rt, _ := newRuntime(h, func([]BoundDevice) bool { return true })
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatal(err)
	}
	if !h.Ran("run iptables -I DOCKER-USER -i gpurent0 -j ACCEPT") {
		t.Errorf("no DOCKER-USER accept for the rental bridge")
	}
	rt.Stop()
	if !h.Ran("run iptables -D DOCKER-USER -i gpurent0 -j ACCEPT") {
		t.Errorf("DOCKER-USER accept not removed at teardown")
	}
}

func TestBadKeyIsRefusedBeforeAnythingHappens(t *testing.T) {
	h := newHost()
	rt, fence := newRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: "ssh-ed25519 AAAA\nruncmd: [rm, -rf, /]"}); err == nil {
		t.Fatal("a multi-line key was accepted")
	}
	if fence.applied != 0 || len(h.Calls) != 0 {
		t.Errorf("work done for a refused key: %v", h.Calls)
	}
}

func TestGroupProblems(t *testing.T) {
	h := fakehost.New()
	h.PCI(testGPU, "nvidia", "0x030200", testGPU, testAudio, "0000:00:01.0", "0000:02:00.0")
	h.PCI(testAudio, "snd_hda_intel", "0x040300")
	h.PCI("0000:00:01.0", "pcieport", "0x060400") // a bridge: fine
	h.PCI("0000:02:00.0", "ixgbe", "0x020000")    // a NIC: not fine
	problems := GroupProblems(h, []string{testGPU})
	if len(problems) != 1 || !strings.Contains(problems[0], "0000:02:00.0") {
		t.Fatalf("problems = %v, want only the NIC", problems)
	}
	funcs, _ := GroupFunctions(h, []string{testGPU})
	if strings.Join(funcs, " ") != "0000:01:00.0 0000:01:00.1 0000:02:00.0" {
		t.Errorf("functions = %v (bridges must be left out)", funcs)
	}
}

func TestDestroyDiskNoticesALoopStillAttached(t *testing.T) {
	h := fakehost.New()
	h.Outputs["losetup -j"] = "/dev/loop7: []: (/var/lib/gpu-agent/rentals/R1/disk.img)\n"
	wiped, detail := DestroyDisk(h, DiskState{File: "/x/disk.img", Loop: "/dev/loop7"})
	if wiped || len(detail) == 0 {
		t.Fatalf("a still-attached loop device counted as wiped")
	}
}

func TestDefaultProbes(t *testing.T) {
	h := fakehost.New()
	h.Outputs["ip route get 1.1.1.1"] = "1.1.1.1 via 192.168.1.1 dev enp1s0 src 192.168.1.50 uid 0\n    cache\n"
	got := strings.Join(DefaultProbes(h), " ")
	if got != "10.254.254.1:22 192.168.1.1:80 192.168.1.50:22" {
		t.Errorf("probes = %s", got)
	}
}
