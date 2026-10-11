#!/usr/bin/env python3
"""Signs a gpu-agent release: writes its release.json (RELEASING.md). Runs on DEVEL61 only, as root -- the only place
the release keys exist -- with python3's standard library and the openssl command. It never prints, copies or
moves key material: openssl reads the private key, and only a signature comes out.

    python3 scripts/release/sign-release.py --version vX.Y.Z --commit <sha> --dir DIR [--key PATH] [--out FILE]

DIR holds the six release binaries, built by scripts/release/build.sh from <sha> and already found byte-identical
with GitHub's (`release-check compare`). Run it from a checkout of exactly <sha> (it refuses otherwise): the
release keys it checks against are the ones that commit compiles into the agent (internal/update/keys/*.pem).

The manifest it signs (internal/update/release.go reads it):
    {"agent": "gpu-agent", "version": "vX.Y.Z", "commit": "<sha>", "released_at": <epoch>,
     "files": {"gpu-agent-linux-amd64": {"sha256": "<hex>", "bytes": <n>}, ...}}
The signature is RSA PKCS#1 v1.5 over SHA-256 of the manifest's exact bytes (`openssl dgst -sha256 -sign`) -- the
backup agent's design (ServCast snapshots/release.py) -- and release.json is
    {"manifest": <base64>, "signature": <base64>, "key": <fingerprint>}

A VERSION IS NEVER SIGNED TWICE OVER DIFFERENT BYTES: the ledger (LEDGER_PATH) refuses it, and signing the same files
again writes back the release.json made the first time.
"""
import argparse
import base64
import hashlib
import json
import os
import re
import subprocess
import sys
import tempfile
import time

AGENT_NAME = 'gpu-agent'
KEY_PATH = '/root/.servcast/gpu-agent-release.pem'
LEDGER_PATH = '/root/.servcast/gpu-agent-releases.json'
ASSETS = ('gpu-agent-linux-amd64', 'gpu-agent-linux-arm64',
          'gpu-agent-windows-amd64.exe', 'gpu-agent-windows-arm64.exe',
          'gpu-agent-darwin-amd64', 'gpu-agent-darwin-arm64')
VERSION_RE = re.compile(r'^v[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}\Z')
COMMIT_RE = re.compile(r'^[0-9a-f]{40}\Z')
REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..'))
KEYS_DIR = os.path.join(REPO, 'internal', 'update', 'keys')


def Die(message):
    sys.stderr.write('error: ' + message + '\n')
    sys.exit(1)


def Openssl(*args, data=None):
    proc = subprocess.run(['openssl'] + list(args), input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    return proc.returncode, proc.stdout


def PublicDer(args):
    code, out = Openssl(*args)
    if code != 0 or not out:
        return None
    return out


def Fingerprint(pem_path):
    """The first 16 hex of SHA-256 over the modulus bytes (internal/update KeyFingerprint)."""
    code, out = Openssl('rsa', '-pubin', '-in', pem_path, '-modulus', '-noout')
    if code != 0:
        return None
    modulus = bytes.fromhex(out.decode('ascii').strip().split('=', 1)[1]).lstrip(b'\0')
    return hashlib.sha256(modulus).hexdigest()[:16]


def PinnedKeys():
    names = sorted(n for n in os.listdir(KEYS_DIR) if n.endswith('.pem'))
    if not names:
        Die('no pinned release key in ' + KEYS_DIR)
    return [os.path.join(KEYS_DIR, n) for n in names]


def LoadLedger():
    try:
        with open(LEDGER_PATH, 'r') as handle:
            return json.load(handle)
    except FileNotFoundError:
        return {}


def SaveLedger(ledger):
    tmp = LEDGER_PATH + '.tmp'
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, 'w') as handle:
        json.dump(ledger, handle, indent=1, sort_keys=True)
    os.replace(tmp, LEDGER_PATH)


def main():
    parser = argparse.ArgumentParser(description='Sign a gpu-agent release (DEVEL61 only).')
    parser.add_argument('--version', required=True)
    parser.add_argument('--commit', required=True, help='the full sha the binaries were built from')
    parser.add_argument('--dir', required=True, help='the six binaries')
    parser.add_argument('--key', default=KEY_PATH, help='private key (default: key 1, ' + KEY_PATH + ')')
    parser.add_argument('--out', help='where to write release.json (default: DIR/release.json)')
    args = parser.parse_args()

    if not VERSION_RE.match(args.version):
        Die(args.version + ' is not a release version (vX.Y.Z)')
    if not COMMIT_RE.match(args.commit):
        Die('--commit must be a full 40-hex sha')
    head = subprocess.run(['git', '-C', REPO, 'rev-parse', 'HEAD'], stdout=subprocess.PIPE).stdout.decode().strip()
    if head != args.commit:
        Die('this checkout is at ' + (head or '?') + ', not ' + args.commit + ': check out the release commit first')
    dirty = subprocess.run(['git', '-C', REPO, 'status', '--porcelain', '--untracked-files=no'],
                           stdout=subprocess.PIPE).stdout.decode().strip()
    if dirty:
        Die('this checkout has local changes; the pinned keys must be the commit\'s own')
    if not os.path.isfile(args.key):
        Die('no release key at ' + args.key)

    # The key must be one the release's agents pin: its public half against each pinned PEM (DER, so formatting
    # does not matter). Only public halves are compared.
    mine = PublicDer(['pkey', '-in', args.key, '-pubout', '-outform', 'DER'])
    if mine is None:
        Die('the release key could not be read')
    pinned = PinnedKeys()
    signer = None
    for pem in pinned:
        if PublicDer(['pkey', '-pubin', '-in', pem, '-outform', 'DER']) == mine:
            signer = pem
    if signer is None:
        Die('the key at ' + args.key + ' is not one of the keys this commit pins (' + ', '.join(pinned) + ')')

    files = {}
    for name in ASSETS:
        path = os.path.join(args.dir, name)
        try:
            with open(path, 'rb') as handle:
                raw = handle.read()
        except OSError as ex:
            Die('missing ' + name + ' in ' + args.dir + ': ' + ex.strerror)
        files[name] = {'sha256': hashlib.sha256(raw).hexdigest(), 'bytes': len(raw)}
    out_path = args.out or os.path.join(args.dir, 'release.json')

    ledger = LoadLedger()
    seen = ledger.get(args.version)
    if seen is not None:
        if seen.get('files') != files or seen.get('commit') != args.commit:
            Die(args.version + ' was signed before over other files or another commit; a version is never signed '
                'twice -- ship the fix as a new version')
        with open(out_path, 'w') as handle:
            handle.write(seen['release'])
        print('re-used the release.json signed for ' + args.version + ' at ' + time.strftime(
            '%Y-%m-%d %H:%M:%SZ', time.gmtime(seen['signed_at'])) + ' -> ' + out_path)
        return

    manifest = json.dumps({'agent': AGENT_NAME, 'version': args.version, 'commit': args.commit,
                           'released_at': int(time.time()), 'files': files},
                          sort_keys=True, separators=(',', ':')).encode('utf-8')
    work = tempfile.mkdtemp(prefix='gpu-agent-sign-')
    try:
        manifest_path = os.path.join(work, 'manifest.json')
        sig_path = os.path.join(work, 'manifest.sig')
        with open(manifest_path, 'wb') as handle:
            handle.write(manifest)
        code, _ = Openssl('dgst', '-sha256', '-sign', args.key, '-out', sig_path, manifest_path)
        if code != 0:
            Die('openssl could not sign')
        # Check it as an agent of this commit will: by the pinned keys, the signer's own among them.
        verified = [pem for pem in pinned
                    if Openssl('dgst', '-sha256', '-verify', pem, '-signature', sig_path, manifest_path)[0] == 0]
        if verified != [signer]:
            Die('the signature does not check out with the pinned key it was made with')
        with open(sig_path, 'rb') as handle:
            signature = handle.read()
    finally:
        for name in os.listdir(work):
            os.remove(os.path.join(work, name))
        os.rmdir(work)

    release = json.dumps({'manifest': base64.b64encode(manifest).decode('ascii'),
                          'signature': base64.b64encode(signature).decode('ascii'),
                          'key': Fingerprint(signer)}, sort_keys=True, separators=(',', ':'))
    ledger[args.version] = {'commit': args.commit, 'files': files, 'release': release,
                            'key': Fingerprint(signer), 'signed_at': int(time.time())}
    SaveLedger(ledger)
    with open(out_path, 'w') as handle:
        handle.write(release)
    print('signed ' + args.version + ' (' + args.commit[:12] + ') with pinned key ' + os.path.basename(signer) +
          ' (' + Fingerprint(signer) + ') -> ' + out_path)
    for name in ASSETS:
        print('  ' + files[name]['sha256'] + '  ' + name)


if __name__ == '__main__':
    main()
