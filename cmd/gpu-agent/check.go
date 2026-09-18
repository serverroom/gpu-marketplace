package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/kardianos/service"

	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/netguard"
	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// detectProvisioner is the one place the agent's provisioner is built. It runs
// the hosting checks, so the provisioner it returns refuses rentals with the
// reasons whenever this machine cannot host one.
func detectProvisioner() *provisioner.Provisioner {
	return provisioner.Detect(vmrt.OSHost{}, runtime.GOOS, runtime.GOARCH, config.DataDir(), version)
}

// printCapability writes the hosting half of status/check.
func printCapability(c control.Capability) {
	if c.UnifiedMemory {
		fmt.Println("GPU memory:   unified — the GPU has no memory of its own; the machine's memory is one pool used by both CPU and GPU, and a rental gets that pool")
	}
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
// can host and exactly what a tenant would be fenced off from. Read-only --
// unless --boot, which runs a real test rental.
func runCheck(svc service.Service, args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	rules := fs.Bool("rules", false, "print the exact nftables rules a rental runs behind")
	boot := fs.Bool("boot", false, "boot a real test rental with the GPU passed through, and record the result")
	yes := fs.Bool("yes", false, "with --boot: do not ask for confirmation")
	fs.Parse(args)

	if *boot {
		runSelfTest(svc, *yes)
		return
	}

	c := detectProvisioner().Capability()
	printCapability(c)

	fmt.Println()
	fmt.Println("While rented, the tenant runs in a microVM attached only to the " + netguard.Bridge +
		" bridge, behind one nftables table (inet " + netguard.Table + ") that the agent loads and reads back before booting anything. It blocks:")
	fmt.Println("  - every private and special range: 10/8, 172.16/12, 192.168/16, 100.64/10, 169.254/16, 127/8, multicast, fc00::/7, fe80::/10")
	fmt.Println("  - every network configured on this machine, whatever its addressing (your LAN, your NAS)")
	fmt.Println("  - every connection to this machine itself, except DHCP and IPv6 neighbour discovery")
	fmt.Println("  - every connection into the tenant from your network; the renter's SSH arrives through the agent's tunnel")
	fmt.Println("and lets the tenant out to the internet only, through NAT.")
	if nets, err := netguard.HostNetworks(); err == nil {
		fmt.Println()
		fmt.Println("Networks on this machine that would be blocked:")
		for _, n := range nets {
			fmt.Printf("  %s\n", n)
		}
		if *rules {
			fmt.Println()
			fmt.Print(netguard.Ruleset(netguard.Bridge, vmrt.GuestSubnet, nets))
		}
	}
	if !c.Ready {
		os.Exit(2)
	}
}

// runSelfTest boots one real rental on this machine -- GPU passed through, the
// fence, an encrypted disk, a key nobody holds -- reads what the VM saw, tears
// it down, and records the verdict that preflight requires before a machine is
// offered to renters.
func runSelfTest(svc service.Service, yes bool) {
	if runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, "a test boot needs a Linux KVM host")
		os.Exit(1)
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "check --boot needs root: run 'sudo gpu-agent check --boot'")
		os.Exit(1)
	}
	p := detectProvisioner()
	rt := p.Runtime()
	if rt.Present() {
		fmt.Fprintln(os.Stderr, "a rental (or the leftover of one) is on this machine; a test boot cannot run now")
		os.Exit(1)
	}
	var blocking []string
	for _, r := range p.Capability().Reasons {
		if !strings.Contains(r, "check --boot") {
			blocking = append(blocking, r)
		}
	}
	if len(blocking) > 0 {
		fmt.Println("This machine cannot run a test boot yet:")
		for _, r := range blocking {
			fmt.Printf("  - %s\n", r)
		}
		os.Exit(2)
	}

	fmt.Println("This boots a test rental for a few minutes:")
	fmt.Printf("  - the GPU (%s) is taken from this machine and given to a microVM; anything using it must be stopped\n", strings.Join(rt.Spec().GPUs, ", "))
	fmt.Println("  - the VM reports what GPU it sees, whether it reaches the internet, and that it CANNOT reach this machine or its network")
	fmt.Println("  - then it is destroyed, its disk key discarded, and the GPU given back and checked")
	if !yes && !confirm("Run the test boot? [y/N]: ") {
		fmt.Println("Nothing was changed.")
		return
	}
	fmt.Println()
	fmt.Println("Booting ... (the first boot can take several minutes)")
	res := rt.SelfTest(version)
	if res.Passed {
		fmt.Printf("PASSED: the VM saw %s, reached the internet, and could not reach %s.\n",
			strings.Join(res.GuestGPUs, "; "), strings.Join(res.Blocked, ", "))
		if _, err := svc.Status(); err == nil {
			if err := service.Control(svc, "restart"); err == nil {
				fmt.Println("The agent service was restarted so it reports this machine as ready.")
			}
		}
		return
	}
	fmt.Println("FAILED:")
	for _, problem := range res.Problems {
		fmt.Printf("  - %s\n", problem)
	}
	os.Exit(2)
}

// runHeadless switches this machine to start without a desktop and closes the
// running one, after saying exactly what happens and how to undo it. The
// desktop closes last: it may be the session this command was typed in.
func runHeadless(yes bool) {
	h := vmrt.OSHost{}
	dataDir := config.DataDir()
	current, err := vmrt.DefaultTarget(h)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runtime prepare --headless failed: %v\n", err)
		os.Exit(1)
	}
	desktop, _ := vmrt.ClassifyGPUHolders(h)

	fmt.Println("A machine that hosts rentals runs without a desktop: while a desktop is on the screen, it holds")
	fmt.Println("the GPU, and a rental cannot be given the GPU.")
	fmt.Println()
	fmt.Println("This will:")
	fmt.Printf("  1. make this machine start without a desktop from now on (it starts into %s today)\n", current)
	if len(desktop) > 0 {
		fmt.Printf("  2. close the desktop now: %s\n", strings.Join(desktop, ", "))
		fmt.Println("     Anything open on the screen closes with it. If you are typing this in a window on the")
		fmt.Println("     machine's own screen, reconnect over SSH (or log in on the text screen) to continue.")
	} else {
		fmt.Println("  2. switch the running system to the same mode (no desktop is using the GPU right now)")
	}
	fmt.Println()
	fmt.Println("Nothing else changes. 'sudo gpu-agent remove' brings the desktop back from the next start,")
	fmt.Printf("and so does 'sudo systemctl set-default %s'.\n", current)
	fmt.Println()
	if !yes && !confirm("Continue? [y/N]: ") {
		fmt.Println("Nothing was changed.")
		return
	}

	if _, err := vmrt.MakeHeadless(h, dataDir); err != nil {
		fmt.Fprintf(os.Stderr, "runtime prepare --headless failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("This machine now starts without a desktop.")
	fmt.Println("Next: sudo gpu-agent check --boot")
	os.Stdout.Sync()
	if err := vmrt.CloseDesktop(h); err != nil {
		fmt.Fprintf(os.Stderr, "the desktop could not be closed now (%v); restart the machine instead ('sudo reboot').\n", err)
		os.Exit(1)
	}
}

// runPrepare installs the runtime's packages (with --install-deps) and bakes
// the rental base image.
func runPrepare(args []string) {
	fs := flag.NewFlagSet("runtime prepare", flag.ExitOnError)
	driver := fs.String("driver", vmrt.DefaultDriver, "NVIDIA driver branch to bake into the rental image, e.g. 580-server-open or 580-server")
	deps := fs.Bool("install-deps", false, "install QEMU, UEFI firmware, cloud-image-utils, cryptsetup and nftables with apt-get")
	headless := fs.Bool("headless", false, "make this machine run without a desktop, so its GPU is free to rent (closes the desktop now); does nothing else")
	yes := fs.Bool("yes", false, "with --headless: do not ask for confirmation")
	fs.Parse(args)

	if runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, "the rental runtime runs on Linux only")
		os.Exit(1)
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "runtime prepare needs root: run 'sudo gpu-agent runtime prepare'")
		os.Exit(1)
	}
	if *headless {
		runHeadless(*yes)
		return
	}
	spec := detectProvisioner().Runtime().Spec()
	fence := netguard.New(vmrt.OSHost{}, netguard.Bridge, vmrt.GuestSubnet, netguard.HostNetworks)
	err := vmrt.Prepare(vmrt.OSHost{}, spec, fence, version, vmrt.PrepareOptions{
		Driver:      *driver,
		InstallDeps: *deps,
		Log:         func(format string, a ...interface{}) { fmt.Printf(format+"\n", a...) },
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "runtime prepare failed: %v\n", err)
		os.Exit(1)
	}
}
