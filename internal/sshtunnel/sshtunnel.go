package sshtunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Forward is a reverse forward: it exposes the agent-local TargetPort on the
// relay's loopback at RelayBindPort, i.e.
// `ssh -R 127.0.0.1:RelayBindPort:127.0.0.1:TargetPort`. Binding to 127.0.0.1
// (with GatewayPorts=no) keeps the forward private to the relay so only the
// relay's proxy can reach this agent's ports.
type Forward struct {
	RelayBindPort int `json:"relay_bind_port"`
	TargetPort    int `json:"target_port"`
}

// Config describes the persistent reverse SSH tunnel from the agent to its relay.
type Config struct {
	RelayHost      string    `json:"relay_host"`
	RelayPort      int       `json:"relay_port"`
	RelayUser      string    `json:"relay_user"`
	IdentityFile   string    `json:"identity_file"`
	KnownHostsFile string    `json:"known_hosts_file"`
	Forwards       []Forward `json:"forwards"`
}

// BuildArgs builds the ssh argument list for the reverse tunnel.
func BuildArgs(c Config) []string {
	args := []string{"-N"}
	if c.IdentityFile != "" {
		args = append(args, "-i", c.IdentityFile)
	}
	if c.RelayPort != 0 {
		args = append(args, "-p", strconv.Itoa(c.RelayPort))
	}
	args = append(args,
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-o", "GatewayPorts=no",
	)
	if c.KnownHostsFile != "" {
		// Pin the relay host key.
		args = append(args,
			"-o", "StrictHostKeyChecking=yes",
			"-o", "UserKnownHostsFile="+c.KnownHostsFile,
		)
	} else {
		args = append(args, "-o", "StrictHostKeyChecking=accept-new")
	}
	for _, f := range c.Forwards {
		args = append(args, "-R",
			fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", f.RelayBindPort, f.TargetPort))
	}
	args = append(args, fmt.Sprintf("%s@%s", c.RelayUser, c.RelayHost))
	return args
}

// Timings, overridable by tests. A run that lasts Healthy counts as up (and
// resets the backoff); the tunnel is reported failing once it has not been up
// for FailingAfter.
var (
	FirstBackoff = 2 * time.Second
	MaxBackoff   = 60 * time.Second
	Healthy      = 60 * time.Second
	FailingAfter = 5 * time.Minute
	// sshBinary is the ssh client run for the tunnel.
	sshBinary = "ssh"
)

// Watch is told how the tunnel is doing: Failing when it has not stayed up
// for FailingAfter (again at every retry after that, with the latest error
// and the last lines ssh printed), Up once a run has lasted Healthy.
type Watch interface {
	Failing(since time.Time, err error, detail string)
	Up()
}

// Supervise runs the reverse tunnel and restarts it with exponential backoff
// until ctx is cancelled (autossh-style persistence). A tunnel that stays up for
// a while resets the backoff so transient drops reconnect fast.
func Supervise(ctx context.Context, c Config) { SuperviseWith(ctx, c, nil) }

// SuperviseWith is Supervise that tells w (nil: nobody) how it goes.
func SuperviseWith(ctx context.Context, c Config, w Watch) {
	backoff := FirstBackoff
	var mu sync.Mutex
	var failingSince time.Time // zero while up
	for {
		if ctx.Err() != nil {
			return
		}
		cmd := exec.CommandContext(ctx, sshBinary, BuildArgs(c)...)
		tail := &tailBuffer{max: 1500}
		cmd.Stdout = os.Stdout
		cmd.Stderr = io.MultiWriter(os.Stderr, tail)
		start := time.Now()
		mu.Lock()
		if failingSince.IsZero() {
			failingSince = start
		}
		mu.Unlock()
		// Up once this run has lasted Healthy.
		healthy := time.AfterFunc(Healthy, func() {
			mu.Lock()
			failingSince = time.Time{}
			mu.Unlock()
			if w != nil {
				w.Up()
			}
		})
		err := cmd.Run()
		healthy.Stop()
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > MaxBackoff {
			backoff = FirstBackoff
		}
		if err == nil {
			err = errors.New("ssh exited")
		}
		log.Printf("ssh tunnel exited (%v); reconnecting in %v", err, backoff)
		mu.Lock()
		since := failingSince
		if since.IsZero() {
			// It was up; the failure starts now.
			failingSince = time.Now()
			since = failingSince
		}
		mu.Unlock()
		if w != nil && time.Since(since) >= FailingAfter {
			w.Failing(since, err, tail.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < MaxBackoff {
			backoff *= 2
		}
	}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}
