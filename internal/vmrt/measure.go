package vmrt

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// What the agent reads off the machine right before a rental and right after
// it, so its host can see what the rental did to it: how much it wrote to the
// disk, how worn the drive was before and after, and whether each rented GPU
// came back with the firmware and the settings it went in with.
//
// It only reads. Nothing here can fail a rental, delay one for long, or hold a
// machine back from renters: a figure that cannot be read is absent, with a
// note saying why, and a machine that does not answer within MeasureTimeout is
// rented and released without it. What the two readings differ in is reported
// (RentalRecord.Changed), not judged.

// MeasureTimeout bounds one reading of the machine.
var MeasureTimeout = 30 * time.Second

// Measurement is one reading of the machine.
type Measurement struct {
	At     int64          `json:"at"`
	GPUs   []GPUReading   `json:"gpus,omitempty"`
	Drives []DriveReading `json:"drives,omitempty"`
	// Notes say what could not be read, and why.
	Notes []string `json:"notes,omitempty"`
}

// GPUReading is one rented GPU as its vendor tool and its own ROM describe it.
// A figure the GPU does not report is absent.
type GPUReading struct {
	BDF string `json:"bdf"`
	// ID stands for the card's serial number and UUID without giving either
	// away: the first 16 hex digits of their SHA-256. A card that comes back
	// with another one is not the card that went, or has lost its InfoROM.
	ID            string `json:"id,omitempty"`
	DriverVersion string `json:"driver_version,omitempty"`
	VBIOS         string `json:"vbios,omitempty"`
	// InfoROM is the version of each part of the card's InfoROM: img, oem, ecc, pwr.
	InfoROM map[string]string `json:"inforom,omitempty"`
	// GOM and ECC are the card's operation mode and ECC mode; the pending value
	// is the one it would take at its next restart, which is where a setting
	// changed during a rental shows.
	GOM        string `json:"gom,omitempty"`
	GOMPending string `json:"gom_pending,omitempty"`
	ECC        string `json:"ecc,omitempty"`
	ECCPending string `json:"ecc_pending,omitempty"`
	// Lifetime counters the card keeps: uncorrectable memory errors, and memory
	// pages it has retired after single-bit and double-bit errors.
	ECCUncorrected *int64 `json:"ecc_uncorrected,omitempty"`
	RetiredSBE     *int64 `json:"retired_sbe,omitempty"`
	RetiredDBE     *int64 `json:"retired_dbe,omitempty"`
	RetiredPending string `json:"retired_pending,omitempty"`
	// The power limits the card's firmware sets, in watts.
	PowerDefaultW *float64 `json:"power_default_w,omitempty"`
	PowerMaxW     *float64 `json:"power_max_w,omitempty"`
	// ROM is the card's expansion ROM, read while a microVM rental holds it.
	ROM *ROMReading `json:"rom,omitempty"`
}

// ROMReading is a GPU's expansion ROM as the kernel reads it.
type ROMReading struct {
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
	// Shadow: the kernel serves the copy the firmware made at boot (a boot
	// display), not the chip, so it cannot show a change made since.
	Shadow bool `json:"shadow,omitempty"`
}

// DriveReading is one physical drive under the rentals' storage.
type DriveReading struct {
	Name  string `json:"name"`
	Model string `json:"model,omitempty"`
	// StatWrittenBytes is what has been written to the drive since the machine
	// started (the kernel's own count): everything on it, the host's own writes
	// included.
	StatWrittenBytes *int64 `json:"stat_written_bytes,omitempty"`
	// The drive's own SMART figures (NVMe): bytes written in its life, how much
	// of its rated endurance is used (which can pass 100), spare capacity left,
	// media errors, and its warning flags when any is raised.
	LifeWrittenBytes *int64 `json:"life_written_bytes,omitempty"`
	PercentUsed      *int   `json:"percent_used,omitempty"`
	SparePct         *int   `json:"spare_pct,omitempty"`
	MediaErrors      *int64 `json:"media_errors,omitempty"`
	CriticalWarning  int    `json:"critical_warning,omitempty"`
}

// MeasureState is what a rental's measurements need at its teardown: the
// reading from before it, what its disk had been written by the time it was
// handed over, and whether the renter ever got it (a rental that never came up
// leaves no record).
type MeasureState struct {
	Before    *Measurement `json:"before,omitempty"`
	DiskBase  *int64       `json:"disk_base,omitempty"`
	Delivered bool         `json:"delivered,omitempty"`
}

// RentalRecord is one rental's before and after, kept on the machine
// (RecordsPath) and told to the marketplace on /status.
type RentalRecord struct {
	RentalID  string `json:"rental_id"`
	Mode      string `json:"mode"` // "vm" or "container"
	StartedAt int64  `json:"started_at"`
	EndedAt   int64  `json:"ended_at"`
	// WrittenBytes is what was written to the rental's own disk between its
	// hand-over and its teardown: the renter's writes and those of the rental's
	// own system (a filesystem finishing its tables in the background, a guest
	// growing its disk at first boot). Absent when the disk was already gone --
	// a machine that restarted under the rental.
	WrittenBytes *int64       `json:"written_bytes,omitempty"`
	Before       *Measurement `json:"before,omitempty"`
	After        *Measurement `json:"after,omitempty"`
	// Changed is what differs between the two readings that a rental should
	// leave alone, one sentence each; empty when nothing does.
	Changed []string `json:"changed,omitempty"`
}

// bounded runs read and returns what it read, or ok false when it has not
// answered within MeasureTimeout. The reading is left to finish by itself.
func bounded[T any](read func() T) (out T, ok bool) {
	done := make(chan T, 1)
	go func() { done <- read() }()
	select {
	case out = <-done:
		return out, true
	case <-time.After(MeasureTimeout):
		return out, false
	}
}

// MeasureMachine reads the rented GPUs (as their vendor tool describes them,
// which needs them on their own driver) and the drives under dir. gpus may be
// empty: a machine without one, or a rental that leaves it with the host.
func MeasureMachine(h Host, dir string, gpus []string) *Measurement {
	now := time.Now().Unix()
	m, ok := bounded(func() *Measurement {
		m := &Measurement{At: now}
		var notes []string
		m.GPUs, notes = readGPUs(h, gpus)
		m.Notes = append(m.Notes, notes...)
		m.Drives, notes = readDrives(h, dir)
		m.Notes = append(m.Notes, notes...)
		return m
	})
	if !ok {
		return &Measurement{At: now, Notes: []string{fmt.Sprintf("the machine did not answer within %v, so nothing was read", MeasureTimeout)}}
	}
	return m
}

// The fields asked of nvidia-smi. Every release answers the first set; an older
// one may not know all of the second, so those are asked together and, when
// that is refused, one at a time. None of their values contains a comma.
var (
	smiCore  = []string{"pci.bus_id", "driver_version", "serial", "uuid", "vbios_version"}
	smiExtra = []string{"inforom.img", "inforom.oem", "inforom.ecc", "inforom.pwr",
		"gom.current", "gom.pending", "ecc.mode.current", "ecc.mode.pending",
		"ecc.errors.uncorrected.aggregate.total", "retired_pages.sbe", "retired_pages.dbe", "retired_pages.pending",
		"power.default_limit", "power.max_limit"}
)

// smiValue is a field's value, or "" for one the GPU does not report.
func smiValue(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "[") || strings.EqualFold(s, "N/A") {
		return ""
	}
	return s
}

// smiQuery asks nvidia-smi for fields (after the bus id) and returns each
// rented GPU's values, by PCI address.
func smiQuery(h Host, gpus, fields []string) (map[string][]string, error) {
	out, err := h.Output("nvidia-smi", "--query-gpu=pci.bus_id,"+strings.Join(fields, ","), "--format=csv,noheader,nounits")
	if err != nil {
		return nil, err
	}
	rows := map[string][]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, ",")
		if len(f) != len(fields)+1 {
			continue
		}
		bdf := rentedBDF(strings.TrimSpace(f[0]), gpus)
		if bdf == "" {
			continue
		}
		values := make([]string, len(fields))
		for i := range fields {
			values[i] = smiValue(f[i+1])
		}
		rows[bdf] = values
	}
	return rows, nil
}

// rentedBDF is the rented GPU nvidia-smi's bus id (00000000:01:00.0) names, or "".
func rentedBDF(bus string, gpus []string) string {
	for _, b := range gpus {
		if rentedBus(bus, []string{b}) {
			return b
		}
	}
	return ""
}

func optInt64(s string) *int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return nil
	}
	return &v
}

// set records one nvidia-smi field on the reading.
func (g *GPUReading) set(field, v string) {
	if v == "" {
		return
	}
	switch field {
	case "driver_version":
		g.DriverVersion = v
	case "vbios_version":
		g.VBIOS = v
	case "inforom.img", "inforom.oem", "inforom.ecc", "inforom.pwr":
		if g.InfoROM == nil {
			g.InfoROM = map[string]string{}
		}
		g.InfoROM[strings.TrimPrefix(field, "inforom.")] = v
	case "gom.current":
		g.GOM = v
	case "gom.pending":
		g.GOMPending = v
	case "ecc.mode.current":
		g.ECC = v
	case "ecc.mode.pending":
		g.ECCPending = v
	case "ecc.errors.uncorrected.aggregate.total":
		g.ECCUncorrected = optInt64(v)
	case "retired_pages.sbe":
		g.RetiredSBE = optInt64(v)
	case "retired_pages.dbe":
		g.RetiredDBE = optInt64(v)
	case "retired_pages.pending":
		g.RetiredPending = v
	case "power.default_limit":
		g.PowerDefaultW = optFloat(v)
	case "power.max_limit":
		g.PowerMaxW = optFloat(v)
	}
}

// cardID stands for a serial number and a UUID without giving them away.
func cardID(serial, uuid string) string {
	if serial == "" && uuid == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(serial + "\n" + uuid))
	return hex.EncodeToString(sum[:8])
}

// readGPUs reads each rented GPU: from nvidia-smi where it is installed and
// lists the GPU, and a firmware version the GPU's own driver publishes in sysfs
// (amdgpu does) where it does not.
func readGPUs(h Host, gpus []string) (readings []GPUReading, notes []string) {
	if len(gpus) == 0 {
		return nil, nil
	}
	byBDF := map[string]*GPUReading{}
	for _, b := range gpus {
		byBDF[b] = &GPUReading{BDF: b}
	}
	// nvidia-smi is asked only about GPUs its driver holds, as the teardown's
	// own check is: one on another driver, or on none, is not its to describe.
	var nvidia []string
	for _, b := range gpus {
		if driverOf(h, b) == "nvidia" {
			nvidia = append(nvidia, b)
		}
	}
	if _, err := h.LookPath("nvidia-smi"); err == nil && len(nvidia) > 0 {
		core, err := smiQuery(h, nvidia, smiCore[1:])
		if err != nil {
			notes = append(notes, "nvidia-smi did not answer, so the GPU's firmware and settings were not read: "+firstLine(err.Error()))
		}
		for bdf, v := range core {
			g := byBDF[bdf]
			g.set("driver_version", v[0])
			g.ID = cardID(v[1], v[2])
			g.set("vbios_version", v[3])
		}
		if len(core) > 0 {
			extra, err := smiQuery(h, nvidia, smiExtra)
			if err == nil {
				for bdf, v := range extra {
					for i, field := range smiExtra {
						byBDF[bdf].set(field, v[i])
					}
				}
			} else {
				// An nvidia-smi that does not know one of them refuses them all.
				for _, field := range smiExtra {
					one, err := smiQuery(h, nvidia, []string{field})
					if err != nil {
						continue
					}
					for bdf, v := range one {
						byBDF[bdf].set(field, v[0])
					}
				}
			}
		}
	}
	for _, b := range gpus {
		g := byBDF[b]
		if g.VBIOS == "" {
			if v, err := h.ReadFile(devPath(b) + "/vbios_version"); err == nil {
				g.VBIOS = strings.TrimSpace(string(v))
			}
		}
		readings = append(readings, *g)
	}
	return readings, notes
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// maxROMBytes is more than any GPU's ROM: a read that returns more is not one.
const maxROMBytes = 32 << 20

// ReadROMs hashes the expansion ROM of each of these GPUs that is on vfio-pci
// -- held for a microVM, with no driver of its own using it, which is when the
// kernel reads the ROM for the VM anyway. A GPU on its own driver is left
// alone. Reading enables the ROM for the length of the read and disables it
// again.
func ReadROMs(h Host, gpus []string) (roms map[string]*ROMReading, notes []string) {
	type result struct {
		roms  map[string]*ROMReading
		notes []string
	}
	r, ok := bounded(func() result {
		var r result
		r.roms = map[string]*ROMReading{}
		for _, b := range gpus {
			if driverOf(h, b) != "vfio-pci" {
				continue
			}
			rom, note := readROM(h, b)
			if rom != nil {
				r.roms[b] = rom
			}
			if note != "" {
				r.notes = append(r.notes, note)
			}
		}
		return r
	})
	if !ok {
		return nil, []string{fmt.Sprintf("the GPU's ROM did not read within %v", MeasureTimeout)}
	}
	return r.roms, r.notes
}

func readROM(h Host, bdf string) (*ROMReading, string) {
	p := devPath(bdf) + "/rom"
	if !h.Exists(p) {
		// Said, not left blank: a record with no ROM in it must not read as
		// one whose ROM nobody looked at.
		return nil, fmt.Sprintf("GPU %s has no ROM the kernel can read, so its ROM is not compared", bdf)
	}
	if err := h.WriteFile(p, []byte("1"), 0200); err != nil {
		return nil, fmt.Sprintf("the ROM of GPU %s could not be opened for reading: %s", bdf, firstLine(err.Error()))
	}
	data, err := h.ReadFile(p)
	_ = h.WriteFile(p, []byte("0"), 0200)
	if err != nil {
		return nil, fmt.Sprintf("the ROM of GPU %s could not be read: %s", bdf, firstLine(err.Error()))
	}
	// A PCI expansion ROM starts 55 AA; anything else is not an image.
	if len(data) < 2 || len(data) > maxROMBytes || data[0] != 0x55 || data[1] != 0xaa {
		return nil, fmt.Sprintf("GPU %s did not give a ROM image to read", bdf)
	}
	sum := sha256.Sum256(data)
	rom := &ROMReading{SHA256: hex.EncodeToString(sum[:]), Bytes: len(data)}
	if v, err := h.ReadFile(devPath(bdf) + "/boot_vga"); err == nil && strings.TrimSpace(string(v)) == "1" {
		rom.Shadow = true
	}
	return rom, ""
}

// SetROMs adds ROM readings to the GPUs of a measurement, and the notes.
func (m *Measurement) SetROMs(roms map[string]*ROMReading, notes []string) {
	if m == nil {
		return
	}
	for bdf, rom := range roms {
		found := false
		for i := range m.GPUs {
			if m.GPUs[i].BDF == bdf {
				m.GPUs[i].ROM = rom
				found = true
			}
		}
		if !found {
			m.GPUs = append(m.GPUs, GPUReading{BDF: bdf, ROM: rom})
		}
	}
	m.Notes = append(m.Notes, notes...)
}

// smartReader is a host that can read an NVMe drive's SMART log itself, with
// no tool installed for it (OSHost on Linux).
type smartReader interface {
	NVMeSmartLog(dev string) ([]byte, error)
}

// maxDrives bounds how many drives one reading follows (a wide RAID).
const maxDrives = 8

// readDrives reads the physical drives under dir: the directory the rentals'
// disks live in.
func readDrives(h Host, dir string) (drives []DriveReading, notes []string) {
	if dir == "" {
		return nil, nil
	}
	out, err := h.Output("findmnt", "-n", "-o", "SOURCE", "--target", dir)
	source := strings.TrimSpace(out)
	if i := strings.IndexByte(source, '['); i >= 0 { // a btrfs subvolume: /dev/sda2[/@]
		source = source[:i]
	}
	if err != nil || !strings.HasPrefix(source, "/dev/") {
		return nil, []string{"the drive under " + dir + " could not be told, so its wear was not read"}
	}
	name := path.Base(source)
	if link, err := h.Readlink(source); err == nil { // /dev/mapper/vg-root -> ../dm-0
		name = path.Base(link)
	}
	names := leafDisks(h, name, map[string]bool{}, 0)
	sort.Strings(names)
	if len(names) > maxDrives {
		names = names[:maxDrives]
	}
	for _, n := range names {
		d := DriveReading{Name: n}
		if v, err := h.ReadFile("/sys/block/" + n + "/device/model"); err == nil {
			d.Model = strings.TrimSpace(string(v))
		}
		if w, ok := statWritten(h, n); ok {
			d.StatWrittenBytes = &w
		}
		if sr, ok := h.(smartReader); ok && strings.HasPrefix(n, "nvme") {
			if log, err := sr.NVMeSmartLog("/dev/" + n); err != nil {
				notes = append(notes, fmt.Sprintf("the SMART log of %s could not be read: %s", n, firstLine(err.Error())))
			} else {
				d.setSmart(log)
			}
		}
		drives = append(drives, d)
	}
	return drives, notes
}

// leafDisks follows a block device down to the whole disks under it: through
// device-mapper and RAID members (its slaves) and from a partition to its disk.
func leafDisks(h Host, name string, seen map[string]bool, depth int) []string {
	if name == "" || seen[name] || depth > 8 {
		return nil
	}
	seen[name] = true
	base := "/sys/class/block/" + name
	if slaves, _ := h.Glob(base + "/slaves/*"); len(slaves) > 0 {
		var out []string
		for _, s := range slaves {
			out = append(out, leafDisks(h, path.Base(s), seen, depth+1)...)
		}
		return out
	}
	if h.Exists(base + "/partition") {
		// /sys/class/block/nvme0n1p2 -> ../../devices/.../nvme0n1/nvme0n1p2
		if link, err := h.Readlink(base); err == nil {
			return leafDisks(h, path.Base(path.Dir(link)), seen, depth+1)
		}
		return nil
	}
	for _, virtual := range []string{"loop", "ram", "zram", "nbd"} {
		if strings.HasPrefix(name, virtual) {
			return nil
		}
	}
	return []string{name}
}

// statWritten is what the kernel has counted written to a block device since
// it appeared, in bytes: the seventh figure of its stat file, in 512-byte
// sectors whatever the device's own sector size.
func statWritten(h Host, name string) (int64, bool) {
	data, err := h.ReadFile("/sys/block/" + name + "/stat")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(data))
	if len(f) < 7 {
		return 0, false
	}
	sectors, err := strconv.ParseInt(f[6], 10, 64)
	if err != nil || sectors < 0 || sectors > (1<<62)/512 {
		return 0, false
	}
	return sectors * 512, true
}

// MapperWritten is what has been written through a dm-crypt mapping since it
// was opened, in bytes.
func MapperWritten(h Host, mapper string) (int64, bool) {
	if mapper == "" {
		return 0, false
	}
	// The node is a link to the device where udev made it, and a device node
	// of its own where libdevmapper did (dmudev.go): then sysfs names the device.
	name := ""
	if link, err := h.Readlink(mapper); err == nil {
		name = path.Base(link)
	} else if dev, _ := dmDevice(h, path.Base(mapper)); dev != "" {
		name = dev
	} else if out, err := h.RunLimited(dmLimit, nil, nil, "dmsetup", "info", "-c", "--noheadings", "-o", "blkdevname", path.Base(mapper)); err == nil {
		name = strings.TrimSpace(out)
	}
	if !strings.HasPrefix(name, "dm-") {
		return 0, false
	}
	return statWritten(h, name)
}

// nvmeDataUnit is the unit NVMe counts data in: a thousand 512-byte blocks.
const nvmeDataUnit = 512 * 1000

// setSmart reads an NVMe SMART / Health Information log page (log 02h).
func (d *DriveReading) setSmart(log []byte) {
	if len(log) < 176 {
		return
	}
	d.CriticalWarning = int(log[0])
	spare, used := int(log[3]), int(log[5])
	d.SparePct, d.PercentUsed = &spare, &used
	// Counters are 128-bit little-endian; one that does not fit is left out.
	if lo, hi := binary.LittleEndian.Uint64(log[48:56]), binary.LittleEndian.Uint64(log[56:64]); hi == 0 && lo < (1<<62)/nvmeDataUnit {
		written := int64(lo) * nvmeDataUnit
		d.LifeWrittenBytes = &written
	}
	if lo, hi := binary.LittleEndian.Uint64(log[160:168]), binary.LittleEndian.Uint64(log[168:176]); hi == 0 && lo < 1<<62 {
		errs := int64(lo)
		d.MediaErrors = &errs
	}
}

// Compare says what differs between the readings from before and after a
// rental that a rental should leave alone: the card's identity, its firmware
// and InfoROM versions, its ROM, the settings it keeps across a restart, the
// power limits its firmware sets -- and what its hardware has newly logged
// (memory errors, retired pages, a drive's media errors or warnings). A figure
// read only once is not compared. Wear is not a difference: it is in the
// figures themselves.
func Compare(before, after *Measurement) []string {
	if before == nil || after == nil {
		return nil
	}
	var out []string
	say := func(format string, a ...interface{}) { out = append(out, fmt.Sprintf(format, a...)) }
	for _, b := range before.GPUs {
		var a *GPUReading
		for i := range after.GPUs {
			if after.GPUs[i].BDF == b.BDF {
				a = &after.GPUs[i]
			}
		}
		if a == nil {
			continue
		}
		differs := func(what, was, now string) {
			if was != "" && now != "" && was != now {
				say("%s of GPU %s changed during the rental (%s, now %s)", what, b.BDF, was, now)
			}
		}
		if b.ID != "" && a.ID != "" && b.ID != a.ID {
			say("GPU %s came back with another serial number or UUID than it went with", b.BDF)
		}
		differs("the firmware (VBIOS) version", b.VBIOS, a.VBIOS)
		var parts []string
		for part := range b.InfoROM {
			parts = append(parts, part)
		}
		sort.Strings(parts)
		for _, part := range parts {
			differs("the InfoROM ("+part+") version", b.InfoROM[part], a.InfoROM[part])
		}
		if b.ROM != nil && a.ROM != nil && !b.ROM.Shadow && !a.ROM.Shadow && b.ROM.SHA256 != a.ROM.SHA256 {
			say("the ROM of GPU %s changed during the rental (sha256 %s, now %s)", b.BDF, short(b.ROM.SHA256), short(a.ROM.SHA256))
		}
		differs("the operation mode", b.GOM, a.GOM)
		differs("the operation mode set for the next restart", b.GOMPending, a.GOMPending)
		differs("the ECC mode", b.ECC, a.ECC)
		differs("the ECC mode set for the next restart", b.ECCPending, a.ECCPending)
		watts := func(what string, was, now *float64) {
			if was != nil && now != nil && *was != *now {
				say("%s of GPU %s changed during the rental (%g W, now %g W)", what, b.BDF, *was, *now)
			}
		}
		watts("the default power limit", b.PowerDefaultW, a.PowerDefaultW)
		watts("the maximum power limit", b.PowerMaxW, a.PowerMaxW)
		rose := func(what string, was, now *int64) {
			if was != nil && now != nil && *now > *was {
				say("GPU %s logged %d new %s during the rental (%d in its life)", b.BDF, *now-*was, what, *now)
			}
		}
		rose("uncorrectable memory errors", b.ECCUncorrected, a.ECCUncorrected)
		rose("memory pages retired after single-bit errors", b.RetiredSBE, a.RetiredSBE)
		rose("memory pages retired after double-bit errors", b.RetiredDBE, a.RetiredDBE)
	}
	for _, b := range before.Drives {
		for _, a := range after.Drives {
			if a.Name != b.Name {
				continue
			}
			if b.MediaErrors != nil && a.MediaErrors != nil && *a.MediaErrors > *b.MediaErrors {
				say("the drive %s logged %d new media errors during the rental (%d in its life)", b.Name, *a.MediaErrors-*b.MediaErrors, *a.MediaErrors)
			}
			if a.CriticalWarning != 0 && a.CriticalWarning != b.CriticalWarning {
				say("the drive %s raised a SMART warning during the rental (flags %#02x, before %#02x)", b.Name, a.CriticalWarning, b.CriticalWarning)
			}
		}
	}
	return out
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}

// maxRecords is how many rentals' measurements the machine keeps.
const maxRecords = 20

// maxRecordAge is how long a rental's measurements stay on the machine: three
// years after the rental ended, which is how long the marketplace keeps its
// own copy. The count alone kept them for good on a machine that is not
// rented again.
const maxRecordAge = 3 * 365 * 24 * time.Hour

// ReportedRecords is how many of them /status tells the marketplace: the
// latest, so one is still told after an agent restart or a missed answer.
const ReportedRecords = 3

// RecordsPath is where the rentals' measurements are kept, newest last.
func RecordsPath(dataDir string) string { return path.Join(dataDir, "measurements.json") }

// LoadRecords returns the kept measurements, oldest first; none when the file
// is absent or unreadable. A record past maxRecordAge is dropped here, from
// the answer and from the file, so an idle machine sheds its old rentals the
// next time anything asks: the marketplace reads them every half minute.
func LoadRecords(h Host, dataDir string) []RentalRecord {
	data, err := h.ReadFile(RecordsPath(dataDir))
	if err != nil {
		return nil
	}
	var records []RentalRecord
	if json.Unmarshal(data, &records) != nil {
		return nil
	}
	kept := fresh(records, time.Now().Unix())
	if len(kept) != len(records) {
		// Best effort: a file that cannot be written is read and cut again next time.
		_ = writeRecords(h, dataDir, kept)
	}
	return kept
}

// fresh leaves out the records of rentals that ended more than maxRecordAge
// before now. A record with no end time cannot be aged and is kept.
func fresh(records []RentalRecord, now int64) []RentalRecord {
	cut := now - int64(maxRecordAge/time.Second)
	kept := make([]RentalRecord, 0, len(records))
	for _, r := range records {
		if r.EndedAt != 0 && r.EndedAt < cut {
			continue
		}
		kept = append(kept, r)
	}
	return kept
}

// LatestRecords are the last n kept measurements, oldest first.
func LatestRecords(h Host, dataDir string, n int) []RentalRecord {
	records := LoadRecords(h, dataDir)
	if len(records) > n {
		records = records[len(records)-n:]
	}
	return records
}

// keepRecord adds a rental's record to the kept ones. A rental already there
// keeps its first record: a teardown run again finds less to read.
func keepRecord(h Host, dataDir string, rec RentalRecord) error {
	records := LoadRecords(h, dataDir)
	for _, r := range records {
		if r.RentalID == rec.RentalID {
			return nil
		}
	}
	records = append(records, rec)
	if len(records) > maxRecords {
		records = records[len(records)-maxRecords:]
	}
	return writeRecords(h, dataDir, records)
}

// writeRecords replaces the kept measurements with these.
func writeRecords(h Host, dataDir string, records []RentalRecord) error {
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	if err := h.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	tmp := RecordsPath(dataDir) + ".tmp"
	if err := h.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return h.Rename(tmp, RecordsPath(dataDir))
}

// measured reports whether a start is a rental to measure: the runtime's own
// test boots and image builds are not.
func measured(o StartOptions) bool { return !IsSetupID(o.ID) && o.Probes == nil }

// beginMeasure takes the reading from before a rental, once its disk exists:
// the machine as it is, and what the disk has been written so far (its base
// image or its filesystem), so the teardown can tell the rental's own writes.
func beginMeasure(h Host, dir string, gpus []string, disk DiskState) *MeasureState {
	ms := &MeasureState{Before: MeasureMachine(h, dir, gpus)}
	if w, ok := bounded(func() *int64 {
		if w, ok := MapperWritten(h, disk.Mapper); ok {
			return &w
		}
		return nil
	}); ok {
		ms.DiskBase = w
	}
	return ms
}

// rentalWritten is what the rental has written to its disk since hand-over,
// read while the mapping still exists; nil when it cannot be told.
func rentalWritten(h Host, st *State) *int64 {
	if st.Measure == nil || st.Measure.DiskBase == nil {
		return nil
	}
	w, ok := bounded(func() *int64 {
		if w, ok := MapperWritten(h, st.Disk.Mapper); ok {
			return &w
		}
		return nil
	})
	if !ok || w == nil || *w < *st.Measure.DiskBase {
		return nil
	}
	written := *w - *st.Measure.DiskBase
	return &written
}

// finishMeasure takes the reading from after a rental the renter had, compares
// it with the one from before, and keeps the record. gpus are the GPUs to ask
// the vendor tool about: none while they are not back on their driver.
func finishMeasure(h Host, dataDir, dir, mode string, st *State, gpus []string, written *int64, roms map[string]*ROMReading, romNotes []string) {
	if st.Measure == nil || !st.Measure.Delivered {
		return
	}
	after := MeasureMachine(h, dir, gpus)
	after.SetROMs(roms, romNotes)
	rec := RentalRecord{RentalID: st.RentalID, Mode: mode, StartedAt: st.StartedAt, EndedAt: after.At,
		WrittenBytes: written, Before: st.Measure.Before, After: after}
	rec.Changed = Compare(rec.Before, rec.After)
	_ = keepRecord(h, dataDir, rec)
}

// Records are the measurements of the last rentals on this machine, oldest first.
func (rt *Runtime) Records() []RentalRecord {
	return LatestRecords(rt.h, rt.spec.DataDir, ReportedRecords)
}

// Records are the measurements of the last rentals on this machine, oldest first.
func (rt *ContainerRuntime) Records() []RentalRecord {
	return LatestRecords(rt.h, rt.spec.DataDir, ReportedRecords)
}

// Bytes prints a byte count the way a person reads one.
func Bytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTP"[exp])
}

// Summary is one rental's record in a few lines, for 'gpu-agent status'.
func (r RentalRecord) Summary() []string {
	var lines []string
	if r.WrittenBytes != nil {
		lines = append(lines, "it wrote "+Bytes(*r.WrittenBytes)+" to its disk")
	}
	if r.Before != nil && r.After != nil {
		for _, b := range r.Before.Drives {
			for _, a := range r.After.Drives {
				if a.Name != b.Name {
					continue
				}
				line := "drive " + b.Name
				if b.PercentUsed != nil && a.PercentUsed != nil {
					line += fmt.Sprintf(": %d%% of its rated life used before, %d%% after", *b.PercentUsed, *a.PercentUsed)
				}
				switch {
				case b.LifeWrittenBytes != nil && a.LifeWrittenBytes != nil && *a.LifeWrittenBytes >= *b.LifeWrittenBytes:
					line += "; " + Bytes(*a.LifeWrittenBytes-*b.LifeWrittenBytes) + " written to it in all meanwhile"
				case b.StatWrittenBytes != nil && a.StatWrittenBytes != nil && *a.StatWrittenBytes >= *b.StatWrittenBytes:
					line += "; " + Bytes(*a.StatWrittenBytes-*b.StatWrittenBytes) + " written to it in all meanwhile"
				}
				if line != "drive "+b.Name {
					lines = append(lines, line)
				}
			}
		}
	}
	if len(r.Changed) == 0 {
		if r.Before != nil && r.After != nil && len(r.Before.GPUs) > 0 && len(r.After.GPUs) > 0 {
			lines = append(lines, "the GPU came back as it went")
		}
	} else {
		for _, c := range r.Changed {
			lines = append(lines, "CHANGED: "+c)
		}
	}
	return lines
}
