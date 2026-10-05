//go:build linux

package vmrt

import (
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// nvmeAdminCmd is the kernel's struct nvme_passthru_cmd (linux/nvme_ioctl.h).
type nvmeAdminCmd struct {
	Opcode      uint8
	Flags       uint8
	Rsvd1       uint16
	Nsid        uint32
	Cdw2        uint32
	Cdw3        uint32
	Metadata    uint64
	Addr        uint64
	MetadataLen uint32
	DataLen     uint32
	Cdw10       uint32
	Cdw11       uint32
	Cdw12       uint32
	Cdw13       uint32
	Cdw14       uint32
	Cdw15       uint32
	TimeoutMs   uint32
	Result      uint32
}

const (
	// nvmeIoctlAdminCmd is NVME_IOCTL_ADMIN_CMD: _IOWR('N', 0x41, struct nvme_passthru_cmd).
	nvmeIoctlAdminCmd = 0xC0484E41
	nvmeGetLogPage    = 0x02 // the admin command
	nvmeSmartLog      = 0x02 // the log: SMART / Health Information
	nvmeSmartLogBytes = 512
)

// NVMeSmartLog reads an NVMe drive's SMART / Health Information log page with
// the Get Log Page admin command, which only reads: what smartctl and nvme-cli
// send for the same figures, without either having to be installed.
func (OSHost) NVMeSmartLog(dev string) ([]byte, error) {
	f, err := os.OpenFile(dev, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, nvmeSmartLogBytes)
	cmd := nvmeAdminCmd{
		Opcode:    nvmeGetLogPage,
		Nsid:      0xFFFFFFFF,
		Addr:      uint64(uintptr(unsafe.Pointer(&buf[0]))),
		DataLen:   nvmeSmartLogBytes,
		Cdw10:     nvmeSmartLog | (nvmeSmartLogBytes/4-1)<<16,
		TimeoutMs: 5000,
	}
	status, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), nvmeIoctlAdminCmd, uintptr(unsafe.Pointer(&cmd)))
	runtime.KeepAlive(buf)
	if errno != 0 {
		return nil, fmt.Errorf("%s: %w", dev, errno)
	}
	if status != 0 {
		return nil, fmt.Errorf("%s: the drive answered NVMe status %#x", dev, status)
	}
	return buf, nil
}
