package vmrt

import (
	"encoding/json"
	"path"
	"strings"
)

// A machine whose processor's GPU can be passed through (pcidev.PassableAPU)
// rents it only once a test boot has proven it on this machine. Before this
// agent version such a machine rented its CPUs without the GPU, and must keep
// doing so when the GPU does not come up in a VM: a test boot that took the
// processor's GPU and failed is recorded here, and while the record holds the
// GPU is left out (provisioner preflight), the machine is tested again without
// it and rents as it did before. The record holds for this agent version on
// this kernel: a new agent or a new kernel tries the GPU once more.

// APUTestPath is where the last failed test boot with the processor's GPU is
// recorded.
func APUTestPath(dataDir string) string { return path.Join(dataDir, "apu-test.json") }

// APUTestFailure is a test boot that took the processor's GPU and failed.
type APUTestFailure struct {
	GPUs         []string `json:"gpus"`
	AgentVersion string   `json:"agent_version"`
	Kernel       string   `json:"kernel"`
	Problems     []string `json:"problems"`
	At           int64    `json:"at"`
}

// kernelRelease is the running kernel's release ("7.0.0-14-generic").
func kernelRelease(h Host) string {
	data, err := h.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// recordAPUTest keeps the outcome of a test boot that took the processor's
// GPU: a failure is recorded, a pass clears an earlier failure. A test that
// did not take the GPU, or was stopped, says nothing of it.
func recordAPUTest(h Host, dataDir string, res SelfTestResult) {
	if res.WithoutGPU || res.Stopped || res.InUse != "" {
		return
	}
	var apus []string
	for _, gpu := range res.HostGPUs {
		if IntegratedGPU(h, gpu) {
			apus = append(apus, gpu)
		}
	}
	if len(apus) == 0 {
		return
	}
	if res.Passed {
		_ = h.Remove(APUTestPath(dataDir))
		return
	}
	data, err := json.MarshalIndent(APUTestFailure{GPUs: apus, AgentVersion: res.AgentVersion,
		Kernel: kernelRelease(h), Problems: res.Problems, At: res.At}, "", "  ")
	if err != nil {
		return
	}
	_ = h.MkdirAll(dataDir, 0700)
	_ = h.WriteFile(APUTestPath(dataDir), data, 0600)
}

// APUTestFailed reports whether this agent version's test boot with the
// processor's GPU bdf failed on this kernel, and what it found.
func APUTestFailed(h Host, dataDir, bdf, version string) (problems []string, failed bool) {
	data, err := h.ReadFile(APUTestPath(dataDir))
	if err != nil {
		return nil, false
	}
	var rec APUTestFailure
	if json.Unmarshal(data, &rec) != nil {
		return nil, false
	}
	if rec.AgentVersion != version || rec.Kernel != kernelRelease(h) {
		return nil, false
	}
	for _, gpu := range rec.GPUs {
		if gpu == bdf {
			return rec.Problems, true
		}
	}
	return nil, false
}
