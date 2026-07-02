"""End-to-end proof of the renter jump pipe (D3 spike).

gpu_route.main() splices its stdin/stdout to 127.0.0.1:<ssh_slot>. These tests
stand in a real loopback TCP server for the agent's reverse-tunnel slot (i.e.
the microVM's sshd as seen on the relay) and a socketpair for the renter's SSH
channel, then prove bytes flow both ways through the pipe -- and that an
out-of-range slot is refused before any connection is made.
"""

import os
import socket
import threading

import gpu_route


def _echo_server():
    # Stand-in for the microVM sshd reachable at the loopback ssh slot: echoes
    # each line back with an "up:" prefix so we can prove both directions.
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(('127.0.0.1', 0))
    srv.listen(1)
    port = srv.getsockname()[1]

    def serve():
        conn, _ = srv.accept()
        with conn:
            while True:
                data = conn.recv(65536)
                if not data:
                    break
                conn.sendall(b'up:' + data)
        srv.close()

    threading.Thread(target=serve, daemon=True).start()
    return port


def test_pipe_splices_both_directions(monkeypatch):
    port = _echo_server()
    # Point the slot validator at our ephemeral port (any port is "valid" here).
    monkeypatch.setattr(gpu_route, 'valid_ssh_slot', lambda s: True)

    renter, agent_side = socket.socketpair()
    rc_box = {}

    def run():
        rc_box['rc'] = gpu_route.main(
            ['gpu_route.py', str(port)], agent_side.fileno(), agent_side.fileno())

    t = threading.Thread(target=run, daemon=True)
    t.start()

    renter.sendall(b'hello\n')
    got = renter.recv(65536)
    assert got == b'up:hello\n'      # renter -> upstream -> renter, proven

    renter.close()
    t.join(timeout=5)
    assert rc_box.get('rc') == 0
    agent_side.close()


def test_out_of_range_slot_refused_without_connecting():
    # gpu_route must reject a bad slot (e.g. a control slot) with no dial.
    r, w = socket.socketpair()
    rc = gpu_route.main(['gpu_route.py', '41000'], r.fileno(), w.fileno())
    assert rc == 2
    r.close()
    w.close()


def test_unreachable_slot_returns_error(monkeypatch):
    monkeypatch.setattr(gpu_route, 'valid_ssh_slot', lambda s: True)
    # Nothing listening on this loopback port -> connect fails cleanly.
    free = socket.socket()
    free.bind(('127.0.0.1', 0))
    port = free.getsockname()[1]
    free.close()

    r, w = socket.socketpair()
    rc = gpu_route.main(['gpu_route.py', str(port)], r.fileno(), w.fileno())
    assert rc == 3
    r.close()
    w.close()
