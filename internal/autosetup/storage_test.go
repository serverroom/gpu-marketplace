package autosetup

import (
	"context"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// smallRoot is machine with the agent's default place on a 7 GB system
// partition and a 460 GB data partition beside it -- the layout of a board or
// server whose big disk is mounted somewhere of the host's own choosing.
func smallRoot(t *testing.T) *fakehost.Host {
	h := machine(t)
	h.Outputs["df --output=avail"] = " Avail\n  1G\n"
	h.Outputs["df -B1G --output=source,target,fstype,avail "+dataDir] = "Filesystem Mounted on Type Avail\n/dev/sda2 / ext4 1G\n"
	h.Outputs["df -B1G --output=source,target,fstype,avail"] = "Filesystem Mounted on Type Avail\n/dev/sda2 / ext4 1G\n" +
		"tmpfs /run tmpfs 2G\n/dev/sda1 /boot/efi vfat 1G\n/dev/sdb1 /data ext4 460G\n"
	h.Links["/sys/block/sdb"] = "../devices/pci0000:00/0000:00:17.0/ata2/host1/target1:0:0/1:0:0:0/block/sdb"
	return h
}

// A machine whose default place is too small, with an internal disk that has
// room, sets itself up there: the move is the setup's first step, then the
// rest of it (here the test boot) runs as usual.
func TestSetupMovesTheRentalsToADiskWithRoom(t *testing.T) {
	h := smallRoot(t)
	detect := func() *provisioner.Provisioner { return provisioner.Detect(h, "linux", "amd64", dataDir, version) }
	var dir string
	for _, f := range detect().Findings() {
		if f.Kind == provisioner.ReasonStorage {
			dir = f.Dir
			if !strings.Contains(f.Text, "/data has 460 GB free, so the automatic setup keeps the rentals' disks in /data/gpu-agent") {
				t.Errorf("finding text = %q", f.Text)
			}
		}
	}
	if dir != "/data/gpu-agent" {
		t.Fatalf("storage finding dir = %q, want /data/gpu-agent", dir)
	}
	plan := PlanFor(detect().Findings(), true)
	if !plan.Eligible() || len(plan.Steps) == 0 || plan.Steps[0] != StepStorage {
		t.Fatalf("plan = %+v, want the storage move first and nothing for a person", plan)
	}

	var moved []string
	r := &Runner{Host: h, Arch: "amd64", DataDir: dataDir, Version: version, Detect: detect,
		MoveStorage: func(d string) error { moved = append(moved, d); return nil }}
	if err := r.step(context.Background(), StepStorage, Attempt{}); err != nil {
		t.Fatalf("storage step: %v", err)
	}
	if strings.Join(moved, ",") != "/data/gpu-agent" {
		t.Errorf("moved to %v, want /data/gpu-agent", moved)
	}
}

// A disk the host plugged in over USB (a backup drive) is never picked by the
// agent: it is named, with the command, for a person to decide.
func TestSetupNeverPicksAUSBDrive(t *testing.T) {
	h := smallRoot(t)
	h.Links["/sys/block/sdb"] = "../devices/pci0000:00/0000:00:14.0/usb2/2-1/2-1:1.0/host3/target3:0:0/3:0:0:0/block/sdb"
	p := provisioner.Detect(h, "linux", "amd64", dataDir, version)
	for _, f := range p.Findings() {
		if f.Kind == provisioner.ReasonStorage {
			t.Fatalf("a USB drive was picked by itself: %+v", f)
		}
	}
	plan := PlanFor(p.Findings(), true)
	if plan.Eligible() || !strings.Contains(strings.Join(plan.Human, " | "), "'sudo gpu-agent setup --data-dir /data/gpu-agent' puts the rentals' disks there") {
		t.Errorf("plan = %+v, want the command named for a person", plan)
	}
}
