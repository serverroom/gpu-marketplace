//go:build windows

package stats

import (
	"os/exec"
	"strconv"
	"strings"
)

func collectDisk() (DiskInfo, error) {
	var info DiskInfo
	out, err := exec.Command("wmic", "logicaldisk", "where", "DeviceID='C:'",
		"get", "Size,FreeSpace", "/value").Output()
	if err != nil {
		return info, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Size=") {
			b, _ := strconv.ParseFloat(strings.TrimPrefix(line, "Size="), 64)
			info.TotalGB = b / 1024 / 1024 / 1024
		} else if strings.HasPrefix(line, "FreeSpace=") {
			b, _ := strconv.ParseFloat(strings.TrimPrefix(line, "FreeSpace="), 64)
			info.FreeGB = b / 1024 / 1024 / 1024
		}
	}
	return info, nil
}
