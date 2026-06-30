package sshtunnel

import (
	"strings"
	"testing"
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
