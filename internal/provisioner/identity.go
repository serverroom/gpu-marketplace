package provisioner

import (
	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/interconnect"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// Whether a machine is an NVIDIA DGX Spark is decided in one place,
// interconnect.MatchIdentity: linux/arm64, an NVIDIA system vendor, a product
// name or family naming a DGX Spark, and a GB10 GPU. The desktop-on-demand
// rule here and the linked-pair checks both use it, so they can never
// disagree about a machine. The raw DMI strings always travel in the
// capability, because the exact values a Spark reports are not yet verified
// on hardware.

// dmiMaxLen caps each DMI string the agent reports.
const dmiMaxLen = interconnect.DMIMaxLen

// ReadIdentity reads the machine's DMI strings and matches them. A missing
// file reads as "".
func ReadIdentity(h vmrt.Host, goos, arch string, gpuNames []string) control.Identity {
	return interconnect.ReadIdentity(h, goos, arch, gpuNames)
}

// MatchIdentity decides whether raw DMI values, the platform and the detected
// GPU names are an NVIDIA DGX Spark.
func MatchIdentity(id control.Identity, goos, arch string, gpuNames []string) control.Identity {
	return interconnect.MatchIdentity(id, goos, arch, gpuNames)
}
