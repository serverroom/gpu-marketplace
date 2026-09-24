package vmrt

import (
	"fmt"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// armHost is a fake machine with a real board's /proc/cpuinfo (from the stats
// fixtures) and, when given, each CPU's top frequency in kHz.
func armHost(t *testing.T, cpuinfo string, freqs map[int]int) *fakehost.Host {
	t.Helper()
	data, err := os.ReadFile("../stats/testdata/" + cpuinfo)
	if err != nil {
		t.Fatal(err)
	}
	h := newHost()
	h.Files["/proc/cpuinfo"] = data
	for cpu, khz := range freqs {
		h.Files[fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq/cpuinfo_max_freq", cpu)] = []byte(fmt.Sprintf("%d\n", khz))
	}
	return h
}

func freqs(from, to, khz int, m map[int]int) map[int]int {
	if m == nil {
		m = map[int]int{}
	}
	for c := from; c <= to; c++ {
		m[c] = khz
	}
	return m
}

func TestTheVMGetsOneCoreType(t *testing.T) {
	for _, c := range []struct {
		name, cpuinfo, arch string
		freqs               map[int]int
		cores, cpu          string
	}{
		// RK3588: cpu0-3 Cortex-A55 at 1.8 GHz, cpu4-7 Cortex-A76 at 2.4 GHz.
		{"RK3588", "cpuinfo-rk3588", "arm64", freqs(0, 3, 1800000, freqs(4, 7, 2400000, nil)), "4,5,6,7", "Cortex-A76"},
		{"RK3588 without cpufreq", "cpuinfo-rk3588", "arm64", nil, "4,5,6,7", "Cortex-A76"},
		// GB10: cpu0-9 Cortex-X925, cpu10-19 Cortex-A725.
		{"GB10", "cpuinfo-gb10", "arm64", freqs(0, 9, 3900000, freqs(10, 19, 2808000, nil)), "0,1,2,3,4,5,6,7,8,9", "Cortex-X925"},
		{"Ampere Altra: one core type", "cpuinfo-altra", "arm64", nil, "", "Neoverse-N1"},
		{"x86", "cpuinfo-x86", "amd64", nil, "", "AMD Ryzen 9 7950X 16-Core Processor"},
	} {
		ch := ChooseGuestCPUs(armHost(t, c.cpuinfo, c.freqs), c.arch)
		if cpuList(ch.Cores) != c.cores || ch.Name != c.cpu {
			t.Errorf("%s: cores %q name %q, want %q %q", c.name, cpuList(ch.Cores), ch.Name, c.cores, c.cpu)
		}
	}
}

// Pinned, the VM gets one vCPU per pinned core, and QEMU and its vCPU threads
// run only there; unpinned, nothing changes.
func TestAPinnedVMRunsOnItsCores(t *testing.T) {
	s := testSpec()
	s.Arch, s.CPUs = "arm64", 8
	s.GuestCores = []int{4, 5, 6, 7}
	r := NewRental(dataDir, "R1")
	launch := strings.Join(LaunchArgs(s, r), " ")
	if !strings.Contains(launch, "--property=CPUAffinity=4,5,6,7 qemu-system-aarch64") || !strings.Contains(launch, "-smp 4 ") {
		t.Errorf("launch = %s", launch)
	}
	s.GuestCores = nil
	if launch := strings.Join(LaunchArgs(s, r), " "); strings.Contains(launch, "CPUAffinity") || !strings.Contains(launch, "-smp 7 ") {
		t.Errorf("unpinned launch = %s", launch)
	}
}

// A pair rental on a GB10 runs pinned too: the card goes to the VM as always,
// and the VM stays on the Cortex-X925 cores.
func TestAPinnedPairRental(t *testing.T) {
	h := pairHost()
	rt := New(h, func() Spec { s := testSpec(); s.GuestCores = []int{0, 1, 2, 3}; return s }(), &fakeFence{h: h}, func([]BoundDevice) bool { return true })
	p := goodPair(t)
	p.OtherMACs = nil
	if err := rt.Start(StartOptions{ID: pairID, Pubkey: key(t), Pair: p}); err != nil {
		t.Fatal(err)
	}
	if launch := h.Call("run systemd-run"); !strings.Contains(launch, "CPUAffinity=0,1,2,3") || !strings.Contains(launch, "vfio-pci,host="+nic0) {
		t.Errorf("launch = %s", launch)
	}
	rt.Stop()
}

// The rentals and the base image may live on another disk than the state.
func TestTheRentalsLiveInTheStorageDir(t *testing.T) {
	h := newHost()
	s := testSpec()
	s.StorageDir = "/mnt/nvme/gpu-agent"
	rt := New(h, s, &fakeFence{h: h}, func([]BoundDevice) bool { return true })
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatal(err)
	}
	want := path.Join("/mnt/nvme/gpu-agent", "rentals", "R1")
	if !h.Ran("run truncate -s 100G " + path.Join(want, "disk.img")) {
		t.Errorf("the disk is not in the storage dir: %v", h.Calls)
	}
	if st, _ := LoadState(h, dataDir); st == nil || st.Rental.Dir != want {
		t.Errorf("state = %+v", st)
	}
	rt.Stop()
}
