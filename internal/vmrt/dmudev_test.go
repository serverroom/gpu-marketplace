package vmrt

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// A machine's device-mapper as the agent meets it. The kernel's mappings are
// in sysfs whatever udev does about them; the node in /dev/mapper is udev's to
// make (a link), or libdevmapper's own when DM_DISABLE_UDEV=1 tells it not to
// wait (a device node).
const (
	dmName      = "/sys/block/dm-7/dm/name"
	noUdevOpen  = "DM_DISABLE_UDEV=1 cryptsetup open"
	noUdevClose = "DM_DISABLE_UDEV=1 cryptsetup close"
	cookieHead  = "Cookie       Semid      Value      Last semop time           Last change time\n"
	// A cookie the host's own tools hold: not the agent's to touch.
	hostCookie = "0xd4d0001    5          1          Thu Oct  8 12:00:00 2026  Thu Oct  8 12:00:00 2026\n"
	leftCookie = "0xd4d9b7c    32770      1          Thu Oct  8 12:00:07 2026  Thu Oct  8 12:00:07 2026\n"
)

func kernelMap(h *fakehost.Host, cmd string) {
	h.SetFile(dmName, []byte(fakehost.LastField(cmd)+"\n"))
}
func kernelUnmap(h *fakehost.Host, cmd string) { h.DeleteFile(dmName) }
func mapperNode(cmd string) string             { return "/dev/mapper/" + fakehost.LastField(cmd) }

// dmHost is a machine with sysfs, one loop device to hand out and a cookie of
// the host's own outstanding.
func dmHost() *fakehost.Host {
	h := fakehost.New()
	h.Files["/sys/block/loop7/size"] = []byte("0\n")
	h.Outputs["losetup --find --show"] = "/dev/loop7\n"
	h.Outputs["dmsetup udevcookies"] = cookieHead + hostCookie
	h.OnRun["truncate -s"] = func(h *fakehost.Host, cmd string) { h.SetFile(fakehost.LastField(cmd), nil) }
	h.OnRun["ipcrm -s 32770"] = func(h *fakehost.Host, cmd string) {
		h.SetOutput("dmsetup udevcookies", cookieHead+hostCookie)
	}
	return h
}

// answeringUdev: udev confirms every device. cryptsetup returns once the
// kernel has the mapping and udev has made its link, and the other way round.
func answeringUdev(h *fakehost.Host) *fakehost.Host {
	delete(h.Fail, "cryptsetup open")
	delete(h.Fail, "cryptsetup close")
	h.OnRun["cryptsetup open"] = func(h *fakehost.Host, cmd string) {
		kernelMap(h, cmd)
		h.SetLink(mapperNode(cmd), "../dm-7")
	}
	h.OnRun["cryptsetup close"] = func(h *fakehost.Host, cmd string) {
		kernelUnmap(h, cmd)
		h.DeleteLink(mapperNode(cmd))
	}
	return h
}

// silentUdev: udev is running and never confirms a device-mapper device. The
// kernel does what cryptsetup asked at once; cryptsetup then waits on its
// cookie until it is killed at its limit, and nothing makes or removes the
// node. With DM_DISABLE_UDEV=1 libdevmapper makes and removes the node itself
// and waits for nobody.
func silentUdev(t *testing.T, h *fakehost.Host) *fakehost.Host {
	t.Helper()
	never := fmt.Errorf("cryptsetup: %w", ErrTimedOut)
	leaveCookie := func(h *fakehost.Host) { h.SetOutput("dmsetup udevcookies", cookieHead+hostCookie+leftCookie) }
	h.Fail["cryptsetup open"] = never
	h.OnFail["cryptsetup open"] = func(h *fakehost.Host, cmd string) {
		kernelMap(h, cmd)
		leaveCookie(h)
	}
	h.Fail["cryptsetup close"] = never
	h.OnFail["cryptsetup close"] = func(h *fakehost.Host, cmd string) {
		kernelUnmap(h, cmd)
		leaveCookie(h)
	}
	h.OnRun[noUdevOpen] = func(h *fakehost.Host, cmd string) {
		if h.Exists(dmName) {
			t.Errorf("the mapping was made again over the one the ended command left")
		}
		kernelMap(h, cmd)
		h.SetFile(mapperNode(cmd), nil)
	}
	h.OnRun[noUdevClose] = func(h *fakehost.Host, cmd string) {
		kernelUnmap(h, cmd)
		h.DeleteFile(mapperNode(cmd))
		h.DeleteLink(mapperNode(cmd))
	}
	return h
}

func dmSpec() Spec { return Spec{DataDir: dataDir, DiskGB: 10, AgentVersion: "v9"} }

// limited reports whether every command starting with prefix ran under the
// device-mapper limit, and that there was one.
func limited(h *fakehost.Host, prefix string) bool {
	found := false
	for cmd, limit := range h.Limits {
		if strings.HasPrefix(cmd, prefix) {
			if limit != dmLimit {
				return false
			}
			found = true
		}
	}
	return found
}

// Where udev answers, nothing changes but the limit: the ordinary commands,
// no variable set, nothing recorded and nothing to tell the host.
func TestADiskIsOpenedAndClosedTheOrdinaryWayWhereUdevAnswers(t *testing.T) {
	h := answeringUdev(dmHost())
	ds, err := openEncrypted(h, dmSpec(), "/r", "R1")
	if err != nil || ds.Mapper != "/dev/mapper/gpu-rental-R1" {
		t.Fatalf("openEncrypted = %+v, %v", ds, err)
	}
	if !limited(h, "cryptsetup open") {
		t.Errorf("cryptsetup open ran with no time limit: %v", h.Limits)
	}
	if len(h.Input("cryptsetup open")) != 64 {
		t.Errorf("the key did not reach cryptsetup on its stdin")
	}
	wiped, detail := DestroyDisk(h, dmSpec(), ds)
	if !wiped || len(detail) != 0 {
		t.Fatalf("DestroyDisk = %v %v", wiped, detail)
	}
	if !limited(h, "cryptsetup close") {
		t.Errorf("cryptsetup close ran with no time limit: %v", h.Limits)
	}
	for _, c := range h.Calls {
		if strings.Contains(c, "DM_DISABLE_UDEV") || strings.Contains(c, "ipcrm") || strings.Contains(c, "udevcomplete") {
			t.Errorf("a machine whose udev answers ran %q", c)
		}
	}
	if LoadDMUdev(h, dataDir) != nil || DMUdevNote(h, dataDir, "v9") != "" {
		t.Errorf("something was recorded about a machine whose udev answers")
	}
}

// The machine this was written for: cryptsetup open never returns. It is ended
// at its limit, what it left is removed, the disk is made without waiting for
// udev, and the machine remembers.
func TestAMachineWhoseUdevNeverAnswersStillGetsItsDisk(t *testing.T) {
	h := silentUdev(t, dmHost())
	ds, err := openEncrypted(h, dmSpec(), "/r", "R1")
	if err != nil {
		t.Fatalf("openEncrypted: %v", err)
	}
	if !h.Exists(ds.Mapper) || !mapped(h, "gpu-rental-R1") {
		t.Fatalf("no disk: node %v, kernel %v", h.Exists(ds.Mapper), mapped(h, "gpu-rental-R1"))
	}
	before(t, h, "run cryptsetup open", "run "+noUdevClose+" gpu-rental-R1")
	before(t, h, "run "+noUdevClose+" gpu-rental-R1", "run "+noUdevOpen)
	if !limited(h, noUdevOpen) || !limited(h, noUdevClose) {
		t.Errorf("a command without udev ran with no time limit: %v", h.Limits)
	}
	// The same key both times, on stdin, and never in a command line.
	first, second := h.Input("cryptsetup open"), h.Input(noUdevOpen)
	if len(first) != 64 || first != second {
		t.Errorf("the second attempt was not keyed as the first (%d and %d bytes)", len(first), len(second))
	}
	// The cookie the ended command waited on is removed; the host's own is not.
	if !h.Ran("run ipcrm -s 32770") || h.Ran("run ipcrm -s 5") || h.Ran("run dmsetup udevcomplete_all") {
		t.Errorf("cookies: %v", h.Calls)
	}

	rec := LoadDMUdev(h, dataDir)
	if rec == nil || !rec.Unconfirmed || rec.AgentVersion != "v9" || !strings.Contains(rec.Why, "cryptsetup open") {
		t.Fatalf("record = %+v", rec)
	}
	note := DMUdevNote(h, dataDir, "v9")
	for _, want := range []string{"does not confirm device-mapper devices", "cryptsetup open was still waiting for udev after 20s", "nothing for you to do"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q does not say %q", note, want)
		}
	}

	// The teardown goes the same way from its first command, and verifies.
	plain := h.Count("run cryptsetup")
	wiped, detail := DestroyDisk(h, dmSpec(), ds)
	if !wiped || len(detail) != 0 || mapped(h, "gpu-rental-R1") || h.Exists(ds.Mapper) {
		t.Fatalf("DestroyDisk = %v %v", wiped, detail)
	}
	if h.Count("run cryptsetup") != plain {
		t.Errorf("the teardown waited for udev again on a machine known not to answer")
	}
}

// What a machine learned it uses from the first command, with nothing to wait
// out. The next agent version tries the ordinary way once more.
func TestWhatWasLearnedHoldsForTheAgentVersionThatLearnedIt(t *testing.T) {
	h := silentUdev(t, dmHost())
	if err := MarkDMUdevUnconfirmed(h, dataDir, "v9", "cryptsetup open was still waiting for udev after 20s"); err != nil {
		t.Fatal(err)
	}
	ds, err := openEncrypted(h, dmSpec(), "/r", "R1")
	if err != nil || h.Ran("run cryptsetup") || !h.Ran("run "+noUdevOpen) {
		t.Fatalf("a machine known not to answer: %v, calls %v", err, h.Calls)
	}
	if wiped, detail := DestroyDisk(h, dmSpec(), ds); !wiped || h.Ran("run cryptsetup") {
		t.Fatalf("DestroyDisk = %v %v, calls %v", wiped, detail, h.Calls)
	}

	next := dmSpec()
	next.AgentVersion = "v10"
	if DMUdevNote(h, dataDir, "v10") != "" {
		t.Errorf("another version's finding was reported as this one's")
	}
	if _, err := openEncrypted(h, next, "/r", "R2"); err != nil || !h.Ran("run cryptsetup open") {
		t.Fatalf("the next version did not try the ordinary way: %v", err)
	}
	if rec := LoadDMUdev(h, dataDir); rec == nil || rec.AgentVersion != "v10" {
		t.Errorf("the next version did not record what it found: %+v", rec)
	}
}

// A disk opened while udev answered, on a machine whose udev stops answering
// before the rental ends: the close never returns. The kernel had removed the
// mapping before cryptsetup began to wait, so the disk is closed; the link
// udev never removed goes, the wipe verifies, and the machine remembers.
func TestACloseThatNeverReturnsDoesNotLeaveTheDiskOpen(t *testing.T) {
	h := answeringUdev(dmHost())
	ds, err := openEncrypted(h, dmSpec(), "/r", "R1")
	if err != nil {
		t.Fatal(err)
	}
	silentUdev(t, h)
	wiped, detail := DestroyDisk(h, dmSpec(), ds)
	if !wiped || len(detail) != 0 {
		t.Fatalf("DestroyDisk = %v %v", wiped, detail)
	}
	if mapped(h, "gpu-rental-R1") || h.Exists(ds.Mapper) {
		t.Errorf("left behind: kernel %v, node %v", mapped(h, "gpu-rental-R1"), h.Exists(ds.Mapper))
	}
	if !h.Ran("run ipcrm -s 32770") {
		t.Errorf("the cookie the ended close waited on was left")
	}
	if rec := LoadDMUdev(h, dataDir); rec == nil || !strings.Contains(rec.Why, "cryptsetup close") {
		t.Errorf("record = %+v", rec)
	}
}

// The close is ended before the kernel removed anything: the mapping, and the
// key with it, is still there. It is removed without waiting for udev.
func TestACloseEndedBeforeTheKernelActedIsFinishedWithoutUdev(t *testing.T) {
	h := answeringUdev(dmHost())
	ds, err := openEncrypted(h, dmSpec(), "/r", "R1")
	if err != nil {
		t.Fatal(err)
	}
	silentUdev(t, h)
	h.OnFail["cryptsetup close"] = func(*fakehost.Host, string) {}
	wiped, detail := DestroyDisk(h, dmSpec(), ds)
	if !wiped || len(detail) != 0 || mapped(h, "gpu-rental-R1") {
		t.Fatalf("DestroyDisk = %v %v", wiped, detail)
	}
	before(t, h, "run cryptsetup close", "run "+noUdevClose)
}

// A disk that will not close either way is not a wiped disk, and is reported
// as it always was. dmsetup is asked too before giving up.
func TestADiskThatWillNotCloseEitherWayIsNotWiped(t *testing.T) {
	h := answeringUdev(dmHost())
	ds, err := openEncrypted(h, dmSpec(), "/r", "R1")
	if err != nil {
		t.Fatal(err)
	}
	silentUdev(t, h)
	h.OnFail["cryptsetup close"] = func(*fakehost.Host, string) {}
	h.Fail[noUdevClose] = errors.New("Device gpu-rental-R1 is still in use.")
	h.Fail["DM_DISABLE_UDEV=1 dmsetup remove"] = errors.New("device-mapper: remove ioctl on gpu-rental-R1 failed: Device or resource busy")
	wiped, detail := DestroyDisk(h, dmSpec(), ds)
	if wiped {
		t.Fatalf("an open mapping counted as wiped: %v", detail)
	}
	all := strings.Join(detail, " | ")
	for _, want := range []string{"still in use", "/dev/mapper/gpu-rental-R1 is still open"} {
		if !strings.Contains(all, want) {
			t.Errorf("detail %q does not say %q", all, want)
		}
	}
	if !h.Ran("run DM_DISABLE_UDEV=1 dmsetup remove gpu-rental-R1") {
		t.Errorf("dmsetup was not asked to remove what cryptsetup would not close")
	}
	if LoadDMUdev(h, dataDir) != nil {
		t.Errorf("a finding was recorded although doing without udev did not work")
	}
}

// On a machine whose udev does nothing, the node says nothing: a mapping can
// be open with no node, and a node can outlive its mapping. The kernel is
// asked.
func TestWhetherADiskIsOpenIsAskedOfTheKernel(t *testing.T) {
	// Open in the kernel, no node: not wiped. The node alone would have said wiped.
	h := dmHost()
	h.SetFile(dmName, []byte("gpu-rental-R1\n"))
	h.Fail["cryptsetup close"] = errors.New("Device gpu-rental-R1 is still in use.")
	wiped, detail := DestroyDisk(h, dmSpec(), DiskState{Mapper: "/dev/mapper/gpu-rental-R1"})
	if wiped || !strings.Contains(strings.Join(detail, " | "), "still open") {
		t.Errorf("a mapping with no node counted as wiped: %v %v", wiped, detail)
	}

	// Gone from the kernel, node left behind: wiped, and the node removed.
	h = dmHost()
	h.SetFile("/dev/mapper/gpu-rental-R1", nil)
	h.Fail["cryptsetup close"] = errors.New("Device gpu-rental-R1 is not active.")
	wiped, detail = DestroyDisk(h, dmSpec(), DiskState{Mapper: "/dev/mapper/gpu-rental-R1"})
	if !wiped || len(detail) != 0 || h.Exists("/dev/mapper/gpu-rental-R1") {
		t.Errorf("a node whose mapping is gone: wiped %v, %v, node left %v", wiped, detail, h.Exists("/dev/mapper/gpu-rental-R1"))
	}

	// Another rental's mapping is not this one's.
	h = dmHost()
	h.SetFile(dmName, []byte("gpu-rental-R2\n"))
	if mapped(h, "gpu-rental-R1") || !mapped(h, "gpu-rental-R2") {
		t.Errorf("mappings were told apart wrongly")
	}
	if dev, known := dmDevice(h, "gpu-rental-R2"); dev != "dm-7" || !known {
		t.Errorf("dmDevice = %q %v", dev, known)
	}
}

// An open that fails without udev too is a failure a host can read, leaves no
// mapping behind, and proves nothing about udev.
func TestAnOpenThatFailsBothWaysSaysSoAndLeavesNothing(t *testing.T) {
	h := silentUdev(t, dmHost())
	h.Fail[noUdevOpen] = errors.New("device-mapper: reload ioctl on gpu-rental-R1 failed: No such file or directory")
	// The second attempt had made its mapping too by the time it failed.
	h.OnFail[noUdevOpen] = kernelMap
	ds, err := openEncrypted(h, dmSpec(), "/r", "R1")
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"encrypt disk", "had not opened the disk after 20s", "without waiting for udev either", "reload ioctl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
	if mapped(h, "gpu-rental-R1") {
		t.Errorf("the mapping the ended command made was left")
	}
	if LoadDMUdev(h, dataDir) != nil {
		t.Errorf("a finding was recorded although doing without udev did not work")
	}
	// The teardown still knows the name, should anything be there by then.
	if ds.Mapper != "/dev/mapper/gpu-rental-R1" {
		t.Errorf("the mapping's name was not kept for the teardown: %+v", ds)
	}
	if wiped, detail := DestroyDisk(h, dmSpec(), ds); !wiped {
		t.Errorf("DestroyDisk = %v %v", wiped, detail)
	}
}

// An ordinary failure is passed on as it was, with nothing tried after it:
// only a command that never returned is done again without udev.
func TestACommandThatFailsOutrightIsNotTriedAgain(t *testing.T) {
	h := answeringUdev(dmHost())
	h.Fail["cryptsetup open"] = errors.New("Cannot use device /dev/loop7 which is in use")
	_, err := openEncrypted(h, dmSpec(), "/r", "R1")
	if err == nil || !strings.Contains(err.Error(), "encrypt disk: Cannot use device") {
		t.Fatalf("err = %v", err)
	}
	if h.Ran("run DM_DISABLE_UDEV") || h.Ran("run ipcrm") || LoadDMUdev(h, dataDir) != nil {
		t.Errorf("an ordinary failure was treated as udev not answering: %v", h.Calls)
	}

	h = answeringUdev(dmHost())
	ds, err := openEncrypted(h, dmSpec(), "/r", "R1")
	if err != nil {
		t.Fatal(err)
	}
	h.Fail["cryptsetup close"] = errors.New("Device gpu-rental-R1 is still in use.")
	wiped, detail := DestroyDisk(h, dmSpec(), ds)
	if wiped || !strings.Contains(strings.Join(detail, " | "), "still in use") {
		t.Fatalf("DestroyDisk = %v %v", wiped, detail)
	}
	if h.Ran("run DM_DISABLE_UDEV") || h.Ran("run ipcrm") || LoadDMUdev(h, dataDir) != nil {
		t.Errorf("an ordinary failure was treated as udev not answering: %v", h.Calls)
	}
}

// A machine without dmsetup still gets its disk: only the cookie is left.
func TestWithoutDmsetupTheDiskIsStillMade(t *testing.T) {
	h := silentUdev(t, dmHost())
	h.Tools = map[string]bool{"cryptsetup": true}
	if _, err := openEncrypted(h, dmSpec(), "/r", "R1"); err != nil {
		t.Fatalf("openEncrypted: %v", err)
	}
	if h.Ran("run dmsetup") || h.Ran("run ipcrm") {
		t.Errorf("a tool that is not installed was run: %v", h.Calls)
	}
}

// A whole rental on such a machine: it starts, it ends clean, and the machine
// can be rented again.
func TestARentalStartsAndEndsCleanOnAMachineWhoseUdevNeverAnswers(t *testing.T) {
	h := silentUdev(t, newHost())
	h.Files["/sys/block/loop7/size"] = []byte("0\n")
	h.Outputs["dmsetup udevcookies"] = cookieHead
	spec := testSpec()
	spec.AgentVersion = "v9"
	rt := New(h, spec, &fakeFence{h: h}, func([]BoundDevice) bool { return true })
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !mapped(h, MapperName("R1")) {
		t.Fatal("the rental has no disk")
	}
	res := rt.Stop()
	if !res.Clean() {
		t.Fatalf("Stop = %+v", res)
	}
	if st, _ := LoadState(h, dataDir); st != nil {
		t.Errorf("state left: %+v", st)
	}
	if mapped(h, MapperName("R1")) || h.Exists("/dev/mapper/"+MapperName("R1")) {
		t.Errorf("the rental's disk is still open")
	}
	if DMUdevNote(h, dataDir, "v9") == "" {
		t.Errorf("the machine did not remember")
	}
	if err := rt.Start(StartOptions{ID: "R2", Pubkey: key(t)}); err != nil {
		t.Errorf("the next rental: %v", err)
	}
}

// A rental whose disk cannot be made either way fails, and what the ended
// command left is gone by the time the failure is reported: the machine is
// not quarantined for it.
func TestARentalThatCannotGetItsDiskLeavesTheMachineRentable(t *testing.T) {
	h := silentUdev(t, newHost())
	h.Files["/sys/block/loop7/size"] = []byte("0\n")
	h.Fail[noUdevOpen] = errors.New("device-mapper: reload ioctl on gpu-rental-R1 failed: No such file or directory")
	rt, _ := newRuntime(h, func([]BoundDevice) bool { return true })
	err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)})
	if err == nil || !strings.Contains(err.Error(), "had not opened the disk after 20s") {
		t.Fatalf("Start: %v", err)
	}
	if st, _ := LoadState(h, dataDir); st != nil {
		t.Errorf("the machine was left with state (dirty %v) after a disk that was never made", st.Dirty)
	}
	if mapped(h, MapperName("R1")) {
		t.Errorf("a mapping was left")
	}
}

// leftBehind is what an agent stopped while a rental's disk was being made
// leaves: a state that names no disk, and in the kernel the mapping and the
// loop device under it.
func leftBehind(h *fakehost.Host, id string) Rental {
	r := NewRental(dataDir, id)
	img := r.Dir + "/disk.img"
	h.SetFile(img, nil)
	h.SetFile(dmName, []byte(MapperName(id)+"\n"))
	h.SetLink("/dev/mapper/"+MapperName(id), "../dm-7")
	h.SetOutput("losetup -j "+img, "/dev/loop7: []: ("+img+")\n")
	h.OnRun["losetup -d /dev/loop7"] = func(h *fakehost.Host, cmd string) {
		if !h.Exists(dmName) {
			h.SetOutput("losetup -j "+img, "")
		}
	}
	_ = SaveState(h, dataDir, &State{RentalID: id, Rental: r, StartedAt: 1})
	return r
}

// The teardown of such a state looks for the disk by the names it would have,
// closes it and verifies: going by the record alone it would call a disk that
// is still open wiped.
func TestATeardownFindsTheDiskItsRecordNeverNamed(t *testing.T) {
	h := answeringUdev(newHost())
	h.Files["/sys/block/loop7/size"] = []byte("0\n")
	r := leftBehind(h, "R1")
	rt, _ := newRuntime(h, func([]BoundDevice) bool { return true })
	res := rt.Stop()
	if !res.Clean() {
		t.Fatalf("Stop = %+v", res)
	}
	if mapped(h, MapperName("R1")) || h.Exists("/dev/mapper/"+MapperName("R1")) {
		t.Errorf("the mapping no record named was left open")
	}
	if !h.Ran("run losetup -d /dev/loop7") || h.Exists(r.Dir+"/disk.img") {
		t.Errorf("its loop device or its file was left: %v", h.Calls)
	}
	if st, _ := LoadState(h, dataDir); st != nil {
		t.Errorf("state left: %+v", st)
	}

	// One that will not close keeps the machine from renting, as any other.
	h = answeringUdev(newHost())
	h.Files["/sys/block/loop7/size"] = []byte("0\n")
	leftBehind(h, "R1")
	h.Fail["cryptsetup close"] = errors.New("Device gpu-rental-R1 is still in use.")
	rt, _ = newRuntime(h, func([]BoundDevice) bool { return true })
	if res = rt.Stop(); res.Wiped || res.Clean() {
		t.Fatalf("a disk still open was called wiped: %+v", res)
	}
	if st, _ := LoadState(h, dataDir); st == nil || !st.Dirty {
		t.Errorf("the machine was not kept from renting: %+v", st)
	}
}

// A container rental's is found the same way, its volume unmounted first.
func TestAContainerTeardownFindsTheVolumeItsRecordNeverNamed(t *testing.T) {
	h := answeringUdev(newHost())
	h.Files["/sys/block/loop7/size"] = []byte("0\n")
	r := leftBehind(h, "R1")
	h.SetFile(volumeDir(r.Dir)+"/lost+found", nil)
	rt := NewContainer(h, testSpec(), &fakeFence{h: h}, ContainerImageRef, nil, func([]BoundDevice) bool { return true })
	res := rt.Stop()
	if !res.Wiped || mapped(h, MapperName("R1")) {
		t.Fatalf("Stop = %+v", res)
	}
	before(t, h, "run umount "+volumeDir(r.Dir), "run cryptsetup close "+MapperName("R1"))
}

// A record that names its disk is taken at its word, and a state with no
// rental directory names nothing.
func TestKnownDiskOnlyFillsWhatTheRecordLeftOut(t *testing.T) {
	h := fakehost.New()
	ds := DiskState{File: "/x/disk.img", Loop: "/dev/loop3", Mapper: "/dev/mapper/gpu-rental-R1"}
	if got := knownDisk(h, ds, "/elsewhere", "R9"); got != ds || h.Ran("run losetup") {
		t.Errorf("knownDisk = %+v", got)
	}
	if got := knownDisk(h, DiskState{}, "", "R1"); got != (DiskState{}) {
		t.Errorf("no rental directory: %+v", got)
	}
	want := DiskState{File: "/r/disk.img", Mapper: "/dev/mapper/gpu-rental-R1"}
	if got := knownDisk(h, DiskState{}, "/r", "R1"); got != want {
		t.Errorf("nothing attached: %+v", got)
	}
}

// The test boot is where a machine finds this out, and it passes all the same
// and says what it found.
func TestATestBootPassesOnAMachineWhoseUdevNeverAnswersAndSaysSo(t *testing.T) {
	h := silentUdev(t, selfTestHost(func(string) (string, bool) { return unlockedReport, true }))
	h.Files["/sys/block/loop7/size"] = []byte("0\n")
	spec := testSpec()
	spec.AgentVersion = "v9"
	rt := New(h, spec, &fakeFence{h: h}, func([]BoundDevice) bool { return true })
	res := rt.SelfTest("v9")
	if !res.Passed {
		t.Fatalf("the test boot failed: %v", res.Problems)
	}
	if notes := strings.Join(res.Notes, " | "); !strings.Contains(notes, "does not confirm device-mapper devices") {
		t.Errorf("notes = %q", notes)
	}
	if saved, _ := LoadSelfTest(h, dataDir); saved == nil || !saved.Passed {
		t.Errorf("recorded = %+v", saved)
	}
	// The next one has nothing to wait out and says it again.
	plain := h.Count("run cryptsetup")
	if res = rt.SelfTest("v9"); !res.Passed || h.Count("run cryptsetup") != plain || len(res.Notes) == 0 {
		t.Errorf("the second test boot: passed %v, notes %v", res.Passed, res.Notes)
	}
}

// What a rental wrote is still read where the node is libdevmapper's own and
// not a link: sysfs names the device, with no tool asked.
func TestMapperWrittenReadsAMappingMadeWithoutUdev(t *testing.T) {
	h := fakehost.New()
	h.Files["/dev/mapper/gpu-rental-R1"] = nil
	h.Files[dmName] = []byte("gpu-rental-R1\n")
	h.Files["/sys/block/dm-7/stat"] = blockStat(4096)
	if w, ok := MapperWritten(h, "/dev/mapper/gpu-rental-R1"); !ok || w != 4096*512 || h.Ran("run dmsetup") {
		t.Errorf("%d %v, calls %v", w, ok, h.Calls)
	}
}
