//go:build !windows

package wsl

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/serverroom/gpu-marketplace/internal/provisioner"
)

var errNotWindows = errors.New("WSL 2 hosting is for Windows")

// ErrRestart: Windows must restart before WSL 2 can run.
var ErrRestart = errors.New("Windows must restart to finish installing WSL 2")

// Options size the distribution's VM.
type Options struct {
	Arch   string
	MemMB  int
	CPUs   int
	Log    func(format string, args ...interface{})
	Stderr io.Writer
}

func Facts() provisioner.WinFacts { return provisioner.WinFacts{Problem: errNotWindows.Error()} }

func MemAvailableMB() int { return -1 }

func Setup(context.Context, Options) error { return errNotWindows }

func Exec([]byte, ...string) ([]byte, []byte, int, error) { return nil, nil, -1, errNotWindows }

func Dial(string) (net.Conn, error) { return nil, errNotWindows }

func Keep() func() { return func() {} }

func Remove() error { return nil }
