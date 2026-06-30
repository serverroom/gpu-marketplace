# GPU Marketplace Agent Installer — Windows
# Usage: irm https://raw.githubusercontent.com/serverroom/gpu-marketplace/main/scripts/install.ps1 | iex

$ErrorActionPreference = "Stop"
$REPO = "serverroom/gpu-marketplace"
$INSTALL_DIR = "$env:ProgramFiles\gpu-agent"
$CONFIG_DIR = "$env:ProgramData\gpu-agent"
$SCRIPT_URL = "https://raw.githubusercontent.com/$REPO/main/scripts/install.ps1"

Write-Host "===============================" -ForegroundColor Cyan
Write-Host " GPU Marketplace Agent Installer" -ForegroundColor Cyan
Write-Host "===============================" -ForegroundColor Cyan
Write-Host ""

# Check admin privileges — self-elevate if needed.
#
# IMPORTANT: this script is meant to be run via `irm ... | iex`, which executes
# in the *caller's* session scope. A bare `exit` would therefore terminate the
# entire PowerShell window. So on the non-admin path we open a fresh elevated
# window and `return` instead.
$isAdmin = ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
    Write-Host "This installer needs Administrator rights to:" -ForegroundColor Yellow
    Write-Host "  - install WireGuard" -ForegroundColor Yellow
    Write-Host "  - write to '$INSTALL_DIR'" -ForegroundColor Yellow
    Write-Host "  - edit the system PATH" -ForegroundColor Yellow
    Write-Host "  - register the 'GPU Marketplace Agent' Windows service" -ForegroundColor Yellow
    Write-Host ""
    Write-Host "Requesting elevation - please approve the UAC prompt..." -ForegroundColor Yellow

    $elevatedCommand = "irm $SCRIPT_URL | iex"
    try {
        Start-Process -FilePath "powershell.exe" -Verb RunAs -ArgumentList @(
            "-NoExit",
            "-ExecutionPolicy", "Bypass",
            "-Command", $elevatedCommand
        )
        Write-Host "An elevated window has been opened to continue the install." -ForegroundColor Green
    } catch {
        Write-Host ""
        Write-Host "Elevation was cancelled. To install, open PowerShell with" -ForegroundColor Red
        Write-Host "'Run as administrator' and run:" -ForegroundColor Red
        Write-Host "  irm $SCRIPT_URL | iex" -ForegroundColor White
    }
    # `return`, not `exit`: this keeps the caller's window open under `irm | iex`.
    return
}

# Detect architecture
$arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture
$goarch = switch ($arch) {
    "X64"   { "amd64" }
    "Arm64" { "arm64" }
    default {
        # `throw` (not `exit`) so the host window survives under `irm | iex`.
        throw "Unsupported architecture: $arch"
    }
}
Write-Host "Detected architecture: $arch ($goarch)"

# Install WireGuard
function Install-WireGuard {
    if (Get-Command "wireguard.exe" -ErrorAction SilentlyContinue) {
        Write-Host "WireGuard already installed."
        return
    }

    Write-Host "Installing WireGuard..."
    if (Get-Command "winget" -ErrorAction SilentlyContinue) {
        winget install --id WireGuard.WireGuard --accept-source-agreements --accept-package-agreements --silent
    } elseif (Get-Command "choco" -ErrorAction SilentlyContinue) {
        choco install wireguard -y
    } else {
        Write-Host "Downloading WireGuard installer..."
        $wgInstaller = "$env:TEMP\wireguard-installer.exe"
        Invoke-WebRequest -Uri "https://download.wireguard.com/windows-client/wireguard-installer.exe" -OutFile $wgInstaller
        Start-Process -FilePath $wgInstaller -ArgumentList "/S" -Wait
        Remove-Item $wgInstaller -ErrorAction SilentlyContinue
    }
    Write-Host "WireGuard installed."
}

# Verify a downloaded file against the release's checksums.txt (SHA-256).
# checksums.txt format (one line per asset): "<sha256>  <asset-filename>"
# Set GPU_AGENT_REQUIRE_CHECKSUM=1 to hard-fail when no checksum is available.
function Verify-Checksum {
    param(
        [Parameter(Mandatory)] [string]$FilePath,
        [Parameter(Mandatory)] [string]$AssetName,
        [Parameter(Mandatory)] [string]$Tag
    )

    $require = ($env:GPU_AGENT_REQUIRE_CHECKSUM -eq "1")
    $checksumsUrl = "https://github.com/$REPO/releases/download/$Tag/checksums.txt"
    $checksumsFile = Join-Path $env:TEMP "gpu-agent-checksums.txt"

    try {
        Invoke-WebRequest -Uri $checksumsUrl -OutFile $checksumsFile -ErrorAction Stop
    } catch {
        if ($require) { throw "No checksums.txt for $Tag and GPU_AGENT_REQUIRE_CHECKSUM=1. Aborting." }
        Write-Host "WARNING: No checksums.txt for $Tag - skipping integrity verification." -ForegroundColor Yellow
        return
    }

    $expected = $null
    foreach ($line in Get-Content $checksumsFile) {
        $parts = $line.Trim() -split '\s+', 2
        if ($parts.Count -eq 2 -and $parts[1].TrimStart('*') -eq $AssetName) {
            $expected = $parts[0].ToLower()
            break
        }
    }
    Remove-Item $checksumsFile -ErrorAction SilentlyContinue

    if (-not $expected) {
        if ($require) { throw "No checksum entry for $AssetName in checksums.txt. Aborting." }
        Write-Host "WARNING: No checksum entry for $AssetName - skipping verification." -ForegroundColor Yellow
        return
    }

    $actual = (Get-FileHash -Path $FilePath -Algorithm SHA256).Hash.ToLower()
    if ($actual -ne $expected) {
        Remove-Item $FilePath -ErrorAction SilentlyContinue
        throw "Checksum MISMATCH for $AssetName`n  expected: $expected`n  actual:   $actual`nDeleted the downloaded binary. Aborting."
    }
    Write-Host "Checksum verified ($AssetName)." -ForegroundColor Green
}

# Download latest agent binary
function Download-Agent {
    Write-Host "Downloading gpu-agent for windows/$goarch..."

    # Create install directory
    New-Item -ItemType Directory -Path $INSTALL_DIR -Force | Out-Null

    $assetName = "gpu-agent-windows-$goarch.exe"

    # Resolve the release tag (fall back to v0.1.0 if there are no releases yet)
    try {
        $release = Invoke-RestMethod "https://api.github.com/repos/$REPO/releases/latest" -ErrorAction Stop
        $tag = $release.tag_name
        Write-Host "Latest release: $tag"
    } catch {
        Write-Host "Warning: No releases found. Using v0.1.0..."
        $tag = "v0.1.0"
    }

    $downloadUrl = "https://github.com/$REPO/releases/download/$tag/$assetName"
    $target = "$INSTALL_DIR\gpu-agent.exe"

    try {
        Invoke-WebRequest -Uri $downloadUrl -OutFile $target -ErrorAction Stop
    } catch {
        Write-Host "Error: Failed to download gpu-agent binary." -ForegroundColor Red
        Write-Host "You may need to build from source: go build ./cmd/gpu-agent/"
        # `throw` (not `exit`) so the host window survives under `irm | iex`.
        throw "Failed to download gpu-agent binary from $downloadUrl"
    }

    # Verify integrity before we put it on PATH / execute it.
    Verify-Checksum -FilePath $target -AssetName $assetName -Tag $tag

    # Add to PATH if not already there
    $currentPath = [Environment]::GetEnvironmentVariable("Path", "Machine")
    if ($currentPath -notlike "*$INSTALL_DIR*") {
        [Environment]::SetEnvironmentVariable("Path", "$currentPath;$INSTALL_DIR", "Machine")
        $env:Path += ";$INSTALL_DIR"
        Write-Host "Added $INSTALL_DIR to system PATH."
    }

    Write-Host "Installed to $INSTALL_DIR\gpu-agent.exe"
}

# Run setup
function Run-Setup {
    Write-Host ""
    Write-Host "Running agent setup..."
    & "$INSTALL_DIR\gpu-agent.exe" setup
}

# Install as Windows Service
function Install-Service {
    Write-Host ""
    Write-Host "Installing as Windows Service..."
    & "$INSTALL_DIR\gpu-agent.exe" install
    & "$INSTALL_DIR\gpu-agent.exe" start
    Write-Host "Service installed and started."
    Write-Host ""
    Write-Host "Check status: gpu-agent status"
    Write-Host "View service: services.msc (look for 'GPU Marketplace Agent')"
}

# Main
Install-WireGuard
Download-Agent
Run-Setup
Install-Service

Write-Host ""
Write-Host "================================================" -ForegroundColor Green
Write-Host " GPU Marketplace Agent installed successfully!" -ForegroundColor Green
Write-Host "================================================" -ForegroundColor Green
Write-Host ""
Write-Host "Your server is now reporting stats to the hub."
Write-Host "Stats endpoint: http://localhost:9100/stats"
