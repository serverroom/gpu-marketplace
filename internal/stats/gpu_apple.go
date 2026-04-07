package stats

import (
	"encoding/json"
	"os/exec"
)

// collectAppleGPU uses system_profiler on macOS.
func collectAppleGPU() ([]GPUInfo, error) {
	out, err := exec.Command("system_profiler", "SPDisplaysDataType", "-json").Output()
	if err != nil {
		return nil, err
	}

	var result struct {
		SPDisplaysDataType []struct {
			ChipsetModel string `json:"sppci_model"`
			VRAM         string `json:"sppci_vram"`
			// Apple Silicon reports unified memory as VRAM
			VRAMShared string `json:"sppci_vram_shared"`
		} `json:"SPDisplaysDataType"`
	}

	if err := json.Unmarshal(out, &result); err != nil {
		return nil, err
	}

	var gpus []GPUInfo
	for _, d := range result.SPDisplaysDataType {
		gpu := GPUInfo{
			Model: d.ChipsetModel,
		}
		// Apple Silicon reports VRAM as shared unified memory
		// system_profiler doesn't give utilization or temp for Apple GPU
		gpus = append(gpus, gpu)
	}

	return gpus, nil
}
