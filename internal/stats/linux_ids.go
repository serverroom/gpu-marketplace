package stats

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// linuxArmCPU names an arm64 Linux machine's CPU from its device tree and
// /proc/cpuinfo (see arm.go).
func linuxArmCPU(gpuModels []string) (model, coresDetail string) {
	compatible, _ := os.ReadFile("/proc/device-tree/compatible")
	cpuinfo, _ := os.ReadFile("/proc/cpuinfo")
	cores := ParseArmCores(string(cpuinfo))
	return ArmCPUModel(compatible, cores, gpuModels), CoresDetail(cores)
}

// linuxBoard is the device tree's model, else the DMI product name.
func linuxBoard() string {
	if model, err := os.ReadFile("/proc/device-tree/model"); err == nil {
		if b := BoardName(model); b != "" {
			return b
		}
	}
	if name, err := os.ReadFile("/sys/class/dmi/id/product_name"); err == nil {
		return BoardName(name)
	}
	return ""
}

// hottestZone is the hottest /sys/class/thermal zone in whole degrees C.
func hottestZone() int {
	zones, _ := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
	hottest := 0
	for _, z := range zones {
		data, err := os.ReadFile(z)
		if err != nil {
			continue
		}
		milli, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err == nil && milli/1000 > hottest && milli/1000 < 150 {
			hottest = milli / 1000
		}
	}
	return hottest
}
