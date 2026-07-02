"""Relay manager core: slot allocation + restricted authorized_keys lines.

The relay terminates each provider agent's reverse SSH tunnel. Two controls carry
the security: an agent may bind ONLY its assigned loopback slots (enforced by the
authorized_keys ``restrict`` + ``permitlisten`` options -- no shell, no other
ports, no gateway exposure), and slots never collide across listings.
"""

CONTROL_BASE = 41000
SSH_BASE = 42000
SLOT_RANGE = 1000

# Where the renter jump pipe (gpu_route.py) lives on the relay. Baked into the
# forced command so a renter key can do nothing but pipe to its own ssh slot.
ROUTE_BIN = '/opt/gpu-relay/gpu_route.py'


def build_authorized_keys_line(pubkey, control_slot, ssh_slot):
    # `restrict` disables everything (pty, agent/X11 forwarding, ALL port
    # forwarding, user rc). Each `permitlisten` then re-enables exactly ONE
    # reverse-forward bind. Net effect: the agent can bind only its two loopback
    # slots and do nothing else -- no shell, no other listens, no gateway ports.
    opts = ('restrict,'
            'permitlisten="127.0.0.1:%d",'
            'permitlisten="127.0.0.1:%d"' % (control_slot, ssh_slot))
    return opts + ' ' + pubkey.strip()


def valid_ssh_slot(slot):
    # The renter jump pipe may only reach an agent SSH slot (never a control
    # slot or an arbitrary port). gpu_route enforces this too, defence in depth.
    try:
        s = int(slot)
    except (TypeError, ValueError):
        return False
    return SSH_BASE <= s < SSH_BASE + SLOT_RANGE


def build_renter_authorized_keys_line(pubkey, ssh_slot, route_bin=ROUTE_BIN):
    # A renter key is a forwarding-only jump: `restrict` kills pty/forwarding/rc,
    # and the forced `command` replaces whatever the renter asks with a pipe to
    # exactly ONE agent ssh slot. The renter's real SSH to the microVM runs
    # end-to-end *through* that pipe, so the relay never sees the inner session.
    if not valid_ssh_slot(ssh_slot):
        raise ValueError('ssh_slot %r out of range' % (ssh_slot,))
    cmd = 'command="python3 %s %d"' % (route_bin, int(ssh_slot))
    return 'restrict,%s %s' % (cmd, pubkey.strip())


class SlotAllocator:
    """Assigns a collision-free (control, ssh) loopback-port pair per listing."""

    def __init__(self, assigned=None):
        # assigned: {listing_id: {'control': int, 'ssh': int}}
        self.assigned = dict(assigned or {})

    def allocate(self, listing_id):
        if listing_id in self.assigned:
            a = self.assigned[listing_id]
            return a['control'], a['ssh']

        used = {a['control'] - CONTROL_BASE for a in self.assigned.values()}
        offset = 0
        while offset in used:
            offset += 1
        if offset >= SLOT_RANGE:
            raise RuntimeError('relay slot range exhausted')

        control, ssh = CONTROL_BASE + offset, SSH_BASE + offset
        self.assigned[listing_id] = {'control': control, 'ssh': ssh}
        return control, ssh

    def release(self, listing_id):
        return self.assigned.pop(listing_id, None) is not None
