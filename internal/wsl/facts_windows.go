//go:build windows

package wsl

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// Facts reads this Windows machine and the agent's WSL 2 environment on it
// (provisioner.ReadWindows).
func Facts() provisioner.WinFacts {
	var f provisioner.WinFacts
	v := windows.RtlGetVersion()
	f.Build = int(v.BuildNumber)
	f.Server = v.ProductType != 1 // VER_NT_WORKSTATION
	f.Name = productName(f.Build, f.Server)
	var ms memoryStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms))); r != 0 {
		f.TotalMemMB = int(ms.TotalPhys >> 20)
	}
	f.CPUs = runtime.NumCPU()
	f.WSLInstalled = exists(Exe())
	f.RestartPending = restartPending()
	f.GPUs = nvidiaGPUs()
	f.LANIP, f.Gateway = lanIP(), gateway()

	dir := ""
	if accountExists() {
		if s, err := openSession(); err == nil {
			if base, ok := s.registered(); ok {
				f.Ready = true
				dir = base
			}
		}
	}
	if dir == "" {
		dir = chooseDir()
	}
	f.StorageDrive = filepath.VolumeName(dir)
	f.StorageFreeGB = freeGB(dir)
	f.Machine = vmrt.ExecHost{}
	if f.Ready {
		f.Machine = vmrt.ExecHost{Exec: Exec, Dial: Dial, Keep: Keep}
		f.Dial = Dial
	}
	return f
}

// MemAvailableMB is the memory Windows has free now.
func MemAvailableMB() int {
	var ms memoryStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms))); r == 0 {
		return -1
	}
	return int(ms.AvailPhys >> 20)
}

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetTickCount64       = kernel32.NewProc("GetTickCount64")
)

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// productName is "Windows 11 Pro", "Windows Server 2022 Standard": the
// registry's ProductName, which still says "Windows 10" on Windows 11.
func productName(build int, server bool) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return "Windows"
	}
	defer k.Close()
	name, _, _ := k.GetStringValue("ProductName")
	if !server && build >= 22000 {
		name = strings.Replace(name, "Windows 10", "Windows 11", 1)
	}
	return name
}

// hidden runs a Windows tool with no window and a time limit.
func hidden(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.Output()
	return string(out), err
}

// nvidiaGPUs are the NVIDIA GPUs the Windows driver has, from nvidia-smi.exe.
func nvidiaGPUs() []provisioner.WinGPU {
	smi, err := exec.LookPath("nvidia-smi.exe")
	if err != nil {
		smi = filepath.Join(os.Getenv("ProgramFiles"), "NVIDIA Corporation", "NVSMI", "nvidia-smi.exe")
		if !exists(smi) {
			return nil
		}
	}
	out, err := hidden(20*time.Second, smi, "--query-gpu=pci.bus_id,name,uuid,pci.device_id,memory.total,driver_version", "--format=csv,noheader,nounits")
	if err != nil {
		return nil
	}
	return parseNVIDIA(out)
}

// lanIP is the address Windows reaches the internet from (no packet is sent).
func lanIP() string {
	c, err := net.Dial("udp4", "1.1.1.1:53")
	if err != nil {
		return ""
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}

func gateway() string {
	out, err := hidden(10*time.Second, "route.exe", "print", "-4", "0.0.0.0")
	if err != nil {
		return ""
	}
	return parseGateway(out)
}

func freeGB(dir string) int {
	root := filepath.VolumeName(dir) + `\`
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return 0
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0
	}
	return int(free >> 30)
}

// chooseDir is where the distribution's disk goes: under ProgramData on the
// system drive while it has room for a rental (40 GB free: 20 for the disk, 20
// kept), else on the fixed drive with the most room.
func chooseDir() string {
	sys := filepath.Join(os.Getenv("ProgramData"), "gpu-agent", "wsl")
	if freeGB(sys) >= 20+provisioner.WindowsKeepGB {
		return sys
	}
	best, bestFree := sys, freeGB(sys)
	mask, _ := windows.GetLogicalDrives()
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, _ := windows.UTF16PtrFromString(root)
		if windows.GetDriveType(p) != windows.DRIVE_FIXED {
			continue
		}
		if free := freeGB(root); free > bestFree {
			best, bestFree = filepath.Join(root, "gpu-agent", "wsl"), free
		}
	}
	return best
}

// restartPath records that the setup turned on Windows features that need a
// restart, with the boot it happened in.
func restartPath() string { return filepath.Join(config.ConfigDir(), "wsl-restart.json") }

func bootTime() time.Time {
	ms, _, _ := procGetTickCount64.Call()
	return time.Now().Add(-time.Duration(ms) * time.Millisecond)
}

func markRestart() {
	data, _ := json.Marshal(map[string]int64{"boot": bootTime().Unix()})
	_ = os.WriteFile(restartPath(), data, 0600)
}

// restartPending: the marker is from this boot (Windows has not restarted
// since the features were turned on).
func restartPending() bool {
	data, err := os.ReadFile(restartPath())
	if err != nil {
		return false
	}
	var rec map[string]int64
	if json.Unmarshal(data, &rec) != nil {
		return false
	}
	d := bootTime().Unix() - rec["boot"]
	if d < -120 || d > 120 {
		_ = os.Remove(restartPath())
		return false
	}
	return true
}
