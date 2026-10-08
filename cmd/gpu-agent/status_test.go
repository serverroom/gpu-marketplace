package main

import (
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/register"
)

// The idle line is the only thing a provider sees in an otherwise empty daemon
// log, so each state has to name the command that actually unblocks it.
func TestIdleReasonNamesTheNextCommand(t *testing.T) {
	tests := []struct {
		name  string
		state register.State
		want  string
	}{
		{
			name:  "fresh install",
			state: register.State{},
			want:  "gpu-agent register --code",
		},
		{
			name:  "registered but location assignment never finished",
			state: register.State{Registered: true, ListingID: "L-7"},
			want:  "gpu-agent select-location",
		},
		{
			name:  "state files present but unreadable",
			state: register.State{Registered: true, Unreadable: true},
			want:  "sudo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := idleReason(tt.state)
			if !strings.Contains(got, tt.want) {
				t.Errorf("idleReason() = %q, want it to mention %q", got, tt.want)
			}
		})
	}
}

// A machine with two types of core says which a rental runs on, and why; a
// machine with one says nothing.
func TestRentalCPUsLine(t *testing.T) {
	for name, c := range map[string]struct {
		guest *control.Guest
		want  string
	}{
		"a vendor kernel": {&control.Guest{VCPUs: 4, CPU: "Cortex-A76", CoresNote: "a rental runs on the 4 Cortex-A76 cores only: this machine's kernel (Linux 6.1.84-vendor-rk35xx) cannot run one VM on two types of core, which takes Linux 6.3 or later"},
			"Rental CPUs:  4, Cortex-A76. A rental runs on the 4 Cortex-A76 cores only: this machine's kernel (Linux 6.1.84-vendor-rk35xx) cannot run one VM on two types of core, which takes Linux 6.3 or later"},
		"both core types": {&control.Guest{VCPUs: 6, CPU: "4× Cortex-A76 + 2× Cortex-A55", CoresNote: "each vCPU runs on one core of its own, as this machine's test boot proved"},
			"Rental CPUs:  6, 4× Cortex-A76 + 2× Cortex-A55. Each vCPU runs on one core of its own, as this machine's test boot proved"},
		"one core type": {&control.Guest{VCPUs: 14, CPU: "AMD Ryzen 9 7950X 16-Core Processor"}, ""},
		"no guest":      {nil, ""},
	} {
		if got := rentalCPUsLine(c.guest); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// How a machine's memory is split, with what the machine uses of its own share
// when that can be read.
func TestRentalMemoryLine(t *testing.T) {
	for name, c := range map[string]struct {
		total, guest, avail int
		want                string
	}{
		"a 16 GB board, idle": {15718, 11622, 13084,
			"Rental memory: 11.3 GB of this machine's 15.3 GB; the machine keeps 4.0 GB for itself beside a rental, and its own system uses 2.6 GB of that now"},
		"while a rental holds memory": {15718, 11622, -1,
			"Rental memory: 11.3 GB of this machine's 15.3 GB; the machine keeps 4.0 GB for itself beside a rental"},
		"too small to rent": {3000, 0, 2000, ""},
		"unknown":           {0, 0, -1, ""},
	} {
		if got := rentalMemoryLine(c.total, c.guest, c.avail); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// "not registered" and "registered but stuck" are the two states that used to
// look identical from outside; they must not produce the same line.
func TestIdleReasonDistinguishesUnregisteredFromStuck(t *testing.T) {
	fresh := idleReason(register.State{})
	stuck := idleReason(register.State{Registered: true, ListingID: "L-7"})
	if fresh == stuck {
		t.Fatalf("both states produced the same line: %q", fresh)
	}
	if strings.Contains(fresh, "select-location") {
		t.Errorf("an unregistered agent was told to run select-location: %q", fresh)
	}
}

// A system daemon's run state is not readable without root on macOS, and
// reporting the resulting blind spot as "stopped" is how a healthy agent came
// to look broken in the first place.
func TestServiceStateVisibility(t *testing.T) {
	tests := []struct {
		goos string
		euid int
		want bool
	}{
		{"darwin", 501, false}, // the case that misreported a running daemon
		{"darwin", 0, true},
		{"linux", 1000, true}, // systemctl is-active answers unprivileged
		{"windows", 1000, true},
	}

	for _, tt := range tests {
		if got := serviceStateVisible(tt.goos, tt.euid); got != tt.want {
			t.Errorf("serviceStateVisible(%q, %d) = %v, want %v", tt.goos, tt.euid, got, tt.want)
		}
	}
}

// What `gpu-agent status` says about the host's own use and the last test
// boot, in the owner's words.
func TestStatusLinesForTheHostsUse(t *testing.T) {
	hb := &control.HostBusy{Holders: []string{"llama-server (pid 11435)", "llama-server (pid 11436)"}}
	if got := hostBusyLine(hb); got != "In use by you: llama-server; the marketplace still offers this machine, and you are told when it is rented." {
		t.Errorf("host busy = %q", got)
	}
	hb.MemoryShortGB = 12
	if got := hostBusyLine(hb); !strings.HasPrefix(got, "In use by you: llama-server, and 12 GB of the memory a rental needs;") {
		t.Errorf("host busy with memory = %q", got)
	}

	c := control.Capability{AgentVersion: "v0.2.3", SelfTest: &control.SelfTestSummary{Passed: true, At: 1789700000, AgentVersion: "v0.2.3"}, RetestPending: true}
	if got := testBootLine(c); !strings.Contains(got, "without the GPU, which was in use; the GPU's handover is tested when the GPU is free, and always before a rental starts") {
		t.Errorf("a pass without the GPU = %q", got)
	}
	c.SelfTest.GPUVerified, c.SelfTest.AgentVersion = true, "v0.2.2"
	if got := testBootLine(c); got != "passed 2026-09-18 02:53 UTC with agent v0.2.2; agent v0.2.3 tests again when the GPU is free, and always before a rental starts" {
		t.Errorf("an older version's pass = %q", got)
	}
	c.RetestPending = false
	if got := testBootLine(c); got != "passed 2026-09-18 02:53 UTC with agent v0.2.2, the GPU handed over" {
		t.Errorf("a full pass = %q", got)
	}
}
