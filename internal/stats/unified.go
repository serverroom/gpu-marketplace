package stats

import "strings"

// unifiedMemoryModels are GPUs with no memory of their own. The CPU and the GPU
// share one pool of system memory, and either takes as much of it as the work
// needs — so nvidia-smi reports memory.total as [N/A], and that means "no
// separate pool", not "no memory". On these machines the whole pool IS the
// GPU's memory, and it is the machine's memory too.
//
// Kept deliberately narrow, like the control plane's own list: GH200/GB200
// carry real HBM that nvidia-smi does report, so they are not unified here. A
// GPU that reports no memory and is not on this list is treated as a GPU whose
// memory cannot be read, not guessed at.
var unifiedMemoryModels = []string{"gb10"}

// IsUnifiedMemoryModel reports whether a GPU model name is a known
// unified-memory part (NVIDIA GB10: DGX Spark, ASUS Ascent GX10, HP ZGX Nano).
func IsUnifiedMemoryModel(model string) bool {
	m := strings.ToLower(model)
	for _, part := range unifiedMemoryModels {
		if strings.Contains(m, part) {
			return true
		}
	}
	return false
}

// fillUnifiedMemory gives each unified-memory GPU the machine's memory pool as
// its memory, since that is what it can use. Used is what the host is using
// now, because the host and the GPU draw on the same pool.
func fillUnifiedMemory(gpus []GPUInfo, mem MemoryInfo) {
	for i := range gpus {
		if !gpus[i].UnifiedMemory || mem.TotalGB <= 0 {
			continue
		}
		gpus[i].VRAMTotalGB = mem.TotalGB
		if used := mem.TotalGB - mem.AvailableGB; used > 0 {
			gpus[i].VRAMUsedGB = used
		}
	}
}
