# GPU Marketplace Agent Installer — Windows
# Usage: irm https://raw.githubusercontent.com/serverroom/gpu-marketplace/main/scripts/install.ps1 | iex

$ErrorActionPreference = "Stop"
$REPO = "serverroom/gpu-marketplace"
$INSTALL_DIR = "$env:ProgramFiles\gpu-agent"
$CONFIG_DIR = "$env:ProgramData\gpu-agent"

Write-Host "===============================" -ForegroundColor Cyan
Write-Host " GPU Marketplace Agent Installer" -ForegroundColor Cyan
Write-Host "===============================" -ForegroundColor Cyan
Write-Host ""

# Check admin privileges
$isAdmin = ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
    Write-Host "Error: This script must be run as Administrator." -ForegroundColor Red
    Write-Host "Right-click PowerShell and select 'Run as Administrator'."
    exit 1
}

# Detect architecture
$arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture
$goarch = switch ($arch) {
    "X64"   { "amd64" }
    "Arm64" { "arm64" }
    default {
        Write-Host "Unsupported architecture: $arch" -ForegroundColor Red
        exit 1
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
        $wgInstaller = "$env:TEMP\wireguard-installer.msi"
        Invoke-WebRequest -Uri "https://download.wireguard.com/windows-client/wireguard-installer.exe" -OutFile $wgInstaller
        Start-Process -FilePath $wgInstaller -ArgumentList "/S" -Wait
        Remove-Item $wgInstaller -ErrorAction SilentlyContinue
    }
    Write-Host "WireGuard installed."
}

# Download latest agent binary
function Download-Agent {
    Write-Host "Downloading gpu-agent for windows/$goarch..."

    # Create install directory
    New-Item -ItemType Directory -Path $INSTALL_DIR -Force | Out-Null

    # Get latest release
    try {
        $release = Invoke-RestMethod "https://api.github.com/repos/$REPO/releases/latest" -ErrorAction Stop
        $tag = $release.tag_name
        Write-Host "Latest release: $tag"
        $downloadUrl = "https://github.com/$REPO/releases/download/$tag/gpu-agent-windows-$goarch.exe"
    } catch {
        Write-Host "Warning: No releases found. Using v0.1.0..."
        $downloadUrl = "https://github.com/$REPO/releases/download/v0.1.0/gpu-agent-windows-$goarch.exe"
    }

    try {
        Invoke-WebRequest -Uri $downloadUrl -OutFile "$INSTALL_DIR\gpu-agent.exe" -ErrorAction Stop
    } catch {
        Write-Host "Error: Failed to download gpu-agent binary." -ForegroundColor Red
        Write-Host "You may need to build from source: go build ./cmd/gpu-agent/"
        exit 1
    }

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
