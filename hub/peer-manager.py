#!/usr/bin/env python3
"""
GPU Marketplace Hub — Peer Manager

Flask API for dynamic WireGuard peer registration and management.
Runs on each hub server alongside the WireGuard interface.

Endpoints:
  POST   /register       Register a new provider, return WireGuard config
  POST   /heartbeat      Receive heartbeat from a provider
  GET    /peers           List all registered peers
  DELETE /unregister/<id> Remove a peer

Usage: python3 peer-manager.py
"""

import json
import os
import subprocess
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path

import yaml
from flask import Flask, jsonify, request

app = Flask(__name__)

CONFIG_DIR = "/etc/gpu-agent/hub"
PEERS_FILE = os.path.join(CONFIG_DIR, "peers.json")
WG_INTERFACE = "wg-gpu"


def load_hub_config():
    with open(os.path.join(CONFIG_DIR, "hub.yaml")) as f:
        return yaml.safe_load(f)


def save_hub_config(cfg):
    with open(os.path.join(CONFIG_DIR, "hub.yaml"), "w") as f:
        yaml.dump(cfg, f)


def load_peers():
    if os.path.exists(PEERS_FILE):
        with open(PEERS_FILE) as f:
            return json.load(f)
    return {}


def save_peers(peers):
    with open(PEERS_FILE, "w") as f:
        json.dump(peers, f, indent=2)


def generate_keypair():
    """Generate a WireGuard keypair."""
    private = subprocess.check_output(["wg", "genkey"]).decode().strip()
    public = subprocess.check_output(
        ["wg", "pubkey"], input=private.encode()
    ).decode().strip()
    return private, public


def add_wg_peer(public_key, allowed_ip):
    """Add a peer to the live WireGuard interface."""
    subprocess.run([
        "wg", "set", WG_INTERFACE,
        "peer", public_key,
        "allowed-ips", f"{allowed_ip}/32",
    ], check=True)


def remove_wg_peer(public_key):
    """Remove a peer from the live WireGuard interface."""
    subprocess.run([
        "wg", "set", WG_INTERFACE,
        "peer", public_key, "remove",
    ], check=True)


def next_ip(hub_cfg):
    """Allocate the next available tunnel IP."""
    current = hub_cfg.get("next_ip", "10.99.1.1")
    parts = current.split(".")
    last = int(parts[3])
    third = int(parts[2])

    # Increment
    last += 1
    if last > 254:
        last = 1
        third += 1

    hub_cfg["next_ip"] = f"10.99.{third}.{last}"
    save_hub_config(hub_cfg)
    return current


@app.route("/register", methods=["POST"])
def register():
    """Register a new provider and return WireGuard config."""
    data = request.get_json() or {}

    hub_cfg = load_hub_config()
    peers = load_peers()

    # Generate peer identity
    peer_id = str(uuid.uuid4())[:8]
    private_key, public_key = generate_keypair()
    tunnel_ip = next_ip(hub_cfg)

    # Add to WireGuard
    add_wg_peer(public_key, tunnel_ip)

    # Determine hub's public endpoint
    # The hub's public IP should be configured; fall back to request origin
    hub_public_ip = hub_cfg.get("public_ip", request.host.split(":")[0])
    hub_port = hub_cfg.get("port", 51820)
    hub_public_key = hub_cfg.get("public_key", "")

    # Store peer info
    peers[peer_id] = {
        "peer_id": peer_id,
        "public_key": public_key,
        "tunnel_ip": tunnel_ip,
        "hostname": data.get("hostname", "unknown"),
        "os": data.get("os", "unknown"),
        "arch": data.get("arch", "unknown"),
        "registered_at": datetime.now(timezone.utc).isoformat(),
        "last_seen": datetime.now(timezone.utc).isoformat(),
        "remote_ip": request.remote_addr,
    }
    save_peers(peers)

    return jsonify({
        "peer_id": peer_id,
        "private_key": private_key,
        "address": tunnel_ip,
        "hub_public_key": hub_public_key,
        "endpoint": f"{hub_public_ip}:{hub_port}",
    })


@app.route("/heartbeat", methods=["POST"])
def heartbeat():
    """Receive a heartbeat from a provider."""
    data = request.get_json() or {}
    peer_id = data.get("peer_id")

    if not peer_id:
        return jsonify({"error": "missing peer_id"}), 400

    peers = load_peers()
    if peer_id not in peers:
        return jsonify({"error": "unknown peer"}), 404

    peers[peer_id]["last_seen"] = datetime.now(timezone.utc).isoformat()
    peers[peer_id]["remote_ip"] = request.remote_addr
    save_peers(peers)

    return jsonify({"status": "ok"})


@app.route("/peers", methods=["GET"])
def list_peers():
    """List all registered peers."""
    peers = load_peers()

    # Annotate with online/offline status (offline if no heartbeat in 5 min)
    now = time.time()
    for peer_id, peer in peers.items():
        last_seen = datetime.fromisoformat(peer["last_seen"]).timestamp()
        peer["online"] = (now - last_seen) < 300  # 5 minutes

    return jsonify(peers)


@app.route("/unregister/<peer_id>", methods=["DELETE"])
def unregister(peer_id):
    """Remove a registered peer."""
    peers = load_peers()

    if peer_id not in peers:
        return jsonify({"error": "unknown peer"}), 404

    peer = peers[peer_id]
    remove_wg_peer(peer["public_key"])
    del peers[peer_id]
    save_peers(peers)

    return jsonify({"status": "removed", "peer_id": peer_id})


if __name__ == "__main__":
    os.makedirs(CONFIG_DIR, exist_ok=True)
    # Listen on port 51821 (WG port + 1)
    app.run(host="0.0.0.0", port=51821)
