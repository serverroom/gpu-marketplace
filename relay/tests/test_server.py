"""Relay HTTP contract for renter routing (D3 spike).

Drives /authorize-renter and /renter/<id> against a real Flask test client with
the store + authorized_keys files redirected to a tmp dir, proving the renter
line the relay writes is pinned to the agent's own ssh slot and that revocation
(explicit, and cascaded from agent removal) actually clears it.
"""

import pytest

server = pytest.importorskip("server")


@pytest.fixture
def client(tmp_path, monkeypatch):
    monkeypatch.setattr(server, 'STORE', str(tmp_path / 'agents.json'))
    monkeypatch.setattr(server, 'AUTHORIZED_KEYS', str(tmp_path / 'agent_keys'))
    monkeypatch.setattr(server, 'RENTER_STORE', str(tmp_path / 'renters.json'))
    monkeypatch.setattr(server, 'RENTER_AUTHORIZED_KEYS', str(tmp_path / 'renter_keys'))
    server.app.config['TESTING'] = True
    return server.app.test_client(), tmp_path


def _register_agent(c, listing_id='L1', pubkey='ssh-ed25519 AGENT'):
    return c.post('/register-agent', json={'listing_id': listing_id,
                                           'agent_pubkey': pubkey})


def test_authorize_renter_pins_to_agent_ssh_slot(client):
    c, tmp = client
    r = _register_agent(c)
    ssh_slot = r.get_json()['ssh_slot']

    resp = c.post('/authorize-renter',
                  json={'listing_id': 'L1', 'renter_pubkey': 'ssh-ed25519 RENTER'})
    assert resp.status_code == 200
    assert resp.get_json()['ssh_slot'] == ssh_slot

    keys = (tmp / 'renter_keys').read_text()
    assert 'ssh-ed25519 RENTER' in keys
    assert 'command="python3 /opt/gpu-relay/gpu_route.py %d"' % ssh_slot in keys
    assert 'restrict,' in keys


def test_authorize_renter_without_agent_is_409(client):
    c, _ = client
    resp = c.post('/authorize-renter',
                  json={'listing_id': 'GHOST', 'renter_pubkey': 'ssh-ed25519 R'})
    assert resp.status_code == 409


def test_revoke_renter_clears_key(client):
    c, tmp = client
    _register_agent(c)
    c.post('/authorize-renter',
           json={'listing_id': 'L1', 'renter_pubkey': 'ssh-ed25519 RENTER'})
    assert 'ssh-ed25519 RENTER' in (tmp / 'renter_keys').read_text()

    resp = c.delete('/renter/L1')
    assert resp.status_code == 200
    assert 'ssh-ed25519 RENTER' not in (tmp / 'renter_keys').read_text()


def test_removing_agent_cascades_to_renter(client):
    c, tmp = client
    _register_agent(c)
    c.post('/authorize-renter',
           json={'listing_id': 'L1', 'renter_pubkey': 'ssh-ed25519 RENTER'})
    assert 'ssh-ed25519 RENTER' in (tmp / 'renter_keys').read_text()

    # Tearing down the agent tunnel must not leave a renter routed to a dead slot.
    resp = c.delete('/agent/L1')
    assert resp.status_code == 200
    assert 'ssh-ed25519 RENTER' not in (tmp / 'renter_keys').read_text()
