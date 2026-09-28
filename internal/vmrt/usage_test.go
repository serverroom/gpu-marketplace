package vmrt

import (
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// v0.3.4: a running rental's usage and the renter's SSH, for the staff's view.

// usageContainerHost is a container rental with its cgroup, GPU, volume and veth.
func usageContainerHost(t *testing.T, usec int64) (*ContainerRuntime, *fakehost.Host) {
	t.Helper()
	h := newContainerHost()
	rt, _ := newContainerRuntime(h, nil)
	st := &State{RentalID: "R1", Mode: ModeContainer, ContainerID: containerName("R1"), StartedAt: 1790000000,
		VolumeMount: "/var/lib/gpu-agent/rentals/R1/vol", Net: NetState{VethHost: "grveth0h"}}
	if err := SaveState(h, rt.spec.DataDir, st); err != nil {
		t.Fatal(err)
	}
	const cg = "/machine.slice/libpod-abc.scope"
	h.Outputs["podman inspect --format {{.State.CgroupPath}}"] = cg + "\n"
	h.Files["/sys/fs/cgroup"+cg+"/cpu.stat"] = []byte("usage_usec " + strconv.FormatInt(usec, 10) + "\nuser_usec 1\n")
	h.Files["/sys/fs/cgroup"+cg+"/memory.current"] = []byte("4294967296\n") // 4 GiB
	h.Files["/proc/401/cgroup"] = []byte("0::" + cg + "/container\n")
	h.Outputs["nvidia-smi --query-compute-apps=pid,used_memory"] = "401, 10240\n900, 5000\n"
	h.Outputs["nvidia-smi --query-gpu=pci.bus_id,utilization.gpu"] = "00000000:01:00.0, 87, [N/A], [N/A], 71\n"
	h.Outputs["df --output=used"] = " Used\n 51200\n"
	h.Files["/sys/class/net/grveth0h/statistics/tx_bytes"] = []byte("1000\n") // what the rental received
	h.Files["/sys/class/net/grveth0h/statistics/rx_bytes"] = []byte("2000\n") // what it sent
	return rt, h
}

func TestAContainerRentalsUsage(t *testing.T) {
	rt, h := usageContainerHost(t, 1_000_000)
	rt.spec.Unified = true
	rt.spec.GPUs = []string{"0000:01:00.0"}
	u := rt.Usage()
	if u == nil || u.RentalID != "R1" || u.Mode != "container" || u.StartedAt != 1790000000 {
		t.Fatalf("usage = %+v", u)
	}
	if u.CPUPct != nil {
		t.Errorf("first look: cpu %v, want none until there is a rate", *u.CPUPct)
	}
	// 4 GiB of its own plus its 10240 MiB on the GPU (not the host's 5000).
	if u.MemoryUsedMB == nil || *u.MemoryUsedMB != 4096+10240 {
		t.Errorf("memory = %v, want 14336", u.MemoryUsedMB)
	}
	if len(u.GPUs) != 1 || u.GPUs[0].UtilPct == nil || *u.GPUs[0].UtilPct != 87 || u.GPUs[0].MemoryUsedMB != nil || *u.GPUs[0].TempC != 71 {
		t.Errorf("gpus = %+v", u.GPUs)
	}
	if u.DiskUsedGB == nil || *u.DiskUsedGB != 50 || *u.NetRxBytes != 1000 || *u.NetTxBytes != 2000 {
		t.Errorf("disk %v rx %v tx %v", u.DiskUsedGB, u.NetRxBytes, u.NetTxBytes)
	}

	// A second look: its CPUs' busy share since the first.
	rt.usageCPU.at = time.Now().Add(-10 * time.Second)
	h.Files["/sys/fs/cgroup/machine.slice/libpod-abc.scope/cpu.stat"] = []byte("usage_usec 51000000\n")
	u = rt.Usage()
	if u.CPUPct == nil || *u.CPUPct < 20 || *u.CPUPct > 30 {
		t.Errorf("cpu = %v, want about 25%% (50 s of CPU over 10 s on %d CPUs)", u.CPUPct, u.CPUs)
	}
}

func TestAMicroVMRentalsUsage(t *testing.T) {
	h := fakehost.New()
	rt := New(h, Spec{DataDir: dataDir, TotalMemMB: 65536, CPUs: 16, DiskGB: 200}, &fakeFence{h: h}, nil)
	r := NewRental(dataDir, "R2")
	st := &State{RentalID: "R2", Rental: r, StartedAt: 1790000500, Disk: DiskState{File: r.Dir + "/disk.img"}, Net: NetState{TapDev: true}}
	if err := SaveState(h, dataDir, st); err != nil {
		t.Fatal(err)
	}
	h.Files["/sys/fs/cgroup/system.slice/gpu-rental-R2.service/cpu.stat"] = []byte("usage_usec 5\n")
	h.Outputs["du -s -B1M"] = "20480\t" + r.Dir + "/disk.img\n"
	h.Files["/sys/class/net/"+Tap+"/statistics/tx_bytes"] = []byte("7\n")
	h.Files["/sys/class/net/"+Tap+"/statistics/rx_bytes"] = []byte("9\n")
	u := rt.Usage()
	if u == nil || u.Mode != "vm" || u.MemoryUsedMB != nil || u.GPUs != nil {
		t.Fatalf("usage = %+v; a VM reports no memory or GPU (the host cannot see them)", u)
	}
	if *u.DiskUsedGB != 20 || *u.NetRxBytes != 7 || *u.NetTxBytes != 9 {
		t.Errorf("disk %v rx %v tx %v", *u.DiskUsedGB, *u.NetRxBytes, *u.NetTxBytes)
	}
	if rt2 := New(fakehost.New(), Spec{DataDir: dataDir}, &fakeFence{}, nil); rt2.Usage() != nil {
		t.Error("no rental, want no usage")
	}
}

// The forward counts the renter's SSH: a connection open SessionMin is a
// session; a shorter one (a probe) is not.
func TestTheForwardCountsSessions(t *testing.T) {
	old := SessionMin
	SessionMin = 50 * time.Millisecond
	t.Cleanup(func() { SessionMin = old })

	guest, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	go func() {
		for {
			c, err := guest.Accept()
			if err != nil {
				return
			}
			go io.Copy(c, c) // the guest echoes
		}
	}()
	f, err := StartForward("127.0.0.1:0", guest.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Stop()
	addr := f.ln.Addr().String()

	probe, _ := net.Dial("tcp", addr)
	probe.Close()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
	buf := make([]byte, 64)
	c.Read(buf)
	time.Sleep(120 * time.Millisecond)
	if s := f.Sessions(); s.Active != 1 || s.Sessions != 0 || s.LastStartedAt == 0 {
		t.Errorf("while connected: %+v, want one active session", s)
	}
	c.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && f.Sessions().Sessions == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	s := f.Sessions()
	if s.Active != 0 || s.Sessions != 1 || s.LastEndedAt == 0 || s.BytesToGuest == 0 || s.BytesFromGuest == 0 {
		t.Errorf("after: %+v, want one ended session (the probe not counted) with its bytes", s)
	}
}

func TestCPUPctNeedsTwoLooksAtOneRental(t *testing.T) {
	var s cpuSample
	now := time.Now()
	if s.cpuPct("R1", 0, now, 4) != nil {
		t.Error("first look must have no rate")
	}
	if p := s.cpuPct("R1", 4_000_000, now.Add(2*time.Second), 4); p == nil || *p != 50 {
		t.Errorf("4 s of CPU over 2 s on 4 CPUs = %v, want 50", p)
	}
	if s.cpuPct("R2", 9_000_000, now.Add(4*time.Second), 4) != nil {
		t.Error("another rental starts afresh")
	}
}
