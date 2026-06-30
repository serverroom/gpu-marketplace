# GPU Marketplace

A P2P GPU marketplace agent that lets you list your GPU server for others to rent. The agent maintains a WireGuard tunnel to the nearest hub server and reports system stats on demand.

## How It Works

1. **Install the agent** on your GPU server (Linux, Windows, or macOS)
2. The agent **tests latency** to all hub servers and connects to the fastest one
3. A **WireGuard VPN tunnel** is established between your server and the hub
4. The hub **pulls stats** (CPU, RAM, GPU, VRAM, temperature) from your server when needed
5. Your server appears on the **marketplace listing** for potential renters

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

## Verifying Downloads

Every release publishes a `checksums.txt` (SHA-256) alongside the binaries. The
install scripts automatically verify the downloaded `gpu-agent` binary against it
and abort (deleting the file) if it doesn't match.

If a release has no `checksums.txt`, verification is skipped with a warning. Set
`GPU_AGENT_REQUIRE_CHECKSUM=1` before running an installer to turn a missing
checksum into a hard failure instead.

To verify a manual download yourself, grab the binary and `checksums.txt` from the
[Releases](https://github.com/serverroom/gpu-marketplace/releases) page, then:

```bash
sha256sum  --ignore-missing -c checksums.txt   # Linux
shasum -a 256 --ignore-missing -c checksums.txt # macOS
```

## Manual Setup

### Prerequisites
- [WireGuard](https://www.wireguard.com/install/) installed
- Download the `gpu-agent` binary from [Releases](https://github.com/serverroom/gpu-marketplace/releases)

### Steps

```bash
# 1. Run setup (tests latency, registers with hub, configures WireGuard)
sudo gpu-agent setup

# 2. Install as a system service
sudo gpu-agent install

# 3. Start the service
sudo gpu-agent start
```

## Agent Commands

| Command | Description |
|---------|-------------|
| `gpu-agent setup` | Interactive setup (latency test, hub registration, WireGuard config) |
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
│  gpu-agent (Go)  │        │  US-36, US-50, US-60    │
│  WireGuard peer  │◄──────►│  US-61, EU-72           │
│  HTTP :9100      │  WG    │  WireGuard + peer-mgr   │
└─────────────────┘ tunnel  └────────┬────────────────┘
                                     │ pull /stats
                                     ▼
                            ┌─────────────────────────┐
                            │  Website GPU Listing     │
                            └─────────────────────────┘
```

- **Agent**: Single Go binary using [kardianos/service](https://github.com/kardianos/service) for cross-platform service management
- **Tunnel**: WireGuard VPN with automatic NAT traversal (PersistentKeepalive=25)
- **Hub selection**: Latency-based (TCP probe to all hubs, picks fastest)
- **Stats**: Pulled on-demand by the hub via the WireGuard tunnel
- **GPU detection**: NVIDIA (nvidia-smi), AMD (rocm-smi), Apple Silicon (system_profiler)

## Building from Source

```bash
# Requires Go 1.26+ (see go.mod)
go build -o gpu-agent ./cmd/gpu-agent/

# Cross-compile for Linux
GOOS=linux GOARCH=amd64 go build -o gpu-agent-linux-amd64 ./cmd/gpu-agent/

# Cross-compile for macOS ARM
GOOS=darwin GOARCH=arm64 go build -o gpu-agent-darwin-arm64 ./cmd/gpu-agent/
```

## Releasing

Releases are built automatically by [`.github/workflows/release.yml`](.github/workflows/release.yml).
Push a version tag and the workflow cross-compiles every supported OS/arch, generates
`checksums.txt`, and publishes a GitHub Release with all assets:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The asset names it produces (`gpu-agent-<os>-<arch>[.exe]`) match exactly what the
install scripts download and verify, so a tagged release is all that's needed to ship
an update.

## Hub Setup (Internal)

See [hub/](hub/) for hub server setup scripts:
- `wg-hub-setup.sh` — One-time WireGuard hub configuration
- `peer-manager.py` — Flask API for peer registration
- `stats-puller.py` — Pull stats from all connected providers

## License

MIT
