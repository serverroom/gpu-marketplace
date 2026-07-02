import pytest

from relaymgr import (build_authorized_keys_line,
                      build_renter_authorized_keys_line, valid_ssh_slot,
                      SlotAllocator, SSH_BASE, SLOT_RANGE)


def test_authorized_keys_line_restricts_to_assigned_slots():
    line = build_authorized_keys_line('ssh-ed25519 AAAA', 41000, 42000)
    assert line.startswith('restrict,')
    assert 'permitlisten="127.0.0.1:41000"' in line
    assert 'permitlisten="127.0.0.1:42000"' in line
    assert line.endswith('ssh-ed25519 AAAA')
    # restrict already disables pty/forwarding; no opener for a shell or extra ports
    assert 'permitopen' not in line
    assert 'pty' not in line


def test_allocate_is_idempotent_per_listing():
    a = SlotAllocator()
    assert a.allocate('L1') == a.allocate('L1')


def test_allocate_no_collision_across_listings():
    a = SlotAllocator()
    c1, s1 = a.allocate('L1')
    c2, s2 = a.allocate('L2')
    assert c1 != c2
    assert s1 != s2


def test_release_frees_slot_for_reuse():
    a = SlotAllocator()
    first = a.allocate('L1')
    assert a.release('L1') is True
    assert a.allocate('L2') == first  # the freed offset is reused


def test_valid_ssh_slot_range():
    assert valid_ssh_slot(SSH_BASE) is True
    assert valid_ssh_slot(SSH_BASE + SLOT_RANGE - 1) is True
    assert valid_ssh_slot(SSH_BASE - 1) is False       # control-slot range
    assert valid_ssh_slot(SSH_BASE + SLOT_RANGE) is False
    assert valid_ssh_slot('nan') is False
    assert valid_ssh_slot(None) is False


def test_renter_line_forces_pipe_to_its_slot_only():
    line = build_renter_authorized_keys_line('ssh-ed25519 RRRR', 42007)
    assert line.startswith('restrict,')
    assert 'command="python3 /opt/gpu-relay/gpu_route.py 42007"' in line
    assert line.endswith('ssh-ed25519 RRRR')
    # A renter key must not be able to open forwards or a shell.
    assert 'permitlisten' not in line
    assert 'permitopen' not in line


def test_renter_line_rejects_non_ssh_slot():
    # A control slot (or junk) must never become a renter pipe target.
    with pytest.raises(ValueError):
        build_renter_authorized_keys_line('ssh-ed25519 RRRR', 41000)
    with pytest.raises(ValueError):
        build_renter_authorized_keys_line('ssh-ed25519 RRRR', 'shell')
