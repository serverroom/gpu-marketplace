//go:build windows

package stats

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	kernel32                             = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx             = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetLogicalProcessorInformationEx = kernel32.NewProc("GetLogicalProcessorInformationEx")
)

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func memWindows() (MemoryInfo, error) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r1, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r1 == 0 {
		return MemoryInfo{}, err
	}
	return MemoryInfo{
		TotalGB:     float64(m.TotalPhys) / 1024 / 1024 / 1024,
		AvailableGB: float64(m.AvailPhys) / 1024 / 1024 / 1024,
	}, nil
}

func cpuWindows(threads int) (CPUInfo, error) {
	info := CPUInfo{Threads: threads, Cores: threads}

	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE)
	if err == nil {
		if name, _, err := k.GetStringValue("ProcessorNameString"); err == nil {
			info.Model = strings.TrimSpace(name)
		}
		k.Close()
	}

	if cores := physicalCoreCount(); cores > 0 {
		info.Cores = cores
	}

	return info, nil
}

// physicalCoreCount counts RelationProcessorCore entries from
// GetLogicalProcessorInformationEx. Returns 0 if the call fails.
func physicalCoreCount() int {
	const relationProcessorCore = 0

	var size uint32
	r1, _, err := procGetLogicalProcessorInformationEx.Call(
		relationProcessorCore, 0, uintptr(unsafe.Pointer(&size)))
	if r1 != 0 || err != windows.ERROR_INSUFFICIENT_BUFFER || size == 0 {
		return 0
	}

	buf := make([]byte, size)
	r1, _, _ = procGetLogicalProcessorInformationEx.Call(
		relationProcessorCore, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r1 == 0 {
		return 0
	}

	// Each SYSTEM_LOGICAL_PROCESSOR_INFORMATION_EX entry starts with
	// Relationship (uint32) followed by Size (uint32); entries are variable length.
	cores := 0
	for offset := uint32(0); offset+8 <= size; {
		entrySize := *(*uint32)(unsafe.Pointer(&buf[offset+4]))
		if entrySize == 0 {
			break
		}
		cores++
		offset += entrySize
	}
	return cores
}
