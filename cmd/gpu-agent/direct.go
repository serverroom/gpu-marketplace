package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/direct"
	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/register"
	"github.com/serverroom/gpu-marketplace/internal/sshtunnel"
)

// How renters reach the machine (internal/direct): through our relay, or
// straight at its own public IPv4 address, IPv6 address, or both, as its host
// chooses in the control panel. The agent reports the public addresses the
// machine leaves by and applies the route the marketplace answers. It reports
// when it starts, when the addresses change, when the marketplace asks
// (POST /connection/check, after the host picked another route), after a
// change that had to wait for a rental, and at least every directRefresh.
const (
	directFirst   = 30 * time.Second
	directEvery   = 10 * time.Minute
	directRefresh = 6 * time.Hour
)

// directState is what the agent last told the marketplace and heard back.
type directState struct {
	ipv4, ipv6 string    // the addresses last reported
	at         time.Time // when
	choice     string    // the host's choice, as the last answer said
	again      bool      // report again on the next round: a change waits, or the report failed
}

// startTunnel brings up the reverse tunnel to the relay.
func (a *gpuAgent) startTunnel(cfg sshtunnel.Config) {
	ctx, cancel := context.WithCancel(context.Background())
	a.netMu.Lock()
	old := a.tunnelCancel
	a.tunnelCancel = cancel
	a.netMu.Unlock()
	if old != nil {
		old()
	}
	go sshtunnel.SuperviseWith(ctx, cfg, tunnelWatch{errs: a.ops.errs, host: cfg.RelayHost})
	a.say("Reverse tunnel to %s started", cfg.RelayHost)
}

// stopTunnel closes the reverse tunnel, if it is up.
func (a *gpuAgent) stopTunnel() {
	a.netMu.Lock()
	cancel := a.tunnelCancel
	a.tunnelCancel = nil
	a.netMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *gpuAgent) tunnelUp() bool {
	a.netMu.Lock()
	defer a.netMu.Unlock()
	return a.tunnelCancel != nil
}

// openPublic serves the control channel on addrs (host:port each). The
// rental's SSH moves there (provisioner.SetSSHListen) only once the
// marketplace has accepted the route: until then a renter's command names the
// relay.
func (a *gpuAgent) openPublic(addrs []string) error {
	cert, _, err := direct.Cert(config.ConfigDir())
	if err != nil {
		return fmt.Errorf("its certificate could not be read: %w", err)
	}
	return a.controlSrv.StartPublic(addrs, direct.TLSConfig(cert))
}

// directLoop looks at the machine's addresses until ctx ends.
func (a *gpuAgent) directLoop(ctx context.Context) {
	wait := directFirst
	force := true
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		a.checkDirect(force)
		wait, force = directEvery, false
	}
}

// checkDirect reports the machine's public addresses when they changed, when
// force is set, or when a report is due, and applies the route the answer
// gives. One at a time: the loop and a nudge from the marketplace can meet.
func (a *gpuAgent) checkDirect(force bool) {
	a.directMu.Lock()
	defer a.directMu.Unlock()
	if register.LoadWithdrawn() != nil || a.controlSrv == nil {
		return
	}
	host := register.MarketplaceHost()
	if host == "" {
		return
	}
	ipv4, ipv6 := direct.Detect(host)
	st := a.direct
	if !force && !st.again && st.ipv4 == ipv4 && st.ipv6 == ipv6 && time.Since(st.at) < directRefresh {
		return
	}
	current, err := direct.Load(config.ConfigDir())
	if err != nil {
		a.warn("read the direct route: %v", err)
	}

	var report *direct.Report
	if ipv4 != "" || ipv6 != "" {
		_, fingerprint, cerr := direct.Cert(config.ConfigDir())
		if cerr != nil {
			a.warn("make the certificate for the public addresses: %v", cerr)
			return
		}
		report = &direct.Report{IPv4: ipv4, IPv6: ipv6, ControlPort: direct.ControlPort, SSHPort: direct.SSHPort, CertSHA256: fingerprint}
		// Listening first, on every address: the marketplace reaches /health
		// there before it answers. Not when the host chose the relay: nothing
		// would be checked, so nothing is opened.
		if st.choice != direct.Relay {
			addrs := direct.Route{IPv4: ipv4, IPv6: ipv6, ControlPort: direct.ControlPort}.ControlAddrs()
			if oerr := a.openPublic(addrs); oerr != nil {
				a.say("This machine has a public address (%s), but port %d could not be opened on it: %v. Renters reach it through our relay",
					strings.Join(report.Addrs(), " and "), direct.ControlPort, oerr)
				a.restorePublic(current)
				report = nil
			}
		}
	}

	answer, err := register.ReportDirect(report)
	a.direct = directState{ipv4: ipv4, ipv6: ipv6, at: time.Now(), choice: st.choice, again: err != nil}
	if err != nil {
		a.restorePublic(current)
		a.warn("report the public addresses: %v", err)
		return
	}
	a.direct.choice = answer.Choice
	a.direct.again = answer.Pending != ""
	for family, why := range answer.Refused {
		a.say("The marketplace could not use this machine's public %s address: %s. To have renters connect there, let TCP ports %d and %d in from the internet",
			strings.ToUpper(family[:2])+family[2:], why, direct.ControlPort, direct.SSHPort)
	}
	if answer.Pending != "" {
		a.say("A route change waits: %s", answer.Pending)
	}
	a.applyRoute(answer, current)
}

// restorePublic puts the public listeners back as the current route has them:
// none on the relay.
func (a *gpuAgent) restorePublic(current *direct.Route) {
	if current == nil {
		a.controlSrv.StopPublic()
		return
	}
	if err := a.openPublic(current.ControlAddrs()); err != nil {
		a.warn("reopen the control channel on %s: %v", strings.Join(current.Addrs(), " and "), err)
	}
}

// applyRoute makes the machine use the route the marketplace answered.
func (a *gpuAgent) applyRoute(answer *register.DirectAnswer, current *direct.Route) {
	dir := config.ConfigDir()
	if answer.Connection != direct.Relay && answer.Direct != nil {
		route := *answer.Direct
		if err := a.openPublic(route.ControlAddrs()); err != nil {
			a.warn("open the control channel on %s: %v", strings.Join(route.Addrs(), " and "), err)
		}
		provisioner.SetSSHListen(route.SSHAddrs()...)
		if err := direct.Save(dir, route); err != nil {
			a.warn("save the direct route: %v", err)
		}
		if err := register.DropTunnel(); err != nil {
			a.warn("remove the tunnel config: %v", err)
		}
		a.stopTunnel()
		a.ops.errs.Resolve(control.AreaTunnel, "")
		if current == nil || current.Connection != route.Connection || strings.Join(current.Addrs(), ",") != strings.Join(route.Addrs(), ",") {
			a.say("%s: renters and the marketplace reach this machine at %s, with no tunnel", direct.Labels[route.Connection], strings.Join(route.Addrs(), " and "))
		}
		return
	}

	// The relay.
	a.controlSrv.StopPublic()
	provisioner.SetSSHListen()
	if err := direct.Remove(dir); err != nil {
		a.warn("remove the direct route: %v", err)
	}
	tunnel := answer.Tunnel
	if tunnel == nil && answer.SelectURL != "" {
		t, err := register.JoinRelay(answer)
		if err != nil {
			a.ops.errs.Raise(control.AreaTunnel, "this machine has to connect through our relay now, and the agent could not get a tunnel yet, so rentals cannot reach it; it tries again in a few minutes", err.Error())
			a.direct.again = true
			return
		}
		tunnel = t
	}
	if tunnel != nil {
		if err := register.SaveTunnel(tunnel); err != nil {
			a.warn("save the tunnel config: %v", err)
			a.direct.again = true
			return
		}
	}
	if tunnel != nil || !a.tunnelUp() {
		cfg, err := register.LoadTunnelConfig()
		if err != nil || cfg == nil {
			a.ops.errs.Raise(control.AreaTunnel, "this machine has to connect through our relay, and the agent has no tunnel settings for it, so rentals cannot reach it; it asks again in a few minutes", fmt.Sprint(err))
			a.direct.again = true
			return
		}
		a.startTunnel(*cfg)
	}
	if current != nil {
		a.say("Private relay: renters and the marketplace reach this machine through our relay again")
	}
}
