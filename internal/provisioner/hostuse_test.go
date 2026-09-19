package provisioner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// hostedHost is goodHost with what Detect reads besides preflight: memory
// (64 GB, 60 GB of it free) and the data disk.
func hostedHost(t *testing.T) *fakehost.Host {
	t.Helper()
	h := goodHost(t, "00000000:01:00.0, NVIDIA L4, 23034\n")
	h.Files["/proc/meminfo"] = []byte("MemTotal:       67108864 kB\nMemAvailable:   62914560 kB\n")
	h.Files[dataDir] = nil
	h.Outputs["df --output=avail"] = " Avail\n  900G\n"
	return h
}

// The host's own program on the GPU does not take the machine off the market:
// it stays ready, and says what the host is using.
func TestAMachineInUseByItsHostStaysReady(t *testing.T) {
	noRelay(t)
	h := hostedHost(t)
	holds(h, "11435", "llama-server", "/dev/nvidia0")
	c := Detect(h, "linux", "amd64", dataDir, version).Capability()
	if !c.Ready || c.HostBusy == nil || strings.Join(c.HostBusy.Holders, ",") != "llama-server (pid 11435)" ||
		c.HostBusy.MemoryShortGB != 0 || c.HostBusy.Since == 0 {
		t.Fatalf("capability = %+v host_busy = %+v", c, c.HostBusy)
	}
	data, _ := json.Marshal(c)
	if !strings.Contains(string(data), `"host_busy":{"since":`) || !strings.Contains(string(data), `"holders":["llama-server (pid 11435)"],"memory_short_gb":0}`) {
		t.Errorf("json = %s", data)
	}
	if c.SelfTest == nil || !c.SelfTest.Passed || !c.SelfTest.GPUVerified || c.SelfTest.AgentVersion != version || c.RetestPending {
		t.Errorf("selftest = %+v retest=%v", c.SelfTest, c.RetestPending)
	}

	delete(h.Links, "/proc/11435/fd/9")
	c = Detect(h, "linux", "amd64", dataDir, version).Capability()
	if data, _ := json.Marshal(c); c.HostBusy != nil || strings.Contains(string(data), "host_busy") {
		t.Errorf("a free machine reports %s", data)
	}
}

// A machine without a GPU is in use by its host when the memory a rental's VM
// needs is in use.
func TestAMachineWithoutAGPUIsInUseWhenItsMemoryIs(t *testing.T) {
	noRelay(t)
	h := cpuHost(t)
	h.Files["/proc/meminfo"] = []byte("MemTotal:       65536000 kB\nMemAvailable:   20480000 kB\n")
	c := Detect(h, "linux", "amd64", dataDir, version).Capability()
	if !c.Ready || c.HostBusy == nil || len(c.HostBusy.Holders) != 0 || c.HostBusy.MemoryShortGB != 37 {
		t.Fatalf("capability ready=%v %v host_busy=%+v", c.Ready, c.Reasons, c.HostBusy)
	}
	if data, _ := json.Marshal(c.HostBusy); !strings.Contains(string(data), `"holders":[]`) {
		t.Errorf("holders must be a list: %s", data)
	}
}
