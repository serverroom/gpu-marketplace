#!/usr/bin/env bash
set -euo pipefail

# WireGuard Hub Server Setup
# Run once on each hub server to configure the WireGuard endpoint.
# Usage: sudo bash wg-hub-setup.sh [hub-name]

HUB_NAME="${1:-}"
WG_INTERFACE="wg-gpu"
WG_PORT=51820
WG_SUBNET="10.99.0.1/16"
CONFIG_DIR="/etc/gpu-agent/hub"

if [ "$(id -u)" -ne 0 ]; then
    echo "Error: Run as root."
    exit 1
fi

if [ -z "$HUB_NAME" ]; then
    echo "Usage: $0 <hub-name>"
    echo "Example: $0 US-60"
    exit 1
fi

echo "================================"
echo " WireGuard Hub Setup: $HUB_NAME"
echo "================================"
echo

# Install WireGuard
echo "1. Installing WireGuard..."
if command -v apt-get &>/dev/null; then
    apt-get update -qq && apt-get install -y -qq wireguard wireguard-tools
elif command -v dnf &>/dev/null; then
    dnf install -y wireguard-tools
elif command -v yum &>/dev/null; then
    yum install -y epel-release wireguard-tools
fi

# Generate hub keypair
echo "2. Generating WireGuard keypair..."
mkdir -p "$CONFIG_DIR"
if [ ! -f "$CONFIG_DIR/privatekey" ]; then
    wg genkey | tee "$CONFIG_DIR/privatekey" | wg pubkey > "$CONFIG_DIR/publickey"
    chmod 600 "$CONFIG_DIR/privatekey"
    echo "   Keypair generated."
else
    echo "   Keypair already exists."
fi

PRIVATE_KEY=$(cat "$CONFIG_DIR/privatekey")
PUBLIC_KEY=$(cat "$CONFIG_DIR/publickey")

# Write WireGuard config
echo "3. Writing WireGuard config..."
cat > "/etc/wireguard/$WG_INTERFACE.conf" <<EOF
[Interface]
PrivateKey = $PRIVATE_KEY
Address = $WG_SUBNET
ListenPort = $WG_PORT
SaveConfig = true

# Peers are added dynamically by peer-manager.py
EOF
chmod 600 "/etc/wireguard/$WG_INTERFACE.conf"

# Enable IP forwarding
echo "4. Enabling IP forwarding..."
sysctl -w net.ipv4.ip_forward=1
if ! grep -q "net.ipv4.ip_forward=1" /etc/sysctl.conf; then
    echo "net.ipv4.ip_forward=1" >> /etc/sysctl.conf
fi

# Open firewall port
echo "5. Configuring firewall..."
if command -v ufw &>/dev/null; then
    ufw allow $WG_PORT/udp
elif command -v firewall-cmd &>/dev/null; then
    firewall-cmd --permanent --add-port=$WG_PORT/udp
    firewall-cmd --reload
fi

# Start WireGuard
echo "6. Starting WireGuard interface..."
wg-quick up "$WG_INTERFACE" 2>/dev/null || true
systemctl enable "wg-quick@$WG_INTERFACE" 2>/dev/null || true

# Save hub info
cat > "$CONFIG_DIR/hub.yaml" <<EOF
name: $HUB_NAME
public_key: $PUBLIC_KEY
port: $WG_PORT
subnet: $WG_SUBNET
next_ip: 10.99.1.1
EOF

echo
echo "================================"
echo " Hub setup complete!"
echo "================================"
echo
echo "Hub name:    $HUB_NAME"
echo "Public key:  $PUBLIC_KEY"
echo "Listen port: $WG_PORT"
echo "Subnet:      $WG_SUBNET"
echo
echo "Next steps:"
echo "  1. Install Python deps: pip install flask pyyaml"
echo "  2. Start peer manager:  python3 peer-manager.py"
echo "  3. Ensure UDP port $WG_PORT is open on the public firewall"
