# GPU Marketplace

A P2P GPU marketplace agent that lets you list your GPU server for others to rent. The agent registers your host with a one-time code, keeps a reverse SSH tunnel to the nearest relay, checks whether the machine can host a rental safely, and — once the rental runtime exists — runs rentals in isolated microVMs.

> **Status: a machine is offered to renters only after it has proven it can host.** The agent carries its own microVM runtime (QEMU/KVM, GPU passthrough over VFIO, a per-rental encrypted disk). A machine reports *ready* only when every hosting check passes **and** it has passed a real test boot on its own hardware (`sudo gpu-agent check --boot`); until then the marketplace neither shows it to renters nor accepts orders for it. Up to v0.1.5 the agent answered rental requests with a stub that reported success and created nothing; that stub is gone. See [Can this machine host a rental?](#can-this-machine-host-a-rental)

## How It Works

1. **Install the agent** on your Linux GPU server
2. Generate a **one-time registration code** in your dashboard and run `gpu-agent register --code <code>`
3. The agent registers, then **tests latency** to the available locations and **prompts you to pick one** (closest preselected) — under the hood it opens a **reverse SSH tunnel** to that location's relay
4. **Configure your listing** in your dashboard; nothing is published until you do
5. Once configured **and** the machine passes its hosting checks, your server appears on the **marketplace listing** for renters
6. When rented, the agent fences the tenant off your network, boots an **isolated microVM** with the GPUs passed through, and **wipes it clean** when the rental ends — refusing the rental outright if any of that cannot be done

## Quick Install

### Linux
```bash
curl -sSL https://raw.githubusercontent.com/serverroom/gpu-marketplace/main/scripts/install.sh | sudo bash
```

### Windows (PowerShell as Admin)
```powershell
irm https://raw.githubusercontent.com/serverroom/gpu-marketplace/main/scripts/install.ps1 | iex
```

### macOS
```bash
curl -sSL https://raw.githubusercontent.com/serverroom/gpu-marketplace/main/scripts/install-mac.sh | sudo bash
```

## Manual Setup

### Prerequisites
- An OpenSSH client (`ssh`, `ssh-keygen`) — present on virtually all Linux systems
- Download the `gpu-agent` binary from [Releases](https://github.com/serverroom/gpu-marketplace/releases)

### Steps

```bash
# 1. Register this host with a one-time code from your dashboard
sudo gpu-agent register --code <code>

# 2. Install as a system service
sudo gpu-agent install

# 3. Start the service
sudo gpu-agent start
```

## Agent Commands

| Command | Description |
|---------|-------------|
| `gpu-agent register --code <code>` | Register this host (latency test, key generation, listing) |
| `gpu-agent check` | Check whether this machine can host a rental, and show what a tenant is fenced off from (`--rules` prints the exact firewall rules) |
| `gpu-agent runtime prepare` | Install the microVM runtime (`--install-deps`) and build the rental base image with the NVIDIA driver (`--driver 580-server-open`) |
| `gpu-agent check --boot` | Boot a real test rental with the GPU passed through, check what it can and cannot reach, tear it down, and record the result |
| `gpu-agent install` | Install as a system service (systemd/launchd/Windows Service) |
| `gpu-agent remove` | Withdraw the listing, revoke relay access and delete the agent completely (`--yes` skips the prompt) |
| `gpu-agent uninstall` | Remove the system service only — keys, token and listing stay; use `remove` to take everything off |
| `gpu-agent start` | Start the service |
| `gpu-agent stop` | Stop the service |
| `gpu-agent status` | Check the service *and* the registration state |
| `gpu-agent test-stats` | Collect and display system stats as JSON |
| `gpu-agent -version` | Print version |

Running without arguments starts the agent interactively or as a managed service.

`status` reports the two halves separately, because they fail independently — a
freshly installed agent is a healthy service with nothing to do yet:

```
$ sudo gpu-agent status
Service:      running
Registration: not registered

not registered — run 'gpu-agent register --code <code>' with a one-time code from your dashboard, then restart the service
```

```
$ sudo gpu-agent status
Service:      running
Registration: listing L-1042, location nyc, tunnel configured
```

Run it with `sudo`. Both halves need it: the registration state is stored 0600
and root-owned, so an unprivileged `status` can see that a registration exists
but not read it — and on macOS the daemon lives in launchd's system domain,
which a normal user cannot query at all, so the service line reads `unknown`
rather than guessing.

## Troubleshooting

**The agent seems to do nothing after installing.** That is the expected state
until you register — there is no tunnel and nothing is listening. The agent says
so on every start; read it with `sudo gpu-agent status`, or in the daemon log:

| Platform | Log |
|---|---|
| macOS | `tail -f /var/log/gpu-agent.err.log` |
| Linux | `journalctl -u gpu-agent -f` |
| Windows | `services.msc` → GPU Marketplace Agent |

**`register` succeeded but the agent is still offline.** Location assignment is a
second step, and the one-time code is already spent by then. `gpu-agent status`
shows `registered, no tunnel configured`; run `sudo gpu-agent select-location` —
it does not need a new code — then restart the service.

## Can this machine host a rental?

`sudo gpu-agent check` answers that without changing anything, and `status`
and `register` print the same answer. The agent reports it to the marketplace at
register and on every start; a machine is only shown to renters, and can only be
ordered, while its latest report says ready. A machine that is not ready also
refuses a rental request itself, with the reasons — it never accepts one it
cannot deliver.

Every one of these must hold, and `check` lists every one that does not:

- **Linux with KVM** (`/dev/kvm`). Rentals run in a microVM; Windows and macOS hosts cannot host.
- **IOMMU enabled** (VT-d / AMD-Vi, or the SMMU on Arm), in firmware and on the kernel command line — without it no GPU can be handed to a microVM.
- **An NVIDIA or AMD GPU the agent can pass through.** Discrete GPUs report their own memory. **Unified-memory GPUs are supported too:** the NVIDIA GB10 in a DGX Spark (and the other GB10 boxes) has no separate GPU memory — `nvidia-smi` shows `[N/A]` — because the machine's memory is one pool used by both the CPU and the GPU. The agent knows this: it reports that pool as the GPU's memory, marks it unified so it is not counted twice, and a rental gets the pool as both its system and its GPU memory. A GPU that reports no memory and is *not* a known unified part is refused rather than guessed at. **Apple Silicon** cannot host: it has no way to pass its GPU through to a microVM.
- **The rental runtime installed and its base image built** — QEMU, UEFI firmware (OVMF/AAVMF), `cloud-image-utils`, `cryptsetup` and `nftables`. `sudo gpu-agent runtime prepare --install-deps` installs them with apt and builds the base image: Ubuntu 26.04's official cloud image, verified against Canonical's published checksums, with the NVIDIA driver baked in.
- **No GPU in use on the host** while a rental starts — the GPU is handed to the microVM whole, so anything using it (a desktop session, a container) must be stopped first. The agent refuses and names what holds it.
- **Enough memory and disk** — at least 6 GB of memory and 20 GB free under `/var/lib/gpu-agent`. A rental gets the machine's memory less a tenth (never under 4 GB) for the host.
- **A passing test boot on this machine.** `sudo gpu-agent check --boot` runs a real rental for a few minutes with a key nobody holds: the VM reports the GPU it sees, that it reaches the internet, and that your machine, its address and its gateway are unreachable; it is then torn down and the GPU checked. The result is recorded and must be for the agent version you are running.

## What the agent does on your machine

It runs as root, because booting a microVM with a GPU passed through, loading
firewall rules and creating an encrypted disk all need it. Concretely:

- **Network: outbound only.** One SSH connection to the relay you picked (port 2222). Its key on the relay is `restrict`ed to two reverse forwards — no shell, no command, nothing else. Nothing is opened on your router.
- **Listens on loopback only.** `127.0.0.1:9101` is the control channel; `127.0.0.1:9100` is the legacy stats endpoint (only when `config.yaml` exists). Nothing on your LAN can reach either.
- **The control channel does four things:** provision, teardown, status, health. Every call but health needs the bearer token minted for this machine at register. There is no command execution, no file access, no shell, and no way for anyone at the marketplace to log in to your machine — nobody asks for, or gets, an account on it.
- **Hosting checks are local.** `check` reads `/dev/kvm`, `/sys/kernel/iommu_groups`, `nvidia-smi`/`rocm-smi` and your `PATH`, and reports the result. It changes nothing.
- **Files:** the binary (`/usr/local/bin/gpu-agent`), `/etc/gpu-agent` (the agent's SSH key, control token, registration and tunnel config, all `0600`), `/var/lib/gpu-agent` (the rental base image, the test-boot result and, while rented, the encrypted rental disk), and the service unit. The installer adds no users, kernel modules or drivers and installs only the OpenSSH client if it is missing; `gpu-agent runtime prepare --install-deps` additionally installs QEMU, UEFI firmware, `cloud-image-utils`, `cryptsetup-bin` and `nftables` with apt — nothing else, and nothing without that flag. While a rental runs, the agent also creates the `gpurent0` bridge, one nftables table, and (only if Docker or a firewall has set iptables' FORWARD policy to DROP) two accept rules for that bridge; all of them are removed when the rental ends.

## What a tenant can reach

Before a microVM boots, the agent loads one nftables table (`inet gpu_rental`)
and **reads it back from the kernel**; if the rules cannot be verified, nothing
boots. The microVM is attached only to the `gpurent0` bridge, and the table:

- drops every packet from the tenant to private and special ranges — `10/8`, `172.16/12`, `192.168/16`, `100.64/10`, `169.254/16`, `127/8`, multicast, `fc00::/7`, `fe80::/10`;
- drops every packet from the tenant to **every network configured on your machine**, whatever its addressing — your LAN and your NAS are blocked even if they are numbered from public space;
- drops every connection from the tenant to your machine itself, except DHCP and IPv6 neighbour discovery;
- drops every connection into the tenant from your network — the renter's SSH arrives through the agent's tunnel, not from your LAN.

`sudo gpu-agent check --rules` prints the exact table for your machine. The
tenant's disk is a per-rental encrypted overlay whose key only ever exists in
memory; at teardown it is destroyed and the GPU is reset and checked, and a
machine whose wipe or reset does not verify is quarantined instead of re-let.

## Removing the agent

```bash
sudo gpu-agent remove
```

This withdraws your listing and revokes the agent's access on the relay (while
the token that proves who it is still exists), stops and uninstalls the service,
deletes the rental firewall table and any rental disks, deletes `/etc/gpu-agent`
— the SSH key, token and registration — and deletes the binary. Your operating
system, drivers, packages and data are untouched; there is nothing to reinstall.
If the marketplace cannot be reached, the local removal still completes: the
agent's private key is gone, so the machine can never connect again, and the
command tells you the listing id to give support. On Windows, run it from an
Administrator PowerShell and delete `gpu-agent.exe` afterwards (Windows keeps a
running program locked).

`gpu-agent uninstall` is not the same thing: it only removes the service
definition and leaves the key, token and live listing behind.

## One machine, one listing

The agent registers the machine it runs on. Two machines — even two connected
to each other — are two agents and two listings; a listing that spans several
machines is not supported. Running `register` again on the same machine creates
a second listing, so do not, unless you mean to replace the first.

## Stats Endpoint

When running with a `config.yaml`, the agent exposes an HTTP endpoint on `127.0.0.1:9100` (loopback only — it answers without authentication, so it is never bound to your LAN):

- `GET /stats` — Returns system stats as JSON
- `GET /health` — Health check

Example response:
```json
{
  "hostname": "gpu-rig-01",
  "os": "linux",
  "arch": "amd64",
  "cpu": {
    "model": "AMD EPYC 7763",
    "cores": 64,
    "threads": 128,
    "usage_pct": 12.5
  },
  "memory": {
    "total_gb": 256,
    "available_gb": 240
  },
  "gpus": [
    {
      "model": "NVIDIA A100",
      "vram_total_gb": 80,
      "vram_used_gb": 2,
      "temp_c": 45,
      "utilization_pct": 0
    }
  ],
  "disk": {
    "total_gb": 2000,
    "free_gb": 1800
  },
  "status": "free",
  "uptime_seconds": 86400,
  "collected_at": "2026-04-07T12:00:00Z"
}
```

## Architecture

```
Provider (behind NAT)              Hub Servers (5 locations)
┌─────────────────┐        ┌─────────────────────────┐
│  gpu-agent (Go)  │        │  5 hub regions          │
│  reverse SSH     │◄──────►│  slot-routed relay      │
│  control :9101   │  SSH   │  forwarding-only        │
└─────────────────┘ tunnel  └────────┬────────────────┘
                                     │ pull /stats
                                     ▼
                            ┌─────────────────────────┐
                            │  Website GPU Listing     │
                            └─────────────────────────┘
```

- **Agent**: Single Go binary using [kardianos/service](https://github.com/kardianos/service) for cross-platform service management
- **Tunnel**: persistent reverse SSH tunnel (autossh-style), NAT-friendly (outbound only), relay host key pinned
- **Relay selection**: the control plane returns the account's relay list at register time; the agent TCP-probes them and prompts the provider to pick a location (closest preselected), then reports the choice back for slot allocation
- **Isolation**: each rental runs in a QEMU/KVM microVM (`-nodefaults`, seccomp sandbox, its own transient systemd unit) with the GPU's whole IOMMU group passed through via VFIO, behind a verified nftables fence with NAT to the internet only; its disk is dm-crypt with a key that never leaves memory; on turnover the key is discarded, the GPU reset, returned to its driver and checked, failing closed. Every step is recorded so a crash or reboot is torn down exactly. A machine that fails its hosting checks or its test boot refuses rentals and reports why
- **Control path**: the control plane reaches an agent only through the relay manager's `/agent/<listing_id>/{health,status,provision,teardown}` proxy, which forwards to that listing's own loopback slot and passes the bearer token through untouched
- **GPU detection**: NVIDIA (nvidia-smi), AMD (rocm-smi), Apple Silicon (system_profiler)

## Building from Source

```bash
# Requires Go 1.22+
go build -o gpu-agent ./cmd/gpu-agent/

# Cross-compile for Linux
GOOS=linux GOARCH=amd64 go build -o gpu-agent-linux-amd64 ./cmd/gpu-agent/

# Cross-compile for macOS ARM
GOOS=darwin GOARCH=arm64 go build -o gpu-agent-darwin-arm64 ./cmd/gpu-agent/
```

## License

MIT
