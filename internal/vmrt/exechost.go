package vmrt

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrNoDistro: the agent's WSL distribution (or the Windows account it runs
// under) does not exist yet. The automatic setup creates both.
var ErrNoDistro = errors.New("the agent's WSL 2 environment is not installed yet")

// WSLExec runs argv as root inside the agent's WSL 2 distribution, feeding it
// stdin, and returns what it wrote and its exit code. err is set only when the
// command could not be run at all -- wsl.exe missing or failing, or the
// distribution absent (ErrNoDistro).
type WSLExec func(stdin []byte, argv ...string) (stdout, stderr []byte, code int, err error)

// ExecHost is a Linux environment the agent owns but does not run in directly:
// a WSL 2 distribution on Windows, a VM on macOS. Every vmrt.Host operation
// becomes a command run inside it through Exec, so the container runtime, the
// fence and the encrypted volumes run there exactly as on a native Linux host;
// only the way a command reaches them differs. Every path is a Linux path.
type ExecHost struct {
	Exec WSLExec
	// Dial opens a stream to addr from inside the distribution: the renter's
	// SSH to the container, which Windows cannot route to.
	Dial func(addr string) (net.Conn, error)
	// Keep holds the distribution up until release is called: WSL shuts a
	// distribution down seconds after its last wsl.exe session ends, whatever
	// still runs in it -- a rental's container included.
	Keep func() (release func())
}

// Hold keeps the distribution running (see Keep) until release.
func (w ExecHost) Hold() (release func()) {
	if w.Keep == nil {
		return func() {}
	}
	return w.Keep()
}

// wslScript runs the command it is handed as base64 in $1: a NUL-separated
// argv, decoded into bash's own array. No argument is ever quoted on the
// Windows command line, so no quoting rule of wsl.exe's or Windows' can change
// one; the command keeps the caller's stdin.
const wslScript = `mapfile -d '' -t a < <(printf %s "$1" | base64 -d); exec "${a[@]}"`

func (w ExecHost) exec(stdin []byte, argv ...string) ([]byte, []byte, int, error) {
	if w.Exec == nil {
		return nil, nil, -1, ErrNoDistro
	}
	var b strings.Builder
	for _, a := range argv {
		b.WriteString(a)
		b.WriteByte(0)
	}
	enc := base64.StdEncoding.EncodeToString([]byte(b.String()))
	return w.Exec(stdin, "/bin/bash", "-c", wslScript, "bash", enc)
}

// do runs argv and turns a failure into an error that names the command, as
// OSHost does.
func (w ExecHost) do(stdin []byte, name string, args ...string) ([]byte, []byte, error) {
	out, errOut, code, err := w.exec(stdin, append([]string{name}, args...)...)
	if err != nil {
		return out, errOut, fmt.Errorf("%s: %w", describe(name, args), err)
	}
	if code != 0 {
		return out, errOut, fmt.Errorf("%s: exit status %d: %s", describe(name, args), code, strings.TrimSpace(string(errOut)+" "+string(out)))
	}
	return out, errOut, nil
}

// sh runs a fixed shell script with args ($1...), and returns its exit code.
func (w ExecHost) sh(stdin []byte, script string, args ...string) ([]byte, int, error) {
	out, _, code, err := w.exec(stdin, append([]string{"/bin/sh", "-c", script, "sh"}, args...)...)
	return out, code, err
}

// lp is a Linux path: a caller on Windows may have joined it with backslashes.
func lp(p string) string { return strings.ReplaceAll(p, `\`, "/") }

// missing is the error a file operation returns for a path that is not there,
// or a distribution that is not installed.
func missing(op, p string) error { return &fs.PathError{Op: op, Path: p, Err: fs.ErrNotExist} }

func (w ExecHost) Run(name string, args ...string) error {
	_, _, err := w.do(nil, name, args...)
	return err
}

func (w ExecHost) RunInput(stdin []byte, name string, args ...string) error {
	_, _, err := w.do(stdin, name, args...)
	return err
}

func (w ExecHost) Output(name string, args ...string) (string, error) {
	out, _, err := w.do(nil, name, args...)
	return string(out), err
}

func (w ExecHost) LookPath(name string) (string, error) {
	out, code, err := w.sh(nil, `command -v -- "$1"`, name)
	if err != nil || code != 0 {
		return "", fmt.Errorf("%s: not found in the WSL environment", name)
	}
	return strings.TrimSpace(string(out)), nil
}

// readCode is the exit code the read scripts use for a path that is not there.
const readCode = 44

func (w ExecHost) read(op, p, script string, args ...string) ([]byte, error) {
	p = lp(p)
	out, code, err := w.sh(nil, script, append([]string{p}, args...)...)
	switch {
	case errors.Is(err, ErrNoDistro):
		return nil, missing(op, p)
	case err != nil:
		return nil, fmt.Errorf("%s %s: %w", op, p, err)
	case code == readCode:
		return nil, missing(op, p)
	case code != 0:
		return nil, fmt.Errorf("%s %s: exit status %d", op, p, code)
	}
	return out, nil
}

func (w ExecHost) ReadFile(p string) ([]byte, error) {
	return w.read("open", p, `[ -e "$1" ] || exit 44; exec cat -- "$1"`)
}

func (w ExecHost) ReadTail(p string, max int64) ([]byte, error) {
	return w.read("open", p, `[ -e "$1" ] || exit 44; exec tail -c "$2" -- "$1"`, strconv.FormatInt(max, 10))
}

func (w ExecHost) WriteFile(p string, data []byte, perm os.FileMode) error {
	p = lp(p)
	_, code, err := w.sh(data, `cat > "$1" && chmod "$2" "$1"`, p, strconv.FormatUint(uint64(perm.Perm()), 8))
	if err == nil && code != 0 {
		err = fmt.Errorf("exit status %d", code)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	return nil
}

func (w ExecHost) Readlink(p string) (string, error) {
	out, err := w.read("readlink", p, `[ -L "$1" ] || exit 44; exec readlink -- "$1"`)
	return strings.TrimSuffix(string(out), "\n"), err
}

func (w ExecHost) Glob(pattern string) ([]string, error) {
	out, code, err := w.sh(nil, `for f in $1; do if [ -e "$f" ] || [ -L "$f" ]; then printf '%s\n' "$f"; fi; done`, lp(pattern))
	if errors.Is(err, ErrNoDistro) {
		return nil, nil
	}
	if err != nil || code != 0 {
		return nil, fmt.Errorf("glob %s: %v (exit %d)", pattern, err, code)
	}
	var matches []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line != "" {
			matches = append(matches, line)
		}
	}
	return matches, nil
}

func (w ExecHost) Exists(p string) bool {
	_, code, err := w.sh(nil, `[ -e "$1" ] || [ -L "$1" ]`, lp(p))
	return err == nil && code == 0
}

func (w ExecHost) Remove(p string) error {
	return w.Run("/bin/sh", "-c", `if [ -d "$1" ] && [ ! -L "$1" ]; then rmdir -- "$1"; else rm -f -- "$1"; fi`, "sh", lp(p))
}

func (w ExecHost) RemoveAll(p string) error { return w.Run("rm", "-rf", "--", lp(p)) }

func (w ExecHost) Rename(from, to string) error {
	return w.Run("mv", "-f", "-T", "--", lp(from), lp(to))
}

func (w ExecHost) MkdirAll(p string, perm os.FileMode) error {
	return w.Run("mkdir", "-p", "-m", strconv.FormatUint(uint64(perm.Perm()), 8), "--", lp(p))
}

func (w ExecHost) DialTCP(addr string, timeout time.Duration) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	secs := int(timeout / time.Second)
	if secs < 1 {
		secs = 1
	}
	_, code, err := w.sh(nil, `exec timeout "$1" bash -c 'exec 3<>"/dev/tcp/$0/$1"' "$2" "$3"`, strconv.Itoa(secs), host, port)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("dial %s: connection failed", addr)
	}
	return nil
}

func (w ExecHost) Download(url, p string) error {
	p = lp(p)
	return w.Run("/bin/sh", "-c", `curl -fsSL --retry 3 -o "$2.part" "$1" && mv -f "$2.part" "$2"`, "sh", url, p)
}

func (w ExecHost) SHA256(p string) (string, error) {
	out, err := w.Output("sha256sum", "--", lp(p))
	if err != nil {
		return "", err
	}
	f := strings.Fields(out)
	if len(f) == 0 {
		return "", fmt.Errorf("sha256sum %s: no output", p)
	}
	return f[0], nil
}

func (ExecHost) Sleep(d time.Duration) { time.Sleep(d) }
