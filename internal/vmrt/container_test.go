package vmrt

import (
	"errors"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// newContainerHost is a machine set up so a container rental can come up on the
// fake host: the encrypted volume tools answer, podman "runs" a container that
// then inspects as running with a pid, and the guest answers on :22.
func newContainerHost() *fakehost.Host {
	h := fakehost.New()
	h.PCI(testGPU, "nvidia", "0x030200", testGPU)
	h.PCIID(testGPU, "10de", "2e12")
	h.Files["/proc/sys/net/ipv4/ip_forward"] = []byte("0\n")
	h.Outputs["losetup --find --show"] = "/dev/loop7\n"
	h.Fail["systemctl is-active --quiet"] = errors.New("inactive")
	h.Fail["iptables -S DOCKER-USER"] = errors.New("no such chain")
	h.Outputs["iptables -S FORWARD"] = "-P FORWARD ACCEPT\n"
	h.Outputs["podman inspect --format {{.State.Pid}}"] = "12345\n"
	h.OnRun["truncate -s"] = func(h *fakehost.Host, cmd string) { h.SetFile(fakehost.LastField(cmd), nil) }
	h.OnRun["cryptsetup open"] = func(h *fakehost.Host, cmd string) { h.SetFile("/dev/mapper/"+fakehost.LastField(cmd), nil) }
	h.OnRun["cryptsetup close"] = func(h *fakehost.Host, cmd string) { h.DeleteFile("/dev/mapper/" + fakehost.LastField(cmd)) }
	h.OnRun["podman run"] = func(h *fakehost.Host, cmd string) {
		h.SetOutput("podman inspect --format {{.State.Running}} "+containerName("R1"), "true\n")
	}
	h.OnRun["podman stop"] = func(h *fakehost.Host, cmd string) {
		h.SetOutput("podman inspect --format {{.State.Running}} "+containerName("R1"), "false\n")
	}
	h.Dial[GuestIP+":22"] = true
	return h
}

func containerSpec() Spec {
	return Spec{Arch: "arm64", DataDir: dataDir, GPUs: []string{testGPU}, TotalMemMB: 131072, CPUs: 20, DiskGB: 200}
}

func newContainerRuntime(h *fakehost.Host, verify func([]BoundDevice) bool) (*ContainerRuntime, *fakeFence) {
	fence := &fakeFence{h: h}
	return NewContainer(h, containerSpec(), fence, ContainerImageRef, []string{"nvidia.com/gpu=GPU-abc"}, verify), fence
}

func hasCall(h *fakehost.Host, substr string) bool {
	for _, c := range h.Calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func TestContainerStartOrderAndHardening(t *testing.T) {
	h := newContainerHost()
	rt, fence := newContainerRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if fence.applied != 1 {
		t.Errorf("fence applied %d times, want 1", fence.applied)
	}
	// Fence first; bridge before the volume; volume before the container; the
	// container before its network is wired in.
	before(t, h, "run fence apply", "run ip link add gpurent0")
	before(t, h, "run ip link add gpurent0", "run cryptsetup open")
	before(t, h, "run cryptsetup open", "run mount /dev/mapper")
	before(t, h, "run mount /dev/mapper", "run podman run")
	before(t, h, "run podman run", "run ip link add grveth0h")

	// The hardening flags and the CDI GPU are on the podman run line; nothing
	// privileged, no host network, no host mounts.
	var runLine string
	for _, c := range h.Calls {
		if strings.HasPrefix(c, "run podman run ") {
			runLine = c
		}
	}
	if runLine == "" {
		t.Fatal("no podman run call recorded")
	}
	for _, want := range []string{"--userns=auto", "--security-opt=no-new-privileges", "--cap-drop=ALL",
		"--read-only", "--network=none", "--device nvidia.com/gpu=GPU-abc"} {
		if !strings.Contains(runLine, want) {
			t.Errorf("podman run missing %q\n  got: %s", want, runLine)
		}
	}
	for _, unwanted := range []string{"--privileged", "--network=host", "--net=host"} {
		if strings.Contains(runLine, unwanted) {
			t.Errorf("podman run must not contain %q", unwanted)
		}
	}
	if !rt.Present() {
		t.Error("a started rental must be Present")
	}
}

func TestContainerTeardownWipesAndVerifiesGPU(t *testing.T) {
	// A verifier that says the GPU is NOT clean must keep the teardown dirty.
	h := newContainerHost()
	rt, _ := newContainerRuntime(h, func([]BoundDevice) bool { return false })
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := rt.Stop()
	if res.Clean() {
		t.Error("teardown must not be clean when the GPU verifier fails")
	}
	if !hasCall(h, "run podman rm") {
		t.Error("teardown must remove the container")
	}
	if !hasCall(h, "run ip link del grveth0h") {
		t.Error("teardown must delete the veth")
	}
	if !hasCall(h, "run umount") {
		t.Error("teardown must unmount the volume")
	}
	if !hasCall(h, "run cryptsetup close") {
		t.Error("teardown must close the dm-crypt mapping (the wipe)")
	}
	if !rt.Dirty() {
		t.Error("an unverified teardown must leave the machine dirty")
	}

	// A clean verifier, on a fresh host, tears down clean.
	h2 := newContainerHost()
	rt2, _ := newContainerRuntime(h2, func([]BoundDevice) bool { return true })
	if err := rt2.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res := rt2.Stop(); !res.Clean() {
		t.Errorf("teardown must be clean: wiped=%v gpuClean=%v detail=%v", res.Wiped, res.GPUClean, res.Detail)
	}
	if rt2.Present() {
		t.Error("a clean teardown must clear the rental state")
	}
}

func TestEvaluateContainer(t *testing.T) {
	clean := StopResult{Wiped: true, GPUClean: true}
	probes := []string{"10.254.254.1:22", "192.0.2.9:22"}
	base := func() SerialReport {
		return SerialReport{Begin: true, End: true, Internet: "ok",
			NVSMI:   []NVSMIRow{{BDF: "0000:01:00.0", Name: "NVIDIA GB10"}},
			Blocked: []string{"10.254.254.1:22", "192.0.2.9:22"}}
	}
	// A good run passes and verifies the GPU.
	if passed, gpu, probs := EvaluateContainer(base(), true, 1, probes, true, clean); !passed || !gpu {
		t.Errorf("good run: passed=%v gpu=%v problems=%v", passed, gpu, probs)
	}
	// A target the fence should block but the container reached: fail.
	r := base()
	r.Reached = []string{"192.0.2.9:22"}
	r.Blocked = []string{"10.254.254.1:22"}
	if passed, _, _ := EvaluateContainer(r, true, 1, probes, true, clean); passed {
		t.Error("a reached probe target must fail the test")
	}
	// GPU not seen over CDI: fail.
	r = base()
	r.NVSMI = nil
	if passed, _, _ := EvaluateContainer(r, true, 1, probes, true, clean); passed {
		t.Error("no GPU over CDI must fail the test")
	}
	// No internet: fail.
	r = base()
	r.Internet = "fail"
	if passed, _, _ := EvaluateContainer(r, true, 1, probes, true, clean); passed {
		t.Error("no internet must fail the test")
	}
	// Unclean teardown: fail.
	if passed, _, _ := EvaluateContainer(base(), true, 1, probes, true, StopResult{Detail: []string{"loop still attached"}}); passed {
		t.Error("an unclean teardown must fail the test")
	}
	// SSH never opened: fail.
	if passed, _, _ := EvaluateContainer(base(), true, 1, probes, false, clean); passed {
		t.Error("sshd never opening must fail the test")
	}
}
