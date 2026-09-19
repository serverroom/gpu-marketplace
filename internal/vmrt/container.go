package vmrt

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/netguard"
)

// ContainerRuntime runs a rental as a hardened rootful-podman container instead
// of a QEMU/VFIO microVM. It is the fallback for a machine whose GPU cannot be
// passed through to a guest at all -- a DGX Spark's GB10, until the signed
// nvgrace-gpu-vfio-pci carries its device id (see CONTAINER-DESIGN.md).
//
// It keeps the microVM's safety contract so nothing downstream changes: the
// netguard fence is applied first and removed last, the renter's writable space
// is the same dm-crypt volume whose key dies with the mapping (the wipe), the
// tenant sits on the same bridge at the same GuestIP with an sshd on :22 (so the
// renter's SSH forward is unchanged), and teardown fails closed -- a rental
// whose wipe or GPU turnover does not verify is quarantined dirty.
//
// The GPU is NOT taken from the host: it stays on the nvidia driver and is
// shared into the container read-only over CDI. Before the rental the host
// desktop is closed and the host's own NVIDIA services stopped so the renter
// has the GPU to themselves; both come back at teardown. "Clean" therefore
// means no renter compute process is left on the GPU and its memory is clear,
// not a driver rebind (the GPU never left the host).
type ContainerRuntime struct {
	h     Host
	spec  Spec
	fence Fence
	// image is the base OCI image a rental runs (the container analog of the
	// golden disk); prepared by 'gpu-agent runtime prepare'.
	image string
	// cdiDevices are the CDI device refs for the GPUs a rental gets
	// (e.g. "nvidia.com/gpu=GPU-<uuid>").
	cdiDevices []string
	// verifyGPU checks, after the container is gone, that nothing of the rental
	// is left on the GPUs. The provisioner supplies it (gpuVerifier), given the
	// rented GPUs as BoundDevice{Driver:"nvidia"} -- the GPU never left nvidia.
	verifyGPU func(returned []BoundDevice) bool
}

// NewContainer builds a container runtime.
func NewContainer(h Host, spec Spec, fence Fence, image string, cdiDevices []string, verifyGPU func([]BoundDevice) bool) *ContainerRuntime {
	return &ContainerRuntime{h: h, spec: spec, fence: fence, image: image, cdiDevices: cdiDevices, verifyGPU: verifyGPU}
}

// Spec is what this runtime gives a rental.
func (rt *ContainerRuntime) Spec() Spec { return rt.spec }

// containerName is the podman container for a rental id. It doubles as the
// state marker that a container rental is present.
func containerName(id string) string { return "gpu-rental-" + id }

// Start brings a container rental up in the fence-first order: fence, the
// bridge, the encrypted volume, the hardened container (no network yet), the
// veth that puts it on the bridge at GuestIP, then wait for its sshd. Each step
// is recorded before the next so a crash tears down exactly what exists.
func (rt *ContainerRuntime) Start(o StartOptions) (err error) {
	if st, lerr := LoadState(rt.h, rt.spec.DataDir); lerr != nil || st != nil {
		if lerr != nil {
			return fmt.Errorf("read rental state: %w", lerr)
		}
		return fmt.Errorf("%w (%s)", ErrRentalPresent, st.RentalID)
	}
	if o.Pair != nil {
		return errors.New("a linked-pair rental cannot run in container mode")
	}
	name := containerName(o.ID)
	r := NewRental(rt.spec.Storage(), o.ID)
	st := &State{RentalID: o.ID, Rental: r, StartedAt: time.Now().Unix(), Mode: ModeContainer, ContainerID: name}
	save := func() error { return SaveState(rt.h, rt.spec.DataDir, st) }
	if err = save(); err != nil {
		return fmt.Errorf("record rental: %w", err)
	}
	defer func() {
		if err != nil {
			rt.Stop()
		}
	}()

	if err = rt.h.MkdirAll(r.Dir, 0700); err != nil {
		return fmt.Errorf("rental directory: %w", err)
	}

	// Fence first: nothing the tenant runs is ever unfenced.
	if err = rt.fence.Apply(); err != nil {
		return fmt.Errorf("isolate network: %w", err)
	}
	st.Fenced = true
	if err = save(); err != nil {
		return err
	}

	// The bridge (no tap: a container attaches over a veth, wired in once it has
	// a network namespace). Reuses the same bridge, forwarding and accepts the
	// microVM uses, so the fence covers it identically.
	st.Net, err = setupBridge(rt.h)
	if serr := save(); err == nil {
		err = serr
	}
	if err != nil {
		return fmt.Errorf("rental network: %w", err)
	}

	// The renter's writable space: the same dm-crypt volume as a microVM disk,
	// formatted with a filesystem and mounted, rather than a golden image.
	st.Disk, st.VolumeMount, err = createEncryptedVolume(rt.h, r.Dir, o.ID, rt.spec.DiskGB)
	if serr := save(); err == nil {
		err = serr
	}
	if err != nil {
		return err
	}

	// The GPU is shared, not taken; give the renter a clean one: close a DGX
	// Spark's desktop and stop the host's NVIDIA services, both restored on
	// teardown. A desktop on any other machine is a reason not to host and was
	// refused in preflight.
	if err = rt.freeGPU(st, save); err != nil {
		return err
	}

	pubkey, perr := NormalizePubkey(o.Pubkey)
	if perr != nil {
		return fmt.Errorf("renter key: %w", perr)
	}
	akFile := r.Dir + "/authorized_keys"
	if err = rt.h.WriteFile(akFile, []byte(pubkey+"\n"), 0644); err != nil {
		return fmt.Errorf("write renter key: %w", err)
	}

	if err = rt.h.Run("podman", rt.runArgs(name, akFile, st.VolumeMount, o)...); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	st.ContainerID = name
	if err = save(); err != nil {
		return err
	}

	// Put the container on the bridge at GuestIP over a veth into its netns.
	st.Net.VethHost, err = rt.attachVeth(name)
	if serr := save(); err == nil {
		err = serr
	}
	if err != nil {
		return fmt.Errorf("attach container network: %w", err)
	}

	if o.NoWait {
		return nil
	}
	return rt.waitGuest(name)
}

// runArgs is the hardened `podman run` command line. What is absent matters as
// much as what is there: no host bind-mounts, no host network, no added
// capabilities, no privilege. userns=auto remaps container-root off host-root;
// the GPU is shared read-only over CDI, never --privileged and never all GPUs.
func (rt *ContainerRuntime) runArgs(name, akFile, volume string, o StartOptions) []string {
	args := []string{
		"run", "--detach", "--name", name,
		// size=65536 maps the full 0..65535 id range into the user namespace.
		// The default (1024) leaves sshd's privsep group (gid 65534, "nogroup")
		// unmapped, so its pre-auth setgroups() fails with EINVAL and the
		// connection resets. container-root is still an unprivileged host uid.
		"--userns=auto:size=65536",
		"--security-opt=no-new-privileges",
		// Drop every capability, then add back only the minimal set sshd needs to
		// run and set up the renter's account: bind its port, generate keys, own
		// the renter's home, drop privilege to the renter, and privsep-chroot.
		// These are namespaced by --userns=auto (they apply inside the container's
		// user namespace, not to host resources), and the dangerous ones
		// (SYS_ADMIN, NET_ADMIN, SYS_PTRACE, ...) stay dropped. The rootfs is
		// writable (the NVIDIA CDI hook injects the driver by writing the
		// container's ldcache/symlinks, which a read-only rootfs breaks) but
		// ephemeral (--rm); the renter's data lives on the encrypted /home/renter.
		"--cap-drop=ALL",
		"--cap-add=CHOWN", "--cap-add=DAC_OVERRIDE", "--cap-add=FOWNER",
		"--cap-add=SETUID", "--cap-add=SETGID", "--cap-add=KILL",
		"--cap-add=NET_BIND_SERVICE", "--cap-add=SYS_CHROOT",
		"--network=none",
		"--pids-limit", "4096",
		"--stop-timeout", "30",
		// The renter's key, read-only; the image's entrypoint installs it for
		// user 'renter' and starts sshd.
		"--mount", "type=bind,src=" + akFile + ",dst=/run/renter/authorized_keys,ro",
		// The encrypted writable space as the renter's home. idmap remaps the
		// host-root-owned volume into the container's user namespace, so the
		// container can create and own the renter's files on it (without idmap
		// the volume appears owned by an unmapped uid and is unwritable).
		"--mount", "type=bind,src=" + volume + ",dst=/home/renter,idmap",
	}
	if mb := rt.spec.GuestMemoryMB(); mb > 0 {
		args = append(args, "--memory", fmt.Sprintf("%dm", mb))
	}
	if n := rt.spec.GuestCPUs(); n > 0 {
		args = append(args, "--cpus", fmt.Sprintf("%d", n))
	}
	// The GPU(s), shared over CDI. Absent on a self-test that runs without the
	// GPU, and on a machine renting CPU only.
	if !o.NoGPU {
		for _, dev := range rt.cdiDevices {
			args = append(args, "--device", dev)
		}
	}
	args = append(args, rt.image)
	return args
}

// freeGPU gives the renter the GPU to themselves. Unlike the microVM, it does
// NOT take the GPU off the host driver or stop the host's NVIDIA services: the
// container shares the GPU over CDI, and CDI mounts the nvidia-persistenced
// socket, so those services must stay up. It only closes a DGX Spark's desktop
// (restored at teardown) so nothing on the host's screen competes for the GPU.
func (rt *ContainerRuntime) freeGPU(st *State, save func() error) error {
	if len(rt.spec.GPUs) == 0 {
		return nil
	}
	desktop, _ := ClassifyGPUHolders(rt.h, rt.spec.GPUs)
	if len(desktop) > 0 {
		if !rt.spec.DesktopOnDemand {
			return desktopInUse(desktop)
		}
		dm := ActiveDisplayManager(rt.h)
		if dm == "" {
			return desktopInUse(desktop)
		}
		st.StoppedDisplayManager = dm
		if err := save(); err != nil {
			return err
		}
		if _, err := CloseDesktop(rt.h); err != nil {
			return fmt.Errorf("close the desktop for the rental: %w", err)
		}
	}
	return nil
}

// attachVeth wires a veth pair from the netguard bridge into the container's
// network namespace and gives its end GuestIP with the host as its gateway --
// the same address a microVM guest gets, so the renter's forward is unchanged.
// Returns the host-side veth name for teardown.
func (rt *ContainerRuntime) attachVeth(name string) (string, error) {
	pidOut, err := rt.h.Output("podman", "inspect", "--format", "{{.State.Pid}}", name)
	if err != nil {
		return "", fmt.Errorf("read container pid: %w", err)
	}
	pid := strings.TrimSpace(pidOut)
	if pid == "" || pid == "0" {
		return "", fmt.Errorf("container has no pid (not running)")
	}
	hostVeth, ctrVeth := "grveth0h", "grveth0c"
	_ = rt.h.Run("ip", "link", "del", hostVeth)
	steps := [][]string{
		{"link", "add", hostVeth, "type", "veth", "peer", "name", ctrVeth},
		{"link", "set", hostVeth, "master", netguard.Bridge},
		{"link", "set", hostVeth, "up"},
		{"link", "set", ctrVeth, "netns", pid},
	}
	for _, s := range steps {
		if err := rt.h.Run("ip", s...); err != nil {
			_ = rt.h.Run("ip", "link", "del", hostVeth)
			return "", fmt.Errorf("wire veth: %w", err)
		}
	}
	// Inside the container's netns: name it, address it, route out via the host.
	nsSteps := [][]string{
		{"link", "set", ctrVeth, "name", "eth0"},
		{"addr", "add", fmt.Sprintf("%s/%d", GuestIP, PrefixLen), "dev", "eth0"},
		{"link", "set", "eth0", "up"},
		{"link", "set", "lo", "up"},
		{"route", "add", "default", "via", HostIP},
	}
	for _, s := range nsSteps {
		if err := rt.h.Run("nsenter", append([]string{"-t", pid, "-n", "ip"}, s...)...); err != nil {
			_ = rt.h.Run("ip", "link", "del", hostVeth)
			return "", fmt.Errorf("configure container network: %w", err)
		}
	}
	return hostVeth, nil
}

// probeScript is the self-test probe the host runs inside a live rental
// container with `podman exec`. It prints the same GPUAGENT-SELFTEST markers the
// microVM prints to serial: the GPU nvidia-smi sees over CDI, whether the
// internet is reachable, and that each fenced target is BLOCKED (a silent drop;
// `timeout` exits 124 when nothing answered, a refused/answered connection is
// REACHED). Running it after the container's network is up, over exec, avoids
// racing sshd for the container's stdout.
func probeScript(probes []string) string {
	return `MARK=GPUAGENT-SELFTEST
say() { echo "$MARK $*"; }
say BEGIN
if command -v nvidia-smi >/dev/null 2>&1; then
  if out=$(nvidia-smi --query-gpu=pci.bus_id,name,memory.total --format=csv,noheader,nounits 2>&1); then
    while IFS= read -r line; do say "NVSMI $line"; done <<< "$out"
  else
    say "NVSMIFAIL $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-300)"
  fi
fi
# curl's connect timeout is in-process (non-blocking connect + poll), so it is
# reliably bounded -- unlike bash /dev/tcp + timeout, whose signal delivery is
# not dependable under 'podman exec'. curl exit 28 is a connect timeout: the
# fence dropped the packet, so the target is BLOCKED. Anything else (a reply, a
# refusal) means the packet got through, so it is REACHED.
if curl -s -o /dev/null --connect-timeout 8 -m 12 http://1.1.1.1 2>/dev/null; then say 'INTERNET ok'; else say 'INTERNET fail'; fi
for t in ` + strings.Join(probes, " ") + `; do
  curl -s -o /dev/null --connect-timeout 6 -m 8 "http://$t" 2>/dev/null
  ec=$?
  if [ "$ec" = 28 ]; then say "BLOCKED $t"; else say "REACHED $t ec=$ec"; fi
done
say END
`
}

func (rt *ContainerRuntime) running(name string) bool {
	out, err := rt.h.Output("podman", "inspect", "--format", "{{.State.Running}}", name)
	return err == nil && strings.TrimSpace(out) == "true"
}

// Alive reports whether the rental's container is running.
func (rt *ContainerRuntime) Alive() bool {
	st, err := LoadState(rt.h, rt.spec.DataDir)
	return err == nil && st != nil && st.ContainerID != "" && rt.running(st.ContainerID)
}

// Present reports whether rental state (a running rental, or a dirty leftover)
// is on the machine.
func (rt *ContainerRuntime) Present() bool {
	st, err := LoadState(rt.h, rt.spec.DataDir)
	return err != nil || st != nil
}

// Dirty reports whether the last teardown did not verify.
func (rt *ContainerRuntime) Dirty() bool {
	st, err := LoadState(rt.h, rt.spec.DataDir)
	return err != nil || (st != nil && st.Dirty)
}

func (rt *ContainerRuntime) waitGuest(name string) error {
	addr := net.JoinHostPort(GuestIP, "22")
	for waited := time.Duration(0); waited < BootTimeout; waited += pollInterval {
		if !rt.running(name) {
			return fmt.Errorf("the container exited while starting: %s", rt.exitReason(name))
		}
		if rt.h.DialTCP(addr, 3*time.Second) == nil {
			return nil
		}
		rt.h.Sleep(pollInterval)
	}
	return fmt.Errorf("the container did not open SSH within %v", BootTimeout)
}

func (rt *ContainerRuntime) exitReason(name string) string {
	out, err := rt.h.Output("podman", "logs", "--tail", "15", name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Stop tears down the container rental and verifies it: the container gone, the
// veth removed, the encrypted volume unmounted and its mapping/loop/file gone,
// the desktop and NVIDIA services back, the GPU clear of the rental, the
// network and fence removed. A teardown that does not verify is left dirty.
func (rt *ContainerRuntime) Stop() StopResult {
	st, err := LoadState(rt.h, rt.spec.DataDir)
	if err != nil {
		return StopResult{Detail: []string{"read rental state: " + err.Error()}}
	}
	if st == nil {
		return StopResult{Wiped: true, GPUClean: true}
	}
	var res StopResult

	gone := rt.killContainer(st.ContainerID)
	if !gone {
		res.Detail = append(res.Detail, "the container would not stop")
	}
	if st.Net.VethHost != "" {
		_ = rt.h.Run("ip", "link", "del", st.Net.VethHost)
	}
	if st.VolumeMount != "" {
		_ = rt.h.Run("umount", st.VolumeMount)
	}

	wiped, detail := DestroyDisk(rt.h, st.Disk)
	res.Wiped = wiped && gone
	res.Detail = append(res.Detail, detail...)

	// Bring the host's GPU services and desktop back.
	for _, unit := range st.ServicesToRestart() {
		_ = rt.h.Run("systemctl", "start", unit)
	}
	if st.StoppedDisplayManager != "" {
		_ = rt.h.Run("systemctl", "start", st.StoppedDisplayManager)
	}

	// The GPU never left nvidia; "clean" is that nothing of the rental is left
	// on it. The verifier is told the rented GPUs as if they came back on nvidia.
	res.GPUClean = gone
	if len(rt.spec.GPUs) > 0 && gone && rt.verifyGPU != nil {
		returned := make([]BoundDevice, 0, len(rt.spec.GPUs))
		for _, bdf := range rt.spec.GPUs {
			returned = append(returned, BoundDevice{BDF: bdf, Driver: "nvidia"})
		}
		if !rt.verifyGPU(returned) {
			res.GPUClean = false
			res.Detail = append(res.Detail, "the GPU did not verify clear after the rental")
		}
	}

	TeardownNetwork(rt.h, st.Net)
	if st.Fenced {
		if err := rt.fence.Remove(); err != nil {
			res.Detail = append(res.Detail, "remove firewall table: "+err.Error())
		}
	}
	_ = rt.h.RemoveAll(st.Rental.Dir)

	if res.Clean() {
		_ = ClearState(rt.h, rt.spec.DataDir)
	} else {
		st.Dirty = true
		st.DirtyDetail = res.Detail
		_ = SaveState(rt.h, rt.spec.DataDir, st)
	}
	return res
}

// killContainer stops the container (SIGTERM, then SIGKILL after the grace) and
// removes it, then confirms it is gone.
func (rt *ContainerRuntime) killContainer(name string) bool {
	if name == "" {
		return true
	}
	_ = rt.h.Run("podman", "stop", "--time", "30", name)
	for waited := time.Duration(0); waited < StopGrace; waited += stopPoll {
		if !rt.running(name) {
			break
		}
		rt.h.Sleep(stopPoll)
	}
	if rt.running(name) {
		_ = rt.h.Run("podman", "kill", "--signal", "SIGKILL", name)
	}
	_ = rt.h.Run("podman", "rm", "--force", name)
	out, err := rt.h.Output("podman", "inspect", "--format", "{{.Id}}", name)
	// `inspect` errors when the container no longer exists: that is success.
	return err != nil || strings.TrimSpace(out) == ""
}
