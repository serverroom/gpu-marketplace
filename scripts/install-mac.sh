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

# Verify a downloaded file against the release's checksums.txt (SHA-256).
# checksums.txt format (one line per asset): "<sha256>  <asset-filename>"
# Set GPU_AGENT_REQUIRE_CHECKSUM=1 to hard-fail when no checksum is available.
verify_checksum() {
    local file="$1" asset="$2" tag="$3"
    local checksums_url="https://github.com/$REPO/releases/download/$tag/checksums.txt"
    local tmp
    tmp="$(mktemp 2>/dev/null || mktemp -t gpu-agent)"

    if ! curl -fsSL -o "$tmp" "$checksums_url"; then
        rm -f "$tmp"
        if [ "${GPU_AGENT_REQUIRE_CHECKSUM:-0}" = "1" ]; then
            echo "Error: no checksums.txt for $tag and GPU_AGENT_REQUIRE_CHECKSUM=1. Aborting." >&2
            exit 1
        fi
        echo "WARNING: No checksums.txt for $tag - skipping integrity verification."
        return
    fi

    local expected
    expected="$(grep -E "[[:space:]]\*?${asset}\$" "$tmp" | awk '{print $1}' | head -1 || true)"
    rm -f "$tmp"

    if [ -z "$expected" ]; then
        if [ "${GPU_AGENT_REQUIRE_CHECKSUM:-0}" = "1" ]; then
            echo "Error: no checksum entry for $asset. Aborting." >&2
            exit 1
        fi
        echo "WARNING: No checksum entry for $asset - skipping verification."
        return
    fi

    local actual
    actual="$(shasum -a 256 "$file" | awk '{print $1}')"

    if [ "$actual" != "$expected" ]; then
        rm -f "$file"
        echo "Error: checksum MISMATCH for $asset!" >&2
        echo "  expected: $expected" >&2
        echo "  actual:   $actual" >&2
        echo "Deleted the downloaded binary. Aborting." >&2
        exit 1
    fi
    echo "Checksum verified ($asset)."
}

# Download latest release binary
download_agent() {
    echo "Downloading gpu-agent for darwin/$GOARCH..."

    local asset="gpu-agent-darwin-$GOARCH"

    # Resolve the release tag (fall back to v0.1.0 if there are no releases yet)
    local tag
    tag="$(curl -sSL "https://api.github.com/repos/$REPO/releases/latest" | grep '"tag_name"' | head -1 | cut -d'"' -f4 || true)"
    if [ -z "$tag" ]; then
        echo "Warning: No releases found. Using v0.1.0..."
        tag="v0.1.0"
    else
        echo "Latest release: $tag"
    fi

    local download_url="https://github.com/$REPO/releases/download/$tag/$asset"

    curl -sSL -o "$INSTALL_DIR/gpu-agent" "$download_url" || {
        echo "Error: Failed to download gpu-agent binary."
        echo "You may need to build from source: go build ./cmd/gpu-agent/"
        exit 1
    }

    # Verify integrity before we chmod +x / execute it.
    verify_checksum "$INSTALL_DIR/gpu-agent" "$asset" "$tag"

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
