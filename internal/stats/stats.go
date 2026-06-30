package stats

import (
	"os"
	"runtime"
	"time"
)

// CPUInfo holds CPU details.
type CPUInfo struct {
	Model   string  `json:"model"`
	Cores   int     `json:"cores"`
	Threads int     `json:"threads"`
	UsagePct float64 `json:"usage_pct"`
}

// MemoryInfo holds memory details.
type MemoryInfo struct {
	TotalGB     float64 `json:"total_gb"`
	AvailableGB float64 `json:"available_gb"`
}

// GPUInfo holds GPU details.
type GPUInfo struct {
	Model          string  `json:"model"`
	VRAMTotalGB    float64 `json:"vram_total_gb"`
	VRAMUsedGB     float64 `json:"vram_used_gb"`
	TempC          int     `json:"temp_c"`
	UtilizationPct float64 `json:"utilization_pct"`
}

// DiskInfo holds disk details.
type DiskInfo struct {
	TotalGB float64 `json:"total_gb"`
	FreeGB  float64 `json:"free_gb"`
}

// SystemStats is the full stats response.
type SystemStats struct {
	Hostname      string     `json:"hostname"`
	OS            string     `json:"os"`
	Arch          string     `json:"arch"`
	CPU           CPUInfo    `json:"cpu"`
	Memory        MemoryInfo `json:"memory"`
	GPUs          []GPUInfo  `json:"gpus"`
	Disk          DiskInfo   `json:"disk"`
	Status        string     `json:"status"`
	UptimeSeconds int64      `json:"uptime_seconds"`
	CollectedAt   string     `json:"collected_at"`
}

// Collect gathers all system stats.
func Collect() (*SystemStats, error) {
	hostname, _ := os.Hostname()

	cpu, err := collectCPU()
	if err != nil {
		cpu = CPUInfo{Model: "unknown", Cores: runtime.NumCPU(), Threads: runtime.NumCPU()}
	}

	mem, err := collectMemory()
	if err != nil {
		mem = MemoryInfo{}
	}

	gpus := collectGPUs()

	disk, err := collectDisk()
	if err != nil {
		disk = DiskInfo{}
	}

	return &SystemStats{
		Hostname:    hostname,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		CPU:         cpu,
		Memory:      mem,
		GPUs:        gpus,
		Disk:        disk,
		Status:      "free",
		CollectedAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// collectGPUs tries all GPU detection methods.
func collectGPUs() []GPUInfo {
	// Try NVIDIA first
	gpus, err := collectNVIDIA()
	if err == nil && len(gpus) > 0 {
		return gpus
	}

	// Try AMD
	gpus, err = collectAMD()
	if err == nil && len(gpus) > 0 {
		return gpus
	}

	// Try Apple Silicon (macOS)
	if runtime.GOOS == "darwin" {
		gpus, err = collectAppleGPU()
		if err == nil && len(gpus) > 0 {
			return gpus
		}
	}

	return []GPUInfo{}
}
