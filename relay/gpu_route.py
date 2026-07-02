#!/usr/bin/env python3
"""Renter jump pipe -- the relay's ForceCommand target for renter SSH keys.

A renter key's authorized_keys entry forces `python3 gpu_route.py <ssh_slot>`
(see relaymgr.build_renter_authorized_keys_line). When the renter uses the relay
as a jump host, sshd runs this with the renter's SSH channel wired to our
stdin/stdout. We splice that channel to 127.0.0.1:<ssh_slot> -- the loopback end
of the agent's reverse tunnel, which lands on the rented microVM's sshd.

The renter's actual SSH session (auth, encryption) runs end-to-end *inside* this
pipe: the relay only moves bytes and can neither read nor MITM it. The slot is
baked into authorized_keys by the relay (never taken from the renter), and is
range-checked here as defence in depth so a tampered entry still can't reach a
control slot or an arbitrary port.
"""

import os
import socket
import sys
import threading

# Kept in sync with relaymgr; imported when this runs from the relay dir, with a
# literal fallback so the pipe has no hard import dependency at connection time.
try:
    from relaymgr import valid_ssh_slot
except Exception:
    SSH_BASE, SLOT_RANGE = 42000, 1000

    def valid_ssh_slot(slot):
        try:
            s = int(slot)
        except (TypeError, ValueError):
            return False
        return SSH_BASE <= s < SSH_BASE + SLOT_RANGE


def _pump(read_fd, write_fd, on_eof=None, done=None):
    # Raw fd copy so it works whether sshd hands us pipe fds (forced command) or
    # socket fds (tests) -- os.read/os.write treat both alike on POSIX.
    try:
        while True:
            chunk = os.read(read_fd, 65536)
            if not chunk:
                break
            os.write(write_fd, chunk)
    except OSError:
        pass
    finally:
        if on_eof is not None:
            try:
                on_eof()
            except OSError:
                pass
        if done is not None:
            done.set()


def main(argv, stdin_fd, stdout_fd):
    if len(argv) != 2 or not valid_ssh_slot(argv[1]):
        sys.stderr.write('gpu-route: invalid or missing ssh slot\n')
        return 2

    slot = int(argv[1])
    try:
        upstream = socket.create_connection(('127.0.0.1', slot), timeout=10)
    except OSError as ex:
        sys.stderr.write('gpu-route: upstream 127.0.0.1:%d unreachable: %s\n' % (slot, ex))
        return 3

    # create_connection's timeout leaves the socket non-blocking; the pump loops
    # use blocking os.read, so restore blocking mode or they'd see EAGAIN (which
    # os.read surfaces as OSError) and mistake it for EOF.
    upstream.settimeout(None)
    up_fd = upstream.fileno()
    done = threading.Event()

    # renter -> microVM. On renter EOF, half-close the upstream write side so the
    # microVM's sshd sees end-of-input -- but do NOT end the session: the reply
    # (the microVM's SSH banner and the rest) still has to flow back.
    def half_close_upstream():
        try:
            upstream.shutdown(socket.SHUT_WR)
        except OSError:
            pass

    threading.Thread(
        target=_pump, args=(stdin_fd, up_fd),
        kwargs={'on_eof': half_close_upstream}, daemon=True,
    ).start()
    # microVM -> renter. When THIS direction ends, the microVM closed the
    # connection, i.e. the session is genuinely over.
    threading.Thread(
        target=_pump, args=(up_fd, stdout_fd),
        kwargs={'done': done}, daemon=True,
    ).start()

    done.wait()
    upstream.close()
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv, sys.stdin.fileno(), sys.stdout.fileno()))
