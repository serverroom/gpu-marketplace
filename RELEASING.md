# Releasing the agent (signed releases, from v0.3.13)

From v0.3.13 an agent installs an update only when the release carries a `release.json`
signed by one of the release keys compiled into it (`internal/update/release.go`,
`internal/update/keys/`). `checksums.txt` vouches for nothing on its own: it sits in the
same GitHub release as the binaries. The release keys never touch GitHub, CI or this
repository, so whoever controls the GitHub repo or its releases can no longer push code to
the machines that update themselves.

The design is the backup agent's (ServCast `snapshots/release.py`): RSA-3072, PKCS#1 v1.5
over SHA-256 of the manifest's exact bytes (`openssl dgst -sha256 -sign`), one
`release.json` = `{"manifest", "signature", "key"}`.

## The keys

| | private half (DEVEL61, root, 0600) | public half (pinned) | fingerprint |
|---|---|---|---|
| key 1, in use | `/root/.servcast/gpu-agent-release.pem` | `internal/update/keys/release-1.pem` | `61facfb726465177` |
| key 2, standby | `/root/.servcast/gpu-agent-release-standby.pem` | `internal/update/keys/release-2.pem` | `c6d99a91c4fa4ac1` |

Made on DEVEL61 (10.61.2.2) on 2026-10-11 with `openssl genpkey` under `umask 077`; the
`.pub` beside each is its public half. Never print, copy or commit a `.pem`. The fingerprint
is the first 16 hex digits of SHA-256 over the modulus bytes. The ledger of everything
signed is `/root/.servcast/gpu-agent-releases.json`: a version is never signed twice over
different bytes (a fix ships as a new version).

ServCast's `marketplace/agentrelease.py` pins the same two public keys (`RELEASE_KEYS`) and
offers a release to the agents only when it is signed. Change the keys in both places
together.

**Rotation.** To retire key 1: ship a release signed with key 1 that pins key 2 and a new
key 3 (generate key 3 on DEVEL61 exactly as above). Once the fleet runs that release, sign
with key 2 (`--key /root/.servcast/gpu-agent-release-standby.pem`). Dropping a key any other
way strands every agent that pins only dropped keys until its host reinstalls it.

**If a key leaks:** sign a release at once with the other key that pins only the other key
plus a new one, and push it to every machine (staff push). Until a machine runs it, the
leaked key still works on it.

## Publishing a release (the runbook)

You need: this PC (Go, `gh` logged in with write access to `serverroom/gpu-marketplace`,
SSH to `root@10.61.2.2`). DEVEL61 needs only `python3`, `openssl` and `git`. Replace
`v0.3.13` throughout. Every command must exit 0 before the next; stop at the first that
does not.

1. **Pre-flight (the DGX Spark rule).** An update restarts the agent. Never let one reach a
   DGX Spark still on a microVM-mode agent (<= v0.2.3): the restart can trigger the GPU
   retest that resets the GB10. Check no listing is on <= v0.2.3:

   ```sh
   mongosh "mongodb://10.60.3.6/marketplace?replicaSet=ServCast&readPreference=nearest" --quiet --eval \
     'db.listings.aggregate([{$match:{withdrawn_at:{$exists:false}}},{$group:{_id:"$capability.agent_version",n:{$sum:1}}},{$sort:{_id:1}}]).forEach(printjson)'
   ```

   (v0.1.x agents cannot update themselves at all; they are not at risk.)

2. **Tag** the reviewed commit on `main` and push the tag. CI builds the six binaries with
   `scripts/release/build.sh` and publishes them as a **pre-release**: never the latest, so
   nothing offers it yet.

   ```sh
   SHA=$(git rev-parse main)            # the commit you reviewed; write it down
   git tag -a v0.3.13 -m "v0.3.13" $SHA && git push origin v0.3.13
   gh run watch -R serverroom/gpu-marketplace $(gh run list -R serverroom/gpu-marketplace -w Release -L 1 --json databaseId -q '.[0].databaseId')
   ```

3. **Build it ourselves and compare** with what GitHub published, from a clean checkout of
   the same commit (the build is reproducible: same toolchain, `-trimpath`, no VCS stamp,
   empty build id):

   ```sh
   git worktree add --detach ../gpu-rel-v0.3.13 $SHA && cd ../gpu-rel-v0.3.13
   sh scripts/release/build.sh v0.3.13 ../gpu-rel-v0.3.13-dist
   go run ./cmd/release-check compare -version v0.3.13 -local ../gpu-rel-v0.3.13-dist
   ```

   It must print `all good`. If GitHub's bytes differ from ours, do not sign GitHub's: find
   out why (toolchain? a changed workflow?) before going on. Signing our own build and
   replacing GitHub's assets with it (`gh release upload v0.3.13 <files> --clobber` plus a
   regenerated `checksums.txt`) is the fallback, never the habit.

4. **Sign on DEVEL61.** Copy our build over, check out exactly `$SHA` in the signer's own
   checkout (git checks the content against the sha, so a tampered GitHub cannot slip a
   different script in), and sign:

   ```sh
   ssh root@10.61.2.2 'mkdir -p /root/.servcast/gpu-agent-release-work/v0.3.13'
   scp ../gpu-rel-v0.3.13-dist/gpu-agent-* root@10.61.2.2:/root/.servcast/gpu-agent-release-work/v0.3.13/
   ssh root@10.61.2.2 "set -e
     [ -d /root/.servcast/gpu-agent-release-src ] || git clone -q https://github.com/serverroom/gpu-marketplace.git /root/.servcast/gpu-agent-release-src
     cd /root/.servcast/gpu-agent-release-src && git fetch -q --tags origin && git checkout -q --detach $SHA
     python3 scripts/release/sign-release.py --version v0.3.13 --commit $SHA --dir /root/.servcast/gpu-agent-release-work/v0.3.13"
   scp root@10.61.2.2:/root/.servcast/gpu-agent-release-work/v0.3.13/release.json ../gpu-rel-v0.3.13-dist/
   ```

   The script refuses a checkout that is not `$SHA` or has local changes, a key the commit
   does not pin, a missing binary, and a version already signed over other bytes; it checks
   its own signature with `openssl dgst -verify` before writing anything.

5. **Check before anyone sees it**, with the agent's own code and the keys this commit pins:

   ```sh
   go run ./cmd/release-check check -version v0.3.13 -release ../gpu-rel-v0.3.13-dist/release.json -local ../gpu-rel-v0.3.13-dist -offline
   ```

6. **Upload, check what GitHub serves, canary.**

   ```sh
   gh release upload v0.3.13 ../gpu-rel-v0.3.13-dist/release.json -R serverroom/gpu-marketplace
   go run ./cmd/release-check check -version v0.3.13 -local ../gpu-rel-v0.3.13-dist
   ```

   Then update our test box SID 2457 by name while the release is still a pre-release:
   `sudo gpu-agent update --version v0.3.13`, and after three minutes
   `sudo gpu-agent status` must show the update `ok`.

7. **Promote.** Only now does it become the release the marketplace offers every agent
   (ServCast re-reads GitHub's latest within the hour and offers it only if it is signed):

   ```sh
   gh release edit v0.3.13 -R serverroom/gpu-marketplace --prerelease=false --latest
   ```

8. **Watch** the fleet move (`capability.agent_version` in `marketplace.listings`, the
   `update` record in each agent's status, `agent_errors`). A machine updates only when
   idle; a rented one waits.

A release that was tagged but is not to ship is left a pre-release (or deleted); without a
signature no v0.3.13+ agent will install it, whoever pushes it.

## The bootstrap: v0.3.13 itself

Agents up to v0.3.12 check only `checksums.txt`. They take v0.3.13 the old way, so that one
step trusts GitHub as before; it cannot be avoided without reinstalling every machine. What
narrows it:

- ServCast's offer is gated on the signature (`agentrelease.CheckSignedRelease`): it offers
  an old agent only a release whose `release.json` verifies AND whose `checksums.txt`
  agrees with the signed digests for the Linux binaries. Deploy that ServCast change
  before promoting v0.3.13.
- `release-check check` (step 6) confirms GitHub's binaries are the signed ones right
  before promotion.

What it does not cover: a host running `gpu-agent update` by hand on a v0.3.12-or-older agent
(it asks GitHub's latest directly), and the installers (`scripts/install.sh`,
`install.ps1`, `install-mac.sh`), which are served from GitHub and check only
`checksums.txt`. Once a machine runs v0.3.13, every later update is signature-checked.
