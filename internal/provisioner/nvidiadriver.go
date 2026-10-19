package provisioner

import (
	"fmt"
	"regexp"
	"strings"
)

// A machine whose NVIDIA driver packages were upgraded while it ran keeps the
// old kernel module loaded until it reboots, and from then on every nvidia-smi
// call fails ("Failed to initialize NVML: Driver/library version mismatch").
// Ubuntu's unattended-upgrades does that upgrade by itself. A rental's VM still
// gets the GPU, but nothing can check the GPU clean after one, so every
// teardown fails and the machine goes dirty, with nothing in its words saying
// why. Our Miami test box sat dirty that way for ten days in October 2026:
// upgraded from 580.173.02 to 580.178.04 on 9 October, never rebooted, every
// teardown from its 11 October test boot on refused.

var (
	kernelModuleVersion = regexp.MustCompile(`Kernel Module(?: for \S+)?\s+([0-9]+\.[0-9]+(?:\.[0-9]+)?)`)
	nvmlLibraryVersion  = regexp.MustCompile(`NVML library version:\s*([0-9]+\.[0-9]+(?:\.[0-9]+)?)`)
)

// NVIDIADriverMismatch says, in words a host can act on, that the machine's
// NVIDIA driver was updated under the one still loaded; "" when nvidia-smi
// answers, fails for another reason, or the machine has none.
func NVIDIADriverMismatch(r Runner) string {
	if _, err := r.LookPath("nvidia-smi"); err != nil {
		return ""
	}
	out, err := r.Output("nvidia-smi", "--query-gpu=driver_version", "--format=csv,noheader")
	if err == nil {
		return ""
	}
	text := out + "\n" + err.Error()
	if !strings.Contains(strings.ToLower(text), "version mismatch") {
		return ""
	}
	var loaded, installed string
	if v, rerr := r.Output("cat", "/proc/driver/nvidia/version"); rerr == nil {
		if m := kernelModuleVersion.FindStringSubmatch(v); m != nil {
			loaded = m[1]
		}
	}
	if m := nvmlLibraryVersion.FindStringSubmatch(text); m != nil {
		installed = m[1]
	}
	what := "the NVIDIA driver on this machine was updated while it ran, and the old one is still loaded"
	switch {
	case installed != "" && loaded != "":
		what = fmt.Sprintf("the NVIDIA driver on this machine was updated to %s while it ran, and %s is still the one loaded", installed, loaded)
	case loaded != "":
		what = fmt.Sprintf("the NVIDIA driver on this machine was updated while it ran, and %s is still the one loaded", loaded)
	}
	return what + ", so nvidia-smi fails and the GPU cannot be checked clean after a rental; reboot the machine to load the new driver"
}
