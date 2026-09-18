// Package hostctl is the running agent's side of the host controls in the
// control panel: "Update agent" (POST /update) and removing a published
// machine (POST /withdrawn, or a capability report answered 410 Gone). It is
// the control channel's control.Host.
package hostctl

import (
	"net/http"
	"sync"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/update"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// Machine is what the controls ask of the provisioner.
type Machine interface {
	Status() string
	SettingUp() bool
	RentalPresent() bool
	Withdraw(reason string)
	Withdrawn() bool
}

// Agent implements control.Host.
type Agent struct {
	Version string
	DataDir string
	PID     int
	Host    vmrt.Host // for the busy record other agent processes leave
	Machine Machine
	Updater *update.Updater
	// WithdrawnMessage is the reason a withdrawn machine gives.
	WithdrawnMessage string
	// MarkWithdrawn records withdrawn.json (register.MarkWithdrawn).
	MarkWithdrawn func(by string) error
	// StopHosting stops the tunnel, the reports and the automatic setup, and
	// removes a rental fence left loaded. It runs once, in the background --
	// after the answer to /withdrawn has left through the tunnel it closes.
	StopHosting func()
	// Later runs f in the background once the answer has gone out; nil runs it
	// in a goroutine at once.
	Later func(f func())

	// ConfigDir holds the host's switch for automatic updates (autoupdate.go);
	// "" means they are on.
	ConfigDir string
	// Now is the clock (nil: time.Now); Log says what an automatic update did.
	Now func() time.Time
	Log func(format string, args ...interface{})

	once sync.Once

	autoMu    sync.Mutex
	lastCheck int64
	tried     map[string]time.Time
}

var _ control.Host = (*Agent)(nil)

func conflict(msg string) error { return &control.StatusError{Code: http.StatusConflict, Msg: msg} }

// AgentVersion is the running version.
func (a *Agent) AgentVersion() string { return a.Version }

// UpdateRecord is update.json, or nil.
func (a *Agent) UpdateRecord() interface{} {
	if r := update.Load(a.DataDir); r != nil {
		return r
	}
	return nil
}

func rented(status string) bool {
	switch status {
	case provisioner.StatusProvisioning, provisioner.StatusRented, provisioner.StatusWiping:
		return true
	}
	return false
}

// busy says why the machine must not be changed under the agent now, or "".
func (a *Agent) busy() string {
	switch {
	case rented(a.Machine.Status()):
		return "this machine is rented; update the agent when the rental ends"
	case a.Machine.SettingUp():
		return "the agent is setting this machine up (building its rental image or running its test rental); update it when that has finished"
	case a.Machine.RentalPresent():
		return "a rental's leftover, a test boot or an image build is on this machine; update the agent once it is gone"
	}
	if b := vmrt.BusyHolder(a.Host, a.DataDir, a.PID); b != nil {
		return "another gpu-agent process is " + b.What + " on this machine; update the agent when it has finished"
	}
	return ""
}

// Update checks the request synchronously -- a release version newer than
// this one, a release build, Linux, no rental, no setup or test boot, no other
// update -- and starts the update in the background.
func (a *Agent) Update(version string) (from, to string, err error) {
	if err := a.Updater.Check(version); err != nil {
		return "", "", err
	}
	if a.Machine.Withdrawn() {
		return "", "", conflict("this machine was removed from the marketplace; register it again first")
	}
	if why := a.busy(); why != "" {
		return "", "", conflict(why)
	}
	rec, err := a.Updater.Start(version)
	if err != nil {
		return "", "", err
	}
	return rec.From, rec.To, nil
}

// Withdraw is POST /withdrawn: the host removed this machine in the control
// panel. Refused while a rental is on it.
func (a *Agent) Withdraw() error { return a.withdraw("control panel") }

// Gone is a capability report answered 410: the same state, with nobody
// waiting for an answer.
func (a *Agent) Gone() { _ = a.withdraw("capability report") }

func (a *Agent) withdraw(by string) error {
	if rented(a.Machine.Status()) {
		return conflict("this machine is rented; it can be removed when the rental ends")
	}
	a.Machine.Withdraw(a.WithdrawnMessage)
	var err error
	if a.MarkWithdrawn != nil {
		err = a.MarkWithdrawn(by)
	}
	a.once.Do(func() {
		if a.StopHosting == nil {
			return
		}
		if a.Later != nil {
			a.Later(a.StopHosting)
		} else {
			go a.StopHosting()
		}
	})
	if err != nil {
		return &control.StatusError{Code: http.StatusInternalServerError,
			Msg: "this machine stopped hosting, but could not record it (" + err.Error() + "); it will host again after a restart"}
	}
	return nil
}
