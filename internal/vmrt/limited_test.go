package vmrt

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// limitedHelper is the variable that turns this test binary into the command
// TestLimitedHelper plays.
const limitedHelper = "VMRT_LIMITED_HELPER"

// TestLimitedHelper is not a test: run again by the tests below, it is a
// command that never finishes, one that answers, and one that fails.
func TestLimitedHelper(t *testing.T) {
	switch os.Getenv(limitedHelper) {
	case "never":
		time.Sleep(time.Hour)
	case "answer":
		in, _ := io.ReadAll(os.Stdin)
		fmt.Printf("stdin=%s var=%s\n", in, os.Getenv("VMRT_LIMITED_VAR"))
		os.Exit(0)
	case "fail":
		fmt.Fprintln(os.Stderr, "no such device")
		os.Exit(3)
	}
}

func helper(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	return exe
}

// A command that never returns is ended at its limit and says so; nothing
// waits for it longer.
func TestACommandWithALimitIsEndedWhenItRunsOut(t *testing.T) {
	exe := helper(t)
	start := time.Now()
	_, err := OSHost{}.RunLimited(300*time.Millisecond, []string{limitedHelper + "=never"}, nil, exe, "-test.run=^TestLimitedHelper$")
	if !errors.Is(err, ErrTimedOut) {
		t.Fatalf("err = %v", err)
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Errorf("a command with a 300ms limit was waited for %v", took)
	}
	if !strings.Contains(err.Error(), "did not finish in time (300ms)") || !strings.Contains(err.Error(), "TestLimitedHelper") {
		t.Errorf("the error does not name the command and the limit: %v", err)
	}
}

// Within its limit it is an ordinary command: its stdin, its added
// environment, its output, and its failure with what it said.
func TestACommandWithALimitRunsAsAnyOtherWithinIt(t *testing.T) {
	exe := helper(t)
	out, err := OSHost{}.RunLimited(time.Minute, []string{limitedHelper + "=answer", "VMRT_LIMITED_VAR=1"}, []byte("key"), exe, "-test.run=^TestLimitedHelper$")
	if err != nil || !strings.Contains(out, "stdin=key var=1") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	_, err = OSHost{}.RunLimited(time.Minute, []string{limitedHelper + "=fail"}, nil, exe, "-test.run=^TestLimitedHelper$")
	if err == nil || errors.Is(err, ErrTimedOut) || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "no such device") {
		t.Errorf("err = %v", err)
	}
}

// In an environment the agent reaches only through a command (WSL 2, a VM),
// timeout(1) inside it ends the command, and the added environment is set
// there.
func TestExecHostEndsACommandAtItsLimitInsideTheEnvironment(t *testing.T) {
	code := timeoutKillCode
	f := &fakeWSL{answer: func([]string) (string, int) { return "", code }}
	h := ExecHost{Exec: f.exec}
	_, err := h.RunLimited(20*time.Second, []string{"DM_DISABLE_UDEV=1"}, []byte("key"), "cryptsetup", "open", "/dev/loop7", "gpu-rental-R1")
	if !errors.Is(err, ErrTimedOut) || !strings.Contains(err.Error(), "cryptsetup open /dev/loop7 gpu-rental-R1") {
		t.Fatalf("err = %v", err)
	}
	if got := strings.Join(f.calls[0], " "); got != "timeout -s KILL 20 env DM_DISABLE_UDEV=1 cryptsetup open /dev/loop7 gpu-rental-R1" {
		t.Errorf("ran %q", got)
	}
	if string(f.stdins[0]) != "key" {
		t.Errorf("stdin = %q", f.stdins[0])
	}

	// Ended with TERM is ended too; no environment, no env; a part of a second
	// is a whole one to timeout(1).
	code = timeoutCode
	if _, err = h.RunLimited(300*time.Millisecond, nil, nil, "cryptsetup", "close", "gpu-rental-R1"); !errors.Is(err, ErrTimedOut) {
		t.Errorf("err = %v", err)
	}
	if got := strings.Join(f.calls[1], " "); got != "timeout -s KILL 1 cryptsetup close gpu-rental-R1" {
		t.Errorf("ran %q", got)
	}
	_, _ = h.RunLimited(1500*time.Millisecond, nil, nil, "true")
	if got := strings.Join(f.calls[2], " "); got != "timeout -s KILL 2 true" {
		t.Errorf("a limit was cut short: ran %q", got)
	}

	// Any other failure is the command's own, and success is its output.
	f.answer = func([]string) (string, int) { return "Device gpu-rental-R1 is not active.", 4 }
	if _, err = h.RunLimited(20*time.Second, nil, nil, "cryptsetup", "close", "gpu-rental-R1"); err == nil || errors.Is(err, ErrTimedOut) ||
		!strings.Contains(err.Error(), "cryptsetup close gpu-rental-R1: exit status 4: Device gpu-rental-R1 is not active.") {
		t.Errorf("err = %v", err)
	}
	f.answer = func([]string) (string, int) { return "dm-3\n", 0 }
	if out, err := h.RunLimited(20*time.Second, nil, nil, "dmsetup", "info"); err != nil || out != "dm-3\n" {
		t.Errorf("out = %q, err = %v", out, err)
	}
	if _, err := (ExecHost{}).RunLimited(time.Second, nil, nil, "true"); !errors.Is(err, ErrNoDistro) {
		t.Errorf("no environment: %v", err)
	}
}
