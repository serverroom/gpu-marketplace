//go:build !windows

package main

import "os"

// isAdmin: the agent runs as root.
func isAdmin() bool { return os.Geteuid() == 0 }
