package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/kardianos/service"

	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/interconnect"
	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/register"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// runCheckPair shows whether this machine can be half of a linked pair: what it
// is, its ConnectX-7 ports, every reason it is not ready, the peers heard on
// its ports, and its last pair test. Read-only: it sends no frame and changes
// no link.
func runCheckPair(jsonOut bool) {
	c := detectProvisioner().Capability()
	if jsonOut {
		data, _ := json.MarshalIndent(map[string]interface{}{"identity": c.Identity, "interconnect": c.Interconnect}, "", "  ")
		fmt.Println(string(data))
	} else {
		printPairCapability(os.Stdout, c, time.Now())
	}
	if c.Interconnect == nil || !c.Interconnect.Ready {
		os.Exit(2)
	}
}

func printPairCapability(w io.Writer, c control.Capability, now time.Time) {
	if id := c.Identity; id != nil {
		fmt.Fprintf(w, "Machine:      %s / %s (board %s, family %s)\n", orDash(id.SysVendor), orDash(id.ProductName), orDash(id.BoardName), orDash(id.ProductFamily))
		if id.ConfirmedDGXSpark {
			fmt.Fprintln(w, "Identity:     confirmed NVIDIA DGX Spark")
		} else {
			fmt.Fprintf(w, "Identity:     not confirmed as an NVIDIA DGX Spark — %s\n", id.Reason)
		}
	}
	ic := c.Interconnect
	if ic == nil {
		fmt.Fprintln(w, "Linked pair:  this agent has no pair runtime")
		return
	}
	if len(ic.Ports) == 0 {
		fmt.Fprintln(w, "ConnectX-7:   no ports found")
	} else {
		fmt.Fprintln(w, "ConnectX-7:")
		for _, p := range ic.Ports {
			link := "no cable"
			if p.Carrier {
				link = fmt.Sprintf("link up at %d GbE", p.SpeedMbps/1000)
			}
			fmt.Fprintf(w, "  %-15s %s  %s  %s, MTU %d, firmware %s, serial %s\n", p.Netdev, p.MAC, p.BDF, link, p.MTU, orDash(p.Firmware), orDash(p.Serial))
		}
	}
	if len(ic.Peers) > 0 {
		fmt.Fprintln(w, "Heard on the cable (other agents, last 24 h):")
		for _, p := range ic.Peers {
			fmt.Fprintf(w, "  listing %s  %s -> %s  %s ago\n", p.ListingID, p.LocalMAC, p.PeerMAC, now.Sub(time.Unix(p.SeenAt, 0)).Round(time.Minute))
		}
	}
	if t := ic.PairTest; t != nil {
		verdict := "FAILED"
		if t.Passed {
			verdict = "passed"
		}
		fmt.Fprintf(w, "Pair test:    %s with agent %s at %s; RDMA %v Gb/s over %s\n", verdict, t.AgentVersion,
			time.Unix(t.At, 0).UTC().Format(time.RFC3339), t.RDMAGbps, strings.Join(t.LocalMACs, ", "))
	}
	if ic.Ready {
		fmt.Fprintln(w, "Linked pair:  ready — this machine can be half of a linked pair")
		return
	}
	fmt.Fprintln(w, "Linked pair:  not ready:")
	for _, r := range ic.Reasons {
		fmt.Fprintf(w, "                - %s\n", r)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// runPairSelfTest runs the pair test boot on this machine; the host runs it on
// the other machine of the pair within ten minutes. Each finds the other on
// the cable, both boot a test VM with the GPU and the ConnectX card, the VMs
// measure every link against each other, and each machine records its verdict.
func runPairSelfTest(svc service.Service, yes bool, minRDMA float64) {
	if runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, "a pair test boot needs a Linux KVM host")
		os.Exit(1)
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "check --boot --pair needs root: run 'sudo gpu-agent check --boot --pair'")
		os.Exit(1)
	}
	if minRDMA <= 0 {
		fmt.Fprintln(os.Stderr, "--min-rdma-gbps must be above 0")
		os.Exit(1)
	}
	if register.LoadWithdrawn() != nil {
		fmt.Println(register.WithdrawnMessage)
		os.Exit(2)
	}
	p := detectProvisioner()
	rt := p.Runtime()
	if rt.Present() {
		fmt.Fprintln(os.Stderr, "a rental (or the leftover of one) is on this machine; a pair test boot cannot run now")
		os.Exit(1)
	}
	if blockers := p.PairTestBlockers(); len(blockers) > 0 {
		fmt.Println("This machine cannot run a pair test boot yet:")
		for _, r := range blockers {
			fmt.Printf("  - %s\n", r)
		}
		os.Exit(2)
	}

	spec := rt.Spec()
	fmt.Println("This boots a pair test rental for a few minutes, together with the other machine of the pair:")
	fmt.Printf("  - run the same command on the other machine within %v; this one waits for it on the ConnectX-7 cable\n", provisioner.PairTestWait)
	if len(spec.GPUs) > 0 {
		fmt.Printf("  - the GPU (%s) and the whole ConnectX card are taken from this machine and given to a microVM\n", strings.Join(spec.GPUs, ", "))
		if spec.DesktopOnDemand {
			fmt.Println("  - this machine's desktop closes for the test (anything open on its screen closes with it) and comes back after it")
		}
	} else {
		fmt.Println("  - the whole ConnectX card is taken from this machine and given to a microVM")
	}
	fmt.Println("  - the two VMs check every link carries 9000-byte frames and measure RDMA over it; each link must reach")
	fmt.Printf("    %g Gb/s; the VM also checks what it can reach, like 'check --boot'\n", minRDMA)
	fmt.Println("  - then it is destroyed, and the GPU and the card are given back and checked")
	if !yes && !confirm("Run the pair test boot? [y/N]: ") {
		fmt.Println("Nothing was changed.")
		return
	}
	// The machine is this command's for the test: the agent's automatic setup,
	// an update or another test boot waits for it, and an agent restart leaves
	// its VM alone.
	release, err := vmrt.AcquireBusy(vmrt.OSHost{}, config.DataDir(), os.Getpid(), "running a pair test boot")
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v; 'gpu-agent setup --status' shows where it is\n", err)
		os.Exit(1)
	}
	fmt.Println()
	// Ctrl-C (or the machine shutting down) ends the test the clean way: the
	// ports go back, and a test VM is torn down and the GPU and card checked.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Printf("Waiting for the other machine on the cable (up to %v; Ctrl-C stops the test cleanly) ...\n", provisioner.PairTestWait)
	res, err := p.PairSelfTest(provisioner.PairTestRun{Ctx: ctx, MinRDMAGbps: minRDMA, Found: func(plan interconnect.PairPlan) {
		fmt.Printf("Found listing %s over %d link(s); this machine is node %s. Booting (the first boot can take several minutes) ...\n",
			plan.PeerListing, len(plan.Links), plan.Node)
	}})
	release()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pair test boot: %v\n", err)
		os.Exit(1)
	}
	if res.Passed {
		var links []string
		for i, mac := range res.LocalMACs {
			links = append(links, fmt.Sprintf("%s at %.1f Gb/s", mac, res.RDMAGbps[i]))
		}
		fmt.Printf("PASSED: node %s, every link carried 9000-byte frames and RDMA: %s.\n", res.Node, strings.Join(links, "; "))
		if _, err := svc.Status(); err == nil {
			if err := service.Control(svc, "restart"); err == nil {
				fmt.Println("The agent service was restarted so it reports this machine as ready for a pair.")
			}
		}
		return
	}
	if res.Stopped {
		fmt.Println("STOPPED: the pair test boot was stopped before it finished and its VM was removed cleanly.")
		fmt.Println("Nothing was recorded: this machine keeps the result of its last finished pair test boot.")
		os.Exit(130)
	}
	fmt.Println("FAILED:")
	for _, problem := range res.Problems {
		fmt.Printf("  - %s\n", problem)
	}
	noteCommand(control.AreaTestBoot, "the pair test boot ('gpu-agent check --boot --pair') failed: "+strings.Join(res.Problems, "; "), "")
	os.Exit(2)
}
