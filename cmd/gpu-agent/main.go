package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"time"

	"github.com/kardianos/service"

	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/register"
	"github.com/serverroom/gpu-marketplace/internal/server"
	"github.com/serverroom/gpu-marketplace/internal/sshtunnel"
	"github.com/serverroom/gpu-marketplace/internal/stats"
)

var version = "dev"

type gpuAgent struct {
	cfg          *config.Config
	httpSrv      *server.Server
	tunnelCancel context.CancelFunc
	controlSrv   *control.Server
	prov         *provisioner.Provisioner
	logger       service.Logger
}

func (a *gpuAgent) Start(s service.Service) error {
	a.say("GPU Agent %s starting...", version)

	// Bring up the persistent reverse SSH tunnel to the relay, if registered.
	tcfg, err := register.LoadTunnelConfig()
	switch {
	case err != nil:
		a.warn("load tunnel config: %v", err)
	case tcfg != nil:
		ctx, cancel := context.WithCancel(context.Background())
		a.tunnelCancel = cancel
		go sshtunnel.Supervise(ctx, *tcfg)
		a.say("Reverse tunnel to %s started", tcfg.RelayHost)
	default:
		// The state between installing and registering. It has no tunnel and
		// no listening ports, so from the outside it is indistinguishable from
		// a crashed agent — say what it is waiting for, every time.
		a.say("%s", idleReason(register.LoadState()))
	}

	// Whether this machine can host a rental is decided once, here, and every
	// rental request is answered from it. Up to v0.1.5 this was a stub that
	// answered success and created nothing.
	a.prov = detectProvisioner()
	if c := a.prov.Capability(); c.Ready {
		a.say("Hosting checks passed: this machine can host a rental")
	} else {
		for _, r := range c.Reasons {
			a.say("Cannot host a rental: %s", r)
		}
	}

	// Start the control channel on loopback; the control plane reaches it through
	// the relay tunnel and authenticates with the token shared at register time.
	if token := register.LoadControlToken(); token != "" {
		addr := fmt.Sprintf("127.0.0.1:%d", register.AgentControlPort)
		a.controlSrv = control.New(addr, token, a.prov)
		if cerr := a.controlSrv.Start(); cerr != nil {
			a.warn("start control channel: %v", cerr)
		}
		go a.reportCapability(a.prov.Capability())
	}

	// Legacy local stats server (best-effort; superseded by push-over-tunnel).
	// Loopback only: it answers without authentication, and a machine on a home
	// LAN must not describe itself to everyone else on that LAN.
	cfg, err := config.Load()
	if err == nil {
		a.cfg = cfg
		a.httpSrv = server.New(fmt.Sprintf("127.0.0.1:%d", cfg.ListenPort))
		if serr := a.httpSrv.Start(); serr != nil {
			a.warn("start http server: %v", serr)
		}
	}

	a.say("GPU Agent started successfully")
	return nil
}

// say logs one line at info level and, when running under a service manager,
// writes the same line to stderr.
//
// The stderr copy is the whole point. kardianos routes the non-interactive
// logger to syslog, which current macOS surfaces neither in the plist's
// StandardErrorPath nor in `log show` — so a daemon that logs only through it
// is completely silent: /var/log/gpu-agent.err.log stays 0 bytes and there is
// no way to tell "waiting to be registered" from "crashed on startup".
// launchd, systemd and the Windows service wrapper all capture stderr, so
// writing there too puts the reason where whoever is looking will find it.
// Interactively kardianos already writes to stderr — don't print twice.
func (a *gpuAgent) say(format string, args ...interface{}) {
	a.emit(false, format, args...)
}

func (a *gpuAgent) warn(format string, args ...interface{}) {
	a.emit(true, format, args...)
}

func (a *gpuAgent) emit(warning bool, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if warning {
		a.logger.Warning(msg)
	} else {
		a.logger.Info(msg)
	}
	if !service.Interactive() {
		fmt.Fprintf(os.Stderr, "%s gpu-agent: %s\n", time.Now().Format(time.RFC3339), msg)
	}
}

// idleReason is the one line that explains an agent with no tunnel: which of
// the two setup steps has not happened, and the exact command that does it.
// Shared by the daemon log and `gpu-agent status` so they cannot disagree.
func idleReason(st register.State) string {
	switch {
	case st.Unreadable:
		return fmt.Sprintf("registered, but %s could not be read — the state files are 0600, so try 'sudo gpu-agent status'", register.RegistrationPath())
	case !st.Registered:
		return "not registered — run 'gpu-agent register --code <code>' with a one-time code from your dashboard, then restart the service"
	default:
		return fmt.Sprintf("registered (listing %s) but no location assigned — run 'gpu-agent select-location' to finish, then restart the service", st.ListingID)
	}
}

// reportCapability tells the marketplace what the hosting checks found, so a
// machine is only ever offered to renters while it can actually host. Retries
// transient failures; a 4xx (an old control plane, a withdrawn listing) will
// not heal and is logged once.
func (a *gpuAgent) reportCapability(c control.Capability) {
	backoff := 5 * time.Second
	for attempt := 1; attempt <= 6; attempt++ {
		err := register.ReportCapability(c)
		if err == nil {
			a.say("Hosting capability reported to the marketplace (ready=%v)", c.Ready)
			return
		}
		var ee *register.EndpointError
		if errors.Is(err, register.ErrNotRegistered) || (errors.As(err, &ee) && ee.Code < 500) {
			a.warn("report hosting capability: %v", err)
			return
		}
		a.warn("report hosting capability (attempt %d): %v", attempt, err)
		time.Sleep(backoff)
		backoff *= 2
	}
}

func (a *gpuAgent) Stop(s service.Service) error {
	a.say("GPU Agent stopping...")

	if a.tunnelCancel != nil {
		a.tunnelCancel()
	}
	if a.controlSrv != nil {
		a.controlSrv.Stop()
	}
	if a.httpSrv != nil {
		a.httpSrv.Stop()
	}

	a.say("GPU Agent stopped")
	return nil
}

func main() {
	svcConfig := &service.Config{
		Name:        "gpu-agent",
		DisplayName: "GPU Marketplace Agent",
		Description: "GPU Marketplace agent: registers the host, keeps a reverse SSH tunnel to the relay, and serves the rental control channel",
		Option: service.KeyValue{
			// kardianos' launchd defaults are RunAtLoad=false with
			// KeepAlive=true — two keys pulling opposite ways. The
			// `launchctl load` behind `gpu-agent start` is told not to start
			// the job, and launchd then starts it anyway on its own schedule
			// because KeepAlive says it must always be running. Neither
			// `start` nor boot is deterministic, and if the agent ever exits
			// early that pair becomes a silent relaunch every 10 seconds.
			// This is a daemon: start on load, start at boot, restart if it
			// dies.
			"RunAtLoad": true,
			"KeepAlive": true,
		},
	}

	agent := &gpuAgent{}
	svc, err := service.New(agent, svcConfig)
	if err != nil {
		log.Fatalf("Failed to create service: %v", err)
	}

	agent.logger, err = svc.Logger(nil)
	if err != nil {
		log.Fatalf("Failed to create logger: %v", err)
	}

	// Parse CLI flags
	versionFlag := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("gpu-agent %s\n", version)
		os.Exit(0)
	}

	args := flag.Args()
	if len(args) > 0 {
		switch args[0] {
		case "install":
			if err := service.Control(svc, "install"); err != nil {
				log.Fatalf("Install failed: %v", err)
			}
			fmt.Println("Service installed successfully")
			return

		case "uninstall":
			if err := service.Control(svc, "uninstall"); err != nil {
				log.Fatalf("Uninstall failed: %v", err)
			}
			fmt.Println("Service uninstalled successfully")
			fmt.Println("The agent's key, token and registration are still on this machine and its listing is still live;")
			fmt.Println("'sudo gpu-agent remove' withdraws the listing and deletes everything.")
			return

		case "remove":
			runRemove(svc, args[1:])
			return

		case "check":
			runCheck(args[1:])
			return

		case "start":
			if err := service.Control(svc, "start"); err != nil {
				log.Fatalf("Start failed: %v", err)
			}
			fmt.Println("Service started")
			return

		case "stop":
			if err := service.Control(svc, "stop"); err != nil {
				log.Fatalf("Stop failed: %v", err)
			}
			fmt.Println("Service stopped")
			return

		case "status":
			runStatus(svc)
			return

		case "register":
			runRegister(args[1:])
			return

		case "select-location":
			if err := register.RetrySelection(); err != nil {
				log.Fatalf("Location selection failed: %v", err)
			}
			return

		case "test-stats":
			runTestStats()
			return

		default:
			fmt.Printf("Unknown command: %s\n", args[0])
			printUsage()
			os.Exit(1)
		}
	}

	// Run interactively or as a service
	if err := svc.Run(); err != nil {
		agent.emit(true, "Run failed: %v", err)
		os.Exit(1)
	}
}

// runStatus prints both halves of "is this thing working": whether the service
// manager has the daemon running, and whether the agent has been registered.
// They fail independently — a freshly installed agent is a healthy service with
// nothing to do — and reporting only the first is what made a normal
// not-yet-registered install read as a crash.
func runStatus(svc service.Service) {
	status, err := svc.Status()
	switch {
	case errors.Is(err, service.ErrNotInstalled):
		// Reliable at any privilege: this is a Stat of the plist/unit file,
		// which lives in a world-readable directory.
		fmt.Println("Service:      not installed")
	case err != nil:
		fmt.Printf("Service:      unknown (%v)\n", err)
	case status == service.StatusRunning:
		// Only reported when a PID was actually seen, so it is never a guess.
		fmt.Println("Service:      running")
	case status == service.StatusStopped && !serviceStateVisible(runtime.GOOS, os.Geteuid()):
		fmt.Println("Service:      unknown — re-run as 'sudo gpu-agent status'")
	case status == service.StatusStopped:
		fmt.Println("Service:      stopped")
	default:
		fmt.Println("Service:      unknown")
	}

	printCapability(detectProvisioner().Capability())

	st := register.LoadState()
	switch {
	case st.Registered && !st.Unreadable && st.HasTunnel:
		fmt.Printf("Registration: listing %s, location %s, tunnel configured\n", st.ListingID, locationOrUnassigned(config.LocationLabel(st.Location)))
		return
	case st.Unreadable:
		fmt.Println("Registration: registered, details unreadable")
	case st.Registered:
		fmt.Println("Registration: registered, no tunnel configured")
	default:
		fmt.Println("Registration: not registered")
	}
	fmt.Println()
	fmt.Println(idleReason(st))
}

// serviceStateVisible reports whether this process can actually observe the
// daemon's run state.
//
// On darwin it cannot unless it is root: the agent installs into launchd's
// system domain, and `launchctl list gpu-agent` from an unprivileged session
// answers "Could not find service" (exit 113) whether the daemon is running or
// not. kardianos sees no PID, sees the plist on disk, and reports Stopped — so
// a healthy daemon reads as stopped, which is the exact confusion this command
// exists to clear up. Say "unknown" instead of guessing wrong.
//
// Linux and Windows are fine: `systemctl is-active` and the service control
// manager both answer honestly to an unprivileged caller.
func serviceStateVisible(goos string, euid int) bool {
	return goos != "darwin" || euid == 0
}

func locationOrUnassigned(name string) string {
	if name == "" {
		return "unassigned"
	}
	return name
}

func runTestStats() {
	fmt.Println("Collecting system stats...")
	fmt.Println()

	st, err := stats.Collect()
	if err != nil {
		log.Fatalf("Failed to collect stats: %v", err)
	}

	data, _ := json.MarshalIndent(st, "", "  ")
	fmt.Println(string(data))
}

func runRegister(args []string) {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	code := fs.String("code", "", "one-time registration code from the dashboard")
	fs.Parse(args)

	if *code == "" {
		log.Fatal("register requires --code")
	}

	capability := detectProvisioner().Capability()
	if err := register.Run(*code, capability); err != nil {
		log.Fatalf("Registration failed: %v", err)
	}
	fmt.Println()
	printCapability(capability)
	if !capability.Ready {
		fmt.Println()
		fmt.Println("The listing is registered, but it will not be shown to renters until these are fixed.")
		fmt.Println("'gpu-agent check' re-runs the checks at any time.")
	}
}

func printUsage() {
	fmt.Println("Usage: gpu-agent [command]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  register         Register this machine to the marketplace (--code)")
	fmt.Println("  select-location  Redo the location choice for an existing registration")
	fmt.Println("  install          Install as a system service")
	fmt.Println("  check            Check whether this machine can host a rental, and what a tenant is fenced off from (--rules)")
	fmt.Println("  remove           Withdraw the listing, revoke relay access and delete the agent completely (--yes)")
	fmt.Println("  uninstall        Remove the system service only (keys and listing stay; see remove)")
	fmt.Println("  start            Start the service")
	fmt.Println("  stop             Stop the service")
	fmt.Println("  status           Check service status")
	fmt.Println("  test-stats       Collect and display system stats")
	fmt.Println("  -version         Print version")
	fmt.Println()
	fmt.Println("Run without arguments to start interactively (or as a service when managed by the OS).")
}
