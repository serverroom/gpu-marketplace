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

    # Resolve the tag once: the binary and the checksums that vouch for it must
    # come from the same release, or the check proves nothing.
    $asset = "gpu-agent-windows-$goarch.exe"
    try {
        $release = Invoke-RestMethod "https://api.github.com/repos/$REPO/releases/latest" -ErrorAction Stop
        $tag = $release.tag_name
        Write-Host "Latest release: $tag"
    } catch {
        Write-Host "Warning: No releases found. Using v0.1.0..."
        $tag = "v0.1.0"
    }
    $releaseUrl = "https://github.com/$REPO/releases/download/$tag"
    $downloadUrl = "$releaseUrl/$asset"

    # Download beside the target, verify, and only then move it into place — a
    # failed or tampered download must not overwrite a working agent.
    $tmpAgent = "$INSTALL_DIR\.gpu-agent.download.$PID"
    try {
        Invoke-WebRequest -Uri $downloadUrl -OutFile $tmpAgent -UseBasicParsing -ErrorAction Stop
    } catch {
        Remove-Item $tmpAgent -Force -ErrorAction SilentlyContinue
        Write-Host "Error: Failed to download gpu-agent binary." -ForegroundColor Red
        Write-Host ""
        Write-Host "The agent is a self-contained binary - you do not need to install"
        Write-Host "anything else to run it. Check the releases page for a windows/$goarch"
        Write-Host "build: https://github.com/$REPO/releases"
        Write-Host "(From a source checkout, with a Go toolchain: go build ./cmd/gpu-agent/)"
        # `throw` (not `exit`) so the host window survives under `irm | iex`.
        throw "Failed to download gpu-agent binary from $downloadUrl"
    }

    # Verify before this becomes an executable in Program Files. A 200 from
    # GitHub says nothing about a truncated or substituted body.
    Write-Host "Verifying checksum..."
    $tmpSums = "$INSTALL_DIR\.gpu-agent.checksums.$PID"
    try {
        # -OutFile, not .Content: GitHub serves release assets as
        # application/octet-stream, and PowerShell 7 hands back a Byte[] for
        # that, which splits into individual byte values instead of lines.
        # Windows PowerShell 5.1 returns a string, so this fails on exactly one
        # of the two hosts a provider might have. Read it off disk instead.
        Invoke-WebRequest -Uri "$releaseUrl/checksums.txt" -OutFile $tmpSums -UseBasicParsing -ErrorAction Stop
        $sums = Get-Content -Path $tmpSums -Raw
    } catch {
        Remove-Item $tmpAgent, $tmpSums -Force -ErrorAction SilentlyContinue
        throw "Could not download $releaseUrl/checksums.txt - refusing to install a binary that cannot be verified."
    }

    # sha256sum writes "<hash>  <name>" in text mode and "<hash> *<name>" in
    # binary mode; accept either rather than depending on how it was produced.
    $expected = $null
    foreach ($line in ($sums -split "`n")) {
        $parts = $line.Trim() -split '\s+', 2
        if ($parts.Count -eq 2 -and ($parts[1] -replace '^\*', '') -eq $asset) {
            $expected = $parts[0]
            break
        }
    }
    if (-not $expected) {
        Remove-Item $tmpAgent, $tmpSums -Force -ErrorAction SilentlyContinue
        throw "checksums.txt for $tag lists no $asset - that release publishes no build for this platform."
    }

    $actual = (Get-FileHash -Path $tmpAgent -Algorithm SHA256).Hash.ToLower()
    if ($actual -ne $expected.ToLower()) {
        Remove-Item $tmpAgent, $tmpSums -Force -ErrorAction SilentlyContinue
        Write-Host "Error: the downloaded binary does not match the published checksum." -ForegroundColor Red
        Write-Host "  expected: $expected"
        Write-Host "  got:      $actual"
        throw "Refusing to install an unverified gpu-agent binary."
    }
    Remove-Item $tmpSums -Force -ErrorAction SilentlyContinue
    Write-Host "Checksum verified."

    Move-Item -Path $tmpAgent -Destination "$INSTALL_DIR\gpu-agent.exe" -Force

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

    # `gpu-agent install` refuses to overwrite an existing service registration,
    # so on an upgrade the box would keep the old one. Replace it instead.
    if (Get-Service -Name "gpu-agent" -ErrorAction SilentlyContinue) {
        Write-Host "Existing service found; replacing it."
        & "$INSTALL_DIR\gpu-agent.exe" uninstall 2>&1 | Out-Null
    }

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
Write-Host ""
Write-Host "Note: Windows cannot host rentals (they run in a Linux KVM microVM); 'gpu-agent check' explains."
Write-Host "Remove the agent and withdraw the listing (Administrator): gpu-agent remove"
