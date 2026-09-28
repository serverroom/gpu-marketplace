package provisioner

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/vmrt"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

// The rental disk: what is free less 20 GB, at most 500 GB unless the host
// chose a size (setup --rental-disk-gb), and never more than is free.
func TestTheRentalDiskSize(t *testing.T) {
	h := fakehost.New()
	h.Files["/data"] = nil
	h.Outputs["df --output=avail"] = " Avail\n  3400G\n"
	for _, c := range []struct{ chosen, want int }{{0, 500}, {2000, 2000}, {5000, 3380}, {100, 100}} {
		if got := rentalDiskGB(h, "/data", c.chosen); got != c.want {
			t.Errorf("chosen %d GB on 3400 GB free: %d GB, want %d", c.chosen, got, c.want)
		}
	}
	h.Outputs["df --output=avail"] = " Avail\n  15G\n"
	if got := rentalDiskGB(h, "/data", 2000); got != 0 {
		t.Errorf("15 GB free: %d, want 0", got)
	}
}

// guardStub counts looks and says when to report a stopped process.
type guardStub struct {
	mu    sync.Mutex
	looks int
	kill  bool
}

func (g *guardStub) GuardMemory() (vmrt.MemoryVerdict, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.looks++
	if g.kill {
		return vmrt.MemoryVerdict{KilledPID: 401, KilledMB: 61440, Reason: "over its limit"}, nil
	}
	return vmrt.MemoryVerdict{}, nil
}

// guardMachine is a Machine that guards its memory.
type guardMachine struct {
	Machine
	*guardStub
}

// The loop looks only where the GPU shares the machine's memory, says what it
// stopped, and ends with the agent.
func TestTheMemoryGuardLoop(t *testing.T) {
	old := GuardMemoryInterval
	GuardMemoryInterval = time.Millisecond
	t.Cleanup(func() { GuardMemoryInterval = old })

	for _, unified := range []bool{false, true} {
		g := &guardStub{kill: true}
		p := New(guardMachine{guardStub: g}, VendorContainerNV, nil, unified)
		var mu sync.Mutex
		var said []string
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			p.GuardMemory(ctx, func(format string, a ...interface{}) {
				mu.Lock()
				said = append(said, format)
				mu.Unlock()
			})
			close(done)
		}()
		time.Sleep(30 * time.Millisecond)
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the guard outlived the agent")
		}
		g.mu.Lock()
		looks := g.looks
		g.mu.Unlock()
		mu.Lock()
		spoke := len(said) > 0 && strings.Contains(said[0], "stopped the rental's GPU process")
		mu.Unlock()
		if unified && (looks == 0 || !spoke) {
			t.Errorf("unified memory: %d looks, said %q; want looks and the stop reported", looks, said)
		}
		if !unified && looks != 0 {
			t.Errorf("a GPU with its own memory: %d looks, want none", looks)
		}
	}
}
