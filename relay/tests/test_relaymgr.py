from relaymgr import build_authorized_keys_line, SlotAllocator


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
