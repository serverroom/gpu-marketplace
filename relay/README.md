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

1. Create the two service accounts. **The details here are load-bearing — both
   defaults of `useradd` break the relay in ways that look like something else:**
   ```
   useradd -m -s /usr/sbin/nologin gpu-tunnel   # never runs anything
   useradd -m -s /bin/bash        gpu-renter    # must be able to run the forced command
   usermod -p '*' gpu-tunnel && usermod -p '*' gpu-renter
   ```
   - `useradd` leaves `!` in the password field, which sshd treats as **locked** and
     refuses before it ever looks at a key: `User gpu-tunnel not allowed because
     account is locked`. `-p '*'` means no password can ever match while leaving the
     account usable, which is what a key-only service account wants.
   - `gpu-renter` needs a **real shell**. sshd runs a forced command through the
     user's login shell, so `nologin` answers `This account is currently not
     available` and `gpu_route.py` never runs. Its isolation comes from `restrict`
     plus the forced command, not from the shell. `gpu-tunnel` keeps `nologin`
     because nothing should ever run for it.
2. Install the pipe: copy `gpu_route.py` (and `relaymgr.py`) to `/opt/gpu-relay/`; ensure `python3` is present.
3. Run the relay's sshd as a **separate instance**, not as a drop-in on the box's
   admin sshd: copy `sshd_config.example` to `/etc/gpu-relay/sshd_config`, set its
   `ListenAddress`/`HostKey`, give it its own systemd unit
   (`/usr/sbin/sshd -D -f /etc/gpu-relay/sshd_config`), and leave the admin sshd
   bound to the internal address. Different process, different config, its own host
   key, and only the relay one faces the internet — so rotating or breaking either
   cannot touch the other, and admin ssh is never exposed by deploying a relay.
4. Run the manager (behind the DMZ; reachable only from the control plane):
   `RELAY_AUTHORIZED_KEYS=/home/gpu-tunnel/.ssh/authorized_keys RELAY_RENTER_AUTHORIZED_KEYS=/home/gpu-renter/.ssh/authorized_keys python3 server.py`
