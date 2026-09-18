package vmrt

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Building the base image, a test boot and the automatic setup each take the
// whole machine for minutes, and must never overlap -- not within the agent,
// and not between the agent and a command a person runs. The one running
// records itself in busy.json; a record whose process is gone, or which was
// written before the machine last started, is stale and ignored.

// Busy is the process running a machine-wide runtime operation.
type Busy struct {
	PID    int    `json:"pid"`
	BootID string `json:"boot_id"`
	What   string `json:"what"`
	At     int64  `json:"at"`
}

// ErrBusy is matched by the error AcquireBusy returns when another process
// holds the machine.
var ErrBusy = errors.New("another gpu-agent is working on this machine")

// BusyError names who holds the machine.
type BusyError struct{ Holder Busy }

func (e *BusyError) Error() string {
	return fmt.Sprintf("another gpu-agent process (pid %d) is %s on this machine, since %s",
		e.Holder.PID, e.Holder.What, time.Unix(e.Holder.At, 0).UTC().Format("15:04 UTC"))
}

func (e *BusyError) Is(target error) bool { return target == ErrBusy }

// BusyPath is where the record lives.
func BusyPath(dataDir string) string { return filepath.Join(dataDir, "busy.json") }

const bootIDFile = "/proc/sys/kernel/random/boot_id"

func bootID(h Host) string {
	data, _ := h.ReadFile(bootIDFile)
	return strings.TrimSpace(string(data))
}

// BusyHolder is the live process other than self that holds the machine, or
// nil when none does.
func BusyHolder(h Host, dataDir string, self int) *Busy {
	data, err := h.ReadFile(BusyPath(dataDir))
	if err != nil {
		return nil
	}
	var b Busy
	if json.Unmarshal(data, &b) != nil || b.PID <= 0 || b.PID == self {
		return nil
	}
	if b.BootID != bootID(h) || !h.Exists(fmt.Sprintf("/proc/%d", b.PID)) {
		return nil
	}
	return &b
}

// AcquireBusy records self as holding the machine for what ("building the
// rental image", ...). release gives it up. It fails with a *BusyError while
// another live process holds it.
func AcquireBusy(h Host, dataDir string, self int, what string) (release func(), err error) {
	if b := BusyHolder(h, dataDir, self); b != nil {
		return nil, &BusyError{Holder: *b}
	}
	rec := Busy{PID: self, BootID: bootID(h), What: what, At: time.Now().Unix()}
	data, _ := json.Marshal(rec)
	if err := h.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	if err := h.WriteFile(BusyPath(dataDir), data, 0600); err != nil {
		return nil, fmt.Errorf("record that this machine is busy: %w", err)
	}
	return func() {
		cur, err := h.ReadFile(BusyPath(dataDir))
		var b Busy
		if err == nil && json.Unmarshal(cur, &b) == nil && b.PID == self {
			_ = h.Remove(BusyPath(dataDir))
		}
	}, nil
}

// The rental ids the runtime uses for its own VMs: the base image build, test
// boots and pair test boots. None is anybody's rental.
const (
	BakeID         = "bake"
	SelfTestPrefix = "selftest-"
	// PairTestSuffix ends a pair test boot's id: "<8 hex>-pairtest", so the
	// VM still has the gpu-<8 hex>-a|b name a pair's VMs need.
	PairTestSuffix = "-pairtest"
)

// IsSetupID reports whether a rental id is one of the runtime's own VMs.
func IsSetupID(id string) bool {
	return id == BakeID || strings.HasPrefix(id, SelfTestPrefix) || strings.HasSuffix(id, PairTestSuffix)
}

// SetupVM reports whether the state on disk is a base image build or a test
// boot rather than a rental, and whether a live process other than this one
// is still running it. An agent that restarts mid-setup finds such a VM
// orphaned, and tears it down instead of taking it for a rental.
func (rt *Runtime) SetupVM() (present, owned bool) {
	st, err := LoadState(rt.h, rt.spec.DataDir)
	if err != nil || st == nil || !IsSetupID(st.RentalID) {
		return false, false
	}
	return true, BusyHolder(rt.h, rt.spec.DataDir, os.Getpid()) != nil
}
