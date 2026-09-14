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

# The agent tunnels over reverse SSH: it needs the OpenSSH client (ssh + ssh-keygen).
install_openssh_client() {
    if command -v ssh &>/dev/null && command -v ssh-keygen &>/dev/null; then
        echo "OpenSSH client already installed."
        return
    fi

    echo "Installing OpenSSH client..."
    if command -v apt-get &>/dev/null; then
        apt-get update -qq
        apt-get install -y -qq openssh-client
    elif command -v dnf &>/dev/null; then
        dnf install -y openssh-clients
    elif command -v yum &>/dev/null; then
        yum install -y openssh-clients
    elif command -v pacman &>/dev/null; then
        pacman -Sy --noconfirm openssh
    elif command -v zypper &>/dev/null; then
        zypper install -y openssh
    else
        echo "Error: Unsupported package manager. Install the OpenSSH client manually."
        exit 1
    fi
    echo "OpenSSH client installed."
}

# Download latest release binary
download_agent() {
    echo "Downloading gpu-agent for linux/$GOARCH..."

    # Resolve the tag once: the binary and the checksums that vouch for it must
    # come from the same release, or the check proves nothing.
    ASSET="gpu-agent-linux-$GOARCH"
    TAG=$(curl -sSL "https://api.github.com/repos/$REPO/releases/latest" | grep '"tag_name"' | head -1 | cut -d'"' -f4)
    if [ -z "$TAG" ]; then
        echo "Warning: No releases found. Downloading from v0.1.0..."
        TAG="v0.1.0"
    else
        echo "Latest release: $TAG"
    fi
    RELEASE_URL="https://github.com/$REPO/releases/download/$TAG"
    DOWNLOAD_URL="$RELEASE_URL/$ASSET"

    # $INSTALL_DIR is on the default PATH but is not guaranteed to exist — a
    # slim container image often ships without /usr/local/bin, and curl then
    # fails the write, not the transfer.
    mkdir -p "$INSTALL_DIR"

    # Download beside the target, then move it into place: --fail keeps an HTTP
    # error page from being saved and chmod +x'd as if it were the binary, and
    # the temp file means a partial download never lands on $INSTALL_DIR.
    TMP_AGENT="$INSTALL_DIR/.gpu-agent.download.$$"
    TMP_SUMS="$INSTALL_DIR/.gpu-agent.checksums.$$"
    download_failed() {
        rm -f "$TMP_AGENT" "$TMP_SUMS"
        exit 1
    }

    curl -fsSL -o "$TMP_AGENT" "$DOWNLOAD_URL" || {
        echo "Error: Failed to download gpu-agent binary."
        echo "  URL: $DOWNLOAD_URL"
        echo
        echo "The agent is a self-contained binary — you do not need to install"
        echo "anything else to run it. Check the releases page for a linux/$GOARCH"
        echo "build: https://github.com/$REPO/releases"
        echo "(From a source checkout, with a Go toolchain: go build ./cmd/gpu-agent/)"
        download_failed
    }

    # Verify before this becomes a root-owned executable on PATH. `curl -f`
    # only proves the server answered 200; it says nothing about a truncated or
    # substituted body, and the next two steps are chmod +x and mv into PATH.
    # The release publishes a sha256 per asset, so consult it.
    echo "Verifying checksum..."
    curl -fsSL -o "$TMP_SUMS" "$RELEASE_URL/checksums.txt" || {
        echo "Error: could not download checksums.txt for $TAG."
        echo "  URL: $RELEASE_URL/checksums.txt"
        echo "Refusing to install a binary that cannot be verified."
        download_failed
    }

    # sha256sum writes "<hash>  <name>" in text mode and "<hash> *<name>" in
    # binary mode; accept either rather than depending on how it was produced.
    EXPECTED=$(awk -v a="$ASSET" '$2 == a || $2 == "*" a {print $1; exit}' "$TMP_SUMS")
    if [ -z "$EXPECTED" ]; then
        echo "Error: checksums.txt for $TAG lists no $ASSET."
        echo "That release publishes no build for this platform."
        download_failed
    fi

    ACTUAL=$(sha256sum "$TMP_AGENT" | awk '{print $1}')
    if [ "$EXPECTED" != "$ACTUAL" ]; then
        echo "Error: the downloaded binary does not match the published checksum."
        echo "  expected: $EXPECTED"
        echo "  got:      $ACTUAL"
        echo "Refusing to install it."
        download_failed
    fi
    echo "Checksum verified."
    rm -f "$TMP_SUMS"

    chmod +x "$TMP_AGENT"
    mv -f "$TMP_AGENT" "$INSTALL_DIR/gpu-agent"
    echo "Installed to $INSTALL_DIR/gpu-agent"
}

# Install as systemd service
install_service() {
    echo
    echo "Installing as system service..."

    # `gpu-agent install` refuses to overwrite an existing unit, so on an
    # upgrade the box would keep running the old one. Replace it instead.
    if [ -f "/etc/systemd/system/gpu-agent.service" ]; then
        echo "Existing service found; replacing it."
        "$INSTALL_DIR/gpu-agent" uninstall >/dev/null 2>&1 || true
    fi

    "$INSTALL_DIR/gpu-agent" install
    "$INSTALL_DIR/gpu-agent" start
    echo "Service installed and started."
    echo
    echo "Check status: gpu-agent status"
    echo "View logs:    journalctl -u gpu-agent -f"
}

# Main
install_openssh_client
download_agent
install_service

echo
echo "================================================"
echo " GPU Marketplace Agent installed successfully!"
echo "================================================"
echo
echo "Next: generate a one-time registration code in your dashboard, then run:"
echo "  gpu-agent register --code <code>"
echo "and restart the service to bring the tunnel up:"
echo "  gpu-agent stop && gpu-agent start"
echo
echo "Whether this machine can host a rental:  sudo gpu-agent check"
echo "Remove the agent and withdraw the listing: sudo gpu-agent remove"
