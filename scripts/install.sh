#!/usr/bin/env bash
set -euo pipefail

# GPU Marketplace Agent Installer — Linux
# Usage: curl -sSL https://raw.githubusercontent.com/serverroom/gpu-marketplace/main/scripts/install.sh | sudo bash

REPO="serverroom/gpu-marketplace"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/gpu-agent"

echo "==============================="
echo " GPU Marketplace Agent Installer"
echo "==============================="
echo

# Check root
if [ "$(id -u)" -ne 0 ]; then
    echo "Error: This script must be run as root (or with sudo)."
    exit 1
fi

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
    x86_64)  GOARCH="amd64" ;;
    aarch64) GOARCH="arm64" ;;
    arm64)   GOARCH="arm64" ;;
    *)
        echo "Unsupported architecture: $ARCH"
        exit 1
        ;;
esac
echo "Detected architecture: $ARCH ($GOARCH)"

# Detect package manager and install WireGuard
install_wireguard() {
    if command -v wg &>/dev/null; then
        echo "WireGuard already installed."
        return
    fi

    echo "Installing WireGuard..."
    if command -v apt-get &>/dev/null; then
        apt-get update -qq
        apt-get install -y -qq wireguard wireguard-tools
    elif command -v dnf &>/dev/null; then
        dnf install -y wireguard-tools
    elif command -v yum &>/dev/null; then
        yum install -y epel-release
        yum install -y wireguard-tools
    elif command -v pacman &>/dev/null; then
        pacman -Sy --noconfirm wireguard-tools
    elif command -v zypper &>/dev/null; then
        zypper install -y wireguard-tools
    else
        echo "Error: Unsupported package manager. Install WireGuard manually."
        exit 1
    fi
    echo "WireGuard installed."
}

# Download latest release binary
download_agent() {
    echo "Downloading gpu-agent for linux/$GOARCH..."

    # Get latest release tag
    LATEST=$(curl -sSL "https://api.github.com/repos/$REPO/releases/latest" | grep '"tag_name"' | head -1 | cut -d'"' -f4)
    if [ -z "$LATEST" ]; then
        echo "Warning: No releases found. Downloading from main branch..."
        DOWNLOAD_URL="https://github.com/$REPO/releases/download/v0.1.0/gpu-agent-linux-$GOARCH"
    else
        echo "Latest release: $LATEST"
        DOWNLOAD_URL="https://github.com/$REPO/releases/download/$LATEST/gpu-agent-linux-$GOARCH"
    fi

    curl -sSL -o "$INSTALL_DIR/gpu-agent" "$DOWNLOAD_URL" || {
        echo "Error: Failed to download gpu-agent binary."
        echo "You may need to build from source: go build ./cmd/gpu-agent/"
        exit 1
    }
    chmod +x "$INSTALL_DIR/gpu-agent"
    echo "Installed to $INSTALL_DIR/gpu-agent"
}

# Run setup (latency test, registration, WireGuard config)
run_setup() {
    echo
    echo "Running agent setup..."
    "$INSTALL_DIR/gpu-agent" setup
}

# Install as systemd service
install_service() {
    echo
    echo "Installing as system service..."
    "$INSTALL_DIR/gpu-agent" install
    "$INSTALL_DIR/gpu-agent" start
    echo "Service installed and started."
    echo
    echo "Check status: gpu-agent status"
    echo "View logs:    journalctl -u gpu-agent -f"
}

# Main
install_wireguard
download_agent
run_setup
install_service

echo
echo "================================================"
echo " GPU Marketplace Agent installed successfully!"
echo "================================================"
echo
echo "Your server is now reporting stats to the hub."
echo "Stats endpoint: http://localhost:9100/stats"
