package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
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
