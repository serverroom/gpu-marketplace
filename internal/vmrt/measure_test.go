package vmrt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

const (
	smiCoreCmd  = "nvidia-smi --query-gpu=pci.bus_id,driver_version,serial,uuid,vbios_version"
	smiExtraCmd = "nvidia-smi --query-gpu=pci.bus_id,inforom.img,"
	testROM     = "/sys/bus/pci/devices/" + testGPU + "/rom"
	testVBIOS   = "92.00.36.00.01"
)

// smartLog is an NVMe SMART log page with these figures.
func smartLog(used, spare, warning byte, unitsWritten, mediaErrors uint64) []byte {
	log := make([]byte, 512)
	log[0], log[3], log[5] = warning, spare, used
	binary.LittleEndian.PutUint64(log[48:], unitsWritten)
	binary.LittleEndian.PutUint64(log[160:], mediaErrors)
	return log
}

// blockStat is a block device's stat file with this many sectors written.
func blockStat(sectorsWritten int64) []byte {
	return []byte(fmt.Sprintf("    4100      0   88000    300    9000      0 %d   7000      0   6000   7300\n", sectorsWritten))
}

func romImage(fill byte) []byte {
	rom := make([]byte, 4096)
	rom[0], rom[1] = 0x55, 0xaa
	for i := 2; i < len(rom); i++ {
		rom[i] = fill
	}
	return rom
}

// measurable gives a host everything a reading looks at: nvidia-smi answering
// for the GPU, its ROM, the rentals' storage on a partition of an NVMe drive
// with a SMART log, and the rental's mapping as dm-3.
func measurable(h *fakehost.Host) *fakehost.Host {
	h.Outputs[smiCoreCmd] = "00000000:01:00.0, 550.54.15, 1320921012345, GPU-6f0e2c44, " + testVBIOS + "\n"
	h.Outputs[smiExtraCmd] = "00000000:01:00.0, G001.0000.03.03, 2.0, 6.16, N/A, All On, All On, Enabled, Enabled, 0, 0, 0, No, 250.00, 250.00\n"
	h.Files[testROM] = romImage(0x11)
	h.Outputs["findmnt -n -o SOURCE --target"] = "/dev/nvme0n1p2\n"
	h.Links["/sys/class/block/nvme0n1p2"] = "../../devices/pci0000:00/0000:00:1d.0/0000:3d:00.0/nvme/nvme0/nvme0n1/nvme0n1p2"
	h.Files["/sys/class/block/nvme0n1p2/partition"] = []byte("2\n")
	h.Files["/sys/block/nvme0n1/device/model"] = []byte("Samsung SSD 990 PRO 2TB   \n")
	h.Files["/sys/block/nvme0n1/stat"] = blockStat(1_000_000)
	h.Smart["/dev/nvme0n1"] = smartLog(3, 100, 0, 2_000_000, 0)
	h.Outputs["dmsetup info -c --noheadings -o blkdevname"] = "dm-3\n"
	h.Files["/sys/block/dm-3/stat"] = blockStat(20_000)
	return h
}

func oneRecord(t *testing.T, h *fakehost.Host) RentalRecord {
	t.Helper()
	records := LoadRecords(h, dataDir)
	if len(records) != 1 {
		t.Fatalf("want one kept record, got %d: %+v", len(records), records)
	}
	return records[0]
}

func TestARentalLeavesWhatTheMachineReadBeforeAndAfter(t *testing.T) {
	h := measurable(newHost())
	// While the renter has it: 3 GiB go to the rental's disk, 5 GiB to the drive in all.
	h.OnRun["systemctl stop gpu-rental"] = func(h *fakehost.Host, cmd string) {
		h.SetFile("/sys/block/dm-3/stat", blockStat(20_000+3<<21))
		h.SetFile("/sys/block/nvme0n1/stat", blockStat(1_000_000+5<<21))
		h.Smart["/dev/nvme0n1"] = smartLog(4, 100, 0, 2_000_000+20_972, 0)
	}
	rt, _ := newRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The vendor tool is asked while the GPU is still on its driver; the ROM is
	// read once the GPU is held for the VM, before it boots, and disabled again.
	before(t, h, "run "+smiCoreCmd, "write /sys/bus/pci/drivers/nvidia/unbind")
	before(t, h, "write /sys/bus/pci/drivers_probe", "write "+testROM+"=1")
	before(t, h, "write "+testROM+"=1", "write "+testROM+"=0")
	before(t, h, "write "+testROM+"=0", "run systemd-run")
	st, _ := LoadState(h, dataDir)
	if st == nil || st.Measure == nil || !st.Measure.Delivered || st.Measure.Before == nil || st.Measure.DiskBase == nil {
		t.Fatalf("the reading from before the rental is not in its state: %+v", st)
	}

	if res := rt.Stop(); !res.Clean() {
		t.Fatalf("teardown not clean: %+v", res)
	}
	rec := oneRecord(t, h)
	if rec.RentalID != "R1" || rec.Mode != "vm" || rec.StartedAt == 0 || rec.EndedAt < rec.StartedAt {
		t.Errorf("record: %+v", rec)
	}
	if rec.WrittenBytes == nil || *rec.WrittenBytes != 3<<30 {
		t.Errorf("the rental wrote 3 GiB to its disk, recorded %v", rec.WrittenBytes)
	}
	if rec.Before == nil || rec.After == nil || len(rec.Before.GPUs) != 1 || len(rec.After.GPUs) != 1 {
		t.Fatalf("readings: %+v", rec)
	}
	g := rec.Before.GPUs[0]
	if g.BDF != testGPU || g.VBIOS != testVBIOS || g.DriverVersion != "550.54.15" || len(g.ID) != 16 ||
		g.InfoROM["img"] != "G001.0000.03.03" || g.InfoROM["oem"] != "2.0" || g.InfoROM["ecc"] != "6.16" || g.InfoROM["pwr"] != "" ||
		g.GOM != "All On" || g.ECC != "Enabled" || g.ECCPending != "Enabled" || g.RetiredPending != "No" ||
		g.ECCUncorrected == nil || *g.ECCUncorrected != 0 || g.PowerMaxW == nil || *g.PowerMaxW != 250 {
		t.Errorf("GPU before: %+v", g)
	}
	// A serial number never leaves the machine.
	if data, _ := h.ReadFile(RecordsPath(dataDir)); strings.Contains(string(data), "1320921012345") || strings.Contains(string(data), "GPU-6f0e2c44") {
		t.Error("the record holds the card's serial number or UUID")
	}
	for name, m := range map[string]*Measurement{"before": rec.Before, "after": rec.After} {
		if rom := m.GPUs[0].ROM; rom == nil || rom.Bytes != 4096 || len(rom.SHA256) != 64 || rom.Shadow {
			t.Errorf("ROM %s: %+v", name, rom)
		}
		if len(m.Drives) != 1 || m.Drives[0].Name != "nvme0n1" || m.Drives[0].Model != "Samsung SSD 990 PRO 2TB" {
			t.Fatalf("drives %s: %+v", name, m.Drives)
		}
	}
	b, a := rec.Before.Drives[0], rec.After.Drives[0]
	if *b.PercentUsed != 3 || *a.PercentUsed != 4 || *b.SparePct != 100 || *b.LifeWrittenBytes != 2_000_000*512_000 ||
		*a.LifeWrittenBytes-*b.LifeWrittenBytes != 20_972*512_000 || *a.StatWrittenBytes-*b.StatWrittenBytes != 5<<30 {
		t.Errorf("drive before %+v after %+v", b, a)
	}
	// Wear is in the figures; it is not a difference.
	if len(rec.Changed) != 0 || len(rec.Before.Notes) != 0 || len(rec.After.Notes) != 0 {
		t.Errorf("changed %v, notes %v %v", rec.Changed, rec.Before.Notes, rec.After.Notes)
	}
	if got := rt.Records(); len(got) != 1 || got[0].RentalID != "R1" {
		t.Errorf("Records: %+v", got)
	}
	summary := strings.Join(rec.Summary(), "\n")
	for _, want := range []string{"it wrote 3.2 GB to its disk", "drive nvme0n1: 3% of its rated life used before, 4% after", "the GPU came back as it went"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary is missing %q:\n%s", want, summary)
		}
	}
}

// What a rental changed is said, and nothing more: the machine is not held back.
func TestAChangedGPUIsReportedAndTheMachineStaysRentable(t *testing.T) {
	h := measurable(newHost())
	h.OnRun["systemctl stop gpu-rental"] = func(h *fakehost.Host, cmd string) {
		h.SetOutput(smiCoreCmd, "00000000:01:00.0, 550.54.15, 1320921012345, GPU-6f0e2c44, 92.00.99.00.07\n")
		h.SetOutput(smiExtraCmd, "00000000:01:00.0, G001.0000.03.03, 2.0, 6.16, N/A, All On, All On, Enabled, Disabled, 2, 0, 1, No, 250.00, 300.00\n")
		h.SetFile(testROM, romImage(0x22))
	}
	rt, _ := newRuntime(h, func([]BoundDevice) bool { return true })
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res := rt.Stop(); !res.Clean() {
		t.Fatalf("a changed reading must not hold the machine back: %+v", res)
	}
	if rt.Present() {
		t.Error("state left behind")
	}
	changed := strings.Join(oneRecord(t, h).Changed, "\n")
	for _, want := range []string{
		"the firmware (VBIOS) version of GPU " + testGPU + " changed during the rental (" + testVBIOS + ", now 92.00.99.00.07)",
		"the ROM of GPU " + testGPU + " changed during the rental",
		"the ECC mode set for the next restart of GPU " + testGPU + " changed during the rental (Enabled, now Disabled)",
		"the maximum power limit of GPU " + testGPU + " changed during the rental (250 W, now 300 W)",
		"GPU " + testGPU + " logged 2 new uncorrectable memory errors during the rental (2 in its life)",
		"GPU " + testGPU + " logged 1 new memory pages retired after double-bit errors during the rental (1 in its life)",
	} {
		if !strings.Contains(changed, want) {
			t.Errorf("missing %q in:\n%s", want, changed)
		}
	}
	if n := len(oneRecord(t, h).Changed); n != 6 {
		t.Errorf("want 6 differences, got %d:\n%s", n, changed)
	}
}

// A test boot is the agent's own, and a rental that never reached its renter
// did nothing to the machine: neither leaves a record.
func TestOnlyARentalTheRenterHadIsRecorded(t *testing.T) {
	h := measurable(newHost())
	rt, _ := newRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t), Probes: []string{"1.1.1.1:53"}, NoWait: true}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st, _ := LoadState(h, dataDir); st == nil || st.Measure != nil {
		t.Errorf("a test boot was measured: %+v", st)
	}
	rt.Stop()
	if h.Ran("run "+smiCoreCmd) || h.Ran("write "+testROM) || h.Ran("smart ") {
		t.Error("a test boot read the machine")
	}

	h.Fail["systemd-run"] = errors.New("qemu: could not open vfio device")
	if err := rt.Start(StartOptions{ID: "R2", Pubkey: key(t)}); err == nil {
		t.Fatal("Start succeeded although the VM did not boot")
	}
	if records := LoadRecords(h, dataDir); len(records) != 0 {
		t.Errorf("records for rentals nobody had: %+v", records)
	}
}

// Nothing a reading cannot do fails a rental: it says so and goes on.
func TestAMachineThatCannotBeReadIsStillRented(t *testing.T) {
	h := newHost()
	h.Fail["nvidia-smi --query-gpu=pci.bus_id,driver_version"] = errors.New("NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver")
	h.Fail["findmnt"] = errors.New("findmnt: not found")
	h.Fail["dmsetup"] = errors.New("dmsetup: not found")
	h.Fail["write "+testROM] = errors.New("permission denied")
	h.Files[testROM] = romImage(0x11)
	rt, _ := newRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res := rt.Stop(); !res.Clean() {
		t.Fatalf("teardown not clean: %+v", res)
	}
	rec := oneRecord(t, h)
	if rec.WrittenBytes != nil || len(rec.Changed) != 0 {
		t.Errorf("record: %+v", rec)
	}
	notes := strings.Join(rec.Before.Notes, "\n")
	for _, want := range []string{"nvidia-smi did not answer", "the drive under " + dataDir, "the ROM of GPU " + testGPU + " could not be opened"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes are missing %q:\n%s", want, notes)
		}
	}
}

// A teardown that does not verify is run again later; the rental keeps the
// record of its first one, which read the most.
func TestARentalTornDownTwiceKeepsOneRecord(t *testing.T) {
	h := measurable(newHost())
	clean := false
	rt, _ := newRuntime(h, func([]BoundDevice) bool { return clean })
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res := rt.Stop(); res.Clean() || !rt.Dirty() {
		t.Fatal("the first teardown should have left the machine dirty")
	}
	first := oneRecord(t, h)
	clean = true
	if res := rt.Stop(); !res.Clean() {
		t.Fatalf("second teardown: %+v", res)
	}
	if again := oneRecord(t, h); again.EndedAt != first.EndedAt || again.WrittenBytes == nil {
		t.Errorf("the record was replaced: %+v", again)
	}
}

func TestARentalsRecordLeavesTheMachineThreeYearsAfterItEnded(t *testing.T) {
	h := fakehost.New()
	now := time.Now().Unix()
	year := int64(365 * 24 * 3600)
	old := []RentalRecord{
		{RentalID: "four-years", EndedAt: now - 4*year},
		{RentalID: "one-year", EndedAt: now - year},
		{RentalID: "no-end"},
	}
	if err := writeRecords(h, dataDir, old); err != nil {
		t.Fatal(err)
	}
	// An idle machine: nothing is rented, something only reads the records.
	got := LoadRecords(h, dataDir)
	if len(got) != 2 || got[0].RentalID != "one-year" || got[1].RentalID != "no-end" {
		t.Fatalf("after three years a record should be gone and the others kept: %+v", got)
	}
	data, err := h.ReadFile(RecordsPath(dataDir))
	if err != nil || strings.Contains(string(data), "four-years") {
		t.Errorf("the old record is still in the file (%v): %s", err, data)
	}
	// And a new rental does not bring it back.
	if err := keepRecord(h, dataDir, RentalRecord{RentalID: "today", EndedAt: now}); err != nil {
		t.Fatal(err)
	}
	if got := LoadRecords(h, dataDir); len(got) != 3 || got[2].RentalID != "today" {
		t.Errorf("records after a new rental: %+v", got)
	}
}

func TestOnlyTheLastRentalsAreKept(t *testing.T) {
	h := fakehost.New()
	for i := 0; i < maxRecords+5; i++ {
		if err := keepRecord(h, dataDir, RentalRecord{RentalID: fmt.Sprintf("R%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	all := LoadRecords(h, dataDir)
	if len(all) != maxRecords || all[0].RentalID != "R5" || all[len(all)-1].RentalID != fmt.Sprintf("R%d", maxRecords+4) {
		t.Errorf("kept %d, first %s", len(all), all[0].RentalID)
	}
	if latest := LatestRecords(h, dataDir, ReportedRecords); len(latest) != ReportedRecords || latest[ReportedRecords-1].RentalID != all[len(all)-1].RentalID {
		t.Errorf("latest: %+v", latest)
	}
}

// An nvidia-smi that does not know one of the newer fields refuses the whole
// question; the fields are then asked one at a time.
func TestFieldsAnOlderToolDoesNotKnowAreAskedOneByOne(t *testing.T) {
	h := newHost()
	h.Outputs[smiCoreCmd] = "00000000:01:00.0, 470.82, 0321, GPU-1, 86.00.4d.00.04\n"
	h.Fail[smiExtraCmd] = errors.New(`Field "gom.current" is not a valid field to query.`)
	h.Outputs["nvidia-smi --query-gpu=pci.bus_id,inforom.img --format"] = "00000000:01:00.0, 80.0\n"
	h.Outputs["nvidia-smi --query-gpu=pci.bus_id,ecc.mode.current --format"] = "00000000:01:00.0, [N/A]\n"
	h.Fail["nvidia-smi --query-gpu=pci.bus_id,gom.current --format"] = errors.New("not a valid field")
	h.Outputs["nvidia-smi --query-gpu=pci.bus_id,power.max_limit --format"] = "00000000:01:00.0, 350.00\n"
	gpus, notes := readGPUs(h, []string{testGPU})
	if len(notes) != 0 || len(gpus) != 1 {
		t.Fatalf("%+v %v", gpus, notes)
	}
	g := gpus[0]
	if g.VBIOS != "86.00.4d.00.04" || g.InfoROM["img"] != "80.0" || g.ECC != "" || g.GOM != "" || g.PowerMaxW == nil || *g.PowerMaxW != 350 {
		t.Errorf("%+v", g)
	}
}

// nvidia-smi is not asked about a GPU its driver does not hold, and a driver
// that publishes its firmware version in sysfs (amdgpu) is read there.
func TestAGPUOnAnotherDriver(t *testing.T) {
	h := fakehost.New()
	h.PCI(testGPU, "amdgpu", "0x030000", testGPU)
	h.Files[devPath(testGPU)+"/vbios_version"] = []byte("113-D7020100-102\n")
	gpus, notes := readGPUs(h, []string{testGPU})
	if h.Ran("run nvidia-smi") || len(notes) != 0 || len(gpus) != 1 || gpus[0].VBIOS != "113-D7020100-102" {
		t.Errorf("%+v %v", gpus, notes)
	}
}

// A GPU on its own driver keeps its ROM to itself: it is read only while the
// GPU is held for a VM.
func TestTheROMIsReadOnlyWhileTheGPUIsHeldForAVM(t *testing.T) {
	h := newHost()
	h.Files[testROM] = romImage(0x11)
	if roms, notes := ReadROMs(h, []string{testGPU}); len(roms) != 0 || len(notes) != 0 || h.Ran("write "+testROM) {
		t.Errorf("the ROM of a GPU on its driver was touched: %v %v", roms, notes)
	}
	h.SetLink(devPath(testGPU)+"/driver", "../../../bus/pci/drivers/vfio-pci")
	h.Files[devPath(testGPU)+"/boot_vga"] = []byte("1\n")
	roms, notes := ReadROMs(h, []string{testGPU})
	if len(notes) != 0 || roms[testGPU] == nil || !roms[testGPU].Shadow {
		t.Fatalf("%v %v", roms, notes)
	}
	// A card with no ROM for the kernel to read says so, rather than leaving a blank.
	h.DeleteFile(testROM)
	if roms, notes := ReadROMs(h, []string{testGPU}); len(roms) != 0 || len(notes) != 1 || !strings.Contains(notes[0], "has no ROM the kernel can read") {
		t.Errorf("%v %v", roms, notes)
	}
	h.Files[testROM] = romImage(0x11)
	// Not an image: said, and not hashed.
	h.Files[testROM] = []byte{0xff, 0xff, 0xff, 0xff}
	if roms, notes := ReadROMs(h, []string{testGPU}); len(roms) != 0 || len(notes) != 1 || !strings.Contains(notes[0], "did not give a ROM image") {
		t.Errorf("%v %v", roms, notes)
	}
}

func TestDrivesAreFollowedThroughVolumesToTheWholeDisks(t *testing.T) {
	h := fakehost.New()
	h.Outputs["findmnt -n -o SOURCE --target"] = "/dev/mapper/vg-rentals\n"
	h.Links["/dev/mapper/vg-rentals"] = "../dm-0"
	h.Files["/sys/class/block/dm-0/slaves/nvme1n1p3"] = nil
	h.Files["/sys/class/block/dm-0/slaves/sda1"] = nil
	h.Files["/sys/class/block/dm-0/slaves/loop3"] = nil
	h.Links["/sys/class/block/nvme1n1p3"] = "../../devices/pci0000:00/0000:00:01.1/0000:02:00.0/nvme/nvme1/nvme1n1/nvme1n1p3"
	h.Files["/sys/class/block/nvme1n1p3/partition"] = []byte("3\n")
	h.Links["/sys/class/block/sda1"] = "../../devices/pci0000:00/0000:00:17.0/ata1/host0/target0:0:0/0:0:0:0/block/sda/sda1"
	h.Files["/sys/class/block/sda1/partition"] = []byte("1\n")
	h.Files["/sys/block/sda/stat"] = blockStat(4096)
	h.Smart["/dev/nvme1n1"] = smartLog(41, 97, 4, 123, 7)
	drives, notes := readDrives(h, "/srv/rentals")
	if len(notes) != 0 || len(drives) != 2 || drives[0].Name != "nvme1n1" || drives[1].Name != "sda" {
		t.Fatalf("%+v %v", drives, notes)
	}
	n, s := drives[0], drives[1]
	if *n.PercentUsed != 41 || *n.SparePct != 97 || n.CriticalWarning != 4 || *n.MediaErrors != 7 || *n.LifeWrittenBytes != 123*512_000 {
		t.Errorf("nvme: %+v", n)
	}
	// A drive with no SMART log the agent can read still says what was written to it.
	if s.PercentUsed != nil || s.StatWrittenBytes == nil || *s.StatWrittenBytes != 4096*512 || h.Ran("smart /dev/sda") {
		t.Errorf("sata: %+v", s)
	}

	// A filesystem with no device under it (ZFS, a network mount) is said, not guessed.
	h.Outputs["findmnt -n -o SOURCE --target"] = "tank/rentals\n"
	if drives, notes := readDrives(h, "/srv/rentals"); len(drives) != 0 || len(notes) != 1 {
		t.Errorf("%+v %v", drives, notes)
	}
	// A btrfs subvolume names its device with the subvolume after it.
	h.Outputs["findmnt -n -o SOURCE --target"] = "/dev/sda1[/@rentals]\n"
	if drives, _ := readDrives(h, "/srv/rentals"); len(drives) != 1 || drives[0].Name != "sda" {
		t.Errorf("%+v", drives)
	}
}

func TestMapperWrittenFollowsTheMappingToItsDevice(t *testing.T) {
	h := fakehost.New()
	h.Links["/dev/mapper/gpu-rental-R1"] = "../dm-7"
	h.Files["/sys/block/dm-7/stat"] = blockStat(2048)
	if w, ok := MapperWritten(h, "/dev/mapper/gpu-rental-R1"); !ok || w != 2048*512 || h.Ran("run dmsetup") {
		t.Errorf("%d %v", w, ok)
	}
	if _, ok := MapperWritten(h, "/dev/mapper/gone"); ok {
		t.Error("a mapping that is not there was read")
	}
	if _, ok := MapperWritten(h, ""); ok {
		t.Error("no mapping was read")
	}
}

func TestCompareLeavesAloneWhatWasReadOnlyOnce(t *testing.T) {
	one, two := int64(1), int64(2)
	before := &Measurement{GPUs: []GPUReading{{BDF: testGPU, ID: "aa", VBIOS: "1", ECCUncorrected: &two,
		ROM: &ROMReading{SHA256: strings.Repeat("a", 64), Shadow: true}}},
		Drives: []DriveReading{{Name: "nvme0n1", MediaErrors: &one}}}
	after := &Measurement{GPUs: []GPUReading{{BDF: testGPU, ECCUncorrected: &one,
		ROM: &ROMReading{SHA256: strings.Repeat("b", 64), Shadow: true}}},
		Drives: []DriveReading{{Name: "nvme0n1", MediaErrors: &two, CriticalWarning: 1}}}
	got := Compare(before, after)
	// A reading with no vendor tool says nothing of the firmware; a boot
	// display's ROM is the firmware's copy; a counter that went down is not new.
	if len(got) != 2 || !strings.Contains(got[0], "nvme0n1 logged 1 new media errors") || !strings.Contains(got[1], "nvme0n1 raised a SMART warning") {
		t.Errorf("%v", got)
	}
	if Compare(nil, after) != nil || Compare(before, nil) != nil {
		t.Error("a missing reading was compared")
	}
	after.GPUs[0].ID = "bb"
	if got := Compare(before, after); len(got) != 3 || !strings.Contains(got[0], "came back with another serial number") {
		t.Errorf("%v", got)
	}
}

// A machine that does not answer is not waited for.
func TestASlowMachineIsNotWaitedFor(t *testing.T) {
	old := MeasureTimeout
	MeasureTimeout = 20 * time.Millisecond
	defer func() { MeasureTimeout = old }()
	h := fakehost.New()
	h.OnRun["findmnt"] = func(*fakehost.Host, string) { time.Sleep(400 * time.Millisecond) }
	start := time.Now()
	m := MeasureMachine(h, "/srv/rentals", nil)
	if time.Since(start) > 300*time.Millisecond || len(m.Notes) != 1 || !strings.Contains(m.Notes[0], "did not answer") {
		t.Errorf("after %v: %+v", time.Since(start), m)
	}
}

func TestBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 999: "999 B", 1500: "1.5 kB", 3 << 30: "3.2 GB", 2_400_000_000_000: "2.4 TB"} {
		if got := Bytes(n); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestAContainerRentalLeavesItsRecordToo(t *testing.T) {
	h := measurable(newContainerHost())
	h.OnRun["podman stop "] = func(h *fakehost.Host, cmd string) {
		h.SetFile("/sys/block/dm-3/stat", blockStat(20_000+1<<21))
	}
	rt, _ := newContainerRuntime(h, func([]BoundDevice) bool { return true })
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The volume is read after its filesystem is made, so that is not the renter's.
	before(t, h, "run mkfs.ext4", "run dmsetup info")
	before(t, h, "run "+smiCoreCmd, "run podman run")
	if res := rt.Stop(); !res.Clean() {
		t.Fatalf("teardown not clean: %+v", res)
	}
	before(t, h, "run umount", "run cryptsetup close")
	rec := oneRecord(t, h)
	if rec.Mode != "container" || rec.WrittenBytes == nil || *rec.WrittenBytes != 1<<30 || len(rec.Changed) != 0 {
		t.Errorf("record: %+v", rec)
	}
	// The GPU never leaves its driver in a container rental: its ROM is not read.
	if h.Ran("write "+testROM) || rec.Before.GPUs[0].ROM != nil || rec.After.GPUs[0].VBIOS != testVBIOS {
		t.Errorf("GPU readings: %+v %+v", rec.Before.GPUs, rec.After.GPUs)
	}
	if got := rt.Records(); len(got) != 1 {
		t.Errorf("Records: %+v", got)
	}
}
