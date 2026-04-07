#!/usr/bin/env bash
set -euo pipefail

# GPU Marketplace Agent Installer — macOS
# Usage: curl -sSL https://raw.githubusercontent.com/serverroom/gpu-marketplace/main/scripts/install-mac.sh | sudo bash

REPO="serverroom/gpu-marketplace"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/Library/Application Support/gpu-agent"

echo "==============================="
echo " GPU Marketplace Agent Installer"
echo "==============================="
echo

# Check root
if [ "$(id -u)" -ne 0 ]; then
    echo "Error: This script must be run with sudo."
    exit 1
fi

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
    x86_64)  GOARCH="amd64" ;;
    arm64)   GOARCH="arm64" ;;
    *)
        echo "Unsupported architecture: $ARCH"
        exit 1
        ;;
esac
echo "Detected architecture: $ARCH ($GOARCH)"

# Install WireGuard via Homebrew
install_wireguard() {
    if command -v wg &>/dev/null; then
        echo "WireGuard already installed."
        return
    fi

    echo "Installing WireGuard..."
    if command -v brew &>/dev/null; then
        # Run as the real user, not root
        REAL_USER="${SUDO_USER:-$USER}"
        sudo -u "$REAL_USER" brew install wireguard-tools
    else
        echo "Error: Homebrew not found. Install it first:"
        echo "  /bin/bash -c \"\$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)\""
        echo "Then re-run this installer."
        exit 1
    fi
    echo "WireGuard installed."
}

# Download latest release binary
download_agent() {
    echo "Downloading gpu-agent for darwin/$GOARCH..."

    LATEST=$(curl -sSL "https://api.github.com/repos/$REPO/releases/latest" | grep '"tag_name"' | head -1 | cut -d'"' -f4)
    if [ -z "$LATEST" ]; then
        echo "Warning: No releases found. Downloading from v0.1.0..."
        DOWNLOAD_URL="https://github.com/$REPO/releases/download/v0.1.0/gpu-agent-darwin-$GOARCH"
    else
        echo "Latest release: $LATEST"
        DOWNLOAD_URL="https://github.com/$REPO/releases/download/$LATEST/gpu-agent-darwin-$GOARCH"
    fi

    curl -sSL -o "$INSTALL_DIR/gpu-agent" "$DOWNLOAD_URL" || {
        echo "Error: Failed to download gpu-agent binary."
        echo "You may need to build from source: go build ./cmd/gpu-agent/"
        exit 1
    }
    chmod +x "$INSTALL_DIR/gpu-agent"
    echo "Installed to $INSTALL_DIR/gpu-agent"
}

# Run setup
run_setup() {
    echo
    echo "Running agent setup..."
    "$INSTALL_DIR/gpu-agent" setup
}

# Install as launchd daemon
install_service() {
    echo
    echo "Installing as launchd daemon..."
    "$INSTALL_DIR/gpu-agent" install
    "$INSTALL_DIR/gpu-agent" start
    echo "Service installed and started."
    echo
    echo "Check status: gpu-agent status"
    echo "View logs:    log show --predicate 'processImagePath contains \"gpu-agent\"' --last 1h"
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
