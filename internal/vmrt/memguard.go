package vmrt

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The rental's memory where the GPU shares the machine's (a GB10). CUDA's
// allocations there are the driver's, not the container's: --memory bounds what
// the container's processes hold themselves, and a renter can still allocate
// the rest of the machine's memory on the GPU -- 40 GiB under a 16 GiB limit,
// as a host measured -- and starve the host. So the agent keeps the limit
// itself: the container's own memory (its cgroup's memory.current) plus what
// its processes hold on the GPU may not pass the rental's memory, and the
// machine's free memory may not fall under half of what it keeps for itself.
// Either way the rental's biggest GPU process is stopped (SIGKILL), as the
// kernel's out-of-memory killer stops a process that passes --memory. The host's
// own processes are never touched.
//
// The container's own memory leaves out its page cache: memory.current counts
// the files its processes have read, which the kernel drops whenever memory is
// short. llama.cpp maps a model's weights from its file and copies them to the
// GPU, so counting the cache would count each loaded model twice, and a renter
// whose two models fit easily was stopped (2026-10-19). What stays counted is
// what the rental holds: anonymous memory, the kernel's for it, and its tmpfs
// (/dev/shm, /tmp: shmem, which nothing reclaims without swap).
//
// Whether a GPU allocation is also in memory.current depends on how it was made
// (the 40 GiB above was not; a model loaded by Ollama's runner seemingly is), and
// counted on both sides it stopped a renter's second model with the machine half
// free (2026-10-19). So the sum is held to what the machine has in use: the
// rental cannot hold more than MemTotal - MemAvailable, less what the host's own
// processes hold (the root cgroup's anon and shmem beside the rental's). Both
// readings are at least what the rental holds, so the smaller one is too, and
// the double count goes. The machine's floor reads MemAvailable, which already
// counts the cache as free.

// MemoryVerdict is one look by GuardMemory: what the rental held, and the
// process it stopped (0: none).
type MemoryVerdict struct {
	// RentalMB is the container's own memory (page cache left out) plus its
	// GPU memory, no more than the machine has in use beside the host's own
	// processes; -1 when the GPU does not say what a process holds.
	RentalMB  int64
	OwnMB     int64 // the container's own memory
	GPUMB     int64 // what its processes hold on the GPU
	InUseMB   int64 // the machine's memory in use beside the host's processes; -1: unknown
	LimitMB   int64
	FreeMB    int64
	FloorMB   int64
	KilledPID int
	KilledMB  int64 // what the stopped process held on the GPU
	Reason    string
}

// gpuProc is one compute process on the GPU and what it holds (-1: unknown).
type gpuProc struct {
	pid int
	mb  int64
}

// GuardMemory is one look at a container rental's memory on a unified-memory
// GPU, stopping its biggest GPU process when the rental has passed its memory
// or the machine is running out. A machine with no container rental on it is
// left at once, without asking the GPU anything.
func (rt *ContainerRuntime) GuardMemory() (MemoryVerdict, error) {
	var v MemoryVerdict
	st, err := LoadState(rt.h, rt.spec.DataDir)
	if err != nil || st == nil || st.Mode != ModeContainer || st.ContainerID == "" {
		return v, err
	}
	v.LimitMB = int64(rt.spec.GuestMemoryMB())
	if v.LimitMB <= 0 {
		return v, nil
	}
	// Half of what the machine keeps for itself beside the rental.
	v.FloorMB = (int64(rt.spec.TotalMemMB) - v.LimitMB) / 2
	if v.FloorMB < 1024 {
		v.FloorMB = 1024
	}

	// The container's cgroup; none while it is not running (yet, or any more):
	// nothing to hold to a limit.
	cgroup := rt.containerCgroup(st.ContainerID)
	if cgroup == "" {
		return v, nil
	}
	own, err := cgroupHeldBytes(rt.h, "/sys/fs/cgroup"+cgroup)
	if err != nil {
		return v, fmt.Errorf("read the rental's memory: %w", err)
	}

	out, err := rt.h.Output("nvidia-smi", "--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits")
	if err != nil {
		return v, fmt.Errorf("list the GPU's processes: %w", err)
	}
	var mine []gpuProc
	known := true
	gpuMB := int64(0)
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, ",")
		if len(f) < 2 {
			continue
		}
		pid, perr := strconv.Atoi(strings.TrimSpace(f[0]))
		if perr != nil || pid <= 0 || !procInCgroup(rt.h, pid, cgroup) {
			continue
		}
		mb, merr := strconv.ParseInt(strings.TrimSpace(f[1]), 10, 64)
		if merr != nil {
			mb, known = -1, false
		} else {
			gpuMB += mb
		}
		mine = append(mine, gpuProc{pid: pid, mb: mb})
	}
	v.OwnMB, v.GPUMB = own/(1024*1024), gpuMB
	v.FreeMB = memAvailableMB(rt.h)
	v.InUseMB = -1
	if total := memInfoMB(rt.h, "MemTotal:"); total > 0 && v.FreeMB >= 0 {
		v.InUseMB = total - v.FreeMB - hostHeldMB(rt.h, "/sys/fs/cgroup"+cgroup)
	}
	v.RentalMB = v.OwnMB + v.GPUMB
	if v.InUseMB >= 0 && v.InUseMB < v.RentalMB {
		v.RentalMB = v.InUseMB
	}
	if !known {
		v.RentalMB = -1
	}

	switch {
	case known && v.RentalMB > v.LimitMB:
		v.Reason = fmt.Sprintf("the rental's memory and GPU memory came to %d MiB, over its %d MiB (%d MiB its own, %d MiB on the GPU, %d MiB in use on the machine beside the host's)",
			v.RentalMB, v.LimitMB, v.OwnMB, v.GPUMB, v.InUseMB)
	case v.FreeMB >= 0 && v.FreeMB < v.FloorMB:
		v.Reason = fmt.Sprintf("the machine had %d MiB of memory free, under the %d MiB it keeps", v.FreeMB, v.FloorMB)
	default:
		return v, nil
	}
	if len(mine) == 0 {
		return v, nil // nothing of the rental's on the GPU to stop
	}
	sort.SliceStable(mine, func(i, j int) bool { return mine[i].mb > mine[j].mb })
	victim := mine[0]
	if err := rt.h.Run("kill", "-KILL", strconv.Itoa(victim.pid)); err != nil {
		return v, fmt.Errorf("stop GPU process %d: %w", victim.pid, err)
	}
	v.KilledPID, v.KilledMB = victim.pid, victim.mb
	return v, nil
}

// cgroupHeldBytes is what a cgroup holds: memory.current less its page cache
// (memory.stat's file, less shmem, which is tmpfs and stays). Without
// memory.stat it is memory.current, as before.
func cgroupHeldBytes(h Host, dir string) (int64, error) {
	cur, err := readInt(h, dir+"/memory.current")
	if err != nil {
		return 0, err
	}
	data, err := h.ReadFile(dir + "/memory.stat")
	if err != nil {
		return cur, nil
	}
	st := memStatFields(data)
	if cache := st["file"] - st["shmem"]; cache > 0 {
		cur -= cache
	}
	if cur < 0 {
		cur = 0
	}
	return cur, nil
}

// procInCgroup reports whether a host process lives in cgroup (or below it).
func procInCgroup(h Host, pid int, cgroup string) bool {
	data, err := h.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		// cgroup v2: "0::/machine.slice/libpod-<id>.scope/container"
		if _, p, ok := strings.Cut(line, "::"); ok {
			p = strings.TrimSpace(p)
			if p == cgroup || strings.HasPrefix(p, cgroup+"/") {
				return true
			}
		}
	}
	return false
}

// memStatFields reads a memory.stat: each counter by its name, in bytes.
func memStatFields(data []byte) map[string]int64 {
	st := map[string]int64{}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		if n, err := strconv.ParseInt(f[1], 10, 64); err == nil {
			st[f[0]] = n
		}
	}
	return st
}

// hostHeldMB is what the host's own processes hold, in MiB: the root cgroup's
// anon and shmem less the rental's (dir). 0 when the root cgroup does not say
// (the bound it goes into is then only looser).
func hostHeldMB(h Host, dir string) int64 {
	root, err := h.ReadFile("/sys/fs/cgroup/memory.stat")
	if err != nil {
		return 0
	}
	mine, err := h.ReadFile(dir + "/memory.stat")
	if err != nil {
		return 0
	}
	r, m := memStatFields(root), memStatFields(mine)
	held := (r["anon"] + r["shmem"]) - (m["anon"] + m["shmem"])
	if held < 0 {
		return 0
	}
	return held / (1024 * 1024)
}

// memAvailableMB is the machine's MemAvailable, in MiB; -1 when unknown.
func memAvailableMB(h Host) int64 { return memInfoMB(h, "MemAvailable:") }

// memInfoMB is one of /proc/meminfo's lines (key "MemTotal:"), in MiB; -1
// when unknown.
func memInfoMB(h Host, key string) int64 {
	data, err := h.ReadFile("/proc/meminfo")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == key {
			kb, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				return -1
			}
			return kb / 1024
		}
	}
	return -1
}

// readInt reads a file holding one integer (a cgroup counter).
func readInt(h Host, path string) (int64, error) {
	data, err := h.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}
