package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/kardianos/service"

	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/netguard"
	"github.com/serverroom/gpu-marketplace/internal/pcidev"
	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/register"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// detectProvisioner is the one place the agent's provisioner is built. It runs
// the hosting checks, so the provisioner it returns refuses rentals with the
// reasons whenever this machine cannot host one.
func detectProvisioner() *provisioner.Provisioner {
	p := provisioner.Detect(vmrt.OSHost{}, runtime.GOOS, runtime.GOARCH, config.DataDir(), version)
	p.SetListingID(func() string { return register.LoadState().ListingID })
	return p
}

// printCapability writes the hosting half of status/check.
func printCapability(c control.Capability) {
	for i, g := range c.GPUs {
		label := "Rented GPUs:"
		if i > 0 {
			label = ""
		}
		var detail []string
		if g.PCIID != "" {
			detail = append(detail, g.PCIID)
		}
		if g.MemoryMB > 0 {
			detail = append(detail, fmt.Sprintf("%d MB", g.MemoryMB))
		}
		switch {
		case c.Kind == provisioner.KindContainer:
			detail = append(detail, "the host's driver, shared into the container")
		case c.Kind == provisioner.KindContainerWSL:
			detail = append(detail, "the Windows driver, shared into the container through WSL 2")
		case g.Driver != "":
			detail = append(detail, "driver "+g.Driver+" in the VM")
		default:
			detail = append(detail, "no driver in the VM")
		}
		fmt.Printf("%-14s%s (%s)\n", label, g.Model, strings.Join(detail, ", "))
	}
	if c.GPUCount != nil && *c.GPUCount == 0 {
		fmt.Println("GPU:          none — a rental on this machine gets its CPUs, memory and disk")
	}
	if c.UnifiedMemory {
		fmt.Println("GPU memory:   unified — the GPU has no memory of its own; the machine's memory is one pool used by both CPU and GPU, and a rental gets that pool")
	}
	if len(c.Excluded) > 0 {
		fmt.Println("Left out:     GPUs on this machine that rentals do not take:")
		for _, e := range c.Excluded {
			fmt.Printf("                - %s\n", e)
		}
	}
	if c.SelfTest != nil {
		fmt.Printf("Test boot:    %s\n", testBootLine(c))
	}
	if c.Ready {
		fmt.Println("Hosting:      ready — this machine can host a rental")
		if c.HostBusy != nil {
			fmt.Printf("In use:       %s\n", hostBusyLine(c.HostBusy))
		}
	} else {
		fmt.Println("Hosting:      not ready — the marketplace will not offer this machine to renters:")
		for _, r := range c.Reasons {
			fmt.Printf("                - %s\n", r)
		}
	}
	// A DGX Spark can also be half of a linked pair; say where that stands.
	if c.Identity != nil && c.Identity.ConfirmedDGXSpark && c.Interconnect != nil {
		if c.Interconnect.Ready {
			fmt.Println("Linked pair:  ready — this DGX Spark can be half of a linked pair")
		} else {
			fmt.Println("Linked pair:  not ready — 'sudo gpu-agent check --pair' says what is missing")
		}
	}
}

// hostBusyLine is what `status` says while the host uses what a rental would
// take.
func hostBusyLine(hb *control.HostBusy) string {
	use := vmrt.HostUse{Holders: hb.Holders, MemoryShortMB: hb.MemoryShortGB * 1024}
	return "In use by you: " + use.Describe() + "; the marketplace still offers this machine, and you are told when it is rented."
}

// testBootLine is the last test boot in one line.
func testBootLine(c control.Capability) string {
	st := c.SelfTest
	when := time.Unix(st.At, 0).UTC().Format("2006-01-02 15:04 UTC")
	noGPU := c.GPUCount != nil && *c.GPUCount == 0
	switch {
	case !st.Passed:
		return fmt.Sprintf("failed %s with agent %s ('sudo gpu-agent check --boot' shows why)", when, st.AgentVersion)
	case !st.GPUVerified:
		return fmt.Sprintf("passed %s with agent %s, without the GPU, which was in use; the GPU's handover is tested when the GPU is free, "+
			"and always before a rental starts", when, st.AgentVersion)
	case c.RetestPending:
		return fmt.Sprintf("passed %s with agent %s; agent %s tests again when the GPU is free, and always before a rental starts",
			when, st.AgentVersion, c.AgentVersion)
	case noGPU:
		return fmt.Sprintf("passed %s with agent %s", when, st.AgentVersion)
	}
	return fmt.Sprintf("passed %s with agent %s, the GPU handed over", when, st.AgentVersion)
}

// printPending says, for `status` and `check`, what a rental waiting for this
// machine asks of the host -- the same words the host was sent.
func printPending(p *provisioner.Provisioner) {
	pr, err := provisioner.LoadPending(vmrt.OSHost{}, config.DataDir())
	if err != nil || pr == nil {
		return
	}
	spark := false
	if spec, ok := p.RuntimeSpec(); ok {
		spark = spec.DesktopOnDemand
	}
	switch pr.State {
	case control.PendingWaiting:
		fmt.Printf("Rented:       %s\n", provisioner.RentedNotice(pr, spark, time.Local))
	case control.PendingStarting:
		fmt.Printf("Rented:       the rental %s is starting (first a test rental, when this agent version has not run one with the GPU)\n", pr.RentalID)
	case control.PendingFailed:
		line := fmt.Sprintf("the rental %s did not start: %s", pr.RentalID, pr.Reason)
		if pr.Detail != "" {
			line += " (" + pr.Detail + ")"
		}
		fmt.Printf("Last rental:  %s\n", line)
	}
}

// runCheck shows a provider, before anything is rented, whether this machine
// can host and exactly what a tenant would be fenced off from. Read-only --
// unless --boot, which runs a real test rental.
func runCheck(svc service.Service, args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	rules := fs.Bool("rules", false, "print the exact nftables rules a rental runs behind")
	boot := fs.Bool("boot", false, "boot a real test rental (with the GPU passed through, on a machine that has one), and record the result")
	yes := fs.Bool("yes", false, "with --boot: do not ask for confirmation")
	pair := fs.Bool("pair", false, "check whether this machine can be half of a linked pair of DGX Sparks (read-only); with --boot, run the pair test boot")
	jsonOut := fs.Bool("json", false, "with --pair: print the identity and interconnect report as JSON")
	minRDMA := fs.Float64("min-rdma-gbps", vmrt.DefaultMinRDMAGbps, "with --boot --pair: the RDMA write bandwidth every link must reach, in Gb/s")
	fs.Parse(args)

	if *pair && !*boot {
		runCheckPair(*jsonOut)
		return
	}
	if *pair {
		runPairSelfTest(svc, *yes, *minRDMA)
		return
	}
	if *boot {
		runSelfTest(svc, *yes)
		return
	}

	if register.LoadWithdrawn() != nil {
		fmt.Println(register.WithdrawnMessage)
		os.Exit(2)
	}
	p := detectProvisioner()
	c := p.Capability()
	printCapability(c)
	printPending(p)
	printSetup()

	fmt.Println()
	tenant := "a microVM"
	switch c.Kind {
	case provisioner.KindContainer:
		tenant = "a hardened container"
	case provisioner.KindContainerWSL:
		tenant = "a hardened container inside the agent's own WSL 2 VM,"
	}
	fmt.Println("While rented, the tenant runs in " + tenant + " attached only to the " + netguard.Bridge +
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
	if !hostsRentals() {
		fmt.Fprintln(os.Stderr, "a test boot needs a Linux KVM host, or Windows with WSL 2")
		os.Exit(1)
	}
	if !isAdmin() {
		fmt.Fprintf(os.Stderr, "check --boot needs administrator rights: %s\n", adminHint("check --boot"))
		os.Exit(1)
	}
	p := detectProvisioner()
	if p.IsContainer() {
		runContainerSelfTest(svc, yes, p)
		return
	}
	rt := p.Runtime()
	if rt.Present() {
		fmt.Fprintln(os.Stderr, "a rental (or the leftover of one) is on this machine; a test boot cannot run now")
		os.Exit(1)
	}
	var blocking []string
	for _, f := range p.Findings() {
		if f.Kind != provisioner.ReasonTestBoot {
			blocking = append(blocking, f.Text)
		}
	}
	if len(blocking) > 0 {
		fmt.Println("This machine cannot run a test boot yet:")
		for _, r := range blocking {
			fmt.Printf("  - %s\n", r)
		}
		os.Exit(2)
	}

	gpus := rt.Spec().GPUs
	// Nothing of the host's is stopped: a GPU its programs hold is left out of
	// the test, and a machine whose memory is in use gets a smaller test VM.
	plan := vmrt.PlanTest(vmrt.OSHost{}, rt.Spec(), true)
	if plan.Wait != "" {
		fmt.Printf("A test boot cannot run now: %s.\n", plan.Wait)
		os.Exit(2)
	}
	fmt.Println("This boots a test rental for a few minutes:")
	switch {
	case len(gpus) > 0 && plan.NoGPU:
		fmt.Printf("  - the GPU is in use on this machine (%s), so the test runs WITHOUT it: nothing of yours is stopped\n", strings.Join(plan.InUse, ", "))
		fmt.Println("  - a microVM is booted as a rental gets it, but without the GPU; it reports whether it reaches the internet, and that it CANNOT reach this machine or its network")
		fmt.Println("  - then it is destroyed and its disk key discarded")
		fmt.Println("  - a pass lets the machine be rented; the GPU's handover is tested when the GPU is free, and always before a rental starts")
	case len(gpus) > 0:
		fmt.Printf("  - the GPU (%s) is taken from this machine and given to a microVM\n", strings.Join(gpus, ", "))
		if rt.Spec().DesktopOnDemand {
			fmt.Println("  - this machine's desktop closes for the test (anything open on its screen closes with it) and comes back after it")
		}
		fmt.Println("  - the VM reports which GPUs it sees (any make passes once it is there), whether it reaches the internet, and that it CANNOT reach this machine or its network")
		fmt.Println("  - then it is destroyed, its disk key discarded, and the GPU given back and checked")
	default:
		fmt.Println("  - a microVM is booted with a share of this machine's CPUs, memory and disk, as a rental gets them")
		fmt.Println("  - the VM reports whether it reaches the internet, and that it CANNOT reach this machine or its network")
		fmt.Println("  - then it is destroyed and its disk key discarded")
	}
	if plan.MemoryMB > 0 {
		fmt.Printf("  - the test VM gets %.1f GB of memory, less than a rental's, because this machine's memory is in use\n", float64(plan.MemoryMB)/1024)
	}
	if !yes && !confirm("Run the test boot? [y/N]: ") {
		fmt.Println("Nothing was changed.")
		return
	}
	release, err := vmrt.AcquireBusy(vmrt.OSHost{}, config.DataDir(), os.Getpid(), "running a test boot")
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v; 'gpu-agent setup --status' shows where it is\n", err)
		os.Exit(1)
	}
	fmt.Println()
	// Ctrl-C ends the test the clean way: the VM is torn down and the GPU
	// given back and checked, instead of left running.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Println("Booting ... (the first boot can take several minutes; Ctrl-C stops the test cleanly)")
	res := rt.SelfTestWith(ctx, version, plan.TestOptions)
	release()
	if res.InUse != "" {
		fmt.Printf("NOT RUN: the GPU was taken while the test started (%s). Nothing was recorded; run the test again.\n", res.InUse)
		os.Exit(2)
	}
	if res.Passed {
		switch {
		case res.WithoutGPU:
			fmt.Printf("PASSED without the GPU: the VM booted, reached the internet, and could not reach %s.\n", strings.Join(res.Blocked, ", "))
			fmt.Println("  The machine can be rented on this pass; the GPU's handover is tested when the GPU is free, and always before a rental starts.")
		case len(gpus) > 0:
			fmt.Printf("PASSED: the VM saw %s, reached the internet, and could not reach %s.\n",
				strings.Join(res.GuestGPUs, "; "), strings.Join(res.Blocked, ", "))
		default:
			fmt.Printf("PASSED: the VM booted, reached the internet, and could not reach %s.\n", strings.Join(res.Blocked, ", "))
		}
		for _, note := range res.Notes {
			fmt.Printf("  Note: %s\n", note)
		}
		if _, err := svc.Status(); err == nil {
			if err := service.Control(svc, "restart"); err == nil {
				fmt.Println("The agent service was restarted so it reports this machine as ready.")
			}
		}
		return
	}
	if res.Stopped {
		fmt.Println("STOPPED: the test boot was stopped before it finished and its VM was removed cleanly. Nothing was")
		fmt.Println("recorded: this machine keeps the result of its last finished test boot.")
		os.Exit(130)
	}
	fmt.Println("FAILED:")
	for _, problem := range res.Problems {
		fmt.Printf("  - %s\n", problem)
	}
	noteCommand(control.AreaTestBoot, "the test boot run by hand ('gpu-agent check --boot') failed: "+strings.Join(res.Problems, "; "), "")
	os.Exit(2)
}

// runContainerSelfTest runs a container test boot: a hardened rental container
// with the GPU shared over CDI, behind the fence, on the encrypted volume; it
// reports what it saw, is torn down and wiped, and the verdict is recorded.
func runContainerSelfTest(svc service.Service, yes bool, p *provisioner.Provisioner) {
	crt := p.ContainerRuntime()
	if crt == nil {
		fmt.Fprintln(os.Stderr, "this machine does not host as a container")
		os.Exit(1)
	}
	if crt.Present() {
		fmt.Fprintln(os.Stderr, "a rental (or the leftover of one) is on this machine; a test boot cannot run now")
		os.Exit(1)
	}
	var blocking []string
	for _, f := range p.Findings() {
		if f.Kind != provisioner.ReasonTestBoot {
			blocking = append(blocking, f.Text)
		}
	}
	if len(blocking) > 0 {
		fmt.Println("This machine cannot run a test boot yet:")
		for _, r := range blocking {
			fmt.Printf("  - %s\n", r)
		}
		os.Exit(2)
	}
	fmt.Println("This starts a test rental as a hardened container for a minute:")
	if runtime.GOOS == "windows" {
		fmt.Println("  - it runs in the agent's own WSL 2 environment; the GPU, when there is one, is shared")
		fmt.Println("    into it through WSL 2 (Windows keeps the driver)")
	} else {
		fmt.Println("  - the GPU is shared into the container over CDI (the host keeps the driver)")
	}
	fmt.Println("  - it runs behind the fence, on an encrypted disk, and reports whether it reaches")
	fmt.Println("    the internet and that it CANNOT reach this machine or its network")
	fmt.Println("  - then it is destroyed and its disk key discarded")
	if !yes && !confirm("Run the test boot? [y/N]: ") {
		fmt.Println("Nothing was changed.")
		return
	}
	release, err := vmrt.AcquireBusy(vmrt.OSHost{}, config.DataDir(), os.Getpid(), "running a test boot")
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v; 'gpu-agent setup --status' shows where it is\n", err)
		os.Exit(1)
	}
	fmt.Println()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Println("Starting ... (the first run pulls and builds the image; Ctrl-C stops the test cleanly)")
	res := crt.SelfTestContext(ctx, version)
	release()
	if res.Passed {
		fmt.Printf("PASSED: the container ran, saw the GPU, reached the internet, and could not reach %s.\n", strings.Join(res.Blocked, ", "))
		if _, err := svc.Status(); err == nil {
			if err := service.Control(svc, "restart"); err == nil {
				fmt.Println("The agent service was restarted so it reports this machine as ready.")
			}
		}
		return
	}
	if res.Stopped {
		fmt.Println("STOPPED: the test was stopped before it finished and its container was removed cleanly.")
		os.Exit(130)
	}
	fmt.Println("FAILED:")
	for _, problem := range res.Problems {
		fmt.Printf("  - %s\n", problem)
	}
	noteCommand(control.AreaTestBoot, "the container test boot run by hand ('gpu-agent check --boot') failed: "+strings.Join(res.Problems, "; "), "")
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
	var gpus []string
	for _, d := range pcidev.Display(h) {
		gpus = append(gpus, d.BDF)
	}
	desktop, _ := vmrt.ClassifyGPUHolders(h, gpus)

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
	notRestarted, err := vmrt.CloseDesktop(h)
	if err != nil {
		fmt.Fprintf(os.Stderr, "the desktop could not be closed now (%v); restart the machine instead ('sudo reboot').\n", err)
		os.Exit(1)
	}
	for _, unit := range notRestarted {
		fmt.Fprintf(os.Stderr, "%s stopped with the desktop and did not start again; run 'sudo systemctl start %s'.\n", unit, unit)
	}
	if len(notRestarted) > 0 {
		os.Exit(1)
	}
}

// runPrepare installs the runtime's packages (with --install-deps) and bakes
// the rental base image.
func runPrepare(args []string) {
	fs := flag.NewFlagSet("runtime prepare", flag.ExitOnError)
	driver := fs.String("driver", "", "NVIDIA driver branch to bake into the rental image, e.g. 580-server-open (Turing and later) or 580-server (also Maxwell, Pascal and Volta), or none (default: match this machine's own driver; none on a machine without an NVIDIA GPU)")
	deps := fs.Bool("install-deps", false, "install QEMU, UEFI firmware, cloud-image-utils, cryptsetup and nftables with apt-get")
	headless := fs.Bool("headless", false, "make this machine run without a desktop, so its GPU is free to rent (closes the desktop now); does nothing else")
	yes := fs.Bool("yes", false, "with --headless: do not ask for confirmation")
	fs.Parse(args)

	if !hostsRentals() {
		fmt.Fprintln(os.Stderr, "the rental runtime runs on Linux and Windows only")
		os.Exit(1)
	}
	if !isAdmin() {
		fmt.Fprintf(os.Stderr, "runtime prepare needs administrator rights: %s\n", adminHint("runtime prepare"))
		os.Exit(1)
	}
	if *headless {
		runHeadless(*yes)
		return
	}
	if detectProvisioner().IsContainer() {
		runContainerPrepare(*deps)
		return
	}
	if err := provisioner.CheckBakeDriver(*driver); err != nil {
		fmt.Fprintf(os.Stderr, "runtime prepare: %v\n", err)
		os.Exit(1)
	}
	if *driver == "" && !provisioner.HasNVIDIAGPU(vmrt.OSHost{}) {
		fmt.Println("This machine has no NVIDIA GPU: the base image is built without the NVIDIA driver.")
		fmt.Println("If a GPU is added later, run 'sudo gpu-agent runtime prepare' again.")
	}
	release, err := vmrt.AcquireBusy(vmrt.OSHost{}, config.DataDir(), os.Getpid(), "building the rental image")
	if err != nil {
		fmt.Fprintf(os.Stderr, "runtime prepare: %v; 'gpu-agent setup --status' shows where it is\n", err)
		os.Exit(1)
	}
	spec := detectProvisioner().Runtime().Spec()
	fence := netguard.New(vmrt.OSHost{}, netguard.Bridge, vmrt.GuestSubnet, netguard.HostNetworks)
	err = vmrt.Prepare(vmrt.OSHost{}, spec, fence, version, vmrt.PrepareOptions{
		Driver:      *driver,
		InstallDeps: *deps,
		Log:         func(format string, a ...interface{}) { fmt.Printf(format+"\n", a...) },
	})
	release()
	if err != nil {
		fmt.Fprintf(os.Stderr, "runtime prepare failed: %v\n", err)
		os.Exit(1)
	}
}

// runContainerPrepare sets up a machine that hosts as a container (its GPU
// cannot be passed through to a microVM): with --install-deps it installs
// podman and the NVIDIA Container Toolkit, then it configures the host (CDI
// spec + user-namespace ranges) and builds the container rental image.
func runContainerPrepare(deps bool) {
	h, dataDir := machineHost()
	logf := func(format string, a ...interface{}) { fmt.Printf(format+"\n", a...) }
	release, err := vmrt.AcquireBusy(vmrt.OSHost{}, config.DataDir(), os.Getpid(), "preparing container hosting")
	if err != nil {
		fmt.Fprintf(os.Stderr, "runtime prepare: %v; 'gpu-agent setup --status' shows where it is\n", err)
		os.Exit(1)
	}
	defer release()
	if deps && runtime.GOOS == "windows" {
		if err := prepareWindows(logf)(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "runtime prepare failed: %v\n", err)
			os.Exit(1)
		}
	}
	if deps {
		if err := vmrt.InstallContainerPackages(h, logf); err != nil {
			fmt.Fprintf(os.Stderr, "runtime prepare failed: %v\n", err)
			os.Exit(1)
		}
	}
	spec, _ := detectProvisioner().RuntimeSpec()
	if err := vmrt.EnsureContainerHost(h, len(spec.GPUs) > 0, logf); err != nil {
		fmt.Fprintf(os.Stderr, "runtime prepare failed: %v\n", err)
		os.Exit(1)
	}
	if err := vmrt.PrepareContainerImage(h, dataDir, logf); err != nil {
		fmt.Fprintf(os.Stderr, "runtime prepare failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("The machine is set up for container hosting. Next: %s\n", adminHint("check --boot"))
}
