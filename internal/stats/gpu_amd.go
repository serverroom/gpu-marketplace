package stats

import (
	"os/exec"
	"strconv"
	"strings"
)

// collectAMD queries rocm-smi for AMD GPU info.
func collectAMD() ([]GPUInfo, error) {
	// Try rocm-smi --showproductname --showmeminfo vram --showtemp --showuse --csv
	out, err := exec.Command("rocm-smi",
		"--showproductname", "--showmeminfo", "vram",
		"--showtemp", "--showuse", "--csv",
	).Output()
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return nil, nil
	}

	// Parse CSV header to find column indices
	header := strings.Split(lines[0], ",")
	colIdx := make(map[string]int)
	for i, col := range header {
		colIdx[strings.TrimSpace(strings.ToLower(col))] = i
	}

	var gpus []GPUInfo
	for _, line := range lines[1:] {
		fields := strings.Split(line, ",")
		gpu := GPUInfo{Model: "AMD GPU"}

		// Extract model name
		for key, idx := range colIdx {
			if idx >= len(fields) {
				continue
			}
			val := strings.TrimSpace(fields[idx])
			switch {
			case strings.Contains(key, "card series"):
				gpu.Model = val
			case strings.Contains(key, "vram total"):
				mb, _ := strconv.ParseFloat(val, 64)
				gpu.VRAMTotalGB = mb / 1024
			case strings.Contains(key, "vram used"):
				mb, _ := strconv.ParseFloat(val, 64)
				gpu.VRAMUsedGB = mb / 1024
			case strings.Contains(key, "temperature"):
				t, _ := strconv.ParseFloat(val, 64)
				gpu.TempC = int(t)
			case strings.Contains(key, "gpu use"):
				u, _ := strconv.ParseFloat(strings.TrimSuffix(val, "%"), 64)
				gpu.UtilizationPct = u
			}
		}

		gpus = append(gpus, gpu)
	}

	return gpus, nil
}
