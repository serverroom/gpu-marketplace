package vmrt

import (
	"encoding/base64"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"
)

// fakeWSL decodes each call the way wslScript does, records the argv that
// would reach the distribution, and answers from answer.
type fakeWSL struct {
	calls  [][]string
	stdins [][]byte
	answer func(argv []string) (string, int)
	absent bool
}

func (f *fakeWSL) exec(stdin []byte, argv ...string) ([]byte, []byte, int, error) {
	if f.absent {
		return nil, nil, -1, ErrNoDistro
	}
	if len(argv) != 5 || argv[0] != "/bin/bash" || argv[1] != "-c" || argv[2] != wslScript || argv[3] != "bash" {
		return nil, []byte("unexpected wrapper"), 99, nil
	}
	raw, err := base64.StdEncoding.DecodeString(argv[4])
	if err != nil {
		return nil, []byte(err.Error()), 98, nil
	}
	decoded := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	f.calls = append(f.calls, decoded)
	f.stdins = append(f.stdins, stdin)
	out, code := "", 0
	if f.answer != nil {
		out, code = f.answer(decoded)
	}
	return []byte(out), nil, code, nil
}

// Every argument reaches the distribution exactly as given -- spaces, quotes,
// newlines, backslashes and empty ones included -- because none is ever quoted
// on the Windows command line.
func TestWSLHostPassesArgumentsVerbatim(t *testing.T) {
	f := &fakeWSL{}
	h := WSLHost{Exec: f.exec}
	args := []string{"-c", `printf '%s\n' "a b" 'c"d' $HOME`, "", "x\ny", `C:\not\a\path`}
	if err := h.Run("bash", args...); err != nil {
		t.Fatal(err)
	}
	got := f.calls[0]
	if len(got) != len(args)+1 || got[0] != "bash" {
		t.Fatalf("argv = %q", got)
	}
	for i, a := range args {
		if got[i+1] != a {
			t.Errorf("arg %d = %q, want %q", i, got[i+1], a)
		}
	}
}

// A path that is not there, and a distribution that is not installed yet, both
// read as not existing -- so a machine before its setup has no rental state.
func TestWSLHostMissingFiles(t *testing.T) {
	f := &fakeWSL{answer: func(argv []string) (string, int) { return "", readCode }}
	h := WSLHost{Exec: f.exec}
	if _, err := h.ReadFile("/var/lib/gpu-agent/rental.json"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
	f.absent = true
	if _, err := h.ReadFile("/var/lib/gpu-agent/rental.json"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("no distribution: %v", err)
	}
	if st, err := LoadState(h, "/var/lib/gpu-agent"); st != nil || err != nil {
		t.Errorf("no distribution must mean no rental: %+v %v", st, err)
	}
	if h.Exists("/var/lib/gpu-agent") {
		t.Error("nothing exists without a distribution")
	}
	if err := h.Run("true"); !errors.Is(err, ErrNoDistro) {
		t.Errorf("a command without a distribution: %v", err)
	}
}

// Files are written through stdin with their mode; paths joined with
// backslashes on Windows reach the distribution as Linux paths.
func TestWSLHostFiles(t *testing.T) {
	f := &fakeWSL{answer: func(argv []string) (string, int) {
		if strings.Contains(strings.Join(argv, " "), "for f in") {
			return "/proc/12/fd/3\n/proc/12/fd/4\n", 0
		}
		return "", 0
	}}
	h := WSLHost{Exec: f.exec}
	if err := h.WriteFile(`\var\lib\gpu-agent\rental.json`, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	call := f.calls[0]
	if call[len(call)-2] != "/var/lib/gpu-agent/rental.json" || call[len(call)-1] != "600" || string(f.stdins[0]) != "{}" {
		t.Errorf("write = %q stdin %q", call, f.stdins[0])
	}
	got, err := h.Glob("/proc/[0-9]*/fd/*")
	if err != nil || strings.Join(got, ",") != "/proc/12/fd/3,/proc/12/fd/4" {
		t.Errorf("glob = %v %v", got, err)
	}
	if err := h.DialTCP(GuestIP+":22", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	last := f.calls[len(f.calls)-1]
	if strings.Join(last[len(last)-3:], " ") != "3 "+GuestIP+" 22" {
		t.Errorf("dial = %q", last)
	}
}

// A command that fails names itself and what it said.
func TestWSLHostFailures(t *testing.T) {
	f := &fakeWSL{answer: func(argv []string) (string, int) { return "no such device", 1 }}
	h := WSLHost{Exec: f.exec}
	err := h.Run("cryptsetup", "open", "/dev/loop7", "gpu-rental-R1")
	if err == nil || !strings.Contains(err.Error(), "cryptsetup open /dev/loop7 gpu-rental-R1: exit status 1: no such device") {
		t.Errorf("err = %v", err)
	}
	if _, err := h.LookPath("podman"); err == nil {
		t.Error("a tool the distribution lacks must not be found")
	}
}

// A container rental on WSL holds the distribution up from its start to its
// teardown; WSL would otherwise stop it, container and all.
func TestContainerOnWSLHoldsTheDistribution(t *testing.T) {
	held := 0
	h := newContainerHost()
	rt, _ := newContainerRuntime(h, func([]BoundDevice) bool { return true })
	rt.h = heldHost{Host: h, hold: func() func() { held++; return func() { held-- } }}
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatal(err)
	}
	if held != 1 {
		t.Fatalf("held %d during the rental, want 1", held)
	}
	rt.Stop()
	if held != 0 {
		t.Errorf("held %d after teardown, want 0", held)
	}
}

type heldHost struct {
	Host
	hold func() func()
}

func (h heldHost) Hold() func() { return h.hold() }
