package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/kardianos/service"

	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/heartbeat"
	"github.com/serverroom/gpu-marketplace/internal/register"
	"github.com/serverroom/gpu-marketplace/internal/server"
	"github.com/serverroom/gpu-marketplace/internal/sshtunnel"
	"github.com/serverroom/gpu-marketplace/internal/stats"
)

var version = "dev"

type gpuAgent struct {
	cfg          *config.Config
	httpSrv      *server.Server
	heartbeat    *heartbeat.Heartbeat
	tunnelCancel context.CancelFunc
	controlSrv   *control.Server
	logger       service.Logger
}

func (a *gpuAgent) Start(s service.Service) error {
	a.logger.Info("GPU Agent starting...")

	// Bring up the persistent reverse SSH tunnel to the relay, if registered.
	tcfg, err := register.LoadTunnelConfig()
	if err != nil {
		a.logger.Warningf("load tunnel config: %v", err)
	} else if tcfg != nil {
		ctx, cancel := context.WithCancel(context.Background())
		a.tunnelCancel = cancel
		go sshtunnel.Supervise(ctx, *tcfg)
		a.logger.Infof("Reverse tunnel to %s started", tcfg.RelayHost)
	} else {
		a.logger.Info("No tunnel configured yet (run 'gpu-agent register' first)")
	}

	// Start the control channel on loopback; the control plane reaches it through
	// the relay tunnel and authenticates with the token shared at register time.
	if token := register.LoadControlToken(); token != "" {
		addr := fmt.Sprintf("127.0.0.1:%d", register.AgentControlPort)
		a.controlSrv = control.New(addr, token, control.NewStubProvisioner())
		if cerr := a.controlSrv.Start(); cerr != nil {
			a.logger.Warningf("start control channel: %v", cerr)
		}
	}

	// Legacy stats server + heartbeat (best-effort; superseded by push-over-tunnel).
	cfg, err := config.Load()
	if err == nil {
		a.cfg = cfg
		a.httpSrv = server.New(fmt.Sprintf(":%d", cfg.ListenPort))
		if serr := a.httpSrv.Start(); serr != nil {
			a.logger.Warningf("start http server: %v", serr)
		}
		if cfg.PeerID != "" && cfg.HubName != "" {
			for _, hub := range cfg.Hubs {
				if hub.Name == cfg.HubName {
					a.heartbeat = heartbeat.New(hub.Host, hub.Port, cfg.PeerID, 60*time.Second)
					a.heartbeat.Start()
					break
				}
			}
		}
	}

	a.logger.Info("GPU Agent started successfully")
	return nil
}

func (a *gpuAgent) Stop(s service.Service) error {
	a.logger.Info("GPU Agent stopping...")

	if a.tunnelCancel != nil {
		a.tunnelCancel()
	}
	if a.controlSrv != nil {
		a.controlSrv.Stop()
	}
	if a.heartbeat != nil {
		a.heartbeat.Stop()
	}
	if a.httpSrv != nil {
		a.httpSrv.Stop()
	}

	a.logger.Info("GPU Agent stopped")
	return nil
}

func main() {
	svcConfig := &service.Config{
		Name:        "gpu-agent",
		DisplayName: "GPU Marketplace Agent",
		Description: "GPU Marketplace agent: registers the host, keeps a reverse SSH tunnel to the relay, and serves the rental control channel",
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
			status, err := svc.Status()
			if err != nil {
				log.Fatalf("Status check failed: %v", err)
			}
			switch status {
			case service.StatusRunning:
				fmt.Println("Service is running")
			case service.StatusStopped:
				fmt.Println("Service is stopped")
			default:
				fmt.Println("Service status unknown")
			}
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
		agent.logger.Errorf("Run failed: %v", err)
	}
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

	if err := register.Run(*code); err != nil {
		log.Fatalf("Registration failed: %v", err)
	}
}

func printUsage() {
	fmt.Println("Usage: gpu-agent [command]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  register         Register this machine to the marketplace (--code)")
	fmt.Println("  select-location  Redo the location choice for an existing registration")
	fmt.Println("  install          Install as a system service")
	fmt.Println("  uninstall        Remove the system service")
	fmt.Println("  start            Start the service")
	fmt.Println("  stop             Stop the service")
	fmt.Println("  status           Check service status")
	fmt.Println("  test-stats       Collect and display system stats")
	fmt.Println("  -version         Print version")
	fmt.Println()
	fmt.Println("Run without arguments to start interactively (or as a service when managed by the OS).")
}
