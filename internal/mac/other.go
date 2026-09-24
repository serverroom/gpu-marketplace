//go:build !darwin

package mac

import (
	"context"
	"errors"
	"net"

	"github.com/serverroom/gpu-marketplace/internal/provisioner"
)

var errNotMac = errors.New("VM hosting on this OS is macOS-only")

// Options size the agent's VM.
type Options struct {
	Arch   string
	MemMB  int
	CPUs   int
	DiskGB int
	Log    func(format string, args ...interface{})
}

func Facts() provisioner.MacFacts { return provisioner.MacFacts{Problem: errNotMac.Error()} }

func MemAvailableMB() int { return -1 }

func Setup(context.Context, Options) error { return errNotMac }

func Boot(context.Context, Options) error { return errNotMac }

func Exec([]byte, ...string) ([]byte, []byte, int, error) { return nil, nil, -1, errNotMac }

func Dial(string) (net.Conn, error) { return nil, errNotMac }

func Keep() func() { return func() {} }

func Stop() {}

func Remove() error { return nil }
