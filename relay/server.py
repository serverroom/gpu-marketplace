"""Relay manager HTTP service (DMZ).

Terminates provider agent reverse-SSH tunnels: the control plane calls
/register-agent with the agent's public key; the relay assigns collision-free
loopback slots and writes a `restrict`+`permitlisten` authorized_keys entry so the
agent can bind ONLY those slots. Holds no DB access and no secrets beyond its own
scoped API token (verified upstream / by network policy).

Run as the gpu-tunnel-manager service inside the DMZ. NOT for the public internet.
"""

import json
import os
import traceback

from flask import Flask, request, jsonify

from relaymgr import build_authorized_keys_line, SlotAllocator

app = Flask(__name__)

STORE = os.environ.get('RELAY_STORE', '/etc/gpu-relay/agents.json')
AUTHORIZED_KEYS = os.environ.get('RELAY_AUTHORIZED_KEYS',
                                 '/home/gpu-tunnel/.ssh/authorized_keys')


def load_store():
    if os.path.exists(STORE):
        with open(STORE) as f:
            return json.load(f)
    return {}


def save_store(data):
    os.makedirs(os.path.dirname(STORE), exist_ok=True)
    with open(STORE, 'w') as f:
        json.dump(data, f, indent=2)


def rewrite_authorized_keys(store):
    lines = [build_authorized_keys_line(a['pubkey'], a['control'], a['ssh'])
             for a in store.values()]
    os.makedirs(os.path.dirname(AUTHORIZED_KEYS), exist_ok=True)
    with open(AUTHORIZED_KEYS, 'w') as f:
        f.write('\n'.join(lines))
        if lines:
            f.write('\n')
    os.chmod(AUTHORIZED_KEYS, 0o600)


@app.route('/register-agent', methods=['POST'])
def register_agent():
    try:
        data = request.get_json(force=True)
        listing_id = data['listing_id']
        pubkey = data['agent_pubkey']

        store = load_store()
        alloc = SlotAllocator({k: {'control': v['control'], 'ssh': v['ssh']}
                               for k, v in store.items()})
        control, ssh = alloc.allocate(listing_id)
        store[listing_id] = {'pubkey': pubkey, 'control': control, 'ssh': ssh}
        save_store(store)
        rewrite_authorized_keys(store)
        return jsonify(control_slot=control, ssh_slot=ssh), 200
    except Exception as ex:
        return jsonify(error=str(ex), trace=traceback.format_exc()), 500


@app.route('/agent/<listing_id>', methods=['DELETE'])
def unregister_agent(listing_id):
    try:
        store = load_store()
        if listing_id in store:
            del store[listing_id]
            save_store(store)
            rewrite_authorized_keys(store)
        return jsonify(removed=True), 200
    except Exception as ex:
        return jsonify(error=str(ex)), 500


if __name__ == '__main__':
    app.run(host='0.0.0.0', port=int(os.environ.get('RELAY_PORT', '5001')))
