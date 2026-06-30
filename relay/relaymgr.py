"""Relay manager core: slot allocation + restricted authorized_keys lines.

The relay terminates each provider agent's reverse SSH tunnel. Two controls carry
the security: an agent may bind ONLY its assigned loopback slots (enforced by the
authorized_keys ``restrict`` + ``permitlisten`` options -- no shell, no other
ports, no gateway exposure), and slots never collide across listings.
"""

CONTROL_BASE = 41000
SSH_BASE = 42000
SLOT_RANGE = 1000


def build_authorized_keys_line(pubkey, control_slot, ssh_slot):
    # `restrict` disables everything (pty, agent/X11 forwarding, ALL port
    # forwarding, user rc). Each `permitlisten` then re-enables exactly ONE
    # reverse-forward bind. Net effect: the agent can bind only its two loopback
    # slots and do nothing else -- no shell, no other listens, no gateway ports.
    opts = ('restrict,'
            'permitlisten="127.0.0.1:%d",'
            'permitlisten="127.0.0.1:%d"' % (control_slot, ssh_slot))
    return opts + ' ' + pubkey.strip()


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
