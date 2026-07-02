# GPU Marketplace

A P2P GPU marketplace agent that lets you list your GPU server for others to rent. The agent registers your host with a one-time code, keeps a reverse SSH tunnel to the nearest relay, and runs rentals in isolated microVMs.

## How It Works

1. **Install the agent** on your Linux GPU server
2. Generate a **one-time registration code** in your dashboard and run `gpu-agent register --code <code>`
3. The agent registers, then **tests latency** to the available locations and **prompts you to pick one** (closest preselected) — under the hood it opens a **reverse SSH tunnel** to that location's relay
4. **Configure your listing** in your dashboard; nothing is published until you do
5. Once configured, your server appears on the **marketplace listing** for renters
6. When rented, the agent boots an **isolated microVM** with the GPUs passed through, and **wipes it clean** when the rental ends

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
| `gpu-agent install` | Install as a system service (systemd/launchd/Windows Service) |
| `gpu-agent uninstall` | Remove the system service |
| `gpu-agent start` | Start the service |
| `gpu-agent stop` | Stop the service |
| `gpu-agent status` | Check service status |
| `gpu-agent test-stats` | Collect and display system stats as JSON |
| `gpu-agent -version` | Print version |

Running without arguments starts the agent interactively or as a managed service.

## Stats Endpoint

When running, the agent exposes an HTTP endpoint on port 9100:

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
- **Isolation**: each rental runs in a Kata microVM with the GPUs passed through via VFIO; the disk is wiped and the GPU reset on turnover
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

## Relay Setup (Internal)

See [relay/](relay/) for the relay — the SSH jump host that terminates agent tunnels:
- `relaymgr.py` — slot allocation + restricted `authorized_keys`
- `server.py` — agent authorization API
- `sshd_config.example` — forwarding-only sshd drop-in

## License

MIT
