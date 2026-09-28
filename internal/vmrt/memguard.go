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

// MemoryVerdict is one look by GuardMemory: what the rental held, and the
// process it stopped (0: none).
type MemoryVerdict struct {
	// RentalMB is the container's own memory plus its GPU memory; -1 when
	// the GPU does not say what a process holds.
	RentalMB  int64
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
	own, err := readInt(rt.h, "/sys/fs/cgroup"+cgroup+"/memory.current")
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
	v.RentalMB = own/(1024*1024) + gpuMB
	if !known {
		v.RentalMB = -1
	}
	v.FreeMB = memAvailableMB(rt.h)

	switch {
	case known && v.RentalMB > v.LimitMB:
		v.Reason = fmt.Sprintf("the rental's memory and GPU memory came to %d MiB, over its %d MiB", v.RentalMB, v.LimitMB)
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

// memAvailableMB is the machine's MemAvailable, in MiB; -1 when unknown.
func memAvailableMB(h Host) int64 {
	data, err := h.ReadFile("/proc/meminfo")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemAvailable:" {
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
