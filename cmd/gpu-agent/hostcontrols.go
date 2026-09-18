package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/hostctl"
	"github.com/serverroom/gpu-marketplace/internal/netguard"
	"github.com/serverroom/gpu-marketplace/internal/register"
	"github.com/serverroom/gpu-marketplace/internal/update"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// runningBinary is the path of this agent's binary, symlinks resolved.
func runningBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}

func newUpdater(binary string) *update.Updater {
	return &update.Updater{
		GOOS:    runtime.GOOS,
		GOARCH:  runtime.GOARCH,
		Current: version,
		Binary:  binary,
		DataDir: config.DataDir(),
	}
}

// hostControls are the control panel's "Update agent" and "Remove from
// marketplace" for this agent.
func (a *gpuAgent) hostControls() *hostctl.Agent {
	return &hostctl.Agent{
		Version:          version,
		DataDir:          config.DataDir(),
		PID:              os.Getpid(),
		Host:             vmrt.OSHost{},
		Machine:          a.prov,
		Updater:          newUpdater(runningBinary()),
		WithdrawnMessage: register.WithdrawnMessage,
		MarkWithdrawn:    register.MarkWithdrawn,
		StopHosting:      a.stopHosting,
		// The answer to /withdrawn leaves through the tunnel that stopping
		// closes: give it a moment.
		Later: func(f func()) {
			go func() {
				time.Sleep(2 * time.Second)
				f()
			}()
		},
	}
}

// report sends a capability; an answer of 410 Gone means the host removed the
// machine in the control panel, and the agent stops hosting as if told so.
func (a *gpuAgent) report(c control.Capability) (*register.CapabilityResponse, error) {
	resp, err := register.ReportCapability(c)
	var ee *register.EndpointError
	if errors.As(err, &ee) && ee.Code == http.StatusGone && a.host != nil {
		a.host.Gone()
	}
	return resp, err
}

// stopHosting is what a withdrawal leaves running: nothing that hosts. The
// automatic setup stops (tearing its VM down), the reports and the tunnel
// stop, the control channel closes, and a rental fence a crash left loaded is
// removed. The binary and its config stay, so 'gpu-agent remove' still works.
func (a *gpuAgent) stopHosting() {
	a.say("%s", register.WithdrawnMessage)
	if a.bgCancel != nil {
		a.bgCancel()
		select {
		case <-a.bgDone:
		case <-time.After(stopGrace):
		}
	}
	if a.tunnelCancel != nil {
		a.tunnelCancel()
	}
	if a.controlSrv != nil {
		a.controlSrv.Stop()
	}
	if runtime.GOOS == "linux" && !a.prov.RentalPresent() {
		_ = exec.Command("nft", "delete", "table", "inet", netguard.Table).Run()
	}
}

// runUpdate is `gpu-agent update`: the control panel's update, in the
// foreground.
func runUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	to := fs.String("version", "", "the release to update to, e.g. v0.1.11 (default: the latest release)")
	fs.Parse(args)

	if runtime.GOOS != "linux" {
		exitf("updating the agent this way works on Linux hosts; run the installer again instead")
	}
	if os.Geteuid() != 0 {
		exitf("update needs root: run 'sudo gpu-agent update'")
	}
	u := newUpdater(runningBinary())
	if *to == "" {
		latest, err := u.Latest()
		if err != nil {
			exitf("could not ask GitHub for the latest release (%v); name one with --version", err)
		}
		*to = latest
	}
	if err := u.Check(*to); err != nil {
		exitf("%v", err)
	}
	p := detectProvisioner()
	if p.RentalPresent() {
		exitf("a rental (or the leftover of one) is on this machine; update the agent when it is gone")
	}
	if b := vmrt.BusyHolder(vmrt.OSHost{}, config.DataDir(), os.Getpid()); b != nil {
		exitf("another gpu-agent process is %s on this machine; update the agent when it has finished", b.What)
	}
	rec, err := u.Begin(*to)
	if err != nil {
		exitf("%v", err)
	}
	fmt.Printf("Updating the agent from %s to %s ...\n", rec.From, rec.To)
	rec = u.Run(rec)
	if rec.Status == update.StatusFailed {
		exitf("the update failed, and this agent (%s) is unchanged: %s", version, rec.Error)
	}
	fmt.Printf("Installed %s at %s; the previous version is kept as %s.prev.\n", rec.To, u.Binary, u.Binary)
	fmt.Println("The agent restarts in a few seconds. Three minutes later the previous version checks that it is")
	fmt.Println("running and puts itself back if it is not; 'sudo gpu-agent status' shows the outcome.")
}

// runUpdateCheck is the check three minutes after an update, run by the
// previous binary from a transient systemd timer.
func runUpdateCheck(args []string) {
	fs := flag.NewFlagSet("update-check", flag.ExitOnError)
	to := fs.String("to", "", "the version the update installed")
	binary := fs.String("binary", "", "the agent's binary")
	fs.Parse(args)
	if *to == "" {
		exitf("update-check needs --to")
	}
	if *binary == "" {
		*binary = strings.TrimSuffix(runningBinary(), ".prev")
	}
	rec := newUpdater(*binary).Verify(*to)
	fmt.Printf("update to %s: %s %s\n", rec.To, rec.Status, rec.Error)
	if rec.Status != update.StatusOK {
		os.Exit(1)
	}
}

// updateSummary is the last update for `status`.
func updateSummary() string {
	r := update.Load(config.DataDir())
	if r == nil {
		return ""
	}
	when := time.Unix(r.StartedAt, 0).UTC().Format("2006-01-02 15:04 UTC")
	line := fmt.Sprintf("%s -> %s started %s: %s", r.From, r.To, when, strings.ReplaceAll(r.Status, "_", " "))
	if r.Error != "" {
		line += " (" + r.Error + ")"
	}
	return line
}
