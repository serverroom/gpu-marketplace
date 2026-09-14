package stats

import (
	"os/exec"
	"strconv"
	"strings"
)

// collectNVIDIA queries nvidia-smi for GPU info.
func collectNVIDIA() ([]GPUInfo, error) {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=name,memory.total,memory.used,temperature.gpu,utilization.gpu",
		"--format=csv,noheader,nounits",
	).Output()
	if err != nil {
		return nil, err
	}

	var gpus []GPUInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ", ")
		if len(fields) < 5 {
			continue
		}

		model := strings.TrimSpace(fields[0])
		vramTotal, totalErr := strconv.ParseFloat(strings.TrimSpace(fields[1]), 64)
		vramUsed, _ := strconv.ParseFloat(strings.TrimSpace(fields[2]), 64)
		temp, _ := strconv.Atoi(strings.TrimSpace(fields[3]))
		util, _ := strconv.ParseFloat(strings.TrimSpace(fields[4]), 64)

		gpus = append(gpus, GPUInfo{
			Model:          model,
			VRAMTotalGB:    vramTotal / 1024, // MiB to GB
			VRAMUsedGB:     vramUsed / 1024,
			TempC:          temp,
			UtilizationPct: util,
			// [N/A] on a GB10 is not "no memory": its memory is the machine's
			// pool, filled in by Collect once host memory is known.
			UnifiedMemory: (totalErr != nil || vramTotal <= 0) && IsUnifiedMemoryModel(model),
		})
	}
	return gpus, nil
}
