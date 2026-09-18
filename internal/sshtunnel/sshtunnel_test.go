package sshtunnel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuildArgsReverseForwardAndPinning(t *testing.T) {
	c := Config{
		RelayHost:      "relay.example.com",
		RelayPort:      2222,
		RelayUser:      "tunnel",
		IdentityFile:   "/etc/gpu-agent/agent_ed25519",
		KnownHostsFile: "/etc/gpu-agent/relay_known_hosts",
		Forwards:       []Forward{{RelayBindPort: 41001, TargetPort: 9101}},
	}
	joined := strings.Join(BuildArgs(c), " ")

	for _, want := range []string{
		"-N",
		"-i /etc/gpu-agent/agent_ed25519",
		"-p 2222",
		"ExitOnForwardFailure=yes",
		"ServerAliveInterval=30",
		"StrictHostKeyChecking=yes",
		"UserKnownHostsFile=/etc/gpu-agent/relay_known_hosts",
		"-R 127.0.0.1:41001:127.0.0.1:9101",
		"tunnel@relay.example.com",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q\n got: %s", want, joined)
		}
	}
}

func TestBuildArgsAcceptNewWithoutKnownHosts(t *testing.T) {
	joined := strings.Join(BuildArgs(Config{RelayHost: "r", RelayUser: "u"}), " ")
	if !strings.Contains(joined, "StrictHostKeyChecking=accept-new") {
		t.Errorf("expected accept-new without a known-hosts file; got: %s", joined)
	}
	if strings.Contains(joined, "StrictHostKeyChecking=yes") {
		t.Errorf("must not pin without a known-hosts file; got: %s", joined)
	}
}

// TestMain lets this test binary stand in for ssh: with SSHTUNNEL_FAKE=up it
// stays running like a connected tunnel.
func TestMain(m *testing.M) {
	if os.Getenv("SSHTUNNEL_FAKE") == "up" {
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type watch struct {
	mu      sync.Mutex
	failing []string
	up      int
}

func (w *watch) Failing(since time.Time, err error, detail string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failing = append(w.failing, err.Error())
}

func (w *watch) Up() { w.mu.Lock(); w.up++; w.mu.Unlock() }

func (w *watch) counts() (int, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.failing), w.up
}

func fastTunnel(t *testing.T, binary string) {
	t.Helper()
	old := []time.Duration{FirstBackoff, MaxBackoff, Healthy, FailingAfter}
	oldBin := sshBinary
	FirstBackoff, MaxBackoff, Healthy, FailingAfter = 5*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond
	sshBinary = binary
	t.Cleanup(func() {
		FirstBackoff, MaxBackoff, Healthy, FailingAfter = old[0], old[1], old[2], old[3]
		sshBinary = oldBin
	})
}

// A tunnel that keeps failing is reported once it has failed for a while,
// not at its first drop.
func TestATunnelThatKeepsFailingIsReported(t *testing.T) {
	fastTunnel(t, filepath.Join(t.TempDir(), "no-such-ssh"))
	w := &watch{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { SuperviseWith(ctx, Config{RelayHost: "relay", RelayUser: "tunnel"}, w); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := w.counts(); n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if n, up := w.counts(); n == 0 || up != 0 {
		t.Fatalf("failing %d, up %d", n, up)
	}
}

// A run that stays up is reported up.
func TestATunnelThatStaysUpIsUp(t *testing.T) {
	fastTunnel(t, os.Args[0])
	t.Setenv("SSHTUNNEL_FAKE", "up")
	w := &watch{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { SuperviseWith(ctx, Config{RelayHost: "relay", RelayUser: "tunnel"}, w); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, up := w.counts(); up > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if n, up := w.counts(); up == 0 || n != 0 {
		t.Fatalf("failing %d, up %d", n, up)
	}
}
