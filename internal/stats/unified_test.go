package stats

import "testing"

func TestIsUnifiedMemoryModel(t *testing.T) {
	for model, want := range map[string]bool{
		"NVIDIA GB10":             true,
		"nvidia gb10":             true,
		"NVIDIA A100-SXM4-80GB":   false,
		"NVIDIA GH200 480GB":      false, // real HBM, reported by nvidia-smi
		"NVIDIA GeForce RTX 5090": false,
		"":                        false,
	} {
		if got := IsUnifiedMemoryModel(model); got != want {
			t.Errorf("IsUnifiedMemoryModel(%q) = %v, want %v", model, got, want)
		}
	}
}

// A DGX Spark: nvidia-smi says [N/A], and the 119 GB the machine has is what the
// GPU can use. It must not publish as a GPU with no memory.
func TestUnifiedGPUGetsTheMachinePool(t *testing.T) {
	gpus := []GPUInfo{
		{Model: "NVIDIA GB10", UnifiedMemory: true},
		{Model: "NVIDIA A100", VRAMTotalGB: 80, VRAMUsedGB: 1},
	}
	fillUnifiedMemory(gpus, MemoryInfo{TotalGB: 119.6, AvailableGB: 110.1})
	if gpus[0].VRAMTotalGB != 119.6 {
		t.Errorf("unified GPU memory = %v, want the machine's 119.6", gpus[0].VRAMTotalGB)
	}
	if got := gpus[0].VRAMUsedGB; got < 9.4 || got > 9.6 {
		t.Errorf("unified GPU used = %v, want what the host uses (~9.5)", got)
	}
	if gpus[1].VRAMTotalGB != 80 || gpus[1].VRAMUsedGB != 1 {
		t.Errorf("a discrete GPU was rewritten: %+v", gpus[1])
	}
}

func TestUnknownMemoryIsNotInvented(t *testing.T) {
	gpus := []GPUInfo{{Model: "NVIDIA GB10", UnifiedMemory: true}}
	fillUnifiedMemory(gpus, MemoryInfo{})
	if gpus[0].VRAMTotalGB != 0 {
		t.Errorf("invented %v GB with no host memory figure", gpus[0].VRAMTotalGB)
	}
}
