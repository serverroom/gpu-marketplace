//go:build !windows

package stats

import (
	"syscall"

	"github.com/serverroom/gpu-marketplace/internal/config"
)

// The machine's disk, as the marketplace shows it, is the filesystem that
// holds the rentals' disks: the storage directory `gpu-agent setup` chose (the
// biggest internal disk with room, or --data-dir), else the agent's data
// directory. Reading "/" instead showed a board that keeps its OS on a small
// eMMC or SD partition and its NVMe elsewhere with the OS partition's 7 GB
// beside a 207 GB rental disk. On a machine with one filesystem it is "/"
// either way.
var storageDir = config.StorageDir

func collectDisk() (DiskInfo, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(existingDir(storageDir(), dirExists), &stat); err != nil {
		if err := syscall.Statfs("/", &stat); err != nil {
			return DiskInfo{}, err
		}
	}
	totalBytes := stat.Blocks * uint64(stat.Bsize)
	freeBytes := stat.Bavail * uint64(stat.Bsize)
	return DiskInfo{
		TotalGB: float64(totalBytes) / 1024 / 1024 / 1024,
		FreeGB:  float64(freeBytes) / 1024 / 1024 / 1024,
	}, nil
}

func dirExists(dir string) bool {
	var stat syscall.Stat_t
	return syscall.Stat(dir, &stat) == nil
}
