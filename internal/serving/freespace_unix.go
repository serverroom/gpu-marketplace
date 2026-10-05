//go:build !windows

package serving

import (
	"path/filepath"
	"syscall"
)

// freeBytes is the space an unprivileged write could use on the filesystem
// holding dir (or its nearest existing parent), as stats.collectDisk reads it.
func freeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	for {
		err := syscall.Statfs(dir, &st)
		if err == nil {
			return st.Bavail * uint64(st.Bsize), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return 0, err
		}
		dir = parent
	}
}
