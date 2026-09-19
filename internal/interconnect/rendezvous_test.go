package interconnect_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/interconnect"
	"github.com/serverroom/gpu-marketplace/internal/interconnect/wiretest"
)

const pairRental = "5e1f00d2-8a3b-4c7d-9e0f-123456789abc"

type met struct {
	agreed, heard bool
	err           error
}

// meet runs one rendezvous window on the given sides at once.
func meet(t *testing.T, w *wiretest.Wire, rentals map[string]string, sides map[string]side) map[string]met {
	t.Helper()
	window, cutoff := 400*time.Millisecond, 100*time.Millisecond
	slot := time.Now().Add(20 * time.Millisecond)
	out := map[string]met{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, s := range sides {
		wg.Add(1)
		go func(name string, s side) {
			defer wg.Done()
			agreed, heard, err := interconnect.Rendezvous(context.Background(), s.h, w.Opener(name), s.ports, rentals[name], slot, window, cutoff, own(s), "")
			mu.Lock()
			out[name] = met{agreed, heard, err}
			mu.Unlock()
		}(name, s)
	}
	wg.Wait()
	return out
}

// Two ready halves of the same rental meet on the cable and both agree to
// start; a half alone hears nobody and does not start.
func TestRendezvousStartsBothHalvesOrNeither(t *testing.T) {
	fast(t)
	a, b, w := cabled()
	got := meet(t, w, map[string]string{"A": pairRental, "B": pairRental}, map[string]side{"A": a, "B": b})
	for name, m := range got {
		if m.err != nil || !m.agreed || !m.heard {
			t.Errorf("%s: %+v", name, m)
		}
	}
	if len(w.Opened()) != 0 {
		t.Errorf("sockets left open: %v", w.Opened())
	}

	got = meet(t, w, map[string]string{"A": pairRental}, map[string]side{"A": a})
	if m := got["A"]; m.err != nil || m.agreed || m.heard {
		t.Errorf("a half alone: %+v", m)
	}
}

// A half hears only its own rental's frames, made with the rental's id: the
// other half of another rental on the same cable, or a frame whose
// authenticator does not match, starts nothing.
func TestRendezvousHearsOnlyItsOwnRental(t *testing.T) {
	fast(t)
	a, b, w := cabled()
	other := "5e1f00d2-0000-4000-8000-000000000000" // same first 8, another rental
	got := meet(t, w, map[string]string{"A": pairRental, "B": other}, map[string]side{"A": a, "B": b})
	for name, m := range got {
		if m.agreed || m.heard {
			t.Errorf("%s agreed with another rental's half: %+v", name, m)
		}
	}
	payload := interconnect.StartPayload(pairRental, "58:a2:e1:00:01:01", true, 1789000020000, 3)
	if mac, heard, slot, ok := interconnect.ParseStart([]byte(payload), pairRental); !ok || !heard || mac != "58:a2:e1:00:01:01" || slot != 1789000020000 {
		t.Errorf("parse = %q %v %d %v", mac, heard, slot, ok)
	}
	forged := payload[:len(payload)-1] + "0"
	if payload[len(payload)-1] == '0' {
		forged = payload[:len(payload)-1] + "1"
	}
	if _, _, _, ok := interconnect.ParseStart([]byte(forged), pairRental); ok {
		t.Error("a frame with a wrong authenticator was accepted")
	}
}

// A half starts only on hearing the other say it heard this one: on a cable
// that carries frames one way, neither starts.
func TestRendezvousNeedsToBeHeardBothWays(t *testing.T) {
	fast(t)
	a, b, w := cabled()
	w.Cut("A/enp1s0f0np0", "B/enp1s0f0np0")
	w.Cut("A/enp1s0f1np1", "B/enp1s0f1np1")
	got := meet(t, w, map[string]string{"A": pairRental, "B": pairRental}, map[string]side{"A": a, "B": b})
	if m := got["A"]; m.agreed || !m.heard {
		t.Errorf("A, which hears B but is not heard: %+v", m)
	}
	if m := got["B"]; m.agreed || m.heard {
		t.Errorf("B, which hears nothing: %+v", m)
	}
}

func TestUntilRendezvousIsTheNextMinute(t *testing.T) {
	now := time.Date(2026, 9, 19, 11, 4, 42, 0, time.UTC)
	if d := interconnect.UntilRendezvous(now); d != 18*time.Second {
		t.Errorf("until the next slot: %v", d)
	}
}

// A half whose ports came up late in the slot can agree only while the other
// still counts: the window is the slot's on the clock. Two halves in different
// slots never agree.
func TestRendezvousWindowsAreTheSlots(t *testing.T) {
	fast(t)
	a, b, w := cabled()
	window, cutoff := 400*time.Millisecond, 100*time.Millisecond
	slot := time.Now().Add(20 * time.Millisecond)
	got := map[string]met{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	run := func(name string, s side, slot time.Time, delay time.Duration) {
		defer wg.Done()
		time.Sleep(delay)
		agreed, heard, err := interconnect.Rendezvous(context.Background(), s.h, w.Opener(name), s.ports, pairRental, slot, window, cutoff, own(s), "")
		mu.Lock()
		got[name] = met{agreed, heard, err}
		mu.Unlock()
	}
	// B opens its ports past A's cutoff: neither agrees (before, B agreed).
	wg.Add(2)
	go run("A", a, slot, 0)
	go run("B", b, slot, 350*time.Millisecond)
	wg.Wait()
	if got["A"].agreed || got["B"].agreed {
		t.Errorf("a late half agreed: %+v", got)
	}
	// Different slots: frames of another slot are not heard.
	got = map[string]met{}
	slot = time.Now().Add(20 * time.Millisecond)
	wg.Add(2)
	go run("A", a, slot, 0)
	go run("B", b, slot.Add(time.Millisecond), 0)
	wg.Wait()
	if got["A"].heard || got["B"].heard {
		t.Errorf("halves of different slots heard each other: %+v", got)
	}
}

// A half that has heard the other keeps answering to the end of the window
// when its agent stops, and reports the agreement it reached.
func TestARendezvousHeardIsNotAbandoned(t *testing.T) {
	fast(t)
	a, b, w := cabled()
	window, cutoff := 400*time.Millisecond, 100*time.Millisecond
	slot := time.Now().Add(20 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	got := map[string]met{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		agreed, heard, err := interconnect.Rendezvous(ctx, a.h, w.Opener("A"), a.ports, pairRental, slot, window, cutoff, own(a), "")
		mu.Lock()
		got["A"] = met{agreed, heard, err}
		mu.Unlock()
	}()
	go func() {
		defer wg.Done()
		agreed, heard, err := interconnect.Rendezvous(context.Background(), b.h, w.Opener("B"), b.ports, pairRental, slot, window, cutoff, own(b), "")
		mu.Lock()
		got["B"] = met{agreed, heard, err}
		mu.Unlock()
	}()
	time.Sleep(150 * time.Millisecond)
	cancel() // A's agent stops mid-window
	wg.Wait()
	if !got["A"].agreed || !got["B"].agreed {
		t.Errorf("an agreement was abandoned: %+v", got)
	}
}
