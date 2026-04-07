package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/kardianos/service"

	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/heartbeat"
	"github.com/serverroom/gpu-marketplace/internal/server"
	"github.com/serverroom/gpu-marketplace/internal/stats"
	"github.com/serverroom/gpu-marketplace/internal/tunnel"
)

var version = "dev"

type gpuAgent struct {
	cfg       *config.Config
	httpSrv   *server.Server
	heartbeat *heartbeat.Heartbeat
	logger    service.Logger
}

func (a *gpuAgent) Start(s service.Service) error {
	a.logger.Info("GPU Agent starting...")

	// Load config
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	a.cfg = cfg

	// Start HTTP stats server
	listenAddr := fmt.Sprintf(":%d", cfg.ListenPort)
	a.httpSrv = server.New(listenAddr)
	if err := a.httpSrv.Start(); err != nil {
		return fmt.Errorf("start http server: %w", err)
	}

	// Start heartbeat if we have a peer ID and hub configured
	if cfg.PeerID != "" && cfg.HubName != "" {
		for _, hub := range cfg.Hubs {
			if hub.Name == cfg.HubName {
				a.heartbeat = heartbeat.New(hub.Host, hub.Port, cfg.PeerID, 60*time.Second)
				a.heartbeat.Start()
				break
			}
		}
	}

	a.logger.Info("GPU Agent started successfully")
	return nil
}

func (a *gpuAgent) Stop(s service.Service) error {
	a.logger.Info("GPU Agent stopping...")

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
		Description: "Reports GPU server stats to the marketplace hub and maintains WireGuard tunnel",
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

		case "setup":
			runSetup()
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

func runSetup() {
	fmt.Println("GPU Marketplace Agent Setup")
	fmt.Println("===========================")
	fmt.Println()

	cfg := config.DefaultConfig()

	// Test latency to all hubs
	fmt.Println("Testing hub latency...")
	tunnel.TestAllHubs(cfg.Hubs)
	fmt.Println()

	// Select best hub
	hub, err := tunnel.SelectBestHub(cfg.Hubs)
	if err != nil {
		log.Fatalf("No hubs reachable: %v", err)
	}
	cfg.HubName = hub.Name

	// Register with hub
	fmt.Printf("Registering with hub %s...\n", hub.Name)
	reg, err := tunnel.Register(hub)
	if err != nil {
		log.Fatalf("Registration failed: %v\n\nMake sure the hub is running and accepting registrations.", err)
	}

	cfg.PeerID = reg.PeerID
	cfg.WireGuard = config.WireGuard{
		PrivateKey: reg.PrivateKey,
		Address:    reg.Address,
		HubPubKey:  reg.HubPubKey,
		Endpoint:   reg.Endpoint,
	}

	// Write WireGuard config
	if err := tunnel.WriteConfig(reg); err != nil {
		log.Fatalf("Failed to write WireGuard config: %v", err)
	}

	// Save agent config
	if err := config.Save(cfg); err != nil {
		log.Fatalf("Failed to save config: %v", err)
	}
	fmt.Printf("Config saved to %s\n", config.ConfigPath())

	// Bring up WireGuard
	fmt.Println("Starting WireGuard tunnel...")
	if err := tunnel.BringUp(); err != nil {
		log.Printf("Warning: Could not start WireGuard automatically: %v", err)
		fmt.Println("Please start WireGuard manually.")
	} else {
		fmt.Println("WireGuard tunnel is up!")
	}

	fmt.Println()
	fmt.Println("Setup complete! Now install and start the service:")
	fmt.Println("  gpu-agent install")
	fmt.Println("  gpu-agent start")
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

func printUsage() {
	fmt.Println("Usage: gpu-agent [command]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  setup       Run interactive setup (latency test, hub registration, WireGuard config)")
	fmt.Println("  install     Install as a system service")
	fmt.Println("  uninstall   Remove the system service")
	fmt.Println("  start       Start the service")
	fmt.Println("  stop        Stop the service")
	fmt.Println("  status      Check service status")
	fmt.Println("  test-stats  Collect and display system stats")
	fmt.Println("  -version    Print version")
	fmt.Println()
	fmt.Println("Run without arguments to start interactively (or as a service when managed by the OS).")
}
