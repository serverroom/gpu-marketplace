// Package vmrt is the rental microVM runtime: it takes a host's GPUs away from
// it, boots a QEMU/KVM microVM with those GPUs passed through over VFIO on a
// fresh encrypted disk behind the netguard fence, lets the renter in with their
// SSH key, and on teardown destroys the VM and the disk and gives the GPUs back.
//
// Everything it does to the machine goes through Host, so each step and each
// failure path is exercised in tests without a machine. What tests cannot prove
// -- that a given GPU really works inside a VM on given hardware -- is what the
// self-test (`gpu-agent check --boot`) exists for, and a machine is not
// reported ready until it has passed one.
package vmrt

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Host is everything the runtime does to the machine. OSHost is the real one;
// internal/vmrt/fakehost is the one tests use.
type Host interface {
	Run(name string, args ...string) error
	RunInput(stdin []byte, name string, args ...string) error
	Output(name string, args ...string) (string, error)
	LookPath(name string) (string, error)
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, perm os.FileMode) error
	Readlink(path string) (string, error)
	Glob(pattern string) ([]string, error)
	Exists(path string) bool
	Remove(path string) error
	RemoveAll(path string) error
	Rename(from, to string) error
	MkdirAll(path string, perm os.FileMode) error
	DialTCP(addr string, timeout time.Duration) error
	Download(url, path string) error
	SHA256(path string) (string, error)
	Sleep(d time.Duration)
}

// OSHost acts on the real machine.
type OSHost struct{}

// sysfsWriteTimeout bounds one write. Unbinding a GPU that a process still
// holds blocks in the kernel until the holder lets go; the rental must fail,
// not hang the agent with it.
const sysfsWriteTimeout = 30 * time.Second

func describe(name string, args []string) string {
	return strings.TrimSpace(name + " " + strings.Join(args, " "))
}

func (OSHost) Run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", describe(name, args), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (OSHost) RunInput(stdin []byte, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", describe(name, args), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (OSHost) Output(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return string(out), fmt.Errorf("%s: %w: %s", describe(name, args), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return string(out), fmt.Errorf("%s: %w", describe(name, args), err)
	}
	return string(out), nil
}

func (OSHost) LookPath(name string) (string, error) { return exec.LookPath(name) }

func (OSHost) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (OSHost) WriteFile(path string, data []byte, perm os.FileMode) error {
	done := make(chan error, 1)
	go func() { done <- os.WriteFile(path, data, perm) }()
	select {
	case err := <-done:
		return err
	case <-time.After(sysfsWriteTimeout):
		return fmt.Errorf("write %s: no answer after %v", path, sysfsWriteTimeout)
	}
}

func (OSHost) Readlink(path string) (string, error) { return os.Readlink(path) }

func (OSHost) Glob(pattern string) ([]string, error) { return filepath.Glob(pattern) }

func (OSHost) Exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func (OSHost) Remove(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (OSHost) RemoveAll(path string) error { return os.RemoveAll(path) }

func (OSHost) Rename(from, to string) error { return os.Rename(from, to) }

func (OSHost) MkdirAll(path string, perm os.FileMode) error { return os.MkdirAll(path, perm) }

func (OSHost) DialTCP(addr string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

func (OSHost) Download(url, path string) error {
	resp, err := (&http.Client{Timeout: 60 * time.Minute}).Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("GET %s: %w", url, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func (OSHost) SHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (OSHost) Sleep(d time.Duration) { time.Sleep(d) }
