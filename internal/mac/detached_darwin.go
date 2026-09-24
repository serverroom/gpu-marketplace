//go:build darwin

package mac

import "syscall"

// detached puts the VM's QEMU in its own session, so it keeps running after the
// short-lived command that started it (the setup, a check) exits.
func detached() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
