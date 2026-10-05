//go:build windows

package serving

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// freeBytes is the space available to this user on the volume holding dir (or
// its nearest existing parent). Serving is Linux only; this exists so the
// package builds and its tests run everywhere the agent does.
func freeBytes(dir string) (uint64, error) {
	for {
		p, err := windows.UTF16PtrFromString(dir)
		if err != nil {
			return 0, err
		}
		var avail, total, free uint64
		err = windows.GetDiskFreeSpaceEx(p, &avail, &total, &free)
		if err == nil {
			return avail, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return 0, err
		}
		dir = parent
	}
}
