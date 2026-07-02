//go:build windows

package stats

import (
	"os"

	"golang.org/x/sys/windows"
)

func collectDisk() (DiskInfo, error) {
	var info DiskInfo

	drive := os.Getenv("SystemDrive")
	if drive == "" {
		drive = "C:"
	}

	path, err := windows.UTF16PtrFromString(drive + `\`)
	if err != nil {
		return info, err
	}

	var freeToCaller, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(path, &freeToCaller, &total, &totalFree); err != nil {
		return info, err
	}

	info.TotalGB = float64(total) / 1024 / 1024 / 1024
	info.FreeGB = float64(freeToCaller) / 1024 / 1024 / 1024
	return info, nil
}
