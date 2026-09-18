package stats

import "github.com/serverroom/gpu-marketplace/internal/pcidev"

// gpusFromPCI names the machine's GPUs from sysfs, for when no vendor tool
// answers: a machine with an Intel card, or an AMD one without ROCm, still
// registers with what it has. The processor's own GPU is left out, as rentals
// leave it out -- a machine with only that one registers without a GPU. The
// host cannot tell the cards' memory; the test boot reports that from inside
// the rental's VM.
func gpusFromPCI(fs pcidev.FS) []GPUInfo {
	var gpus []GPUInfo
	for _, d := range pcidev.Display(fs) {
		if !pcidev.Integrated(fs, d) {
			gpus = append(gpus, GPUInfo{Model: pcidev.Name(fs, d)})
		}
	}
	return gpus
}
