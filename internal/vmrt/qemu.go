package vmrt

import (
	"fmt"
	"path/filepath"
	"strconv"
)

// Rental is where one microVM's pieces live on the host.
type Rental struct {
	ID         string   `json:"id"`
	Dir        string   `json:"dir"`
	Disk       string   `json:"disk"`        // the encrypted block device (or a qcow2 file while baking)
	DiskFormat string   `json:"disk_format"` // raw, or qcow2 while baking
	Seed       string   `json:"seed"`
	Vars       string   `json:"vars"`
	SerialLog  string   `json:"serial_log"`
	Unit       string   `json:"unit"` // the transient systemd unit QEMU runs as
	QMP        string   `json:"qmp"`
	VFIO       []string `json:"vfio"`      // every PCI function handed to the VM
	MemoryMB   int      `json:"memory_mb"` // 0 = Spec.GuestMemoryMB()
}

// NewRental lays out a rental's working directory under dataDir.
func NewRental(dataDir, id string) Rental {
	dir := filepath.Join(dataDir, "rentals", id)
	return Rental{
		ID:         id,
		Dir:        dir,
		Disk:       "/dev/mapper/" + MapperName(id),
		DiskFormat: "raw",
		Seed:       filepath.Join(dir, "seed.iso"),
		Vars:       filepath.Join(dir, "efivars.fd"),
		SerialLog:  filepath.Join(dir, "serial.log"),
		Unit:       "gpu-rental-" + id,
		QMP:        filepath.Join(dir, "qmp.sock"),
	}
}

// QEMUArgs is the complete command line for a rental microVM.
//
// What is deliberately absent matters as much as what is there: -nodefaults so
// QEMU adds no device of its own (above all no user-mode network, which would
// route the tenant through the host's own sockets and around the fence), no
// display, no monitor. The one NIC is a tap on the fenced bridge. -sandbox
// confines the QEMU process itself with seccomp, so a guest that escapes the
// device model still cannot spawn processes or gain privileges on the host.
// QEMU does not daemonize: it runs in the foreground of its own transient
// systemd unit (see Launch), which is also what lets a rental outlive a restart
// of the agent.
func QEMUArgs(s Spec, r Rental) []string {
	memory := r.MemoryMB
	if memory <= 0 {
		memory = s.GuestMemoryMB()
	}
	format := r.DiskFormat
	if format == "" {
		format = "raw"
	}
	args := []string{"-name", "gpu-rental-" + r.ID, "-nodefaults"}
	if s.Arch == "arm64" {
		args = append(args, "-machine", "virt,accel=kvm,gic-version=host")
	} else {
		args = append(args, "-machine", "q35,accel=kvm")
	}
	args = append(args,
		"-cpu", "host",
		"-smp", strconv.Itoa(s.GuestCPUs()),
		"-m", strconv.Itoa(memory),
		"-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny",
		"-drive", "if=pflash,format=raw,readonly=on,file="+s.Firmware.Code,
		"-drive", "if=pflash,format=raw,file="+r.Vars,
	)
	if s.Arch != "arm64" {
		// Datacenter GPUs have BARs far past OVMF's default 32 GB 64-bit MMIO
		// window, and a GPU whose BAR does not fit is simply absent in the guest.
		args = append(args, "-fw_cfg", "name=opt/ovmf/X-PciMmio64Mb,string=262144")
	}
	for i, dev := range r.VFIO {
		port := fmt.Sprintf("rp%d", i)
		args = append(args,
			"-device", fmt.Sprintf("pcie-root-port,id=%s,chassis=%d,slot=%d", port, i+1, i+1),
			"-device", fmt.Sprintf("vfio-pci,host=%s,bus=%s", dev, port))
	}
	args = append(args,
		"-drive", "file="+r.Disk+",if=none,id=root,format="+format+",cache=none,aio=native",
		"-device", "virtio-blk-pci,drive=root,bootindex=0",
		"-drive", "file="+r.Seed+",if=none,id=seed,format=raw,readonly=on",
		"-device", "virtio-blk-pci,drive=seed",
		"-netdev", "tap,id=net0,ifname="+Tap+",script=no,downscript=no",
		"-device", "virtio-net-pci,netdev=net0,mac="+GuestMAC,
		"-display", "none",
		"-serial", "file:"+r.SerialLog,
		"-monitor", "none",
		"-qmp", "unix:"+r.QMP+",server=on,wait=off",
	)
	return args
}

// LaunchArgs wraps the QEMU command line in `systemd-run`, so the VM runs as its
// own unit: out of the agent's cgroup (an agent restart does not kill the
// renter's VM), stoppable by name, and with its stderr in the journal.
func LaunchArgs(s Spec, r Rental) []string {
	args := []string{"--unit=" + r.Unit, "--collect", "--property=Type=exec",
		"--property=TimeoutStopSec=30", s.QEMUBinary()}
	return append(args, QEMUArgs(s, r)...)
}
