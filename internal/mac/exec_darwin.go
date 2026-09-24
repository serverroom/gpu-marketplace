//go:build darwin

package mac

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// sshBase are the ssh options for reaching the VM on loopback: the agent's key
// only, the VM's own known_hosts, no agent or password fallback, and a short
// connect timeout.
func sshBase(p vmPaths) []string {
	return []string{
		"-i", p.keyPath(),
		"-p", strconv.Itoa(GuestSSHPort),
		"-o", "IdentitiesOnly=yes",
		"-o", "UserKnownHostsFile=" + p.knownHosts(),
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=20",
	}
}

// decodeWrapper runs, inside the VM, a NUL-separated argv handed to it as one
// base64 argument: no argument is ever split by the remote shell. It has no
// single quotes, so the whole command is one single-quoted ssh argument.
const decodeWrapper = `mapfile -d "" -t a < <(printf %s "$0" | base64 -d); exec "${a[@]}"`

// Exec is the vmrt.ExecHost transport for the agent's VM: it runs argv as root
// in the VM over ssh, feeding stdin and returning stdout, stderr and the exit
// code. ErrNoDistro until the VM is up.
func Exec(stdin []byte, argv ...string) ([]byte, []byte, int, error) {
	p := paths()
	if !running(p) {
		return nil, nil, -1, vmrt.ErrNoDistro
	}
	var joined bytes.Buffer
	for _, a := range argv {
		joined.WriteString(a)
		joined.WriteByte(0)
	}
	enc := base64.StdEncoding.EncodeToString(joined.Bytes())
	remote := "bash -c '" + decodeWrapper + "' " + enc
	args := append(sshBase(p), "root@127.0.0.1", remote)
	cmd := exec.Command("ssh", args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		// 255 is ssh's own "could not connect", not the command's exit.
		if ee.ExitCode() == 255 && strings.Contains(errOut.String(), "Connection") {
			return out.Bytes(), errOut.Bytes(), -1, fmt.Errorf("reach the VM: %s", strings.TrimSpace(errOut.String()))
		}
		return out.Bytes(), errOut.Bytes(), ee.ExitCode(), nil
	case err != nil:
		return out.Bytes(), errOut.Bytes(), -1, fmt.Errorf("ssh to the VM: %w", err)
	}
	return out.Bytes(), errOut.Bytes(), 0, nil
}

// Dial opens a stream to addr from inside the VM (the renter's SSH to the
// container at GuestIP:22, which macOS cannot route to): ssh -W through the VM.
func Dial(addr string) (net.Conn, error) {
	p := paths()
	if !running(p) {
		return nil, vmrt.ErrNoDistro
	}
	args := append(sshBase(p), "-W", addr, "root@127.0.0.1")
	cmd := exec.Command("ssh", args...)
	w, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	r, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &pipeConn{r: r, w: w, cmd: cmd, addr: addr}, nil
}

// Keep is a no-op release: the VM runs for as long as the agent does (QEMU does
// not stop an idle VM), so a rental needs nothing extra to hold it up.
func Keep() (release func()) { return func() {} }

// pipeConn is a child process's stdio as a net.Conn.
type pipeConn struct {
	r    io.ReadCloser
	w    io.WriteCloser
	cmd  *exec.Cmd
	addr string
	once sync.Once
}

func (c *pipeConn) Read(b []byte) (int, error)  { return c.r.Read(b) }
func (c *pipeConn) Write(b []byte) (int, error) { return c.w.Write(b) }
func (c *pipeConn) Close() error {
	c.once.Do(func() {
		c.w.Close()
		if c.cmd.Process != nil {
			c.cmd.Process.Kill()
		}
		c.cmd.Wait()
	})
	return nil
}
func (c *pipeConn) LocalAddr() net.Addr                { return pipeAddr("vm") }
func (c *pipeConn) RemoteAddr() net.Addr               { return pipeAddr(c.addr) }
func (c *pipeConn) SetDeadline(t time.Time) error      { return nil }
func (c *pipeConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *pipeConn) SetWriteDeadline(t time.Time) error { return nil }

type pipeAddr string

func (a pipeAddr) Network() string { return "vm" }
func (a pipeAddr) String() string  { return string(a) }

// running reports whether the VM's QEMU is alive (its pidfile names a live
// process).
func running(p vmPaths) bool {
	data, err := os.ReadFile(p.pidPath())
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
