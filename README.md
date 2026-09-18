# GPU Marketplace

A P2P GPU marketplace agent that lets you list your GPU server for others to rent. The agent registers your host with a one-time code, keeps a reverse SSH tunnel to the nearest relay, checks whether the machine can host a rental safely, and — once the rental runtime exists — runs rentals in isolated microVMs.

> **Status: a machine is offered to renters only after it has proven it can host.** The agent carries its own microVM runtime (QEMU/KVM, GPU passthrough over VFIO, a per-rental encrypted disk). A machine reports *ready* only when every hosting check passes **and** it has passed a real test boot on its own hardware; until then the marketplace neither shows it to renters nor accepts orders for it. Since v0.1.10 the agent does the setup that gets it there by itself once the machine is linked -- see [Automatic setup after linking](#automatic-setup-after-linking). Up to v0.1.5 the agent answered rental requests with a stub that reported success and created nothing; that stub is gone. See [Can this machine host a rental?](#can-this-machine-host-a-rental)

## How It Works

1. **Install the agent** on your Linux GPU server
2. Generate a **one-time registration code** in your dashboard and run `gpu-agent register --code <code>`
3. The agent registers, then **tests latency** to the available locations and **prompts you to pick one** (closest preselected) — under the hood it opens a **reverse SSH tunnel** to that location's relay. `register` restarts the agent service, so the machine is online at once
4. The agent **finishes the machine's setup by itself**: it installs the microVM runtime, builds the rental image and runs a test rental — about 25-45 minutes, nothing to type; the control panel shows each step ([Automatic setup after linking](#automatic-setup-after-linking))
5. **Configure your listing** in your dashboard; nothing is published until you do
6. Once configured **and** the machine passes its hosting checks, your server appears on the **marketplace listing** for renters
7. When rented, the agent fences the tenant off your network, boots an **isolated microVM** with the GPUs passed through, and **wipes it clean** when the rental ends — refusing the rental outright if any of that cannot be done

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
| `gpu-agent setup` | Finish this machine's setup now, in the foreground with its output: the same steps the agent takes by itself after linking (for support) |
| `gpu-agent setup --status` | Show the last setup attempt, and whether the automatic setup is on |
| `gpu-agent setup --off` / `--on` | Turn the automatic setup off (the agent then waits for the manual commands below) or back on |
| `gpu-agent runtime prepare` | Install the microVM runtime (`--install-deps`) and build the rental base image with the NVIDIA driver — by default the one matching this machine's own driver (`--driver 580-server` picks one) |
| `gpu-agent runtime prepare --headless` | Make a desktop machine (a workstation) start without its desktop, so the GPU is free to rent; closes the desktop now. `gpu-agent remove` brings the desktop back. A DGX Spark does not need it: its desktop closes only while it is rented or testing |
| `gpu-agent check --boot` | Boot a real test rental with the GPU passed through, check what it can and cannot reach, tear it down, and record the result |
| `gpu-agent update` | Update the agent to the latest release (`--version v0.1.11` picks one), exactly as "Update agent" in the control panel does; Linux |
| `gpu-agent install` | Install as a system service (systemd/launchd/Windows Service) |
| `gpu-agent remove` | Withdraw the listing, revoke relay access and delete the agent completely (`--yes` skips the prompt) |
| `gpu-agent uninstall` | Remove the system service only — keys, token and listing stay; use `remove` to take everything off |
| `gpu-agent start` | Start the service |
| `gpu-agent stop` | Stop the service |
| `gpu-agent status` | Check the service *and* the registration state |
| `gpu-agent speedtest` | Measure download, upload and latency to the speed test server in this machine's location, and post them to the listing (not while rented) |
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

Once registered, the agent measures this machine's network once, against the
speed test server in the location of its relay: download and upload in Mbps,
and latency in ms. The result is shown on the listing, so renters see what the
connection can do before they rent it. The test takes up to 40 seconds in the
background. It never runs while the machine is rented, and it stops if a rental
starts; if a rental is on the machine, it waits for the next agent start.
`sudo gpu-agent speedtest` measures again at any time the machine is not rented,
and updates the listing.

## Automatic setup after linking

Linking a machine (`sudo gpu-agent register --code <code>`) is the last command a
host types. The agent restarts, reports the machine to the marketplace, and — when
the only things between the machine and *ready* are steps it can take itself —
takes them in the background:

1. **Installs the microVM runtime** with apt: `qemu-system-x86` (or `qemu-system-arm`),
   `qemu-utils`, `ovmf` (or `qemu-efi-aarch64`), `cloud-image-utils`, `cryptsetup-bin`,
   `nftables`, `iproute2` and `kmod`, the same packages as
   `runtime prepare --install-deps`. Nothing else. A few minutes.
2. **Builds the rental image**: Ubuntu 26.04's official cloud image, checked against
   Canonical's published checksums, with the NVIDIA driver baked in. The driver
   matches this machine's own: the same branch, and the open kernel module when this
   machine runs the open one (`580-server-open` on a DGX Spark), the proprietary one
   otherwise (`580-server`). About 20-30 minutes.
3. **Runs a test rental** with the GPU passed through, exactly as
   `sudo gpu-agent check --boot` does. About 5-15 minutes.

Only the steps a machine still needs run; a machine that already has its image only
runs the test. While it works, the control panel shows one line in place of the
reasons, for example:

```
Setting up automatically: building the rental image (step 2 of 3, started 14:05 UTC). Nothing to do; this takes about 20-30 minutes.
```

`sudo gpu-agent status` shows the same progress, and `sudo gpu-agent setup --status`
the last attempt in full. When it finishes, the machine reports ready. If a step
fails (a mirror is down, the network drops), the panel says why and the agent tries
again after its next restart or in 6 hours; `sudo gpu-agent setup` runs it at once,
with its output.

It never runs while a rental (or the leftover of one) is on the machine, and no rental
can start while it runs: the machine is not ready until it has finished. It runs once
per agent version, host driver and Ubuntu release; a new agent, a driver update or a
different GPU is a new setup. It does **not** try to fix what needs a person — no KVM,
the IOMMU off, a GPU sharing its IOMMU group, too little memory or disk, a desktop on
the GPU of a machine that is not a DGX Spark, a system without apt — those stay in
`gpu-agent check` for you to fix.

**Turning it off:** `sudo gpu-agent setup --off` (it writes `off` to
`/etc/gpu-agent/auto-setup`); `--on` turns it back on. With it off, the machine waits
for the manual commands, which stay available for support:
`sudo gpu-agent runtime prepare --install-deps` (packages and image) and
`sudo gpu-agent check --boot` (the test rental).

## Updating the agent, and removing a machine, from the control panel

**Update agent.** When a newer agent is released, the control panel offers to update
a published Linux machine that is not rented. The agent checks the request at once —
a release version (`vX.Y.Z`) newer than the one running, a release build, no rental,
no setup or test boot and no other update under way — and answers; the rest happens in
the background, and the panel follows it:

1. **downloading** the release for this machine from
   `github.com/serverroom/gpu-marketplace/releases` — the agent builds that address
   itself; only the version comes from the panel;
2. **verifying** it against the release's `checksums.txt` (the installers' rule) and
   by running it: it must say it is the version asked for;
3. **restarting**: the running binary is kept as `gpu-agent.prev`, the new one is moved
   into its place in one step, and the agent restarts a few seconds later.

Three minutes after the restart, the previous binary checks the new agent from a
systemd timer of its own: if the service is not running, or is not running the new
binary, it puts itself back and restarts the agent. The outcome is **ok**,
**rolled_back** or **failed** (anything that fails before the new binary is moved into
place leaves the running agent untouched), recorded in `/var/lib/gpu-agent/update.json`
and shown by the panel and by `sudo gpu-agent status`. `sudo gpu-agent update` does the
same from the machine. Agents older than v0.1.10 cannot update themselves: run the
install command once.

**Remove from marketplace.** When the host removes a published machine in the control
panel (or its capability report is answered *410 Gone*), the agent stops hosting: it
refuses every rental, stops its automatic setup, its reports and its tunnel, and removes
a rental firewall table a crash may have left. It is not uninstalled — the binary and
`/etc/gpu-agent` stay, so the agent says on every start, and in `status` and `check`:

```
This machine was removed from the marketplace in the control panel. Run 'sudo gpu-agent remove' to uninstall the agent, or register again with a new code to list it again.
```

A rented machine is removed once its rental ends. Registering again with a new code
lists it again (the record, `/var/lib/gpu-agent/withdrawn.json`, is cleared).

## DGX Spark: the desktop comes and goes with a rental

A DGX Spark draws its desktop on its only GPU, the GB10. On a machine the agent
confirms as a DGX Spark (Linux on arm64, DMI vendor NVIDIA, product "DGX Spark", a GB10
GPU), the desktop is not a reason to refuse a rental: **the desktop closes while the
Spark is rented or running its test rental, and comes back after.** The agent stops the
display manager (`display-manager.service`, or `gdm3`), waits up to 30 seconds for the
desktop to let go of the GPU, and starts the display manager again once the GPU is back
with its driver — also after an agent crash or a reboot in the middle of a rental.
Anything open on the Spark's screen closes with the desktop, so do not keep unsaved work
on it while it is listed. If the desktop does not let go, the display manager is started
again and the rental is refused. A Spark made headless on purpose
(`runtime prepare --headless`) is left headless. The capability the agent reports carries
the raw DMI strings (`identity`) so the match can be checked.

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
- **The rental runtime installed and its base image built** — QEMU, UEFI firmware (OVMF/AAVMF), `cloud-image-utils`, `cryptsetup` and `nftables`, and Ubuntu 26.04's official cloud image, verified against Canonical's published checksums, with the NVIDIA driver matching this machine's baked in. The agent does this by itself after linking; by hand, `sudo gpu-agent runtime prepare --install-deps`.
- **No GPU in use on the host** while a rental starts — the GPU is handed to the microVM whole, so anything using it (a container, a training job) must be stopped first. The agent refuses and names what holds it. NVIDIA's own background services (`nvidia-persistenced`, `nvidia-powerd`, DCGM) are not a problem: the agent stops the running ones for the rental and starts them again afterwards. Short-lived tools (`nvidia-smi`, `dcgmi`, a bug report) are given up to 5 seconds to finish, and the agent pauses its own GPU queries while it takes the GPU.
- **No desktop on the GPU** — except on a DGX Spark, whose desktop closes while it is rented and comes back after ([above](#dgx-spark-the-desktop-comes-and-goes-with-a-rental)). Any other machine that shows a desktop on its GPU (a workstation) holds it for as long as the desktop runs, so it cannot host until it runs without one. `sudo gpu-agent runtime prepare --headless` makes it start without a desktop from now on and closes the running one (anything open on its screen closes too, so run it over SSH or be ready to log in again on the text screen); `sudo gpu-agent remove` restores the desktop from the next start. `gpu-agent check` names the desktop processes while this is needed.
- **Enough memory and disk** — at least 6 GB of memory and 20 GB free under `/var/lib/gpu-agent`. A rental gets the machine's memory less a tenth (never under 4 GB) for the host.
- **A passing test boot on this machine.** The automatic setup runs it; `sudo gpu-agent check --boot` runs it by hand: a real rental for a few minutes with a key nobody holds: the VM reports the GPU it sees, that it reaches the internet, and that your machine, its address and its gateway are unreachable; it is then torn down and the GPU checked. The result is recorded and must be for the agent version you are running.

## What the agent does on your machine

It runs as root, because booting a microVM with a GPU passed through, loading
firewall rules and creating an encrypted disk all need it. Concretely:

- **Network: outbound only.** One SSH connection to the relay you picked (port 2222). Its key on the relay is `restrict`ed to two reverse forwards — no shell, no command, nothing else. Nothing is opened on your router.
- **Listens on loopback only.** `127.0.0.1:9101` is the control channel; `127.0.0.1:9100` is the legacy stats endpoint (only when `config.yaml` exists). Nothing on your LAN can reach either.
- **The control channel does six things:** provision, teardown, status, health, update (to a newer release of this agent, from GitHub, verified — see above) and withdrawn (stop hosting). Every call but health needs the bearer token minted for this machine at register. There is no command execution, no file access, no shell, and no way for anyone at the marketplace to log in to your machine — nobody asks for, or gets, an account on it.
- **Hosting checks are local.** `check` reads `/dev/kvm`, `/sys/kernel/iommu_groups`, `nvidia-smi`/`rocm-smi` and your `PATH`, and reports the result. It changes nothing.
- **Files:** the binary (`/usr/local/bin/gpu-agent`), `/etc/gpu-agent` (the agent's SSH key, control token, registration and tunnel config, all `0600`, and `auto-setup` if you turned the automatic setup off), `/var/lib/gpu-agent` (the rental base image, the test-boot result, the last setup attempt and, while rented, the encrypted rental disk), and the service unit. The installer adds no users, kernel modules or drivers and installs only the OpenSSH client if it is missing. After linking, the automatic setup (or `gpu-agent runtime prepare --install-deps`) additionally installs QEMU, UEFI firmware, `cloud-image-utils`, `cryptsetup-bin`, `nftables`, `iproute2` and `kmod` with apt — nothing else; `sudo gpu-agent setup --off` before linking keeps it from doing so. While a rental runs, the agent also creates the `gpurent0` bridge, one nftables table, and (only if Docker or a firewall has set iptables' FORWARD policy to DROP) two accept rules for that bridge; all of them are removed when the rental ends.

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

# Releases stamp the version (the automatic setup and the test boot are per version)
go build -ldflags "-s -w -X main.version=v0.1.10" -o gpu-agent ./cmd/gpu-agent/
```

## License

MIT
