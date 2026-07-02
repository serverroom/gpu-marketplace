//go:build !windows

package stats

// Stubs so the runtime.GOOS switches in cpu.go and memory.go compile on
// non-Windows platforms; never called there.

func memWindows() (MemoryInfo, error) {
	return MemoryInfo{}, nil
}

func cpuWindows(threads int) (CPUInfo, error) {
	return CPUInfo{Model: "unknown", Cores: threads, Threads: threads}, nil
}
