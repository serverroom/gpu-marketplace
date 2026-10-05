package main

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/serving"
)

// runServe is `gpu-agent serve off|on|status`: the host's own switch for the
// inference API on this machine. The control panel turns serving on and picks
// the model; `off` here overrides it until `on` lifts the override.
func runServe(args []string) {
	word := "status"
	if len(args) > 0 {
		word = strings.ToLower(strings.TrimSpace(args[0]))
	}
	switch word {
	case "off":
		if err := serving.SetVeto(config.ConfigDir(), true); err != nil {
			exitf("could not write %s: %v", serving.VetoPath(config.ConfigDir()), err)
		}
		fmt.Println("Serving is switched off on this machine: it answers no inference requests and downloads no model,")
		fmt.Println("whatever the control panel says. Rentals are not affected. 'sudo gpu-agent serve on' leaves it to the")
		fmt.Println("control panel again. Weights already downloaded stay on disk:")
		fmt.Println("  " + serving.ModelsDir(config.StorageDir()))
	case "on":
		if err := serving.SetVeto(config.ConfigDir(), false); err != nil {
			exitf("could not remove %s: %v", serving.VetoPath(config.ConfigDir()), err)
		}
		fmt.Println("Serving on this machine is left to the control panel (Marketplace > your listing).")
	case "status":
		for _, line := range servingStatusLines() {
			fmt.Println(line)
		}
	default:
		exitf("serve takes off, on or status")
	}
}

// servingStatusLines describe serving on this machine: the switch, the panel's
// choice, and every model's weights.
func servingStatusLines() []string {
	if runtime.GOOS != "linux" {
		return []string{"Serving:      not available (Linux machines only)"}
	}
	lines := []string{"Serving:      " + servingSummary()}
	cache := &serving.Cache{Dir: serving.ModelsDir(config.StorageDir())}
	for _, m := range serving.Catalog {
		st := cache.Status(m)
		line := fmt.Sprintf("  %-14s weights %s (%.1f of %.1f GB)", m.ID, st.State, gib(st.Bytes), gib(st.Total))
		if st.Error != "" {
			line += ": " + st.Error
		}
		lines = append(lines, line)
	}
	return lines
}

// servingSummary is the one line `status` prints about serving.
func servingSummary() string {
	if serving.Vetoed(config.ConfigDir()) {
		return "switched off on this machine ('sudo gpu-agent serve on' leaves it to the control panel)"
	}
	m, why := serving.Wanted(serving.LoadOffer(config.DataDir()), false)
	if m.ID == "" {
		return "off: " + why
	}
	return "on in the control panel for " + m.ID + " ('sudo gpu-agent serve off' overrides it)"
}

func gib(b int64) float64 { return float64(b) / (1 << 30) }
