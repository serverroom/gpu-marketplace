package autosetup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

const (
	version = "v0.1.10"
	dataDir = "/var/lib/gpu-agent"
	gpu     = "0000:01:00.0"
)

var golden = filepath.Join(dataDir, "golden.img") // as Detect names it

// machine is a CMP 170HX box, proprietary driver 580, that preflight finds
// ready: KVM, IOMMU, a GPU alone in its group, the tools and firmware, a base
// image with the host's driver and a passing test boot for this version.
// Tests take away what they need.
func machine(t *testing.T) *fakehost.Host {
	t.Helper()
	h := fakehost.New()
	h.Files["/dev/kvm"] = nil
	h.Files["/sys/kernel/iommu_groups/13"] = nil
	h.Outputs["nvidia-smi --query-gpu=pci.bus_id,name"] = "00000000:01:00.0, NVIDIA CMP 170HX, 8192\n"
	h.Outputs["nvidia-smi --query-gpu=driver_version"] = "580.82.07\n"
	h.Outputs["modinfo -F license nvidia"] = "NVIDIA\n"
	h.PCI(gpu, "nvidia", "0x030200", gpu)
	h.Files["/proc/meminfo"] = []byte("MemTotal:       32000000 kB\n")
	h.Files[dataDir] = nil
	h.Outputs["df --output=avail"] = " Avail\n  900G\n"
	installed(h)
	image(h, "resolute-server-cloudimg-amd64.img", "580-server")
	passed(h, version)
	return h
}

func installed(h *fakehost.Host) {
	h.Tools = nil // every tool, apt-get included
	h.SetFile("/usr/share/OVMF/OVMF_CODE_4M.fd", nil)
	h.SetFile("/usr/share/OVMF/OVMF_VARS_4M.fd", nil)
}

func image(h *fakehost.Host, base, driver string) {
	info, _ := json.Marshal(vmrt.GoldenInfo{Base: base, Driver: driver, DriverSource: vmrt.DriverFromHost})
	h.SetFile(golden, []byte("golden"))
	h.SetFile(golden+".json", info)
}

func passed(h *fakehost.Host, v string) {
	res, _ := json.Marshal(vmrt.SelfTestResult{Passed: true, AgentVersion: v, HostGPUs: []string{gpu}})
	h.SetFile(vmrt.SelfTestPath(dataDir), res)
}

func noTools(h *fakehost.Host, apt bool) {
	h.Tools = map[string]bool{"apt-get": apt}
	h.DeleteFile("/usr/share/OVMF/OVMF_CODE_4M.fd")
}

func noImage(h *fakehost.Host) {
	h.DeleteFile(golden)
	h.DeleteFile(golden + ".json")
}

func noTestBoot(h *fakehost.Host) { h.DeleteFile(vmrt.SelfTestPath(dataDir)) }

// rig is a daemon on a fake machine with fake steps that do on the fake what
// the real ones do on a real machine, and a clock that moves only when the
// daemon waits.
type rig struct {
	t       *testing.T
	h       *fakehost.Host
	d       *Daemon
	mu      sync.Mutex
	now     time.Time
	steps   []Step
	drivers []string
	reports []control.Capability
	waits   []time.Duration
	// fail makes a step fail (once per entry).
	fail map[Step][]error
	// during runs inside each step, as it does its work.
	during func(Step)
	// onWait runs when the daemon waits; a test cancels there.
	onWait func()
}

func newRig(t *testing.T, h *fakehost.Host) *rig {
	t.Helper()
	r := &rig{t: t, h: h, now: time.Date(2026, 9, 18, 14, 5, 0, 0, time.UTC), fail: map[Step][]error{}}
	detect := func() *provisioner.Provisioner { return provisioner.Detect(h, "linux", "amd64", dataDir, version) }
	r.d = &Daemon{
		Runner: Runner{
			Host: h, Arch: "amd64", DataDir: dataDir, Version: version, Detect: detect,
			Now: r.clock,
			InstallDeps: func(context.Context) error {
				if err := r.did(StepDeps); err != nil {
					return err
				}
				installed(h)
				return nil
			},
			BuildImage: func(_ context.Context, spec vmrt.Spec, drv vmrt.DriverChoice) error {
				r.mu.Lock()
				r.drivers = append(r.drivers, drv.Driver)
				r.mu.Unlock()
				if err := r.did(StepImage); err != nil {
					return err
				}
				image(h, "resolute-server-cloudimg-amd64.img", drv.Driver)
				return nil
			},
			TestBoot: func(context.Context, *vmrt.Runtime) vmrt.SelfTestResult {
				if err := r.did(StepTestBoot); err != nil {
					return vmrt.SelfTestResult{Problems: []string{err.Error()}}
				}
				passed(h, version)
				return vmrt.SelfTestResult{Passed: true, GuestGPUs: []string{"NVIDIA CMP 170HX"}}
			},
		},
		GOOS:      "linux",
		ConfigDir: t.TempDir(),
		PID:       4242,
		Prov:      detect(),
		Report: func(c control.Capability) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.reports = append(r.reports, c)
			return nil
		},
		After: func(d time.Duration) <-chan time.Time {
			r.mu.Lock()
			r.waits = append(r.waits, d)
			r.now = r.now.Add(d)
			r.mu.Unlock()
			if r.onWait != nil {
				r.onWait()
			}
			ch := make(chan time.Time, 1)
			ch <- r.clock()
			return ch
		},
	}
	return r
}

func (r *rig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *rig) did(s Step) error {
	r.mu.Lock()
	r.steps = append(r.steps, s)
	var err error
	if q := r.fail[s]; len(q) > 0 {
		err, r.fail[s] = q[0], q[1:]
	}
	during := r.during
	r.mu.Unlock()
	if during != nil {
		during(s)
	}
	return err
}

func (r *rig) ran() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var s []string
	for _, st := range r.steps {
		s = append(s, string(st))
	}
	return strings.Join(s, ",")
}

// restart is the agent starting again on the same machine: a new daemon and
// provisioner, the same disk.
func (r *rig) restart() *rig {
	n := newRig(r.t, r.h)
	n.d.ConfigDir = r.d.ConfigDir
	n.now = r.clock()
	return n
}

func (r *rig) run() { r.d.Run(context.Background()) }

func (r *rig) last() *Attempt {
	a, err := Load(r.h, dataDir)
	if err != nil {
		r.t.Fatal(err)
	}
	return a
}

func (r *rig) reasons() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.reports {
		out = append(out, strings.Join(c.Reasons, " | "))
	}
	return out
}

// --- eligibility -----------------------------------------------------------

// Each of these is a reason for a person. The setup does not run -- not even
// for the fixable reasons beside it -- and does not come back to it.
func TestNothingRunsForAReasonOnlyAPersonCanFix(t *testing.T) {
	for name, breakIt := range map[string]func(h *fakehost.Host){
		"no KVM":    func(h *fakehost.Host) { h.DeleteFile("/dev/kvm") },
		"IOMMU off": func(h *fakehost.Host) { h.DeleteFile("/sys/kernel/iommu_groups/13") },
		"GPU shares its group": func(h *fakehost.Host) {
			h.PCI(gpu, "nvidia", "0x030200", gpu, "0000:02:00.0")
			h.PCI("0000:02:00.0", "ixgbe", "0x020000")
		},
		"too little memory": func(h *fakehost.Host) { h.SetFile("/proc/meminfo", []byte("MemTotal: 4000000 kB\n")) },
		"too little disk":   func(h *fakehost.Host) { h.Outputs["df --output=avail"] = " Avail\n  30G\n" },
		"no GPU":            func(h *fakehost.Host) { delete(h.Outputs, "nvidia-smi --query-gpu=pci.bus_id,name") },
		"a desktop, not a Spark": func(h *fakehost.Host) {
			h.SetLink("/proc/2558/fd/7", "/dev/nvidia0")
			h.SetFile("/proc/2558/comm", []byte("Xorg\n"))
		},
		"tools missing, no apt-get": func(h *fakehost.Host) { noTools(h, false) },
	} {
		h := machine(t)
		breakIt(h)
		noImage(h) // a fixable reason beside it
		r := newRig(t, h)
		r.run()
		if got := r.ran(); got != "" {
			t.Errorf("%s: steps ran: %s", name, got)
		}
		if r.last() != nil {
			t.Errorf("%s: an attempt was recorded", name)
		}
		if len(r.reports) != 0 || len(r.waits) != 0 {
			t.Errorf("%s: reports=%v waits=%v; nothing may be reported or scheduled", name, r.reasons(), r.waits)
		}
	}
}

func TestNotOffLinux(t *testing.T) {
	h := machine(t)
	noImage(h)
	r := newRig(t, h)
	r.d.GOOS = "windows"
	r.run()
	if r.ran() != "" {
		t.Errorf("steps ran on windows: %s", r.ran())
	}
}

// Each of these the agent fixes itself, with exactly the steps it needs, and
// the machine ends ready.
func TestEveryFixableReasonIsSetUp(t *testing.T) {
	for name, c := range map[string]struct {
		breakIt func(h *fakehost.Host)
		steps   string
	}{
		"tools missing":             {func(h *fakehost.Host) { noTools(h, true) }, "deps,test-boot"},
		"no base image":             {noImage, "image,test-boot"},
		"image from 24.04":          {func(h *fakehost.Host) { image(h, "noble-server-cloudimg-amd64.img", "580-server") }, "image,test-boot"},
		"image with another driver": {func(h *fakehost.Host) { image(h, "resolute-server-cloudimg-amd64.img", "580-server-open") }, "image,test-boot"},
		"never test-booted":         {noTestBoot, "test-boot"},
		"test-booted by v0.1.9":     {func(h *fakehost.Host) { passed(h, "v0.1.9") }, "test-boot"},
		"a fresh machine":           {func(h *fakehost.Host) { noTools(h, true); noImage(h); noTestBoot(h) }, "deps,image,test-boot"},
	} {
		h := machine(t)
		c.breakIt(h)
		r := newRig(t, h)
		r.run()
		if got := r.ran(); got != c.steps {
			t.Errorf("%s: steps %q, want %q", name, got, c.steps)
		}
		if a := r.last(); a == nil || !a.Passed || a.By != ByAgent {
			t.Errorf("%s: attempt = %+v", name, a)
		}
		if !r.d.Prov.Capability().Ready {
			t.Errorf("%s: not ready after the setup: %v", name, r.d.Prov.Capability().Reasons)
		}
		if rs := r.reasons(); len(rs) == 0 || rs[len(rs)-1] != "" || !r.reports[len(r.reports)-1].Ready {
			t.Errorf("%s: the last report is not ready: %v", name, rs)
		}
	}
}

// A ready machine is left alone.
func TestAReadyMachineIsLeftAlone(t *testing.T) {
	r := newRig(t, machine(t))
	r.run()
	if r.ran() != "" || r.last() != nil || len(r.reports) != 0 {
		t.Errorf("steps=%q reports=%v", r.ran(), r.reasons())
	}
}

// --- driver ----------------------------------------------------------------

func TestTheImageGetsTheHostsDriver(t *testing.T) {
	for name, c := range map[string]struct {
		smi, versionFile, license string
		want                      string
	}{
		"proprietary (a CMP 170HX)": {"580.82.07\n", "NVRM version: NVIDIA UNIX x86_64 Kernel Module  580.82.07\n", "NVIDIA\n", "580-server"},
		"open (a DGX Spark)":        {"580.95.05\n", "NVRM version: NVIDIA UNIX Open Kernel Module for aarch64  580.95.05\n", "", "580-server-open"},
		"open, by licence":          {"575.64.03\n", "", "Dual MIT/GPL\n", "575-server-open"},
		"unreadable":                {"", "", "", vmrt.DefaultDriver},
	} {
		h := machine(t)
		noImage(h)
		delete(h.Outputs, "modinfo -F license nvidia")
		h.Outputs["nvidia-smi --query-gpu=driver_version"] = c.smi
		if c.versionFile != "" {
			h.SetFile(vmrt.NVIDIAVersionFile, []byte(c.versionFile))
		}
		if c.license != "" {
			h.Outputs["modinfo -F license nvidia"] = c.license
		}
		r := newRig(t, h)
		r.run()
		if len(r.drivers) != 1 || r.drivers[0] != c.want {
			t.Errorf("%s: image built with %v, want %s", name, r.drivers, c.want)
		}
		if a := r.last(); a == nil || a.Driver != c.want {
			t.Errorf("%s: attempt = %+v", name, a)
		}
	}
}

// --- progress --------------------------------------------------------------

// While it runs, the machine reports one plain line, re-reported as each step
// starts; the line is what the provisioner answers with, and no rental can
// start meanwhile.
func TestProgressIsOneLineReportedAtEachStep(t *testing.T) {
	h := machine(t)
	noTools(h, true)
	noImage(h)
	noTestBoot(h)
	r := newRig(t, h)
	r.during = func(s Step) {
		c := r.d.Prov.Capability()
		if c.Ready || len(c.Reasons) != 1 || !strings.HasPrefix(c.Reasons[0], "Setting up automatically: ") {
			t.Errorf("during %s the machine reports %+v", s, c)
		}
		if !r.d.Prov.SettingUp() {
			t.Errorf("during %s the provisioner does not know the setup runs", s)
		}
		if err := r.d.Prov.Provision("R1", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGq0m4Yb0bJ4QZqvN4ExampleKeyOnlyForTestsXYZ"); err == nil {
			t.Errorf("during %s a rental was accepted", s)
		}
	}
	r.run()
	want := []string{
		"Setting up automatically: installing the rental runtime (QEMU, UEFI firmware, cloud-image-utils, cryptsetup, nftables) (step 1 of 3, started 14:05 UTC). Nothing to do; this takes about a few minutes.",
		"Setting up automatically: building the rental image (step 2 of 3, started 14:05 UTC). Nothing to do; this takes about 20-30 minutes.",
		"Setting up automatically: running a test rental (step 3 of 3, started 14:05 UTC). Nothing to do; this takes about 5-15 minutes.",
		"",
	}
	if got := r.reasons(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("reported:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if r.d.Prov.SettingUp() {
		t.Error("still setting up after it finished")
	}
}

func TestProgressLineOnASparkSaysTheDesktopCloses(t *testing.T) {
	a := Attempt{Steps: []string{"image", "test-boot"}, Step: 2, StepStartedAt: time.Date(2026, 9, 18, 14, 35, 0, 0, time.UTC).Unix()}
	got := ProgressLine(a, true)
	want := "Setting up automatically: running a test rental (step 2 of 2, started 14:35 UTC). Nothing to do; this takes about 5-15 minutes. This machine's desktop closes during the test and comes back after it."
	if got != want {
		t.Errorf("line = %q", got)
	}
	a.Step = 1
	if strings.Contains(ProgressLine(a, true), "desktop") {
		t.Error("the desktop is mentioned for a step that does not close it")
	}
}

// --- failure, retry, one attempt per key -----------------------------------

func TestAFailureIsReportedAndRetriedAfterSixHours(t *testing.T) {
	h := machine(t)
	noImage(h)
	r := newRig(t, h)
	r.fail[StepImage] = []error{errors.New("download base image: GET https://cloud-images.ubuntu.com/resolute/current/resolute-server-cloudimg-amd64.img: connection reset by peer")}
	var failedWith []string
	r.onWait = func() { failedWith = r.d.Prov.Capability().Reasons }
	r.run()

	if len(r.waits) != 1 || r.waits[0] != RetryAfter {
		t.Fatalf("waits = %v, want one of %v", r.waits, RetryAfter)
	}
	want := "Automatic setup failed while building the rental image (step 1 of 2): download base image: GET https://cloud-images.ubuntu.com/resolute/current/resolute-server-cloudimg-amd64.img: connection reset by peer. " +
		"The agent tries again after its next restart or in 6 hours; 'sudo gpu-agent setup' shows the details."
	if len(failedWith) != 1 || failedWith[0] != want {
		t.Errorf("while waiting the machine reported %q\nwant %q", failedWith, want)
	}
	// Six hours later the same attempt ran again, and passed.
	if got := r.ran(); got != "image,image,test-boot" {
		t.Errorf("steps = %s", got)
	}
	if a := r.last(); !a.Passed || !r.d.Prov.Capability().Ready {
		t.Errorf("attempt = %+v", a)
	}
}

func TestAFailedTestBootPointsAtCheckBoot(t *testing.T) {
	h := machine(t)
	noTestBoot(h)
	r := newRig(t, h)
	r.fail[StepTestBoot] = []error{errors.New("nvidia-smi failed inside the VM: No devices were found")}
	ctx, cancel := context.WithCancel(context.Background())
	r.onWait = cancel
	r.d.Run(ctx)
	got := strings.Join(r.d.Prov.Capability().Reasons, " | ")
	if !strings.HasPrefix(got, "Automatic setup failed while running a test rental (step 1 of 1): the test boot failed: nvidia-smi failed inside the VM") ||
		!strings.HasSuffix(got, "'sudo gpu-agent check --boot' shows the details.") {
		t.Errorf("reasons = %q", got)
	}
}

// A restart retries a failed attempt at once ("after its next restart"); a
// passed one never runs again for the same agent, driver, release and GPUs;
// anything new is a new attempt.
func TestOneAttemptPerKey(t *testing.T) {
	h := machine(t)
	noImage(h)
	r := newRig(t, h)
	r.fail[StepImage] = []error{errors.New("apt-get update: temporary failure resolving archive.ubuntu.com")}
	ctx, cancel := context.WithCancel(context.Background())
	r.onWait = cancel
	r.d.Run(ctx)
	if a := r.last(); a == nil || a.Passed || !a.Finished() {
		t.Fatalf("attempt = %+v", a)
	}

	// The agent restarts an hour later: tried again at once.
	r2 := r.restart()
	r2.now = r2.now.Add(time.Hour)
	r2.run()
	if got := r2.ran(); got != "image,test-boot" {
		t.Fatalf("after a restart: steps %q", got)
	}
	if !r2.last().Passed {
		t.Fatalf("attempt = %+v", r2.last())
	}

	// It passed. Something fixable breaks again: not for this key.
	noImage(h)
	r3 := r2.restart()
	r3.run()
	if r3.ran() != "" || len(r3.waits) != 0 {
		t.Errorf("a passed key ran again: steps %q waits %v", r3.ran(), r3.waits)
	}

	// A new driver on the host is a new key.
	h.Outputs["nvidia-smi --query-gpu=driver_version"] = "580.95.05\n"
	r4 := r3.restart()
	r4.run()
	if got := r4.ran(); got != "image,test-boot" {
		t.Errorf("after a driver update: steps %q", got)
	}
}

// Within one agent run, a failure is not retried before six hours are up.
func TestNoRetryBeforeSixHoursWithoutARestart(t *testing.T) {
	h := machine(t)
	noImage(h)
	r := newRig(t, h)
	a := r.d.Runner.Begin(Plan{Steps: []Step{StepImage, StepTestBoot}}, []string{gpu}, ByAgent)
	a.Step, a.FinishedAt, a.Error = 1, r.now.Add(-time.Hour).Unix(), "boom"
	if err := Save(h, dataDir, a); err != nil {
		t.Fatal(err)
	}
	r.d.started = true // not this agent's first look
	ctx, cancel := context.WithCancel(context.Background())
	r.onWait = cancel
	r.d.Run(ctx)
	if r.ran() != "" {
		t.Errorf("retried after an hour: %s", r.ran())
	}
	if len(r.waits) != 1 || r.waits[0] != 5*time.Hour {
		t.Errorf("waits = %v, want 5h", r.waits)
	}
}

// An attempt the agent never finished (killed, power lost) is retried six
// hours after it started, not at once: an agent that keeps dying must not keep
// restarting a half-hour build. The host is told when.
func TestAnInterruptedAttemptWaits(t *testing.T) {
	h := machine(t)
	noImage(h)
	r := newRig(t, h)
	a := r.d.Runner.Begin(Plan{Steps: []Step{StepImage, StepTestBoot}}, []string{gpu}, ByAgent)
	a.Step, a.StartedAt = 1, r.now.Add(-time.Hour).Unix()
	if err := Save(h, dataDir, a); err != nil {
		t.Fatal(err)
	}
	var shown []string
	ctx, cancel := context.WithCancel(context.Background())
	r.onWait = func() { shown = r.d.Prov.Capability().Reasons; cancel() }
	r.d.Run(ctx)
	if r.ran() != "" || len(r.waits) != 1 || r.waits[0] != 5*time.Hour {
		t.Errorf("steps %q waits %v", r.ran(), r.waits)
	}
	want := "Automatic setup was interrupted while building the rental image (step 1 of 2, started 13:05 UTC); the agent tries again at 2026-09-18 19:05 UTC. 'sudo gpu-agent setup' runs it now."
	if len(shown) != 1 || shown[0] != want {
		t.Errorf("shown %q\nwant %q", shown, want)
	}
}

// --- opt-out, rentals, cancellation, concurrency ----------------------------

func TestOptOut(t *testing.T) {
	h := machine(t)
	noImage(h)
	r := newRig(t, h)
	if !Enabled(r.d.ConfigDir) {
		t.Fatal("off by default")
	}
	if err := SetEnabled(r.d.ConfigDir, false); err != nil {
		t.Fatal(err)
	}
	r.run()
	if r.ran() != "" || r.last() != nil {
		t.Errorf("ran while off: %s", r.ran())
	}
	for content, on := range map[string]bool{"off": false, " OFF \n": false, "on\n": true, "": true, "yes": true} {
		if err := writeFile(OptOutPath(r.d.ConfigDir), content); err != nil {
			t.Fatal(err)
		}
		if Enabled(r.d.ConfigDir) != on {
			t.Errorf("%q: enabled=%v", content, !on)
		}
	}
	if err := SetEnabled(r.d.ConfigDir, true); err != nil || !Enabled(r.d.ConfigDir) {
		t.Fatalf("SetEnabled(on) = %v", err)
	}
	r.restart().run()
	if r.last() == nil {
		t.Error("did not run once turned back on")
	}
}

// Never beside a rental, or the leftover of one.
func TestNeverWhileARentalIsPresent(t *testing.T) {
	for name, st := range map[string]*vmrt.State{
		"a rental":             {RentalID: "R1", Rental: vmrt.NewRental(dataDir, "R1")},
		"a dirty leftover":     {RentalID: "R1", Dirty: true, DirtyDetail: []string{"mapping still open"}},
		"a person's test boot": {RentalID: vmrt.SelfTestPrefix + "1", Rental: vmrt.NewRental(dataDir, vmrt.SelfTestPrefix+"1")},
	} {
		h := machine(t)
		passed(h, "v0.1.9")
		if err := vmrt.SaveState(h, dataDir, st); err != nil {
			t.Fatal(err)
		}
		h.SetFile("/proc/9999", nil)
		h.SetFile(vmrt.BusyPath(dataDir), []byte(`{"pid":9999,"what":"running a test boot"}`))
		r := newRig(t, h)
		r.run()
		if r.ran() != "" || r.last() != nil {
			t.Errorf("%s: steps ran: %s", name, r.ran())
		}
	}
}

// Another process setting the machine up (a person's `gpu-agent setup`)
// holds it: the daemon does not start a second setup beside it.
func TestNotBesideAnotherProcessesSetup(t *testing.T) {
	h := machine(t)
	noTestBoot(h)
	h.SetFile("/proc/9999", nil)
	h.SetFile(vmrt.BusyPath(dataDir), []byte(`{"pid":9999,"what":"setting this machine up"}`))
	r := newRig(t, h)
	r.run()
	if r.ran() != "" {
		t.Errorf("steps ran beside another setup: %s", r.ran())
	}
}

// Stopping the agent stops the setup: the step in hand returns (the real ones
// tear their VM down first), the attempt says so, nothing is reported as a
// failure and no retry is scheduled.
func TestStoppingTheAgentStopsTheSetup(t *testing.T) {
	h := machine(t)
	noImage(h)
	r := newRig(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	r.d.Runner.BuildImage = func(ctx context.Context, _ vmrt.Spec, _ vmrt.DriverChoice) error {
		cancel()
		<-ctx.Done()
		return errors.New("the base image build was stopped before it finished: context canceled")
	}
	r.d.Run(ctx)
	a := r.last()
	if a == nil || a.Passed || !a.Finished() || a.Error != "the agent stopped while building the rental image" {
		t.Fatalf("attempt = %+v", a)
	}
	if r.d.Prov.SettingUp() || len(r.waits) != 0 {
		t.Errorf("settingUp=%v waits=%v", r.d.Prov.SettingUp(), r.waits)
	}
	for _, line := range r.reasons() {
		if strings.Contains(line, "failed") {
			t.Errorf("reported a failure on the way out: %s", line)
		}
	}
	if h.Exists(vmrt.BusyPath(dataDir)) {
		t.Error("busy record left behind")
	}
	// The next start tries again at once: it finished, with a reason.
	r2 := r.restart()
	r2.run()
	if r2.ran() != "image,test-boot" {
		t.Errorf("after the restart: %q", r2.ran())
	}
}

func TestNeverTwiceAtOnce(t *testing.T) {
	h := machine(t)
	noImage(h)
	r := newRig(t, h)
	inside, release := make(chan struct{}), make(chan struct{})
	r.d.Runner.BuildImage = func(context.Context, vmrt.Spec, vmrt.DriverChoice) error {
		close(inside)
		<-release
		image(h, "resolute-server-cloudimg-amd64.img", "580-server")
		return nil
	}
	done := make(chan struct{})
	go func() { r.run(); close(done) }()
	<-inside
	r.run() // returns at once: one is running
	close(release)
	<-done
	if got := r.ran(); got != "test-boot" {
		t.Errorf("steps = %q (the image step is the blocking fake)", got)
	}
}

// A step that panics does not take the agent down, and leaves a finished,
// failed attempt rather than one that looks like it is still running.
func TestAPanickingStepIsAFailure(t *testing.T) {
	h := machine(t)
	noImage(h)
	r := newRig(t, h)
	r.d.Runner.BuildImage = func(context.Context, vmrt.Spec, vmrt.DriverChoice) error { panic("nil map") }
	ctx, cancel := context.WithCancel(context.Background())
	r.onWait = cancel
	r.d.Run(ctx)
	if a := r.last(); a == nil || !a.Finished() || !strings.Contains(a.Error, "internal error: nil map") {
		t.Errorf("attempt = %+v", a)
	}
}

// --- plan and text ---------------------------------------------------------

func TestPlanFor(t *testing.T) {
	f := func(kind provisioner.ReasonKind) provisioner.Finding {
		return provisioner.Finding{Kind: kind, Text: string(kind)}
	}
	for name, c := range map[string]struct {
		findings []provisioner.Finding
		apt      bool
		steps    string
		human    int
	}{
		"ready":           {nil, true, "", 0},
		"tools":           {[]provisioner.Finding{f(provisioner.ReasonTools)}, true, "deps,test-boot", 0},
		"tools, no apt":   {[]provisioner.Finding{f(provisioner.ReasonTools)}, false, "", 1},
		"image and tools": {[]provisioner.Finding{f(provisioner.ReasonImage), f(provisioner.ReasonTools)}, true, "deps,image,test-boot", 0},
		"human and image": {[]provisioner.Finding{f(provisioner.ReasonHuman), f(provisioner.ReasonImage)}, true, "image,test-boot", 1},
		"test boot":       {[]provisioner.Finding{f(provisioner.ReasonTestBoot)}, true, "test-boot", 0},
	} {
		p := PlanFor(c.findings, c.apt)
		var steps []string
		for _, s := range p.Steps {
			steps = append(steps, string(s))
		}
		if strings.Join(steps, ",") != c.steps || len(p.Human) != c.human {
			t.Errorf("%s: steps %v human %v", name, steps, p.Human)
		}
		if p.Eligible() != (c.human == 0 && c.steps != "") {
			t.Errorf("%s: eligible = %v", name, p.Eligible())
		}
	}
}

func TestShortenKeepsTheCause(t *testing.T) {
	long := "install packages: env DEBIAN_FRONTEND=noninteractive apt-get install -y qemu-system-arm qemu-utils: exit status 100: " +
		strings.Repeat("Reading package lists...\n", 40) + "E: Unable to locate package qemu-efi-aarch64"
	got := Shorten(long, 300)
	if len([]rune(got)) > 300 || !strings.HasPrefix(got, "install packages:") || !strings.HasSuffix(got, "E: Unable to locate package qemu-efi-aarch64") {
		t.Errorf("Shorten = %q", got)
	}
}

func TestDescribe(t *testing.T) {
	at := time.Date(2026, 9, 18, 14, 5, 0, 0, time.UTC).Unix()
	for _, c := range []struct {
		a    *Attempt
		want string
	}{
		{nil, "has not run on this machine"},
		{&Attempt{Steps: []string{"image", "test-boot"}, Step: 2, StartedAt: at, StepStartedAt: at, By: ByAgent}, "started 2026-09-18 14:05 UTC by the agent: running a test rental (step 2 of 2, since 14:05 UTC), not finished"},
		{&Attempt{Steps: []string{"test-boot"}, Step: 1, FinishedAt: at, Passed: true, By: ByCommand, AgentVersion: version, Driver: "580-server"}, "passed 2026-09-18 14:05 UTC (run by the setup command, agent v0.1.10, rental image driver 580-server)"},
		{&Attempt{Steps: []string{"deps"}, Step: 1, FinishedAt: at, Error: "apt-get update: exit status 100", By: ByAgent}, "failed 2026-09-18 14:05 UTC (run by the agent) while installing the rental runtime (QEMU, UEFI firmware, cloud-image-utils, cryptsetup, nftables) (step 1 of 1): apt-get update: exit status 100"},
	} {
		if got := Describe(c.a); got != c.want {
			t.Errorf("Describe = %q\nwant %q", got, c.want)
		}
	}
}

func writeFile(path, content string) error { return os.WriteFile(path, []byte(content), 0644) }

// A DGX Spark with its desktop up and no test boot yet: the desktop is no
// reason for a person, the setup runs, and the host is told the desktop closes
// during the test.
func TestASparkWithItsDesktopUpIsSetUp(t *testing.T) {
	h := machine(t)
	const spark = "000f:01:00.0"
	h.Outputs["nvidia-smi --query-gpu=pci.bus_id,name"] = "0000000F:01:00.0, NVIDIA GB10, [N/A]\n"
	h.Outputs["nvidia-smi --query-gpu=driver_version"] = "580.95.05\n"
	delete(h.Outputs, "modinfo -F license nvidia")
	h.SetFile(vmrt.NVIDIAVersionFile, []byte("NVRM version: NVIDIA UNIX Open Kernel Module for aarch64  580.95.05\n"))
	h.PCI(spark, "nvidia", "0x030200", spark)
	for name, v := range map[string]string{"sys_vendor": "NVIDIA", "product_name": "NVIDIA DGX Spark", "product_family": "DGX Spark"} {
		h.SetFile("/sys/class/dmi/id/"+name, []byte(v+"\n"))
	}
	h.SetFile("/usr/share/AAVMF/AAVMF_CODE.fd", nil)
	h.SetFile("/usr/share/AAVMF/AAVMF_VARS.fd", nil)
	image(h, "resolute-server-cloudimg-arm64.img", "580-server-open")
	noTestBoot(h)
	h.SetLink("/proc/2558/fd/7", "/dev/nvidia0")
	h.SetFile("/proc/2558/comm", []byte("Xorg\n"))
	h.Outputs["systemctl show -p LoadState --value display-manager.service"] = "loaded\n"

	r := newRig(t, h)
	detect := func() *provisioner.Provisioner { return provisioner.Detect(h, "linux", "arm64", dataDir, version) }
	r.d.Runner.Detect, r.d.Runner.Arch, r.d.Prov = detect, "arm64", detect()
	r.d.Runner.TestBoot = func(context.Context, *vmrt.Runtime) vmrt.SelfTestResult {
		res, _ := json.Marshal(vmrt.SelfTestResult{Passed: true, AgentVersion: version, HostGPUs: []string{spark}})
		h.SetFile(vmrt.SelfTestPath(dataDir), res)
		return vmrt.SelfTestResult{Passed: true}
	}
	r.run()
	rs := r.reasons()
	if len(rs) != 2 || !strings.HasSuffix(rs[0], "This machine's desktop closes during the test and comes back after it.") || rs[1] != "" {
		t.Errorf("reported %q", rs)
	}
	if a := r.last(); a == nil || !a.Passed || a.Driver != "580-server-open" {
		t.Errorf("attempt = %+v", a)
	}
	if c := r.d.Prov.Capability(); !c.Ready || c.Identity == nil || !c.Identity.ConfirmedDGXSpark {
		t.Errorf("capability = %+v", c)
	}
}

// A machine the host removed from the marketplace is not set up.
func TestNotOnAWithdrawnMachine(t *testing.T) {
	h := machine(t)
	noImage(h)
	r := newRig(t, h)
	r.d.Prov.Withdraw("This machine was removed from the marketplace in the control panel.")
	r.run()
	if r.ran() != "" || r.last() != nil || len(r.reports) != 0 {
		t.Errorf("steps %q reports %v", r.ran(), r.reasons())
	}
}
