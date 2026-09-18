package vmrt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

func TestBusyKeepsTwoSetupsApart(t *testing.T) {
	h := fakehost.New()
	h.Files[bootIDFile] = []byte("boot-1\n")
	release, err := AcquireBusy(h, dataDir, 100, "setting this machine up")
	if err != nil {
		t.Fatal(err)
	}
	h.Files["/proc/100"] = nil // alive

	_, err = AcquireBusy(h, dataDir, 200, "running a test boot")
	var be *BusyError
	if !errors.Is(err, ErrBusy) || !errors.As(err, &be) || be.Holder.PID != 100 ||
		!strings.Contains(err.Error(), "pid 100) is setting this machine up") {
		t.Fatalf("second AcquireBusy = %v", err)
	}
	// The holder itself is not "another" process.
	if BusyHolder(h, dataDir, 100) != nil {
		t.Error("a process is busy against itself")
	}

	release()
	if h.Exists(BusyPath(dataDir)) {
		t.Error("release left the record")
	}
	if _, err := AcquireBusy(h, dataDir, 200, "running a test boot"); err != nil {
		t.Errorf("AcquireBusy after release = %v", err)
	}
}

func TestStaleBusyRecordsAreIgnored(t *testing.T) {
	h := fakehost.New()
	h.Files[bootIDFile] = []byte("boot-1\n")
	if _, err := AcquireBusy(h, dataDir, 100, "building the rental image"); err != nil {
		t.Fatal(err)
	}
	// Its process is gone.
	if b := BusyHolder(h, dataDir, 200); b != nil {
		t.Errorf("a dead process holds the machine: %+v", b)
	}
	// Alive, but from before the machine restarted (the pid is someone else's now).
	h.Files["/proc/100"] = nil
	h.Files[bootIDFile] = []byte("boot-2\n")
	if b := BusyHolder(h, dataDir, 200); b != nil {
		t.Errorf("a record from an earlier boot holds the machine: %+v", b)
	}
}

// A release by a process that no longer holds the record leaves the new
// holder's record alone.
func TestReleaseOnlyRemovesItsOwnRecord(t *testing.T) {
	h := fakehost.New()
	release, _ := AcquireBusy(h, dataDir, 100, "a")
	if _, err := AcquireBusy(h, dataDir, 200, "b"); err != nil { // 100 is dead: taken over
		t.Fatal(err)
	}
	release()
	if !h.Exists(BusyPath(dataDir)) {
		t.Error("a stale holder's release removed the new holder's record")
	}
}

// An agent that restarts while its own base image build or test boot was
// running finds that VM orphaned, and must not take it for a rental.
func TestSetupVMIsOrphanedUnlessItsProcessLives(t *testing.T) {
	h := newHost()
	rt, _ := newRuntime(h, nil)
	if present, _ := rt.SetupVM(); present {
		t.Fatal("no state, yet a setup VM")
	}
	if err := SaveState(h, dataDir, &State{RentalID: BakeID}); err != nil {
		t.Fatal(err)
	}
	if present, owned := rt.SetupVM(); !present || owned {
		t.Errorf("orphaned bake VM: present=%v owned=%v", present, owned)
	}
	other := os.Getpid() + 1
	h.Files[BusyPath(dataDir)] = []byte(fmt.Sprintf(`{"pid":%d,"what":"building the rental image"}`, other))
	h.Files[fmt.Sprintf("/proc/%d", other)] = nil
	if present, owned := rt.SetupVM(); !present || !owned {
		t.Errorf("a bake another process runs: present=%v owned=%v", present, owned)
	}
	if err := SaveState(h, dataDir, &State{RentalID: "R1"}); err != nil {
		t.Fatal(err)
	}
	if present, _ := rt.SetupVM(); present {
		t.Error("a rental taken for a setup VM")
	}
	for id, want := range map[string]bool{"bake": true, "selftest-1726600000": true, "1a2b3c4d-pairtest": true,
		"R1": false, "bakery": false, "1a2b3c4d-0000-4000-8000-000000000001": false} {
		if IsSetupID(id) != want {
			t.Errorf("IsSetupID(%q) = %v", id, !want)
		}
	}
}

// A test boot the agent is told to stop tears its VM down like any other and
// is recorded as not passed.
func TestSelfTestStopsWhenCancelled(t *testing.T) {
	h := newHost()
	ctx, cancel := context.WithCancel(context.Background())
	h.OnSleep = func(*fakehost.Host) { cancel() } // the VM never reports; the agent stops
	rt, fence := newRuntime(h, func() bool { return true })
	res := rt.SelfTestContext(ctx, "v0.1.10")
	if res.Passed || len(res.Problems) == 0 || !strings.Contains(res.Problems[0], "stopped before it finished") {
		t.Fatalf("result = %+v", res)
	}
	if h.Driver(testGPU) != "nvidia" || fence.removed != 1 {
		t.Error("the test VM was not torn down")
	}
	if st, _ := LoadState(h, dataDir); st != nil {
		t.Errorf("state left behind: %+v", st)
	}

	h = newHost()
	rt, _ = newRuntime(h, nil)
	if res := rt.SelfTestContext(ctx, "v0.1.10"); res.Passed || h.Ran("run systemd-run") {
		t.Errorf("a cancelled test boot started: %+v", res)
	}
}

func TestPrepareStopsWhenCancelled(t *testing.T) {
	h := prepareHost(t)
	h.OnRun["systemd-run --unit=gpu-rental-bake"] = func(h *fakehost.Host, _ string) {
		h.SetFail("systemctl is-active --quiet gpu-rental-bake", nil) // the bake VM runs
	}
	h.OnRun["systemctl stop gpu-rental-bake"] = func(h *fakehost.Host, _ string) {
		h.SetFail("systemctl is-active --quiet gpu-rental-bake", errors.New("inactive"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.OnSleep = func(*fakehost.Host) { cancel() }
	fence := &fakeFence{h: h}
	err := Prepare(h, testSpec(), fence, "v0.1.10", PrepareOptions{Ctx: ctx})
	if err == nil || !strings.Contains(err.Error(), "stopped before it finished") {
		t.Fatalf("Prepare = %v", err)
	}
	if !h.Ran("run systemctl stop gpu-rental-bake") || fence.removed != 1 {
		t.Error("the bake VM was not torn down")
	}
	if h.Exists(testSpec().GoldenImage) {
		t.Error("a golden image was written from a stopped build")
	}
	if st, _ := LoadState(h, dataDir); st != nil {
		t.Errorf("state left behind: %+v", st)
	}
}
