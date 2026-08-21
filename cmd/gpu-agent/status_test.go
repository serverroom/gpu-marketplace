package main

import (
	"strings"
	"testing"

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
