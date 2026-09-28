package provisioner

import (
	"context"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// GuardMemoryInterval is how often a container rental's memory is looked at on
// a GPU that shares the machine's memory: a renter can allocate tens of GiB
// on the GPU in seconds, so this is quicker than the host-use look.
var GuardMemoryInterval = 500 * time.Millisecond

// memoryGuard is a runtime that holds its rental to its memory itself
// (vmrt.ContainerRuntime.GuardMemory).
type memoryGuard interface {
	GuardMemory() (vmrt.MemoryVerdict, error)
}

// GuardMemory keeps a container rental on a unified-memory GPU (a GB10) within
// its memory until ctx ends: the rental's own memory plus its GPU memory, and
// the machine's free memory, are looked at twice a second, and the rental's
// biggest GPU process is stopped when either is past its line (vmrt/memguard.go).
// log says which process was stopped and why; a failure to look is said once
// until it clears. The runtime is read afresh every time -- a re-check can
// replace it -- and a machine it does not apply to costs one lock a look.
func (p *Provisioner) GuardMemory(ctx context.Context, log func(format string, args ...interface{})) {
	lastErr := ""
	for {
		p.mu.Lock()
		g, ok := p.machine.(memoryGuard)
		ok = ok && p.unified && !p.withdrawn
		p.mu.Unlock()
		if ok {
			v, err := g.GuardMemory()
			switch {
			case err != nil && err.Error() != lastErr:
				lastErr = err.Error()
				if log != nil {
					log("Rental memory limit: %v", err)
				}
			case err == nil:
				lastErr = ""
			}
			if v.KilledPID > 0 && log != nil {
				log("Rental memory limit: %s; stopped the rental's GPU process %d (%d MiB on the GPU)", v.Reason, v.KilledPID, v.KilledMB)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(GuardMemoryInterval):
		}
	}
}
