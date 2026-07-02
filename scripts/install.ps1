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
    Write-Host "  - enable the Windows OpenSSH client (if missing)" -ForegroundColor Yellow
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

# The agent tunnels over reverse SSH: it needs the OpenSSH client (ssh + ssh-keygen).
function Install-OpenSSHClient {
    if ((Get-Command "ssh.exe" -ErrorAction SilentlyContinue) -and
        (Get-Command "ssh-keygen.exe" -ErrorAction SilentlyContinue)) {
        Write-Host "OpenSSH client already installed."
        return
    }

    Write-Host "Enabling the Windows OpenSSH client..."
    $cap = Get-WindowsCapability -Online -Name "OpenSSH.Client*" | Select-Object -First 1
    if ($null -eq $cap) {
        throw "OpenSSH client capability not found; install OpenSSH manually and re-run."
    }
    if ($cap.State -ne "Installed") {
        Add-WindowsCapability -Online -Name $cap.Name | Out-Null
    }
    Write-Host "OpenSSH client ready."
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
        # `throw` (not `exit`) so the host window survives under `irm | iex`.
        throw "Failed to download gpu-agent binary from $downloadUrl"
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
Install-OpenSSHClient
Download-Agent
Install-Service

Write-Host ""
Write-Host "================================================" -ForegroundColor Green
Write-Host " GPU Marketplace Agent installed successfully!" -ForegroundColor Green
Write-Host "================================================" -ForegroundColor Green
Write-Host ""
Write-Host "Next: generate a one-time registration code in your dashboard, then run:"
Write-Host "  gpu-agent register --code <code>"
Write-Host "and restart the service to bring the tunnel up:"
Write-Host "  gpu-agent stop; gpu-agent start"
