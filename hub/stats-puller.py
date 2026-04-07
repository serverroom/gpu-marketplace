#!/usr/bin/env python3
"""
GPU Marketplace Hub — Stats Puller

Pulls system stats from all connected providers via their WireGuard tunnel IPs.
Can be run on-demand or via cron to keep the marketplace listing updated.

Usage:
  python3 stats-puller.py              # Pull stats from all peers, print JSON
  python3 stats-puller.py --mongo      # Pull and store in MongoDB
"""

import argparse
import json
import os
import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime, timezone

import requests

PEERS_FILE = "/etc/gpu-agent/hub/peers.json"
AGENT_PORT = 9100
TIMEOUT = 10  # seconds


def load_peers():
    if not os.path.exists(PEERS_FILE):
        print(f"Error: {PEERS_FILE} not found", file=sys.stderr)
        sys.exit(1)
    with open(PEERS_FILE) as f:
        return json.load(f)


def pull_stats(peer_id, peer_info):
    """Pull stats from a single peer via the WireGuard tunnel."""
    tunnel_ip = peer_info.get("tunnel_ip")
    if not tunnel_ip:
        return {"peer_id": peer_id, "error": "no tunnel IP"}

    url = f"http://{tunnel_ip}:{AGENT_PORT}/stats"
    try:
        resp = requests.get(url, timeout=TIMEOUT)
        resp.raise_for_status()
        stats = resp.json()
        stats["peer_id"] = peer_id
        stats["tunnel_ip"] = tunnel_ip
        stats["online"] = True
        stats["pulled_at"] = datetime.now(timezone.utc).isoformat()
        return stats
    except requests.exceptions.ConnectTimeout:
        return {"peer_id": peer_id, "tunnel_ip": tunnel_ip, "online": False, "error": "timeout"}
    except requests.exceptions.ConnectionError:
        return {"peer_id": peer_id, "tunnel_ip": tunnel_ip, "online": False, "error": "unreachable"}
    except Exception as e:
        return {"peer_id": peer_id, "tunnel_ip": tunnel_ip, "online": False, "error": str(e)}


def pull_all(peers):
    """Pull stats from all peers in parallel."""
    results = []
    with ThreadPoolExecutor(max_workers=20) as executor:
        futures = {
            executor.submit(pull_stats, peer_id, info): peer_id
            for peer_id, info in peers.items()
        }
        for future in as_completed(futures):
            results.append(future.result())
    return results


def store_mongodb(results):
    """Store stats in MongoDB for the website listing."""
    try:
        from pymongo import MongoClient
    except ImportError:
        print("Error: pymongo not installed. Run: pip install pymongo", file=sys.stderr)
        sys.exit(1)

    client = MongoClient("mongodb://10.60.3.6/servcast?replicaSet=ServCast&readPreference=nearest")
    db = client.servcast
    collection = db.gpu_marketplace

    for stats in results:
        peer_id = stats.get("peer_id")
        if not peer_id:
            continue

        # Upsert: update if exists, insert if new
        collection.update_one(
            {"peer_id": peer_id},
            {"$set": {
                **stats,
                "updated_at": datetime.now(timezone.utc),
            }},
            upsert=True,
        )

    print(f"Stored {len(results)} peer stats in MongoDB")


def main():
    parser = argparse.ArgumentParser(description="Pull GPU stats from marketplace providers")
    parser.add_argument("--mongo", action="store_true", help="Store results in MongoDB")
    parser.add_argument("--pretty", action="store_true", help="Pretty-print JSON output")
    args = parser.parse_args()

    peers = load_peers()
    print(f"Pulling stats from {len(peers)} peers...", file=sys.stderr)

    start = time.time()
    results = pull_all(peers)
    elapsed = time.time() - start

    online = sum(1 for r in results if r.get("online"))
    print(f"Done in {elapsed:.1f}s: {online}/{len(results)} online", file=sys.stderr)

    if args.mongo:
        store_mongodb(results)

    indent = 2 if args.pretty else None
    print(json.dumps(results, indent=indent, default=str))


if __name__ == "__main__":
    main()
