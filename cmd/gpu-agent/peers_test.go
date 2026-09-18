package main

import (
	"testing"
	"time"
)

// Two machines hear each other only while both announce, so every agent
// announces on the same UTC slots, whatever time zone or start time it has.
func TestPeerSlotsAreTheSameEverywhere(t *testing.T) {
	ny := time.FixedZone("EDT", -4*3600)
	for now, want := range map[time.Time]string{
		time.Date(2026, 9, 18, 3, 12, 5, 0, time.UTC):   "2026-09-18T06:00:00Z",
		time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC):    "2026-09-18T12:00:00Z",
		time.Date(2026, 9, 18, 23, 59, 59, 0, time.UTC): "2026-09-19T00:00:00Z",
		time.Date(2026, 9, 18, 13, 30, 0, 0, ny):        "2026-09-18T18:00:00Z",
	} {
		if got := nextPeerSlot(now).Format(time.RFC3339); got != want {
			t.Errorf("nextPeerSlot(%v) = %s, want %s", now, got, want)
		}
	}
}
