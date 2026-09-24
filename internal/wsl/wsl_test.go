package wsl

import (
	"strings"
	"testing"
)

func TestParseNVIDIA(t *testing.T) {
	out := "00000000:01:00.0, NVIDIA GeForce RTX 4090, GPU-6b7c1f0e-aaaa-bbbb-cccc-0123456789ab, 0x268410DE, 24564, 581.29\r\n" +
		"00000000:4B:00.0, NVIDIA RTX A6000, GPU-11111111-2222-3333-4444-555555555555, 0x223010DE, 49140, 581.29\r\n"
	gpus := parseNVIDIA(out)
	if len(gpus) != 2 {
		t.Fatalf("gpus = %+v", gpus)
	}
	g := gpus[0]
	if g.BusID != "0000:01:00.0" || g.Name != "NVIDIA GeForce RTX 4090" || g.DeviceID != "10de:2684" || g.MemoryMB != 24564 || g.Driver != "581.29" {
		t.Errorf("gpu = %+v", g)
	}
	if gpus[1].BusID != "0000:4b:00.0" {
		t.Errorf("second bus id = %q", gpus[1].BusID)
	}
	if len(parseNVIDIA("NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver.")) != 0 {
		t.Error("an nvidia-smi error is not a GPU")
	}
}

func TestParseGateway(t *testing.T) {
	out := `===========================================================================
IPv4 Route Table
===========================================================================
Active Routes:
Network Destination        Netmask          Gateway       Interface  Metric
          0.0.0.0          0.0.0.0      192.168.1.1    192.168.1.19     35
===========================================================================
Persistent Routes:
  None
`
	if gw := parseGateway(out); gw != "192.168.1.1" {
		t.Errorf("gateway = %q", gw)
	}
	if gw := parseGateway("Active Routes:\n  None\n"); gw != "" {
		t.Errorf("no route, gateway = %q", gw)
	}
}

func TestSumFor(t *testing.T) {
	sums := "2a790896740b14d637dbdc583cce1ba081ac53b9e9cdb46dc09a2f73abbd9934  ubuntu-noble-wsl-amd64-24.04lts.rootfs.tar.gz\n" +
		"8251e27ffff381a4af5f41dcb94d867de3e0d9774a9241908ab34555d99315ea  ubuntu-noble-wsl-amd64-wsl.rootfs.tar.gz\n"
	if got := sumFor(sums, rootfsName("amd64")); got != "8251e27ffff381a4af5f41dcb94d867de3e0d9774a9241908ab34555d99315ea" {
		t.Errorf("sum = %q", got)
	}
	if sumFor(sums, rootfsName("arm64")) != "" {
		t.Error("a file not listed has no sum")
	}
}

// The VM is sized for the rental plus the distribution, and never takes the
// whole machine; the config keeps WSL on its own NAT with nothing forwarded.
func TestWSLConfig(t *testing.T) {
	if got := VMMemoryMB(65536, 58982); got != 60006 {
		t.Errorf("64 GB machine: %d", got)
	}
	if got := VMMemoryMB(8192, 7000); got != 6144 {
		t.Errorf("capped at the machine less 2 GB: %d", got)
	}
	c := WSLConfig(60006, 15)
	for _, want := range []string{"memory=60006MB", "processors=15", "networkingMode=nat", "localhostForwarding=false", "guiApplications=false"} {
		if !strings.Contains(c, want) {
			t.Errorf(".wslconfig lacks %q:\n%s", want, c)
		}
	}
	for _, want := range []string{"[automount]\nenabled=false", "[interop]\nenabled=false", "appendWindowsPath=false"} {
		if !strings.Contains(wslConf, want) {
			t.Errorf("wsl.conf lacks %q", want)
		}
	}
}
