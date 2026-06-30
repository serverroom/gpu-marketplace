package sshtunnel

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
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

// Supervise runs the reverse tunnel and restarts it with exponential backoff
// until ctx is cancelled (autossh-style persistence). A tunnel that stays up for
// a while resets the backoff so transient drops reconnect fast.
func Supervise(ctx context.Context, c Config) {
	backoff := 2 * time.Second
	const maxBackoff = 60 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		cmd := exec.CommandContext(ctx, "ssh", BuildArgs(c)...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		start := time.Now()
		err := cmd.Run()
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > maxBackoff {
			backoff = 2 * time.Second
		}
		log.Printf("ssh tunnel exited (%v); reconnecting in %v", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}
