package stats

import (
	"sync"
	"time"
)

// The agent's own GPU queries (nvidia-smi, rocm-smi) open the GPU's device
// files for a few hundred milliseconds each. While the runtime takes the GPU
// for a rental or a test boot, one of them would be a holder that refuses the
// start -- a DGX Spark's test boot failed on exactly that ("the GPU is in use
// on this machine by nvidia-smi"). So the runtime pauses them: queries started
// while paused are skipped (Collect answers with the last GPUs it read), and a
// pause waits for the ones already running to exit.

var gpuGate struct {
	mu      sync.Mutex
	paused  int
	running int
	last    []GPUInfo
}

// PauseWait bounds how long PauseGPUQueries waits for running queries to
// exit: a hung nvidia-smi must not hang a rental start with it.
var PauseWait = 10 * time.Second

// PauseGPUQueries stops the agent's GPU queries until resume is called, and
// waits (up to PauseWait) for any running one to exit. Pauses nest; resume is
// safe to call more than once.
func PauseGPUQueries() (resume func()) {
	gpuGate.mu.Lock()
	gpuGate.paused++
	gpuGate.mu.Unlock()
	for waited := time.Duration(0); waited < PauseWait; waited += 20 * time.Millisecond {
		gpuGate.mu.Lock()
		idle := gpuGate.running == 0
		gpuGate.mu.Unlock()
		if idle {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			gpuGate.mu.Lock()
			gpuGate.paused--
			gpuGate.mu.Unlock()
		})
	}
}

// GPUQueriesPaused reports whether the agent's GPU queries are paused.
func GPUQueriesPaused() bool {
	gpuGate.mu.Lock()
	defer gpuGate.mu.Unlock()
	return gpuGate.paused > 0
}

// beginGPUQuery registers a query about to run; false while paused.
func beginGPUQuery() bool {
	gpuGate.mu.Lock()
	defer gpuGate.mu.Unlock()
	if gpuGate.paused > 0 {
		return false
	}
	gpuGate.running++
	return true
}

func endGPUQuery() {
	gpuGate.mu.Lock()
	gpuGate.running--
	gpuGate.mu.Unlock()
}

// gatedGPUs runs collect unless GPU queries are paused, and remembers what it
// read; while paused it answers with the last GPUs read.
func gatedGPUs(collect func() []GPUInfo) []GPUInfo {
	if !beginGPUQuery() {
		gpuGate.mu.Lock()
		defer gpuGate.mu.Unlock()
		return append([]GPUInfo{}, gpuGate.last...)
	}
	gpus := func() []GPUInfo {
		defer endGPUQuery()
		return collect()
	}()
	gpuGate.mu.Lock()
	gpuGate.last = append([]GPUInfo{}, gpus...)
	gpuGate.mu.Unlock()
	return gpus
}
