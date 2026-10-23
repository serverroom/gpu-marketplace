package vmrt

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// v0.3.3: a GB10 host's audit of the container a rental runs in. Each test is
// one of its findings.

// The profile is podman's default with no way to a new user namespace: no rule
// allows clone, clone3 or unshare unconditionally any more, clone and unshare
// are allowed only without CLONE_NEWUSER and refused (EPERM) with it, clone3
// answers ENOSYS, and every other syscall keeps its rule.
func TestTheRentalProfileRefusesNewUserNamespaces(t *testing.T) {
	out, err := denyNewUserNamespaces(embeddedSeccompDefault)
	if err != nil {
		t.Fatal(err)
	}
	var before, after struct {
		DefaultAction string `json:"defaultAction"`
		Syscalls      []struct {
			Names    []string `json:"names"`
			Action   string   `json:"action"`
			ErrnoRet int      `json:"errnoRet"`
			Args     []struct {
				Index    int    `json:"index"`
				Value    uint64 `json:"value"`
				ValueTwo uint64 `json:"valueTwo"`
				Op       string `json:"op"`
			} `json:"args"`
		} `json:"syscalls"`
	}
	json.Unmarshal(embeddedSeccompDefault, &before)
	if err := json.Unmarshal(out, &after); err != nil {
		t.Fatal(err)
	}
	if after.DefaultAction != before.DefaultAction {
		t.Errorf("default action %q, want the host's %q", after.DefaultAction, before.DefaultAction)
	}
	count := func(names []string) (n int) {
		for _, s := range names {
			if s != "clone" && s != "clone3" && s != "unshare" {
				n++
			}
		}
		return n
	}
	var kept, was int
	for _, r := range before.Syscalls {
		was += count(r.Names)
	}
	var allowNoUser, denyUser, clone3Enosys bool
	for _, r := range after.Syscalls {
		kept += count(r.Names)
		for _, n := range r.Names {
			if n != "clone" && n != "clone3" && n != "unshare" {
				continue
			}
			switch {
			case r.Action == "SCMP_ACT_ALLOW" && len(r.Args) == 1 && r.Args[0].Value == cloneNewUser && r.Args[0].ValueTwo == 0 && r.Args[0].Op == "SCMP_CMP_MASKED_EQ" && n != "clone3":
				allowNoUser = true
			case r.Action == "SCMP_ACT_ERRNO" && r.ErrnoRet == errnoEPERM && len(r.Args) == 1 && r.Args[0].ValueTwo == cloneNewUser && n != "clone3":
				denyUser = true
			case r.Action == "SCMP_ACT_ERRNO" && r.ErrnoRet == errnoENOSYS && n == "clone3" && len(r.Args) == 0:
				clone3Enosys = true
			default:
				t.Errorf("rule %+v still lets %s through", r, n)
			}
		}
	}
	if !allowNoUser || !denyUser || !clone3Enosys {
		t.Errorf("allow without CLONE_NEWUSER %v, EPERM with it %v, clone3 ENOSYS %v; want all three", allowNoUser, denyUser, clone3Enosys)
	}
	if kept != was {
		t.Errorf("%d other syscall entries kept of %d", kept, was)
	}
}

// The host's own default is the base when it has one; the one this agent
// carries otherwise.
func TestTheProfileStartsFromTheHostsDefault(t *testing.T) {
	h := fakehost.New()
	h.Files["/usr/share/containers/seccomp.json"] = []byte(`{"defaultAction":"SCMP_ACT_ERRNO","syscalls":[{"names":["read","unshare"],"action":"SCMP_ACT_ALLOW"}]}`)
	out, err := RentalSeccompProfile(h)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"read"`) || strings.Contains(string(out), `"accept4"`) {
		t.Errorf("profile did not start from the host's file:\n%s", out)
	}
	if out, err = RentalSeccompProfile(fakehost.New()); err != nil || !strings.Contains(string(out), `"accept4"`) {
		t.Errorf("without a host default, want the carried one (err %v)", err)
	}
}

func runLine(t *testing.T, h *fakehost.Host) string {
	t.Helper()
	for _, c := range h.Calls {
		if strings.HasPrefix(c, "run podman run ") {
			return c
		}
	}
	t.Fatal("no podman run call recorded")
	return ""
}

// The container: no swap, /dev/shm and /tmp and /var/tmp as memory, the
// listing's cores, and the profile.
func TestTheRentalContainerHardening(t *testing.T) {
	h := newContainerHost()
	rt, _ := newContainerRuntime(h, nil)
	rt.spec.GuestCores = []int{5, 6, 7, 8, 9, 15, 16, 17, 18, 19}
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	line := runLine(t, h)
	mb := rt.spec.GuestMemoryMB()
	for _, want := range []string{
		"--memory " + strconv.Itoa(mb) + "m --memory-swap " + strconv.Itoa(mb) + "m",
		"--shm-size 16384m",
		"--tmpfs /tmp:rw,nosuid,nodev,size=16384m",
		"--tmpfs /var/tmp:rw,nosuid,nodev,size=16384m",
		"--cpuset-cpus 5,6,7,8,9,15,16,17,18,19",
		"--security-opt=seccomp=" + NewRental(rt.spec.Storage(), "R1").Dir + "/seccomp.json",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("podman run missing %q\n  got: %s", want, line)
		}
	}
	if _, ok := h.Files[NewRental(rt.spec.Storage(), "R1").Dir+"/seccomp.json"]; !ok {
		t.Error("the seccomp profile was not written")
	}
}

func TestScratchMountsArePartOfTheMemoryUpTo16GiB(t *testing.T) {
	for _, c := range []struct{ mem, part, want int }{
		{112128, 2, 16384}, {16384, 2, 8192}, {16384, 4, 4096}, {200, 4, 64},
	} {
		if got := scratchMB(c.mem, c.part); got != c.want {
			t.Errorf("scratchMB(%d, %d) = %d, want %d", c.mem, c.part, got, c.want)
		}
	}
}

// The CDI spec is written again before the container starts, and a spec naming
// a node that is not the NVIDIA GPU's refuses the rental.
func TestTheCDISpecIsWrittenAgainBeforeTheContainer(t *testing.T) {
	h := newContainerHost()
	rt, _ := newContainerRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	before(t, h, "run nvidia-ctk cdi generate", "run podman run")
	var spec map[string]interface{}
	json.Unmarshal(h.Files[containerCDIPath], &spec)
	if spec["cdiVersion"] != "0.6.0" || strings.Contains(string(h.Files[containerCDIPath]), "additionalGids") {
		t.Errorf("spec not normalized: %s", h.Files[containerCDIPath])
	}

	// This boot the firmware framebuffer is card0 and the GPU card1 -- but the
	// toolkit names card0 (a stale spec would): no rental on it.
	h = newContainerHost()
	h.Files["/dev/dri/card0"] = nil
	h.Files["/sys/class/drm/card0/device/vendor"] = []byte("0x1af4\n")
	h.OnRun["nvidia-ctk cdi generate"] = func(h *fakehost.Host, cmd string) { h.SetFile(containerCDIPath, []byte(testCDISpec("card0"))) }
	rt, _ = newContainerRuntime(h, nil)
	err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)})
	if err == nil || !strings.Contains(err.Error(), "/dev/dri/card0, which is not the NVIDIA GPU's") {
		t.Fatalf("Start = %v, want a refusal naming card0", err)
	}
	if h.Ran("run podman run") {
		t.Error("the container started on a spec naming another device")
	}
}

func TestContainerCDIProblem(t *testing.T) {
	h := fakehost.New()
	if p := ContainerCDIProblem(h); !strings.Contains(p, "is missing") {
		t.Errorf("no spec: %q", p)
	}
	h.Files[containerCDIPath] = []byte(testCDISpec("card1"))
	h.Files["/dev/dri/renderD128"] = nil
	h.Files["/sys/class/drm/renderD128/device/vendor"] = []byte("0x10de\n")
	if p := ContainerCDIProblem(h); !strings.Contains(p, "/dev/dri/card1, which does not exist") {
		t.Errorf("a card gone this boot: %q", p)
	}
	h.Files["/dev/dri/card1"] = nil
	h.Files["/sys/class/drm/card1/device/vendor"] = []byte("0x10de\n")
	if p := ContainerCDIProblem(h); p != "" {
		t.Errorf("a good spec: %q", p)
	}
}

// The loop device under the rental's disk does direct I/O, and one attached
// without it is used rather than a second one.
func TestTheRentalDiskUsesDirectIO(t *testing.T) {
	h := fakehost.New()
	h.OnRun["truncate -s"] = func(h *fakehost.Host, cmd string) { h.SetFile(fakehost.LastField(cmd), nil) }
	h.Outputs["losetup --direct-io=on --find --show"] = "/dev/loop3\n"
	ds, err := openEncrypted(h, Spec{DiskGB: 10}, "/r", "R1")
	if err != nil || ds.Loop != "/dev/loop3" {
		t.Fatalf("openEncrypted = %+v, %v", ds, err)
	}

	h = fakehost.New()
	h.OnRun["truncate -s"] = func(h *fakehost.Host, cmd string) { h.SetFile(fakehost.LastField(cmd), nil) }
	h.Fail["losetup --direct-io=on"] = errors.New("failed to set direct io: Invalid argument")
	h.Outputs["losetup -j"] = "/dev/loop4: []: (/r/disk.img)\n"
	if ds, err = openEncrypted(h, Spec{DiskGB: 10}, "/r", "R1"); err != nil || ds.Loop != "/dev/loop4" || h.Ran("run losetup --find --show") {
		t.Errorf("attached without direct I/O: %+v, %v; want /dev/loop4 reused, not a second device", ds, err)
	}

	h = fakehost.New()
	h.OnRun["truncate -s"] = func(h *fakehost.Host, cmd string) { h.SetFile(fakehost.LastField(cmd), nil) }
	h.Fail["losetup --direct-io=on"] = errors.New("unsupported")
	h.Outputs["losetup --find --show"] = "/dev/loop5\n"
	if ds, err = openEncrypted(h, Spec{DiskGB: 10}, "/r", "R1"); err != nil || ds.Loop != "/dev/loop5" {
		t.Errorf("no direct I/O at all: %+v, %v; want the plain loop device", ds, err)
	}
}

// The image carries what GPU work needs, tells the renter where their work is
// kept, and is a new tag so every host builds it again.
func TestTheRentalImage(t *testing.T) {
	df := containerDockerfile()
	for _, p := range []string{"python3-venv", "python3-pip", "build-essential", "libgomp1", "tmux", "git", "zstd", "rsync", "htop", "apt-get upgrade -y", "COPY motd /etc/motd"} {
		if !strings.Contains(df, p) {
			t.Errorf("Dockerfile lacks %q", p)
		}
	}
	if ContainerImageRef != "localhost/gpu-agent-rental:6" {
		t.Errorf("image ref %s: a changed image needs a new tag", ContainerImageRef)
	}
	h := fakehost.New()
	if err := PrepareContainerImage(h, "/var/lib/gpu-agent", nil); err != nil {
		t.Fatal(err)
	}
	if m := string(h.Files["/var/lib/gpu-agent/container-build/motd"]); !strings.Contains(m, "/home/renter") {
		t.Errorf("motd = %q", m)
	}
}

// guardHost is a machine with a container rental on a GB10: 128 GiB, the
// rental's cgroup, and GPU processes -- two of the rental's, one of the host's.
func guardHost(t *testing.T, own int64, gpu string, freeMB int64) (*ContainerRuntime, *fakehost.Host) {
	t.Helper()
	h := newContainerHost()
	rt, _ := newContainerRuntime(h, nil)
	rt.spec.TotalMemMB = 124000 // what a GB10 reports
	if err := SaveState(h, rt.spec.DataDir, &State{RentalID: "R1", Mode: ModeContainer, ContainerID: containerName("R1")}); err != nil {
		t.Fatal(err)
	}
	const cg = "/machine.slice/libpod-abc.scope"
	h.Outputs["podman inspect --format {{.State.CgroupPath}}"] = cg + "\n"
	h.Files["/sys/fs/cgroup"+cg+"/memory.current"] = []byte(strconv.FormatInt(own, 10) + "\n")
	h.Files["/proc/401/cgroup"] = []byte("0::" + cg + "/container\n")
	h.Files["/proc/402/cgroup"] = []byte("0::" + cg + "/container\n")
	h.Files["/proc/900/cgroup"] = []byte("0::/user.slice/user-1000.slice/session-2.scope\n")
	h.Outputs["nvidia-smi --query-compute-apps=pid,used_memory"] = gpu
	h.Files["/proc/meminfo"] = []byte("MemTotal: 126976000 kB\nMemAvailable: " + strconv.FormatInt(freeMB*1024, 10) + " kB\n")
	return rt, h
}

const gib = int64(1024 * 1024 * 1024)

func TestTheRentalIsHeldToItsMemoryOnTheGPU(t *testing.T) {
	// 2 GiB of its own plus 60 + 50 GiB on the GPU is past its ~109 GiB: the
	// rental's biggest GPU process goes, the host's 3 GiB one stays. (The
	// machine is then nearly full: 117 of its 121 GiB in use.)
	rt, h := guardHost(t, 2*gib, "401, 61440\n402, 51200\n900, 3072\n", 7000)
	v, err := rt.GuardMemory()
	if err != nil {
		t.Fatal(err)
	}
	if v.KilledPID != 401 || !h.Ran("run kill -KILL 401") || h.Ran("run kill -KILL 900") || h.Ran("run kill -KILL 402") {
		t.Errorf("verdict %+v; want the rental's biggest GPU process (401) stopped, nothing else", v)
	}
	if !strings.Contains(v.Reason, "over its") {
		t.Errorf("reason %q", v.Reason)
	}

	// Within its memory and the machine has room: nothing happens.
	rt, h = guardHost(t, 2*gib, "401, 20480\n900, 30720\n", 40000)
	if v, _ = rt.GuardMemory(); v.KilledPID != 0 || h.Ran("run kill") {
		t.Errorf("within limits: %+v", v)
	}

	// Within its own memory, but the machine is nearly out (the host's own use
	// grew): the rental's GPU process goes, never the host's.
	rt, h = guardHost(t, 2*gib, "401, 20480\n900, 90000\n", 3000)
	if v, _ = rt.GuardMemory(); v.KilledPID != 401 || !strings.Contains(v.Reason, "memory free") || h.Ran("run kill -KILL 900") {
		t.Errorf("machine nearly out: %+v", v)
	}

	// The GPU does not say what a process holds: only the floor applies.
	rt, h = guardHost(t, 2*gib, "401, [N/A]\n", 40000)
	if v, _ = rt.GuardMemory(); v.KilledPID != 0 || v.RentalMB != -1 {
		t.Errorf("unknown GPU memory, room on the machine: %+v", v)
	}
	rt, h = guardHost(t, 2*gib, "401, [N/A]\n", 2000)
	if v, _ = rt.GuardMemory(); v.KilledPID != 401 {
		t.Errorf("unknown GPU memory, machine nearly out: %+v", v)
	}
}

// memStat gives the rental's cgroup a memory.stat: file is its page cache,
// shmem its tmpfs (part of file).
func memStat(h *fakehost.Host, anon, file, shmem int64) {
	const cg = "/machine.slice/libpod-abc.scope"
	h.Files["/sys/fs/cgroup"+cg+"/memory.stat"] = []byte(fmt.Sprintf(
		"anon %d\nfile %d\nkernel 104857600\nsock 0\nshmem %d\nfile_mapped %d\nactive_file %d\ninactive_file %d\n",
		anon, file, shmem, file-shmem, (file-shmem)/2, (file-shmem)-(file-shmem)/2))
}

func TestAModelMappedFromItsFileIsNotCountedTwice(t *testing.T) {
	// A renter with two llama-server models (40 + 30 GiB GGUF, mmap'd) under a
	// ~109 GiB rental: the cgroup holds 4 GiB of its own and 70 GiB of the
	// models' file pages; the GPU holds the 70 GiB the models were copied to.
	// memory.current 74 GiB + 70 GiB on the GPU used to read as 144 GiB, and the
	// second model was stopped. What the rental holds is 4 + 70: no kill.
	rt, h := guardHost(t, 74*gib, "401, 40960\n402, 30720\n900, 4096\n", 30000)
	memStat(h, 4*gib-100<<20, 70*gib, 0)
	v, err := rt.GuardMemory()
	if err != nil {
		t.Fatal(err)
	}
	if v.KilledPID != 0 || h.Ran("run kill") {
		t.Errorf("mapped models within the limit were stopped: %+v", v)
	}
	if v.RentalMB != 4*1024+70*1024 {
		t.Errorf("RentalMB = %d, want %d (page cache left out)", v.RentalMB, 4*1024+70*1024)
	}

	// A real overrun: the same cache, but 45 GiB of the rental's own (a big
	// heap, and 5 GiB in /dev/shm, which is shmem and stays counted) and the GPU
	// at 70 GiB: 115 GiB, past its ~109, and the machine has 119 GiB in use. The
	// biggest GPU process goes.
	rt, h = guardHost(t, 115*gib, "401, 40960\n402, 30720\n900, 4096\n", 5000)
	memStat(h, 40*gib, 75*gib, 5*gib)
	if v, _ = rt.GuardMemory(); v.KilledPID != 401 || !h.Ran("run kill -KILL 401") || h.Ran("run kill -KILL 900") {
		t.Errorf("a real overrun: %+v; want 401 stopped", v)
	}
	if v.RentalMB != 45*1024+70*1024 {
		t.Errorf("RentalMB = %d, want %d", v.RentalMB, 45*1024+70*1024)
	}

	// The cache does not hide the machine running out: the floor still applies.
	rt, h = guardHost(t, 74*gib, "401, 40960\n402, 30720\n", 3000)
	memStat(h, 4*gib, 70*gib, 0)
	if v, _ = rt.GuardMemory(); v.KilledPID != 401 || !strings.Contains(v.Reason, "memory free") {
		t.Errorf("machine nearly out: %+v", v)
	}
}

func TestAGPUAllocationChargedToTheContainerIsNotCountedTwice(t *testing.T) {
	// Rental #25 (2026-10-19): two models loaded without mmap (Ollama's runner,
	// --load-mode dio), memory.current 82,394,537,984 B (78,577 MiB) of anon, and
	// the GPU reporting the same models' 70 GiB: 148 GiB counted under a 109 GiB
	// rental, while the machine had ~39 GiB free. The models are charged to the
	// cgroup AND reported by nvidia-smi; the machine's own reading shows they are
	// held once. The host's processes hold 2 GiB (the root cgroup's anon beside
	// the rental's).
	own := int64(82394537984)
	rt, h := guardHost(t, own, "401, 40960\n402, 30720\n", 124000-78577-2048-3000)
	memStat(h, own-100<<20, 0, 0)
	h.Files["/sys/fs/cgroup/memory.stat"] = []byte(fmt.Sprintf("anon %d\nfile %d\nshmem 0\n", own-100<<20+2*gib, 9*gib))
	v, err := rt.GuardMemory()
	if err != nil {
		t.Fatal(err)
	}
	if v.KilledPID != 0 || h.Ran("run kill") {
		t.Errorf("models charged to the cgroup and on the GPU were stopped: %+v", v)
	}
	if want := int64(78577 + 3000); v.RentalMB != want || v.OwnMB != 78577 || v.GPUMB != 71680 {
		t.Errorf("verdict %+v; want RentalMB %d (the machine's in-use reading)", v, want)
	}

	// The same rental growing for real: it is now held to the machine's reading,
	// and stopped once that passes its limit.
	rt, h = guardHost(t, 105*gib, "401, 40960\n402, 30720\n", 124000-105*1024-2048-6000)
	memStat(h, 105*gib, 0, 0)
	h.Files["/sys/fs/cgroup/memory.stat"] = []byte(fmt.Sprintf("anon %d\nshmem 0\n", 107*gib))
	if v, _ = rt.GuardMemory(); v.KilledPID != 401 || !strings.Contains(v.Reason, "over its") {
		t.Errorf("a real overrun charged to the cgroup: %+v", v)
	}
}

// Without a container rental nothing is asked of the GPU or podman.
func TestTheMemoryGuardIsIdleWithoutARental(t *testing.T) {
	h := newContainerHost()
	rt, _ := newContainerRuntime(h, nil)
	if v, err := rt.GuardMemory(); err != nil || v.KilledPID != 0 || h.Ran("run nvidia-smi") || h.Ran("run podman") {
		t.Errorf("no rental: %+v %v, calls %q", v, err, h.Calls)
	}
}

func TestContainerSwapIsKeptOff(t *testing.T) {
	const sw = "/sys/fs/cgroup/machine.slice/libpod-abc.scope/memory.swap.max"

	// A daemon-reload reset memory.swap.max to "max": the guard writes 0 back.
	rt, h := guardHost(t, 2*gib, "401, 20480\n900, 30720\n", 40000)
	h.Files[sw] = []byte("max\n")
	v, err := rt.GuardMemory()
	if err != nil {
		t.Fatal(err)
	}
	wrote := false
	for _, c := range h.Calls {
		if c == "write "+sw+"=0" {
			wrote = true
		}
	}
	if !v.SwapReset || !wrote {
		t.Errorf("drifted swap.max: SwapReset=%v wrote=%v; calls %v", v.SwapReset, wrote, h.Calls)
	}

	// Already 0: left alone, nothing written.
	rt, h = guardHost(t, 2*gib, "401, 20480\n900, 30720\n", 40000)
	h.Files[sw] = []byte("0\n")
	v, _ = rt.GuardMemory()
	for _, c := range h.Calls {
		if strings.HasPrefix(c, "write "+sw) {
			t.Errorf("wrote swap.max when already 0: %v", h.Calls)
		}
	}
	if v.SwapReset {
		t.Errorf("SwapReset should be false when already 0")
	}
}
