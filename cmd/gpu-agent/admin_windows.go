//go:build windows

package main

import "golang.org/x/sys/windows"

// isAdmin: the agent runs elevated (an Administrator PowerShell, or the
// service as LocalSystem).
func isAdmin() bool { return windows.GetCurrentProcessToken().IsElevated() }
