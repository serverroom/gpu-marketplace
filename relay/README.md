# GPU Marketplace Relay

The relay is a **DMZ** box that terminates provider-agent reverse-SSH tunnels and
(later) routes renters into their rented microVM. It has **no internal database
access** and holds **no secrets** beyond a scoped API token — built to be assumed
breached.

## What's here

| File | Role |
|---|---|
| `relaymgr.py` | Core: collision-free loopback **slot allocation** + the **restricted `authorized_keys` line** (`restrict` + `permitlisten`) that limits each agent to its own slots |
| `server.py` | Flask service (`/register-agent`, `DELETE /agent/<id>`) — the control plane calls it to authorize an agent and get its slots; it rewrites the `gpu-tunnel` `authorized_keys` |
| `sshd_config.example` | Forwarding-only sshd drop-in for the `gpu-tunnel` account |
| `tests/` | Unit tests for the security-critical core |

## Security model

- The `gpu-tunnel` user is **forwarding-only** (`ForceCommand /usr/sbin/nologin`, no PTY/SFTP/agent/X11).
- Each agent key is written as `restrict,permitlisten="127.0.0.1:<control>",permitlisten="127.0.0.1:<ssh>"` — so an agent can bind **only** its two loopback slots and nothing else.
- All forwarded ports stay on the relay's loopback (`GatewayPorts no`).
- Egress from the relay is **default-DENY** to internal subnets (router/firewall enforced); the only allowed dest is the control-plane API. (Not in this repo — network policy.)

## Setup (per POP)

1. Create the forwarding-only account: `useradd -m -s /usr/sbin/nologin gpu-tunnel`
2. Install the sshd drop-in: copy `sshd_config.example` → `/etc/ssh/sshd_config.d/gpu-relay.conf`, `systemctl reload sshd`.
3. Run the manager: `RELAY_AUTHORIZED_KEYS=/home/gpu-tunnel/.ssh/authorized_keys python3 server.py` (behind the DMZ; reachable only from the control plane).

## Not built yet (spec D3 spike)

**Renter routing** — sshpiper (or OpenSSH `Match`+`ForceCommand`) mapping a renter's
key → `listing_id` → the agent's reverse-tunnel loopback slot. The spec flags this
as the load-bearing, unproven mechanism that must be demonstrated end-to-end before
the relay ships. This repo currently covers only **agent tunnel termination**.
