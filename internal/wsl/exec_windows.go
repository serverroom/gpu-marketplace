//go:build windows

package wsl

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

var procCreateProcessWithTokenW = advapi32.NewProc("CreateProcessWithTokenW")

// Exe is WSL 2.x's wsl.exe: the one a process in session 0 may run.
func Exe() string { return filepath.Join(os.Getenv("ProgramFiles"), "WSL", "wsl.exe") }

// isSystem reports whether the agent runs as LocalSystem (the service).
func isSystem() bool {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	return err == nil && tu.User.Sid.String() == "S-1-5-18"
}

// env is the account's environment, with wsl.exe told to print UTF-8.
func (s *session) environ() []string { return append(append([]string(nil), s.env...), "WSL_UTF8=1") }

// run starts wsl.exe with args as the account and waits for it. The service
// (LocalSystem) starts it with the account's token directly; an administrator
// running the agent by hand lacks the privilege that takes, so it goes through
// CreateProcessWithTokenW (the Secondary Logon service) instead.
func (s *session) run(stdin []byte, args ...string) ([]byte, []byte, int, error) {
	if isSystem() {
		cmd := exec.Command(Exe(), args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Token: syscall.Token(s.token), HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
		cmd.Env = s.environ()
		cmd.Dir = s.home
		cmd.Stdin = bytes.NewReader(stdin)
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err := cmd.Run()
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee):
			return out.Bytes(), errOut.Bytes(), ee.ExitCode(), nil
		case err != nil:
			return out.Bytes(), errOut.Bytes(), -1, fmt.Errorf("run wsl.exe as %s: %w", Account, err)
		}
		return out.Bytes(), errOut.Bytes(), 0, nil
	}
	return s.runWithToken(stdin, args...)
}

func (s *session) runWithToken(stdin []byte, args ...string) ([]byte, []byte, int, error) {
	sa := windows.SecurityAttributes{InheritHandle: 1}
	sa.Length = uint32(unsafe.Sizeof(sa))
	pipe := func(parentReads bool) (parent, child windows.Handle, err error) {
		var r, w windows.Handle
		if err = windows.CreatePipe(&r, &w, &sa, 0); err != nil {
			return 0, 0, err
		}
		if parentReads {
			parent, child = r, w
		} else {
			parent, child = w, r
		}
		_ = windows.SetHandleInformation(parent, windows.HANDLE_FLAG_INHERIT, 0)
		return parent, child, nil
	}
	inW, inR, err := pipe(false)
	if err != nil {
		return nil, nil, -1, err
	}
	outR, outW, err := pipe(true)
	if err != nil {
		windows.CloseHandle(inW)
		windows.CloseHandle(inR)
		return nil, nil, -1, err
	}
	errR, errW, err := pipe(true)
	if err != nil {
		for _, h := range []windows.Handle{inW, inR, outR, outW} {
			windows.CloseHandle(h)
		}
		return nil, nil, -1, err
	}

	var line strings.Builder
	line.WriteString(syscall.EscapeArg(Exe()))
	for _, a := range args {
		line.WriteByte(' ')
		line.WriteString(syscall.EscapeArg(a))
	}
	cmdline, _ := windows.UTF16FromString(line.String())
	var envBlock []uint16
	for _, kv := range s.environ() {
		u, _ := windows.UTF16FromString(kv)
		envBlock = append(envBlock, u...)
	}
	envBlock = append(envBlock, 0)
	app, _ := windows.UTF16PtrFromString(Exe())
	dir, _ := windows.UTF16PtrFromString(s.home)
	si := windows.StartupInfo{Flags: windows.STARTF_USESTDHANDLES | windows.STARTF_USESHOWWINDOW, StdInput: inR, StdOutput: outW, StdErr: errW}
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi windows.ProcessInformation
	const logonWithProfile = 1
	r, _, callErr := procCreateProcessWithTokenW.Call(uintptr(s.token), logonWithProfile, uintptr(unsafe.Pointer(app)), uintptr(unsafe.Pointer(&cmdline[0])),
		windows.CREATE_NO_WINDOW|windows.CREATE_UNICODE_ENVIRONMENT, uintptr(unsafe.Pointer(&envBlock[0])), uintptr(unsafe.Pointer(dir)),
		uintptr(unsafe.Pointer(&si)), uintptr(unsafe.Pointer(&pi)))
	windows.CloseHandle(inR)
	windows.CloseHandle(outW)
	windows.CloseHandle(errW)
	if r == 0 {
		windows.CloseHandle(inW)
		windows.CloseHandle(outR)
		windows.CloseHandle(errR)
		return nil, nil, -1, fmt.Errorf("run wsl.exe as %s: %w", Account, callErr)
	}
	defer windows.CloseHandle(pi.Process)
	windows.CloseHandle(pi.Thread)

	in := os.NewFile(uintptr(inW), "stdin")
	outF := os.NewFile(uintptr(outR), "stdout")
	errF := os.NewFile(uintptr(errR), "stderr")
	var out, errOut bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { io.Copy(&out, outF); outF.Close(); wg.Done() }()
	go func() { io.Copy(&errOut, errF); errF.Close(); wg.Done() }()
	in.Write(stdin)
	in.Close()
	windows.WaitForSingleObject(pi.Process, windows.INFINITE)
	wg.Wait()
	var code uint32
	if err := windows.GetExitCodeProcess(pi.Process, &code); err != nil {
		return out.Bytes(), errOut.Bytes(), -1, err
	}
	return out.Bytes(), errOut.Bytes(), int(int32(code)), nil
}

// manage runs a wsl.exe management command (--import, --terminate, ...) as
// the account, and returns its output.
func manage(args ...string) (string, error) {
	s, err := openSession()
	if err != nil {
		return "", err
	}
	out, errOut, code, err := s.run(nil, args...)
	text := strings.TrimSpace(strings.ReplaceAll(string(out)+" "+string(errOut), "\x00", ""))
	if err != nil {
		return text, err
	}
	if code != 0 {
		return text, fmt.Errorf("wsl.exe %s: exit status %d: %s", strings.Join(args, " "), code, text)
	}
	return text, nil
}

// Exec is the vmrt.WSLExec of the agent's distribution.
func Exec(stdin []byte, argv ...string) ([]byte, []byte, int, error) {
	s, err := openSession()
	if err != nil {
		return nil, nil, -1, vmrt.ErrNoDistro
	}
	if _, ok := s.registered(); !ok {
		return nil, nil, -1, vmrt.ErrNoDistro
	}
	return s.run(stdin, append([]string{"-d", Distro, "-u", "root", "--exec"}, argv...)...)
}

// Dial opens a stream to addr from inside the distribution: socat in the
// distribution, its stdio piped to the agent. Only the service opens the
// renter's SSH forward.
func Dial(addr string) (net.Conn, error) {
	s, err := openSession()
	if err != nil {
		return nil, err
	}
	if !isSystem() {
		return nil, errors.New("only the agent service forwards a renter's SSH")
	}
	cmd := exec.Command(Exe(), "-d", Distro, "-u", "root", "--exec", "socat", "STDIO", "TCP:"+addr+",connect-timeout=10")
	cmd.SysProcAttr = &syscall.SysProcAttr{Token: syscall.Token(s.token), HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	cmd.Env = s.environ()
	cmd.Dir = s.home
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
func (c *pipeConn) LocalAddr() net.Addr                { return pipeAddr("wsl") }
func (c *pipeConn) RemoteAddr() net.Addr               { return pipeAddr(c.addr) }
func (c *pipeConn) SetDeadline(t time.Time) error      { return nil }
func (c *pipeConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *pipeConn) SetWriteDeadline(t time.Time) error { return nil }

type pipeAddr string

func (a pipeAddr) Network() string { return "wsl" }
func (a pipeAddr) String() string  { return string(a) }
