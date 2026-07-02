package stats

import (
	"bufio"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// collectCPU returns CPU info. Platform-specific.
func collectCPU() (CPUInfo, error) {
	threads := runtime.NumCPU()

	switch runtime.GOOS {
	case "linux":
		return cpuLinux(threads)
	case "darwin":
		return cpuDarwin(threads)
	case "windows":
		return cpuWindows(threads)
	default:
		return CPUInfo{Model: "unknown", Cores: threads, Threads: threads}, nil
	}
}

func cpuLinux(threads int) (CPUInfo, error) {
	info := CPUInfo{Threads: threads}

	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return info, err
	}
	defer f.Close()

	coreIDs := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "model name") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 && info.Model == "" {
				info.Model = strings.TrimSpace(parts[1])
			}
		}
		if strings.HasPrefix(line, "core id") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				coreIDs[strings.TrimSpace(parts[1])] = true
			}
		}
	}

	if len(coreIDs) > 0 {
		info.Cores = len(coreIDs)
	} else {
		info.Cores = threads
	}

	return info, nil
}

func cpuDarwin(threads int) (CPUInfo, error) {
	info := CPUInfo{Threads: threads}

	out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
	if err == nil {
		info.Model = strings.TrimSpace(string(out))
	}

	out, err = exec.Command("sysctl", "-n", "hw.physicalcpu").Output()
	if err == nil {
		n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
		if n > 0 {
			info.Cores = n
		}
	}

	return info, nil
}
