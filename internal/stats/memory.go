package stats

import (
	"bufio"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

func collectMemory() (MemoryInfo, error) {
	switch runtime.GOOS {
	case "linux":
		return memLinux()
	case "darwin":
		return memDarwin()
	case "windows":
		return memWindows()
	default:
		return MemoryInfo{}, nil
	}
}

func memLinux() (MemoryInfo, error) {
	var info MemoryInfo
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return info, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			info.TotalGB = parseMemKB(line)
		} else if strings.HasPrefix(line, "MemAvailable:") {
			info.AvailableGB = parseMemKB(line)
		}
	}
	return info, nil
}

func parseMemKB(line string) float64 {
	fields := strings.Fields(line)
	if len(fields) >= 2 {
		kb, err := strconv.ParseFloat(fields[1], 64)
		if err == nil {
			return kb / 1024 / 1024 // KB to GB
		}
	}
	return 0
}

func memDarwin() (MemoryInfo, error) {
	var info MemoryInfo

	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return info, err
	}
	bytes, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	info.TotalGB = bytes / 1024 / 1024 / 1024

	// Get available memory from vm_stat
	out, err = exec.Command("vm_stat").Output()
	if err == nil {
		var freePages, inactivePages float64
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "Pages free") {
				freePages = parseVMStatPages(line)
			} else if strings.Contains(line, "Pages inactive") {
				inactivePages = parseVMStatPages(line)
			}
		}
		// Each page is 4096 bytes (16384 on Apple Silicon, but vm_stat reports 4096)
		info.AvailableGB = (freePages + inactivePages) * 4096 / 1024 / 1024 / 1024
	}

	return info, nil
}

func parseVMStatPages(line string) float64 {
	parts := strings.Split(line, ":")
	if len(parts) < 2 {
		return 0
	}
	s := strings.TrimSpace(strings.TrimSuffix(parts[1], "."))
	n, _ := strconv.ParseFloat(s, 64)
	return n
}
