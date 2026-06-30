//go:build !windows

package stats

import "syscall"

func collectDisk() (DiskInfo, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return DiskInfo{}, err
	}
	totalBytes := stat.Blocks * uint64(stat.Bsize)
	freeBytes := stat.Bavail * uint64(stat.Bsize)
	return DiskInfo{
		TotalGB: float64(totalBytes) / 1024 / 1024 / 1024,
		FreeGB:  float64(freeBytes) / 1024 / 1024 / 1024,
	}, nil
}
