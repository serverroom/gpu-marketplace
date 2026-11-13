package vmrt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/serverroom/gpu-marketplace/internal/pcidev"
)

// An AMD APU's GPU has no expansion ROM of its own: the firmware hands its
// VBIOS to the operating system in an ACPI table, VFCT, and amdgpu reads it
// from there. A VM has no such table, so the guest's amdgpu finds no VBIOS and
// gives up on the GPU. QEMU can show the guest a ROM read from a file
// (vfio-pci's romfile), and amdgpu reads a VBIOS from the ROM: so the runtime
// takes the image out of the host's VFCT table and hands it to QEMU.

// vfctTable is where the kernel shows the firmware's VFCT table.
const vfctTable = "/sys/firmware/acpi/tables/VFCT"

// The VFCT layout, as amdgpu reads it (drivers/gpu/drm/amd/amdgpu/amdgpu_bios.c):
// the 36-byte ACPI header, a 16-byte UUID, the offset of the first image, the
// offset of a library image, 16 reserved bytes; then images one after another,
// each a 28-byte header -- bus, device and function (32 bits each), vendor,
// device, subsystem vendor and subsystem IDs (16 bits each), revision and the
// image's length (32 bits each) -- followed by the image itself.
const (
	vfctImageOffset = 36 + 16
	vfctImageHeader = 28
)

// VFCTImage is the VBIOS the firmware's VFCT table holds for one PCI function,
// matched as amdgpu matches it: bus, device, function, vendor and device ID.
func VFCTImage(h Host, bdf string) ([]byte, error) {
	table, err := h.ReadFile(vfctTable)
	if err != nil {
		return nil, fmt.Errorf("the firmware has no VFCT table (%v)", err)
	}
	bus, dev, fn, ok := splitBDF(bdf)
	if !ok {
		return nil, fmt.Errorf("%q is not a PCI address", bdf)
	}
	d := pcidev.Read(h, bdf)
	vendor, err1 := strconv.ParseUint(d.Vendor, 16, 16)
	device, err2 := strconv.ParseUint(d.Device, 16, 16)
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("the PCI IDs of %s cannot be read", bdf)
	}
	if len(table) < vfctImageOffset+4 {
		return nil, errors.New("the firmware's VFCT table is too short")
	}
	le := binary.LittleEndian
	for off := int(le.Uint32(table[vfctImageOffset:])); off+vfctImageHeader <= len(table); {
		hdr := table[off : off+vfctImageHeader]
		size := int(le.Uint32(hdr[24:]))
		start, end := off+vfctImageHeader, off+vfctImageHeader+size
		if size <= 0 || end > len(table) {
			break
		}
		if le.Uint32(hdr[0:]) == bus && le.Uint32(hdr[4:]) == dev && le.Uint32(hdr[8:]) == fn &&
			uint64(le.Uint16(hdr[12:])) == vendor && uint64(le.Uint16(hdr[14:])) == device {
			image := table[start:end]
			if len(image) < 2 || image[0] != 0x55 || image[1] != 0xAA {
				return nil, fmt.Errorf("the firmware's VFCT image for %s is not a VBIOS", bdf)
			}
			return append([]byte(nil), image...), nil
		}
		off = end
	}
	return nil, fmt.Errorf("the firmware's VFCT table holds no VBIOS for %s (%s)", bdf, d.ID())
}

// splitBDF reads "0000:c5:00.0" as bus 0xc5, device 0, function 0.
func splitBDF(bdf string) (bus, dev, fn uint32, ok bool) {
	parts := strings.Split(bdf, ":")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	df := strings.Split(parts[2], ".")
	if len(df) != 2 {
		return 0, 0, 0, false
	}
	b, e1 := strconv.ParseUint(parts[1], 16, 8)
	d, e2 := strconv.ParseUint(df[0], 16, 8)
	f, e3 := strconv.ParseUint(df[1], 10, 8)
	if e1 != nil || e2 != nil || e3 != nil {
		return 0, 0, 0, false
	}
	return uint32(b), uint32(d), uint32(f), true
}

// ROMPath is where the runtime keeps the VBIOS it hands QEMU for one GPU.
func ROMPath(dataDir, bdf string) string {
	return path.Join(dataDir, "vbios", strings.ReplaceAll(bdf, ":", "-")+".rom")
}

// IntegratedGPU reports whether a GPU is the processor's own (pcidev.Integrated).
func IntegratedGPU(h Host, bdf string) bool {
	return pcidev.Integrated(h, pcidev.Read(h, bdf))
}

// writeROMs writes, for each of the GPUs that is the processor's own, the
// VBIOS from the firmware's VFCT table, and returns the files by PCI address:
// what QEMU is given as each one's ROM. A card's own ROM is left to it.
func writeROMs(h Host, dataDir string, gpus []string) (map[string]string, error) {
	var roms map[string]string
	for _, gpu := range gpus {
		if !IntegratedGPU(h, gpu) {
			continue
		}
		image, err := VFCTImage(h, gpu)
		if err != nil {
			return nil, fmt.Errorf("GPU %s: %w", gpu, err)
		}
		p := ROMPath(dataDir, gpu)
		if err := h.MkdirAll(path.Dir(p), 0700); err != nil {
			return nil, err
		}
		if err := h.WriteFile(p, image, 0600); err != nil {
			return nil, fmt.Errorf("write the VBIOS of %s: %w", gpu, err)
		}
		if roms == nil {
			roms = map[string]string{}
		}
		roms[gpu] = p
	}
	return roms, nil
}
