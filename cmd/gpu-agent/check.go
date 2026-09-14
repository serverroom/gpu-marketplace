package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/netguard"
	"github.com/serverroom/gpu-marketplace/internal/provisioner"
)

// detectProvisioner is the one place the agent's provisioner is built. It runs
// the hosting checks, so the provisioner it returns refuses rentals with the
// reasons whenever this machine cannot host one.
func detectProvisioner() *provisioner.Provisioner {
	return provisioner.Detect(provisioner.ExecRunner{}, provisioner.OSProbe{}, runtime.GOOS,
		filepath.Join(config.DataDir(), "golden.img"),
		filepath.Join(config.DataDir(), "disks"),
		version)
}

// printCapability writes the hosting half of status/check.
func printCapability(c control.Capability) {
	if c.Ready {
		fmt.Println("Hosting:      ready — this machine can host a rental")
		return
	}
	fmt.Println("Hosting:      not ready — the marketplace will not offer this machine to renters:")
	for _, r := range c.Reasons {
		fmt.Printf("                - %s\n", r)
	}
}

// runCheck shows a provider, before anything is rented, whether this machine
// can host and exactly what a tenant would be fenced off from. Read-only.
func runCheck(args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	rules := fs.Bool("rules", false, "print the exact nftables rules a rental runs behind")
	fs.Parse(args)

	c := detectProvisioner().Capability()
	printCapability(c)

	fmt.Println()
	fmt.Println("While rented, the tenant runs in a microVM attached only to the " + netguard.Bridge +
		" bridge, behind one nftables table (inet " + netguard.Table + ") that the agent loads and reads back before booting anything. It blocks:")
	fmt.Println("  - every private and special range: 10/8, 172.16/12, 192.168/16, 100.64/10, 169.254/16, 127/8, multicast, fc00::/7, fe80::/10")
	fmt.Println("  - every network configured on this machine, whatever its addressing (your LAN, your NAS)")
	fmt.Println("  - every connection to this machine itself, except DHCP and IPv6 neighbour discovery")
	fmt.Println("  - every connection into the tenant from your network; the renter's SSH arrives through the agent's tunnel")
	if nets, err := netguard.HostNetworks(); err == nil {
		fmt.Println()
		fmt.Println("Networks on this machine that would be blocked:")
		for _, n := range nets {
			fmt.Printf("  %s\n", n)
		}
		if *rules {
			fmt.Println()
			fmt.Print(netguard.Ruleset(netguard.Bridge, nets))
		}
	}
	if !c.Ready {
		os.Exit(2)
	}
}
