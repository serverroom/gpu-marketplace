# GPU Marketplace Relay

The relay is a **DMZ** box that terminates provider-agent reverse-SSH tunnels and
routes renters into their rented microVM. It has **no internal database access**
and holds **no secrets** beyond a scoped API token — built to be assumed breached.

## What's here

| File | Role |
|---|---|
| `relaymgr.py` | Core: collision-free loopback **slot allocation**, the agent **`restrict`+`permitlisten` line**, and the renter **`restrict`+forced-`command` line** |
| `gpu_route.py` | The renter jump pipe: splices a renter's SSH channel to one agent ssh slot; range-checks the slot as defence in depth |
| `server.py` | Flask service: `/register-agent`, `DELETE /agent/<id>` (agents) and `/authorize-renter`, `DELETE /renter/<id>` (renters); rewrites both `authorized_keys` files |
| `sshd_config.example` | sshd drop-in for the `gpu-tunnel` (reverse-forward) and `gpu-renter` (jump-pipe) accounts |
| `tests/` | Unit + integration tests, including an end-to-end proof of the renter pipe |

## Security model

- The `gpu-tunnel` user is **forwarding-only** (`ForceCommand /usr/sbin/nologin`, no PTY/SFTP/agent/X11).
- Each agent key is written as `restrict,permitlisten="127.0.0.1:<control>",permitlisten="127.0.0.1:<ssh>"` — so an agent can bind **only** its two loopback slots and nothing else.
- The `gpu-renter` user is **jump-only**: each renter key is `restrict,command="python3 gpu_route.py <ssh_slot>"`, so it can do nothing but pipe to that one slot. The renter's real SSH to the microVM runs **end-to-end inside the pipe** — the relay moves bytes and holds no key that can read or MITM it.
- All forwarded ports stay on the relay's loopback (`GatewayPorts no`).
- Egress from the relay is **default-DENY** to internal subnets (router/firewall enforced); the only allowed dest is the control-plane API. (Not in this repo — network policy.)

## Renter routing (D3) — how a renter reaches the microVM

1. At provision time the control plane calls `POST /authorize-renter {listing_id, renter_pubkey}`. The relay looks up that listing's agent `ssh_slot` and writes a forced-command line for the renter key on the `gpu-renter` account.
2. The renter connects with the relay as a jump host, e.g.
   `ssh -o ProxyCommand="ssh -i renterkey gpu-renter@<relay>" -i renterkey user@microvm`.
3. sshd runs only the forced `gpu_route.py <ssh_slot>`, which splices the renter's channel to `127.0.0.1:<ssh_slot>` — the loopback end of the agent's reverse tunnel — which lands on the microVM's sshd. The inner SSH authenticates the renter against the microVM end-to-end.
4. On rental end (or agent removal) the routing is revoked via `DELETE /renter/<listing_id>` (removing the agent cascades to its renter).

Proven end-to-end in `tests/` (`test_gpu_route.py` splices real sockets; a hermetic sshd+ForceCommand harness confirms a renter runs a command on a target through the relay while an unauthorized key is refused).

## Setup (per POP)

1. Create the two forwarding-only accounts:
   `useradd -m -s /usr/sbin/nologin gpu-tunnel` and `useradd -m -s /usr/sbin/nologin gpu-renter`
2. Install the pipe: copy `gpu_route.py` (and `relaymgr.py`) to `/opt/gpu-relay/`; ensure `python3` is present.
3. Install the sshd drop-in: copy `sshd_config.example` → `/etc/ssh/sshd_config.d/gpu-relay.conf`, `systemctl reload sshd`.
4. Run the manager (behind the DMZ; reachable only from the control plane):
   `RELAY_AUTHORIZED_KEYS=/home/gpu-tunnel/.ssh/authorized_keys RELAY_RENTER_AUTHORIZED_KEYS=/home/gpu-renter/.ssh/authorized_keys python3 server.py`
