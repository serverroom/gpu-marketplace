package vmrt

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// What a running rental uses, as the host can see it: the staff's live view of
// a rental (and the host's) without logging in to it. The marketplace reads it
// from /status, every 30 seconds.
//
// Some of it the host cannot see, and then it is absent rather than guessed:
// a microVM's GPU is the VM's own (passed through, nvidia-smi on the host no
// longer has it), and its memory cgroup always reads full (VFIO pins all of the
// guest's memory), so a microVM rental reports its CPU, disk and network only.
// A container rental shares the host's kernel and GPU and reports all of it.

// Usage is one look at a running rental.
type Usage struct {
	RentalID  string `json:"rental_id"`
	Mode      string `json:"mode"` // "vm" or "container"
	StartedAt int64  `json:"started_at,omitempty"`
	At        int64  `json:"at"`
	// CPUs is the rental's CPUs; CPUPct is how busy they were since the last
	// look (0-100 of all of them), absent on the first look after a start.
	CPUs   int      `json:"cpus,omitempty"`
	CPUPct *float64 `json:"cpu_pct,omitempty"`
	// MemoryUsedMB is the container's own memory, plus what its processes hold
	// on the GPU where the GPU shares the machine's memory (a GB10).
	MemoryUsedMB *int64     `json:"memory_used_mb,omitempty"`
	MemoryMB     int        `json:"memory_mb,omitempty"`
	GPUs         []GPUUsage `json:"gpus,omitempty"`
	// DiskUsedGB is what the renter has on the disk (a container's filesystem),
	// or what the VM has written to it (its sparse file's allocated size).
	DiskUsedGB *float64 `json:"disk_used_gb,omitempty"`
	DiskGB     int      `json:"disk_gb,omitempty"`
	// Bytes the rental received and sent since it started, over its network
	// interface (the renter's SSH and the internet alike).
	NetRxBytes *int64 `json:"net_rx_bytes,omitempty"`
	NetTxBytes *int64 `json:"net_tx_bytes,omitempty"`
}

// GPUUsage is one rented GPU as nvidia-smi on the host reads it; a figure the
// GPU does not report ([N/A], as a GB10 does for its memory) is absent.
type GPUUsage struct {
	UtilPct       *float64 `json:"util_pct,omitempty"`
	MemoryUsedMB  *int64   `json:"memory_used_mb,omitempty"`
	MemoryTotalMB *int64   `json:"memory_total_mb,omitempty"`
	TempC         *float64 `json:"temp_c,omitempty"`
}

// cpuSample is the last CPU reading of a rental, for the next look's rate.
type cpuSample struct {
	mu     sync.Mutex
	rental string
	usec   int64
	at     time.Time
}

// cpuPct turns a cgroup's cumulative CPU time into how busy the rental's CPUs
// were since the previous look; nil on the first look at a rental.
func (s *cpuSample) cpuPct(rental string, usec int64, now time.Time, cpus int) *float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	prevRental, prevUsec, prevAt := s.rental, s.usec, s.at
	s.rental, s.usec, s.at = rental, usec, now
	if prevRental != rental || cpus <= 0 {
		return nil
	}
	elapsed := now.Sub(prevAt).Microseconds()
	if elapsed <= 0 || usec < prevUsec {
		return nil
	}
	pct := float64(usec-prevUsec) / float64(elapsed) / float64(cpus) * 100
	if pct > 100 {
		pct = 100
	}
	pct = float64(int64(pct*10+0.5)) / 10
	return &pct
}

// Usage is one look at the container rental, or nil when none is running.
func (rt *ContainerRuntime) Usage() *Usage {
	st, err := LoadState(rt.h, rt.spec.DataDir)
	if err != nil || st == nil || st.Mode != ModeContainer || st.ContainerID == "" {
		return nil
	}
	now := time.Now()
	u := &Usage{RentalID: st.RentalID, Mode: "container", StartedAt: st.StartedAt, At: now.Unix(),
		CPUs: rt.spec.GuestCPUs(), MemoryMB: rt.spec.GuestMemoryMB(), DiskGB: rt.spec.DiskGB}
	if cg := rt.containerCgroup(st.ContainerID); cg != "" {
		if usec, ok := cgroupCPUUsec(rt.h, cg); ok {
			u.CPUPct = rt.usageCPU.cpuPct(st.RentalID, usec, now, u.CPUs)
		}
		if own, err := readInt(rt.h, "/sys/fs/cgroup"+cg+"/memory.current"); err == nil {
			mb := own / (1024 * 1024)
			if rt.spec.Unified {
				mb += rentalGPUMemoryMB(rt.h, cg)
			}
			u.MemoryUsedMB = &mb
		}
	}
	u.GPUs = gpuUsage(rt.h, rt.spec.GPUs)
	if st.VolumeMount != "" {
		u.DiskUsedGB = dfUsedGB(rt.h, st.VolumeMount)
	}
	u.NetRxBytes, u.NetTxBytes = netBytes(rt.h, st.Net.VethHost)
	return u
}

// Usage is one look at the microVM rental, or nil when none is running.
func (rt *Runtime) Usage() *Usage {
	st, err := LoadState(rt.h, rt.spec.DataDir)
	if err != nil || st == nil || st.Mode == ModeContainer || st.Rental.Unit == "" {
		return nil
	}
	now := time.Now()
	u := &Usage{RentalID: st.RentalID, Mode: "vm", StartedAt: st.StartedAt, At: now.Unix(),
		CPUs: rt.spec.GuestCPUs(), DiskGB: rt.spec.DiskGB}
	if usec, ok := cgroupCPUUsec(rt.h, "/system.slice/"+st.Rental.Unit+".service"); ok {
		u.CPUPct = rt.usageCPU.cpuPct(st.RentalID, usec, now, u.CPUs)
	}
	if st.Disk.File != "" {
		u.DiskUsedGB = duUsedGB(rt.h, st.Disk.File)
	}
	if st.Net.TapDev {
		u.NetRxBytes, u.NetTxBytes = netBytes(rt.h, Tap)
	}
	return u
}

// containerCgroup is the container's cgroup, asked of podman once per container
// (shared with GuardMemory's cache).
func (rt *ContainerRuntime) containerCgroup(container string) string {
	rt.cgMu.Lock()
	defer rt.cgMu.Unlock()
	if rt.guardFor == container && rt.guardCgroup != "" {
		return rt.guardCgroup
	}
	out, err := rt.h.Output("podman", "inspect", "--format", "{{.State.CgroupPath}}", container)
	if c := strings.TrimSpace(out); err == nil && strings.HasPrefix(c, "/") {
		rt.guardFor, rt.guardCgroup = container, c
		return c
	}
	return ""
}

// cgroupCPUUsec is a cgroup's cumulative CPU time (cpu.stat usage_usec).
func cgroupCPUUsec(h Host, cgroup string) (int64, bool) {
	data, err := h.ReadFile("/sys/fs/cgroup" + cgroup + "/cpu.stat")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "usage_usec" {
			n, err := strconv.ParseInt(f[1], 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// rentalGPUMemoryMB is what the processes in cgroup hold on the GPU.
func rentalGPUMemoryMB(h Host, cgroup string) int64 {
	out, err := h.Output("nvidia-smi", "--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits")
	if err != nil {
		return 0
	}
	var mb int64
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, ",")
		if len(f) < 2 {
			continue
		}
		pid, perr := strconv.Atoi(strings.TrimSpace(f[0]))
		used, uerr := strconv.ParseInt(strings.TrimSpace(f[1]), 10, 64)
		if perr == nil && uerr == nil && procInCgroup(h, pid, cgroup) {
			mb += used
		}
	}
	return mb
}

// gpuUsage is the rented GPUs as nvidia-smi on the host reads them.
func gpuUsage(h Host, bdfs []string) []GPUUsage {
	if len(bdfs) == 0 {
		return nil
	}
	out, err := h.Output("nvidia-smi", "--query-gpu=pci.bus_id,utilization.gpu,memory.used,memory.total,temperature.gpu", "--format=csv,noheader,nounits")
	if err != nil {
		return nil
	}
	var gpus []GPUUsage
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, ",")
		if len(f) < 5 || !rentedBus(strings.TrimSpace(f[0]), bdfs) {
			continue
		}
		gpus = append(gpus, GPUUsage{
			UtilPct: optFloat(f[1]), MemoryUsedMB: optInt(f[2]), MemoryTotalMB: optInt(f[3]), TempC: optFloat(f[4]),
		})
	}
	return gpus
}

// rentedBus reports whether nvidia-smi's bus id (00000000:01:00.0) is one of the
// rented GPUs (0000:01:00.0).
func rentedBus(bus string, bdfs []string) bool {
	bus = strings.ToLower(bus)
	for _, b := range bdfs {
		b = strings.ToLower(b)
		if i := strings.Index(b, ":"); i >= 0 && strings.HasSuffix(bus, b[i:]) {
			return true
		}
	}
	return false
}

func optFloat(s string) *float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return nil
	}
	return &v
}

func optInt(s string) *int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return nil
	}
	return &v
}

// dfUsedGB is what is used on the filesystem mounted at mount.
func dfUsedGB(h Host, mount string) *float64 {
	out, err := h.Output("df", "--output=used", "-B1M", mount)
	if err != nil {
		return nil
	}
	f := strings.Fields(out)
	if len(f) < 2 {
		return nil
	}
	mb, err := strconv.ParseFloat(f[len(f)-1], 64)
	if err != nil {
		return nil
	}
	gb := float64(int64(mb/1024*10+0.5)) / 10
	return &gb
}

// duUsedGB is how much of a sparse file is written (allocated).
func duUsedGB(h Host, file string) *float64 {
	out, err := h.Output("du", "-s", "-B1M", file)
	if err != nil {
		return nil
	}
	f := strings.Fields(out)
	if len(f) < 1 {
		return nil
	}
	mb, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return nil
	}
	gb := float64(int64(mb/1024*10+0.5)) / 10
	return &gb
}

// netBytes is what the rental received and sent over its host-side interface
// dev: what the host side sends, the rental receives.
func netBytes(h Host, dev string) (rx, tx *int64) {
	if dev == "" {
		return nil, nil
	}
	read := func(name string) *int64 {
		n, err := readInt(h, "/sys/class/net/"+dev+"/statistics/"+name)
		if err != nil {
			return nil
		}
		return &n
	}
	return read("tx_bytes"), read("rx_bytes")
}
