# gpu-agent v0.2.0: where the build decided differently from its briefs

v0.2.0 is one release for every machine: a single GPU, no GPU, a single DGX Spark, two
linked DGX Sparks. It was built in two tracks that were merged before release: the
automatic setup / Spark desktop / host controls track (built as "release-v0.1.10",
never released on its own) and the linked-pairs / GPU-less track. The first part below
is the first track's decisions, with the reason; the second part is the second track's,
and the last part is what the merge itself decided.

Everything in this first part is a decision taken while building that first track.
The brief was: automatic setup after linking, DGX Spark desktop on demand; plus two
additions during the work: host controls (section A of CONTRACT-hostcontrols.md, then
made asynchronous) and the transient `nvidia-smi` holder fix.

## Automatic setup

1. **`gpu-agent register` restarts the agent service.** The agent reads its tunnel and
   control token only when it starts, and `register` never restarted it, so after
   "install + register" (all the panel shows) the daemon kept running unregistered: no
   tunnel, no capability report, no setup. The panel's own copy already says register
   "starts the agent itself (restarting it if it was already running)". When the service
   is not installed, register says how to install and start it.
2. **The attempt key also includes the architecture and the GPU set**, not only agent
   version + host driver + base release. Without the GPUs, a machine whose GPU was
   swapped after a passing setup would need a manual `check --boot` (the test boot is
   bound to its GPUs), which is what the brief is removing.
3. **Retry policy.** A failed attempt that *finished* is retried at the next agent start
   (as the failure line promises) or 6 h after it finished. An attempt that *never
   finished* (agent killed, power lost, a crash loop) is retried 6 h after it started,
   not at the next start: otherwise an agent that keeps dying would keep restarting a
   half-hour build. The host is told when ("was interrupted ... tries again at ...").
   A step that panics is recovered and recorded as a failure, so it cannot crash-loop the
   agent. A passed key never runs again automatically. In-process retries repeat every
   6 h for as long as they fail (see risks in the report: on a Spark each retry closes the
   desktop for the test).
4. **"shows the details" names the right command.** The failure line ends
   `'sudo gpu-agent check --boot' shows the details` for a failed test boot, as the brief
   says, but `'sudo gpu-agent setup' shows the details` for a failed package install or
   image build: `check --boot` would only answer "the base image has not been built".
5. **Only the steps a machine needs run**; the test boot always comes last, because
   preflight hides the test-boot reason while other reasons exist.
6. **A driver mismatch is a preflight reason (fixable, kind image) only when the image's
   driver was not chosen by a person** (`--driver`, recorded as `driver_source: "flag"`)
   and the host's driver can be read. Images from older agents carry no driver_source and
   count as unchosen, so a v0.1.9 image with `580-server-open` on a proprietary host is
   rebuilt. `runtime prepare` without `--driver` now matches the host too (it used
   DefaultDriver); `--driver` still wins.
7. **Never concurrent with itself, across processes too.** `busy.json` (pid + boot id)
   is taken by the automatic setup, `gpu-agent setup`, `runtime prepare` and
   `check --boot`; a stale record (dead pid, or from before a reboot) is ignored.
8. **An agent restarted mid-setup tears its own bake/test VM down at start.** Before, a
   running bake VM found by `Resume` was taken for a rental (status rented, SSH forward
   opened). A setup VM whose process is still alive (a person's `check --boot`) is left to
   it.
9. **`gpu-agent setup` does not prompt** (it is the support command, and runs exactly
   what the agent would run by itself); it prints the plan first, ignores the opt-out and
   the retry policy (a person asked for it), and restarts the service on success so the
   daemon reports ready.
10. **Progress durations**: "a few minutes" (packages), "20-30 minutes" (image, as the
    brief), "5-15 minutes" (test rental). On a DGX Spark the test-boot line adds "This
    machine's desktop closes during the test and comes back after it." Error text in the
    panel line is whitespace-collapsed and capped at 300 characters (start and end kept);
    the full error is in `autosetup.json` / `setup --status`.
11. **Agent stop waits up to 75 s** for background work (the setup tearing its VM down);
    systemd kills at 90 s.

## DGX Spark desktop

12. **A Spark's desktop is still a (human) preflight reason when no running display
    manager draws it** (someone started X by hand): the agent could not close it, and a
    rental would fail at start.
13. **The display manager is only stopped when it is active**, and started again only once
    the GPU is back off vfio-pci; if the GPU does not come back, the record stays in the
    dirty state and the teardown that frees the GPU starts the desktop.
14. **`Start` now saves the rental state even when taking the GPUs fails** (a v0.1.9 bug:
    a bind that failed halfway left the progress only in memory, so the teardown could not
    give back functions already on vfio-pci or restart services).
15. **The capability's `identity` carries exactly the four requested fields**
    (sys_vendor, product_name, product_family, confirmed_dgx_spark). The linked-pairs
    branch also has board_name and reason; merging it later only adds fields.

16. **What counts as the desktop is decided by login session, not only by name**
    (review fix): a GPU holder is the desktop when its cgroup is a graphical login's
    `session-<N>.scope` (logind Type x11/wayland/mir, or Class greeter), an app unit under
    `user@<uid>.service` of a user with such a login, or a descendant of the display
    manager's main PID. So a Spark with browsers open is rented without a step. Order of
    classification: desktop by name, then short-lived tools (an `nvidia-smi` in a desktop
    terminal is still waited for on a workstation whose desktop is on another GPU), then
    session, then everything else. On a Spark with its desktop up, the short-lived wait is
    skipped: stopping the display manager takes those with it, and the wait afterwards
    (30 s, a look every second) is for every holder. A holder outside any login is refused
    before the desktop is touched; one still there after the desktop closed is named, the
    display manager started again, and the start refused with "the GPU is still in use on
    this machine by ... after its desktop was closed; the desktop is back" rather than the
    `--headless` hint (a Spark host is not asked to do anything). Non-Spark hosts keep the
    `--headless` refusal, now naming the desktop's apps too.
17. **README tells hosts to save open work on a listed Spark** (its desktop closes during
    test boots and rentals); the panel's test-boot line already says the desktop closes.

## Transient GPU holders (added during the build)

18. **"Children of the gpu-agent process" is read as children of any gpu-agent process**
    (parent is this process, or a process whose executable is gpu-agent), so a
    `gpu-agent status` a person runs during a start does not fail it.
19. **The stats pause waits at most 10 s** for a running query: a hung nvidia-smi must not
    hang a rental start. While paused, stats answer with the last GPUs read.

## Host controls, section A (added during the build)

20. **`/update` is asynchronous** (the coordinator's change): statuses
    downloading -> verifying -> restarting -> ok | failed | rolled_back; the record stays
    "restarting" until the check 3 minutes later. A bad version string is 400 (the
    contract names no code); an update in progress blocks another for up to 10 minutes.
21. **The rollback check is the previous binary**: `gpu-agent.prev update-check --to vX
    --binary <path>` from a transient timer; the new binary may be the broken one. Unit
    names carry a timestamp and `--collect`, so a leftover unit never blocks the next
    update. Besides "service not active" and "binary reports another version", a service
    still running the replaced binary (`/proc/<MainPID>/exe` ends in " (deleted)") counts
    as not restarted and is rolled back.
22. **The `-version` check requires the binary's exact output**, `gpu-agent vX.Y.Z`.
23. **Withdrawn also closes the control channel**, and an agent that starts withdrawn
    still runs its crash-resume teardown (so a Spark gets its desktop back) but nothing
    else. `gpu-agent remove` on a withdrawn machine revokes best-effort and reports the
    listing as already removed instead of "NOT withdrawn".

## Small things

24. `Prepare` and `Stop` now name the last serial log the same way (a Windows-only test
    mismatch); `fakehost` gained OnSleep/SetLink/DeleteLink/Count/SleptFor for the tests.
25. **`runtime prepare --headless` starts NVIDIA's services again after closing the
    desktop** (found on SID 2457): `systemctl isolate multi-user.target` also stops
    nvidia-persistenced on Ubuntu (a static unit wanted by the NVIDIA device, not by a
    target). The NVIDIA services running before the isolate are started after it, and any
    that will not start is named with the command to start it. This runs in the command's
    own process, so it is done when the command is run over SSH, as the command already
    asks; a command typed in a terminal on the desktop being closed may die with it. The
    DGX Spark path stops only the display manager (no isolate) and was checked not to
    have this side effect: the NVIDIA services it stops are the ones the rental records
    and restarts.
26. **The update's transient timers set `AccuracySec=1s`** (`--timer-property`): with
    systemd's default accuracy of one minute, the 2 s restart fired 9-19 s late on SID 2457.

## Verified on real hardware (SID 2457: Dell C4130, CMP 170HX, headless; build of 294a71f stamped v0.1.10)

- Installed over v0.1.9: the automatic setup found the v0.1.9 test boot, planned one step
  (the image's `580-server` kept, matching the host's proprietary 580.173.02), ran the test
  rental, passed in about 2 minutes; capability ready.
- `POST /withdrawn`: the control channel closed and `status` showed the removal message.
- A build stamped v0.1.8 updated itself to the real v0.1.9 release: download, checksum,
  swap, restart, and the `.prev update-check` said ok at +3 minutes. With the service
  stopped after the swap, the check rolled back to `.prev`, restarted the service and
  recorded `rolled_back` in update.json.
- Found there and fixed afterwards: nvidia-persistenced left dead by
  `runtime prepare --headless` (item 25), and the late restart timer (item 26).
- Not yet on real hardware: anything DGX Spark (DMI match, desktop closing with its apps,
  GB10 handover), and the session-based desktop classification (325c3c9).

# Linked pairs and machines without a GPU: where the implementation differs from the contracts

Contracts: CONTRACT.md (s3 agent API), CONTRACT-v2.md (s1 CPU-only),
DESIGN.md (reasoning). Everything not listed here is implemented as written.

## Peer discovery (CONTRACT s3.1)

1. **Announcements run on fixed UTC slots, not "every 6 h from start".** A cable carries frames only while
   BOTH ends are up, and the agent keeps its ports down between runs, so two agents announcing 5 s every 6 h
   from their own start times would practically never overlap. Every agent announces at 00:00, 06:00, 12:00
   and 18:00 UTC (clock-synchronised machines hit the slot within a second of each other) and once at start.
2. **A cable check that proves the peer also records it** in peers.json (the strongest sighting there is), so
   a pair the control plane re-checks daily keeps its mutual peers fresh even if a slot is missed.

## /link/verify (CONTRACT s3.2)

3. **Classification details the contract leaves open** (all fail closed or are neutral):
   - Frames from this machine's own ConnectX MACs are ignored (a multi-function NIC can echo one function's
     frames to another); they are neither peer nor foreign.
   - Ordinary (non-agent) frames from a source MAC that also sent authentic GPUAGENT-LINK1 frames for the
     peer are not reported as `foreign_src` (e.g. a last IPv6 neighbour-discovery packet as the peer's IPv6
     goes off). DESIGN s4.3 says "everything else -> foreign source MAC"; this narrows it to sources not
     proven to be the peer. A switch still gives itself away by its own STP/LLDP/other hosts' traffic.
   - The peer's own GPUAGENT-HELLO1 announcements (its slot can overlap a check) are ignored; a HELLO1 from
     any other listing is a third machine and is reported in `foreign_src`.
   - An authentic LINK1 frame naming a listing other than the peer is reported in `peer_frames` with that
     listing, so the control plane's EvaluateLink refuses it as a third machine.
   - The HMAC key is the challenge's 32 hex characters as ASCII bytes.
4. **Only preflight-eligible ports are touched and reported** (no host address/route, not bonded, not the
   management route, not NetworkManager's/netplan's, no RDMA users). A port the host uses is never brought
   up or down by a check. The raw sockets are promiscuous per socket (PACKET_MR_PROMISC membership, dropped
   with the socket), so STP and foreign unicast are seen without changing the interface.
5. **Extra refusals:** 400 when `self` is not this machine's listing id (a misrouted check); 503 when this
   agent cannot run the check at all; 500 when the ports could not be opened or put back.

## /pair/provision (CONTRACT s3.2)

6. **Strict body:** unknown JSON fields are refused with 400 on /pair/provision and /link/verify (the reason
   /pair/provision exists is that /provision silently ignores fields). The control plane must send only the
   contract's fields.
7. **Stricter validation than the contract lists:** `peer_hostname` must be the other half of THIS rental
   (`gpu-<same 8 hex>-<other node>`), the rental id must give a `gpu-<8 lowercase hex>` name (UUIDs do),
   `intra_key.public` must be ssh-ed25519 and `intra_key.private_openssh` an unencrypted OpenSSH ed25519 key
   whose public half matches (it is re-encoded canonically before it reaches the guest).
8. **409 also** for a machine that is not free or has a cable check running (the contract names 409 only for
   an unknown local MAC). 503 also when the pair preflight, re-run at request time, is not clean.

## Pair guest (DESIGN s5.2)

9. **/etc/hosts**: this machine's own name maps to its own first-link address (DESIGN: `127.0.1.1`), and
   per-link names count from 1: `<name>-l1`, `<name>-l2` (DESIGN: `-l<i>` from 0). Requested by the website
   track: the renter's page tells them to `ping -c 3 gpu-<8>-b` on machine a, which must resolve over the
   cable. Interface names stay `cx7p0`, `cx7p1` (from 0, as DESIGN).
10. **Unverified ConnectX ports** get `match` + `set-name` + `link-local: []` + `optional: true` (DESIGN: only
    match + set-name), so they neither get an address nor hold the first boot.
11. **The link check runs as its own unit** (`systemd-run --unit=gpuagent-linkcheck --no-block` from runcmd), so
    the first boot is not held for up to 10 minutes while it waits for the other machine.
12. **The intra-pair key files** are written base64-encoded with cloud-init `defer: true` (after root's SSH
    directory exists).

## Teardown (DESIGN s5.4)

13. `StopResult.NICDirty` (true = the card did not verify) instead of DESIGN's `NICClean`, so every existing
    `StopResult{Wiped: true, GPUClean: true}` keeps meaning clean. `Clean() = Wiped && GPUClean && !NICDirty`.

## Pair test boot (DESIGN s5.6, CONTRACT s3.2 CLI)

14. **The per-test intra key is a throwaway made on each machine**, not one shared key: the two agents have no
    channel to share a secret before their VMs exist. It exercises the key path of the seed; the test VMs do
    not log in to each other (the RDMA test does not need it).
15. **RDMA:** `ib_write_bw -R` (RDMA CM picks the device and GID; no `-d`/`-x`), 5 s per link, node a serves and
    node b connects on port 18515+i, one link at a time. Markers are `RDMA <i> <Gb/s>` and `RDMAFAIL <i> <why>`
    (DESIGN: `RDMA <gbps>` without the link index).
16. **The verdict also requires the rental's own link check** (`GPUAGENT-LINK ok <i>`) on every link.
17. **At most two links are tested** (the first two by node a's MAC, as the control plane's GuestLinks numbers
    a rental's), matching /pair/provision's 1..2 links.
18. **A test that finds no peer is recorded as a failed pair test** (like a failed single test boot), which
    takes interconnect.ready away until the test passes again. A test that cannot start at all (blocked by
    other problems, a rental present, another check running) records nothing.

## Base image (DESIGN s5.3)

19a. **The RDMA step of the bake is best-effort.** Every bake installs the RDMA tools, but a failure there no
    longer fails the bake: the image still serves single rentals (as in v0.1.9), and `Extras: ["rdma"]` is
    recorded only when the step succeeded (the VM prints `GPUAGENT-BAKE EXTRA rdma`). A pair then asks for a
    rebuild (P9). DESIGN s5.3 bakes them unconditionally; a package missing on one architecture would have
    broken every single host's bake.

## CPU-only (CONTRACT-v2 s1)

19. **`gpu_count` is reported on Linux only** (`*int`, omitted on macOS/Windows) and `kind` stays `qemu-vfio`
    there: ServCast treats an explicit `gpu_count: 0` as a machine without a GPU, and a Mac or Windows machine
    cannot host at all. On Linux: `kind` "qemu" and `gpu_count` 0 without a GPU.
20. **An NVIDIA GPU on the PCI bus that nvidia-smi does not see is refused**, not hosted as CPU-only (it would
    rent out a GPU machine without its GPU). AMD GPUs are detected by rocm-smi only, as before: an AMD GPU
    without ROCm is treated as no GPU (so a Ryzen's integrated GPU does not block CPU-only hosting).
21. **`runtime prepare` driver:** with no `--driver`, the default branch on a machine with an NVIDIA GPU
    (nvidia-smi or PCI) and none otherwise -- which includes AMD GPU hosts, which earlier versions baked the
    (useless there) NVIDIA driver for. `--driver none` is accepted too.

## Not a deviation, but changed

- `TestPreflightNoKVMNoIOMMUNoGPU` asserted that a bare machine is refused for having no GPU and no IOMMU.
  CONTRACT-v2 reverses that; the test now asserts the opposite for a GPU-less machine and keeps the IOMMU
  refusal for a machine with a GPU.
- `rentalDiskGB` looped forever on Windows when the data directory did not exist (`filepath.Dir` of the root is
  the root there, never "/"). Found by a new test; fixed to stop at the root on any OS. No effect on Linux.

# The merge: what integrating the two tracks decided

- **One driver decision.** v0.1.10's `ChooseDriver` (match the host's own driver)
  stays as it was; `vmrt.ChooseImageDriver` wraps it and returns "none" on a machine
  where neither `nvidia-smi` nor the PCI bus shows an NVIDIA GPU. `runtime prepare`
  (no `--driver`) and the automatic setup both use it, so a GPU-less machine sets
  itself up with an image without a driver. The CLI's `--driver` accepts a branch or
  `none`. Replaces the second track's `BakeDriver`.
- **"No GPU" is no longer a reason only a person can fix** (it is a hostable machine);
  an NVIDIA GPU the driver cannot see is (the setup cannot install host drivers).
  The setup test that used "no GPU" as its person-only example now uses the latter.
- **Capability locking:** the first track's `p.mu` guards the capability (its
  `SetCapability`/`Adopt` rely on it); the second track's separate lock is gone.
  `Adopt` also takes the fresh pair view.
- **One DGX Spark matcher** (`interconnect.MatchIdentity`, which now also checks the
  OS) serves the desktop rule and the pair checks; the capability's `identity` has
  CONTRACT s3.1's shape, so it gained `board_name` and `reason` next to the first
  track's four fields (a test pinned the four-field JSON and was updated).
- **The pair test takes the busy record** (`busy.json`) like `check --boot`, and its
  VM id ends in `-pairtest`, which the runtime counts as one of its own VMs: an agent
  restart leaves it to the running test, or tears an orphaned one down.
- **No cable check, peer announcement or pair rental while the automatic setup runs
  or on a withdrawn machine** (409 / 503).
- **Version wording:** "v0.1.10" became "v0.2.0" where it named the release to come
  (README, help text); where comments say "absent from agents up to v0.1.9" they are
  right as they stand, since no v0.1.10 was released. Test fixtures that use
  v0.1.10 -> v0.1.11 as "running -> newer" were left alone. The hardware-verified
  record above (SID 2457) is about the build stamped v0.1.10 and is kept as written.

# ARM boards (CONTRACT-arm section A): where the build decided differently

- **The storage directory is recorded in its own file**, `/etc/gpu-agent/storage.json`,
  not in `config.yaml` (A5 says "persisted in the agent config"): `config.yaml`'s
  presence is what starts the legacy stats server, so an agent that only moved its
  data must not create it. State files (registration, busy.json, rental state,
  journals) stay in `/var/lib/gpu-agent`; only the base image, the downloaded cloud
  image and the rentals' disks move.
- **`--data-dir` always ends in a directory called `gpu-agent`**: `--data-dir /mnt/nvme`
  stores into `/mnt/nvme/gpu-agent`, so `gpu-agent remove` only ever deletes a
  directory the agent made, never a disk's root.
- **40 GB free, not 20 GB** (A7's README line): the runtime still keeps 20 GB free for
  the machine itself beside a 20 GB rental disk, as it did before this release. The
  README, the reasons and `setup --data-dir` all say 40 GB free. Lowering the machine's
  own reserve on small boards was not done: an eMMC root that fills up bricks the board
  until someone logs in.
- **Memory floor:** the host keeps a tenth of its memory, never under 4 GB -- but on a
  machine of less than 8 GB, half; the VM needs at least 2 GB. So a 4 GB board hosts
  (2 GB VM) and a 16 GB board gives 12 GB. Before, a machine needed 6 GB. The x86 and
  Spark numbers are unchanged (a tenth or 4 GB, whichever is more).
- **SD cards are recognised two ways:** `/sys/block/<disk>/removable` = 1 (A5), and the
  MMC device type `SD` (`/sys/block/<disk>/device/type`), because a board's own SD slot
  is usually NOT marked removable. eMMC reports `MMC` and is allowed.
- **Specs ride the capability report too.** A1's CPU model, cores and board were only
  sent at registration, so an ARM host registered by an older agent would keep an empty
  model forever. The capability report now carries `specs` next to `capability`; a
  control plane that ignores the field loses nothing. ServCast must read it (part B).
- **Spark rentals now get 10 vCPUs, not 18.** A2 pins every heterogeneous ARM machine's
  VM to its fastest cluster, and a GB10's is its 10 Cortex-X925 cores (the 10
  Cortex-A725 stay with the host). The capability's `guest.vcpus` says 10, so what a
  renter is told matches. Not verified on a Spark: see the hardware list.
- **An AMD GPU that `rocm-smi` does not list is treated as no GPU** (the machine hosts
  CPU-only), unlike an NVIDIA GPU without its driver, which is refused. AMD's PCI vendor
  id also covers the integrated Radeon in most Ryzen desktop CPUs, so refusing on it
  would turn away ordinary GPU-less machines. A host who wants to rent an AMD GPU
  installs ROCm first, as before.
- **Mali and the NPU are never GPUs:** GPU detection only looks at NVIDIA (`nvidia-smi`,
  PCI vendor 0x10de) and AMD (`rocm-smi`); platform devices are not on either list.
- **Temperature** (A6) is the hottest `/sys/class/thermal` zone, read on every Linux
  machine and reported as `specs.cpu.temp_c` (a GPU's own temperature stays in
  `specs.gpus`); a zone reading 150 C or more is ignored as a broken sensor.

# Operations (CONTRACT-ops section A): where the build decided differently

- **The update offer arrives two ways**, as ServCast built it: the capability report's
  answer (`agent_update`) and the header `X-Marketplace-Agent-Update:
  {"version":"vX.Y.Z","push":bool}` on the marketplace's own `GET /status` (its
  heartbeat). The header is read strictly: a JSON object with exactly `version`
  (`^v\d+\.\d+\.\d+$`) and `push` (boolean); anything else is ignored. Only a newer
  release is acted on.
- **The agent now reports its capability every 30 minutes** as well as on events, so an
  offer (and its problems) reach it even with no heartbeat and nothing changing.
- **Retry spacing** (not in the contract): one try per release per 10 minutes; a
  release that failed or was rolled back is not tried again automatically for 24 hours,
  or 1 hour when pushed. Without it a bad release would restart the agent at every
  heartbeat. The spacing survives the rollback's restart (it reads `update.json`).
- **"Idle"** is the `/update` rule: not rented or provisioning, no rental leftover, no
  automatic setup, no `busy.json` holder (a person's `check --boot`, `check --boot
  --pair` or `setup`), not withdrawn.
- **The automatic updates' switch** is `/etc/gpu-agent/auto-update` ("off"/"on", like
  the automatic setup's `auto-setup`), not `config.yaml`, for the same reason as the
  storage directory.
- **Preflight problems** are the hosting checks' findings (test-boot findings under
  `testboot`), synced at every report and not while the automatic setup runs (its
  progress is the capability's `setup` field then). A withdrawn machine sends no reports,
  so its `register` problem stays local (`status` shows it).
- **`capability.setup`** = {step, state, message, at}: `step` is the setup step id
  (`deps`, `image`, `test-boot`), `state` one of running, passed, failed, interrupted.
- **Command-line runs** (`check --boot`, `check --boot --pair`, `setup`) write their
  failures to the same `errors.json`; the daemon sends them with its next report.

# v0.2.2: any GPU, not NVIDIA only (owner decision, 2026-09-19)

The owner's rule: the microVM only has to make sure the GPU is passed through,
for any GPU; the image gets a driver per make; a machine whose only GPU is its
processor's own keeps hosting CPU-only. This supersedes item 20 and the AMD/ROCm
bullet above.

- **GPUs come from the PCI bus, any make** (`internal/pcidev`): display-class
  devices, plus NVIDIA and AMD accelerator-class ones (the MI300 family is class
  0x12). BMC displays (ASPEED, Matrox, Huawei iBMC, Pilot, Silicon Motion, XGI),
  virtual displays and SR-IOV virtual functions are not GPUs. The host needs no
  driver: `nvidia-smi` only adds names and unified memory where installed.
- **An NVIDIA GPU without its host driver is rented**, not refused (was item 20):
  it is still never passed off as a machine without a GPU. **An AMD GPU no longer
  needs ROCm.** The Ryzen case item 20 guarded is now the integrated-GPU rule: the
  processor's own GPU (Intel at 00:02.0; an AMD APU's behind bridge 00:08.1, or
  with the processor's PSP in its slot) is never rented, so a machine with only that hosts CPU-only, as before.
- **Left out, not refused:** a card already on `vfio-pci`, one drawing a desktop
  that does not close for rentals, one whose IOMMU group holds a non-graphics
  device, and the boot display when another card can be rented (judged after the
  groups, on the cards that can) are listed in the new `capability.excluded`; the
  machine is refused only when it has cards and none is left. Cards sharing a
  group are judged together. Anything else holding a card is judged at rental
  start, as before: a brief holder at agent start takes nothing off the market.
- **Holders, per GPU:** an NVIDIA GPU is its `/dev/nvidia*` nodes only, exactly
  as before (its DRM node is also logind's copy of a desktop's, which would take
  a DGX Spark off the market); other makes are their DRM nodes, plus `/dev/kfd`
  for AMD; every GPU its VFIO group node. systemd and systemd-logind are never
  holders. The same-slot rule accepts any function in an NVIDIA GPU's slot, as
  before; other makes' slots only graphics-card functions (an APU's slot holds
  the host's USB and its PSP).
- **Onboard chips:** besides the BMC vendors, the ATI ES1000, Rage XL and Radeon
  7000 by device, AWS Nitro and Renesas by vendor, and any non-NVIDIA VGA-class
  device whose largest memory window is under 256 MB. The processor's own GPU is
  also any Intel or AMD display function on a root bus (pre-Zen APUs).
- **Image:** `GoldenInfo.vendors`; AMD and Intel cards get
  `linux-firmware-amd-graphics` / `linux-firmware-intel-graphics`. NVIDIA stays
  as items 21 and the driver match decided; an image from before makes were
  recorded counts as NVIDIA when it has a driver, so no NVIDIA host rebuilds.
- **Test boot:** passes when the VM sees each passed GPU by vendor:device with
  every standard memory BAR mapped (an unplaced legacy I/O window is normal); a driver that did not take it is a note. The record's
  `gpus` (model, memory, driver) go to the marketplace as `capability.gpus`.
- **Teardown:** the verifier works from the drivers the rental recorded (an agent
  restarted mid-rental still asks), looks only at the rented GPUs' rows, and runs
  `nvidia-smi`/`rocm-smi` only where installed; an NVIDIA GPU with no memory
  figure is checked by what holds it, like a GB10.
- Not verified on AMD or Intel hardware.

# v0.2.3: the host keeps using the machine until it is rented (CONTRACT-hostuse.md A and D)

Built on origin/main f5eeda5, which is already tagged **v0.2.2** (the any-GPU release
above), so this is the next release, v0.2.3; texts and comments say v0.2.3.

## Host use (A1)

1. **What counts as the host's use:** a process holding a rented GPU that is neither the
   desktop (by name, or -- item 16 of the first part -- anything in a graphical login),
   nor one of NVIDIA's services, nor a short-lived tool or a child of a gpu-agent; and, on
   any machine, less `MemAvailable` than the rental's VM gets (`GuestMemoryMB`). Page
   cache counts as free, as the kernel counts it; ZFS's ARC does not, so a ZFS host may
   read as short of memory. A program started from a terminal inside a DGX Spark's
   desktop session counts as the desktop and closes with it when a rental starts, as in
   v0.2.0 -- **an owner question** if that is not what is wanted.
2. `host_busy.since` is the agent's own clock: it starts again when the agent restarts.
   `holders` is always a list (`[]` when only memory is short).
3. **/provision without `start_by` on a machine in use is refused (409, naming the
   programs)**; before, it was accepted and failed in the background with the GPU in
   use. `start_by` at or before now: 409; `start_by` <= 0: 400. The same rental asked
   again while it waits gets the same 202; another rental: 409.

## Waiting rentals (A2)

4. **/status `pending`** is `{rental_id, since, start_by, holders, state, reason?,
   detail?, memory_short_gb?, waiting_for_peer?}` with `state` `waiting` or `failed`. While
   a rental starts after the host freed the machine (its full test first) /status says
   `provisioning` and shows no `pending` (the record's internal state is `starting`). A
   failed record stays until its /teardown, a new rental, or 48 hours. Reasons: `its GPU
   could not be handed to the rental` (the full test right before the rental failed; the
   top-level `error` starts with the same words) and `the rental could not start on this
   machine` (anything else: the start itself, a machine that stopped being ready).
   /status always carries `host_busy` (null when free) and `retest_pending`.
5. **At start_by** the rental is dropped (no failed record, as the contract says), the
   host told why, and a problem noted for the marketplace.
6. **/teardown of a waiting or failed rental** answers `{"status":"cancelled"}`; during
   its full test the test is stopped cleanly and the rental cancelled; once its VM is
   booting it is torn down as any start (retry shortly).
7. **Notifications:** `wall` reads the text on stdin; `notify-send -u critical` runs as
   each person logged in to a desktop (logind sessions of type x11/wayland/mir and class
   user -- not the login screen, not ttys) through `runuser` and
   `DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/<uid>/bus`; each is bounded by `timeout
   20`, so a stuck terminal or bus cannot hold the agent. A reminder is sent every 2
   hours and once more when an hour or less is left (the title then says so); the times
   are kept in pending.json, so a restart keeps the rhythm. If the first message could
   not reach everyone, that is noted for the marketplace. A free pair half's machine is
   not told anything (there is nothing to do on it).
8. **A host who takes the GPU back in the moment before the start** (the runtime refuses
   a GPU in use) does not lose the rental: it waits on until its start_by.
9. **A direct start on a free machine whose running version owes the full test runs that
   test first, inside `provisioning`** -- up to 5-15 minutes more before `rented`
   (ServCast's boot deadline is 30 minutes). A failure there is the same GPU failure
   (a `failed` pending record) as for a waiting rental. A rental without start_by whose
   test an agent restart cut short is dropped (did not start), not resumed.
10. **Updates are not held back by a waiting rental** (it is on disk and waits on under
    the new agent, which owes its own full test); one that is starting holds them back.
    **Removing the machine is refused (409) while a rental waits** for it.

## Test boots (A3, D2)

11. **Validity across versions.** selftest.json records `host_driver` (each rented GPU's
    host driver with `/sys/module/<driver>/version`, e.g. `nvidia 580.95.05`) and
    `base_image` (the golden image's base, driver and build time). A pass holds while the
    GPUs, the host driver and the image are the same, whatever agent ran it; this is
    stricter than before within one version too (an image rebuilt by hand now asks for a
    new test, which Keep runs). **A record from before v0.2.3** (no `base_image`) holds
    for its own version and, for a later one, while the image was built before the test
    (`created_at` <= `at`); its host driver is unknown and not compared.
12. **`last_full_test`** in selftest.json keeps the last test that took the GPUs across
    tests without them. A failed full test for the same GPUs, driver and image keeps the
    machine off the market even after a pass without the GPU; only a full test lifts it
    (run when the GPU is free, 6 hours after the failure or at the agent's next start).
    `gpu_verified` is true for every passing test on a machine without a GPU.
13. **The test VM is sized down** to the free memory less 1 GB (in 256 MB steps, never
    under 2 GB); with less, no test runs and it waits. The full test before a rental
    runs with the machine free (enough memory by definition).
14. **Keep** (autosetup/keep.go) looks every 5 minutes (first a minute after the agent
    starts): a test the setup will not run again (its key passed) and the running
    version's full retest, only when that interrupts nothing (no host program on the
    GPU, enough memory, on a Spark nobody logged in to the desktop). Everything else it
    hands to the setup's own Run. It runs under BeginSetup, like the setup: **the machine
    is shown not ready, with a progress line, for the test's 5-15 minutes**. It does not
    run with the automatic setup off (the full test before a rental still does).
15. **DGX Spark:** the agent's own tests (setup, Keep) count a person logged in to the
    desktop as use -- they run without the GPU, or wait -- and close only the login
    screen. `check --boot` (a person) and the test before a rental may close the desktop.
16. `check --boot` runs without the GPU when the host's programs hold it (and says so
    before asking), and in a smaller VM when memory is short. The pair test (`check
    --boot --pair`) still needs the GPU free and is recorded per version, unchanged.

## Linked pairs wait together (coordinator addition)

17. A /pair/provision **with start_by** never starts its half alone. Each half is held by
    a goroutine: while its host uses it, it waits as a single rental does (202, the host
    told); once free (and its full test done, when owed) it meets the other half on the
    cable at the start of every minute: 20-second windows of `GPUAGENT-START1` frames
    (the rental id's first 8 characters, the port's MAC, "heard you", a sequence number,
    HMAC-SHA256 keyed with the full rental id). A half starts when the other said, at
    least 5 seconds before the window ended, that it heard this one; both then start
    within the same minute, holding the frame lock from the rendezvous to the start. The
    two clocks must agree within seconds (NTP), as the peer announcements already need. A
    half that heard the other's "heard you" in the last instant before the cutoff, while
    the other did not hear its own, would start alone: frames every 250 ms over 15
    seconds make that need seconds of loss at exactly that moment.
18. **A free half answers 200 `provisioning`** (two free halves meet within the minute and
    boot), and after a rendezvous that did not agree it shows `waiting_for_host` with
    `pending.waiting_for_peer: true` and `holders: []` (ServCast's panel shows such a
    machine as "held by its own programs" today -- it could say "waiting for the other
    machine"). At start_by each half gives up by itself.
19. **pending.json keeps a pair request with its intra-pair key** (0600, root; deleted
    when the half starts or is dropped): the half must start as asked after a restart.
    v0.2.0 kept the key out of rental.json; this is the one place it is on disk.
20. A /pair/provision **without** start_by starts as before. **Agents before v0.2.3
    refuse `start_by` in /pair/provision** (strict decoding: 400) and ignore it in
    /provision (a machine in use then accepts and fails in the background, as before):
    ServCast must send start_by only to agents that report v0.2.3 or later.

## Every error reaches the marketplace (D1)

21. Added to the problems the capability report carries: the relay settings unreadable
    at start (tunnel, active), the automatic setup switched off while it has work to do
    (setup, active until it is on or has nothing to do), a speed test the listing asked
    for that failed twice (agent, event), a rental cancelled at its deadline or because
    its GPU could not be handed to it, or that could not start (rental, event), the
    first notification that could not reach everyone (rental, event), and a waiting
    rental that could not be read back after a restart (rental, event). A failed Keep
    test is the hosting checks' test-boot finding, synced with every report.

## Tests

vmrt (host use, test planning, notifications, a test without the GPU, a failed full test
outweighing a pass without it, legacy records), interconnect (the rendezvous both ways,
one way, another rental's frames, forged frames), provisioner (host-busy capability for a
GPU holder and for memory on a machine without a GPU; 202 + pending.json + notification;
refusals; start when freed with and without the owed full test; failed pre-rental test;
reminders and give-up; teardown cancels; restart; a start the host cut in on; the two
halves of a pair on one wire: waiting together, starting together, giving up together,
cancelled), control (202 body, /status shapes, teardown cancel, pair start_by), autosetup
(first test without the GPU, retest when free, failed GPU test retried only with the GPU
after 6 h, Keep running a test the setup will not, setup off as a problem), hostctl,
status lines. Not run on hardware.
