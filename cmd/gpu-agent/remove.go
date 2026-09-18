package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kardianos/service"

	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/netguard"
	"github.com/serverroom/gpu-marketplace/internal/register"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// runRemove takes the agent off this machine completely and revokes it, so a
// provider never has to reinstall the OS to be sure it is gone. `uninstall`
// only removes the service definition and leaves the SSH key, the control token
// and the relay authorisation in place; this removes all of it.
func runRemove(svc service.Service, args []string) {
	fs := flag.NewFlagSet("remove", flag.ExitOnError)
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	fs.Parse(args)

	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "remove needs root: run 'sudo gpu-agent remove'")
		os.Exit(1)
	}

	exe, _ := os.Executable()
	st := register.LoadState()

	fmt.Println("This removes the GPU Marketplace agent from this machine completely:")
	fmt.Println("  1. withdraws this machine's listing and revokes its access to the relay")
	fmt.Println("  2. stops and uninstalls the gpu-agent service")
	fmt.Println("  3. deletes the rental firewall table and any rental disks the agent created")
	fmt.Printf("  4. deletes %s (the agent's SSH key, control token, registration and tunnel config)\n", config.ConfigDir())
	fmt.Printf("  5. deletes the agent binary %s\n", exe)
	fmt.Println("Your operating system, drivers, packages and data are not touched, and nothing needs reinstalling.")
	if !*yes && !confirm("Remove the agent? [y/N]: ") {
		fmt.Println("Nothing was changed.")
		return
	}
	fmt.Println()

	// 1. Revoke first, while the control token that proves who we are still exists.
	if st.Registered {
		out, err := register.Deregister()
		switch {
		case err != nil:
			fmt.Printf("Listing:  NOT withdrawn (%v).\n", err)
			fmt.Printf("          The agent's key is deleted below, so this machine can no longer connect;\n")
			fmt.Printf("          ask support to withdraw listing %s.\n", st.ListingID)
		case out.Note != "":
			fmt.Printf("Listing:  %s.\n", out.Note)
		default:
			fmt.Printf("Listing:  %s withdrawn", out.ListingID)
			if out.RelayRevoked {
				fmt.Println("; relay access revoked.")
			} else {
				fmt.Println("; the relay could not be reached, so support has been told to revoke its access.")
			}
		}
	} else {
		fmt.Println("Listing:  this machine was never registered; nothing to withdraw.")
	}

	// 2. The service.
	if _, err := svc.Status(); errors.Is(err, service.ErrNotInstalled) {
		fmt.Println("Service:  not installed.")
	} else {
		_ = service.Control(svc, "stop")
		if err := service.Control(svc, "uninstall"); err != nil {
			fmt.Printf("Service:  could not uninstall (%v).\n", err)
		} else {
			fmt.Println("Service:  stopped and uninstalled.")
		}
	}

	// 3. Anything a rental left behind: a running VM is stopped, its disk key
	// discarded and the GPU given back before the directories go.
	if runtime.GOOS == "linux" {
		if rt := detectProvisioner().Runtime(); rt != nil && rt.Present() {
			if res := rt.Stop(); res.Clean() {
				fmt.Println("Rental:   stopped; its disk is destroyed and the GPU is back with its driver.")
			} else {
				fmt.Printf("Rental:   stopped, but not everything verified (%s); reboot this machine to be sure the GPU is released.\n", strings.Join(res.Detail, "; "))
			}
		}
	}
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("nft"); err == nil {
			_ = exec.Command("nft", "delete", "table", "inet", netguard.Table).Run()
		}
	}
	// A machine the agent switched to start without a desktop starts with it
	// again. Read before the data directory, which holds the record, goes.
	if runtime.GOOS == "linux" {
		if restored, target, err := vmrt.RestoreDesktop(vmrt.OSHost{}, config.DataDir()); err != nil {
			fmt.Printf("Desktop:  could not restore it (%v); run 'sudo systemctl set-default graphical.target'.\n", err)
		} else if restored {
			fmt.Printf("Desktop:  this machine starts with its desktop again (%s) from its next start;\n", target)
			fmt.Println("          'sudo systemctl isolate graphical.target' brings it back now.")
		}
	}
	removeTree("Rentals:  ", config.DataDir())

	// 4. Credentials and state.
	removeTree("Config:   ", config.ConfigDir())
	if runtime.GOOS == "darwin" {
		os.Remove("/var/log/gpu-agent.out.log")
		os.Remove("/var/log/gpu-agent.err.log")
	}

	// 5. The binary itself. Windows will not delete a running executable.
	switch {
	case exe == "":
		fmt.Println("Binary:   could not find it; delete gpu-agent by hand.")
	case runtime.GOOS == "windows":
		fmt.Printf("Binary:   delete %s by hand once this window is closed (Windows keeps a running program locked).\n", exe)
	default:
		if err := os.Remove(exe); err != nil && !os.IsNotExist(err) {
			fmt.Printf("Binary:   could not delete %s (%v).\n", exe, err)
		} else {
			fmt.Printf("Binary:   %s deleted.\n", exe)
		}
	}

	fmt.Println()
	fmt.Println("The agent is gone. The only other thing the installer may have added is the OpenSSH client package, which is left in place.")
}

// removeTree deletes one of the agent's own directories, refusing anything that
// could be a system path if the config were ever wrong.
func removeTree(label, dir string) {
	clean := filepath.Clean(dir)
	if !strings.Contains(strings.ToLower(clean), "gpu-agent") {
		fmt.Printf("%srefusing to delete %s: it is not the agent's own directory.\n", label, clean)
		return
	}
	if _, err := os.Stat(clean); os.IsNotExist(err) {
		fmt.Printf("%s%s was already absent.\n", label, clean)
		return
	}
	if err := os.RemoveAll(clean); err != nil {
		fmt.Printf("%scould not delete %s (%v).\n", label, clean, err)
		return
	}
	fmt.Printf("%s%s deleted.\n", label, clean)
}

func confirm(prompt string) bool {
	fmt.Print(prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}
