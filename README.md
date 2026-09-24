# GPU Marketplace

A P2P GPU marketplace agent that lets you list your server for others to rent: a GPU server, a machine without a GPU, a DGX Spark, or two DGX Sparks cabled together and rented as one. The agent registers your host with a one-time code, keeps a reverse SSH tunnel to the nearest relay, checks whether the machine can host a rental safely, and runs rentals in isolated microVMs.

> **Status: a machine is offered to renters only after it has proven it can host.** The agent carries its own microVM runtime (QEMU/KVM, GPU passthrough over VFIO, a per-rental encrypted disk). A machine reports *ready* only when every hosting check passes **and** it has passed a real test boot on its own hardware; until then the marketplace neither shows it to renters nor accepts orders for it. Since v0.2.0 the agent does the setup that gets it there by itself once the machine is linked -- see [Automatic setup after linking](#automatic-setup-after-linking) -- and hosts [machines without a GPU](#hosting-a-machine-without-a-gpu) and [linked pairs of DGX Sparks](#linked-pairs-two-dgx-sparks-rented-as-one) too. Up to v0.1.5 the agent answered rental requests with a stub that reported success and created nothing; that stub is gone. See [Can this machine host a rental?](#can-this-machine-host-a-rental)

## How It Works

1. **Install the agent** on your Linux server — with a GPU or without one
2. Generate a **one-time registration code** in your dashboard and run `gpu-agent register --code <code>`
3. The agent registers, then **tests latency** to the available locations and **prompts you to pick one** (closest preselected) — under the hood it opens a **reverse SSH tunnel** to that location's relay. `register` restarts the agent service, so the machine is online at once
4. The agent **finishes the machine's setup by itself**: it installs the microVM runtime, builds the rental image and runs a test rental — about 25-45 minutes, nothing to type; the control panel shows each step ([Automatic setup after linking](#automatic-setup-after-linking))
5. **Configure your listing** in your dashboard; nothing is published until you do
6. Once configured **and** the machine passes its hosting checks, your server appears on the **marketplace listing** for renters
7. When rented, the agent fences the tenant off your network, boots an **isolated microVM** with the GPUs passed through (when the machine has any), and **wipes it clean** when the rental ends — refusing the rental outright if any of that cannot be done

## Quick Install

### Linux
```bash
curl -sSL https://raw.githubusercontent.com/serverroom/gpu-marketplace/main/scripts/install.sh | sudo bash
```

### Windows (PowerShell as Admin)
```powershell
irm https://raw.githubusercontent.com/serverroom/gpu-marketplace/main/scripts/install.ps1 | iex
```

### macOS
```bash
curl -sSL https://raw.githubusercontent.com/serverroom/gpu-marketplace/main/scripts/install-mac.sh | sudo bash
```

## Manual Setup

### Prerequisites
- An OpenSSH client (`ssh`, `ssh-keygen`) — present on virtually all Linux systems
- Download the `gpu-agent` binary from [Releases](https://github.com/serverroom/gpu-marketplace/releases)

### Steps

```bash
# 1. Register this host with a one-time code from your dashboard
sudo gpu-agent register --code <code>

# 2. Install as a system service
sudo gpu-agent install

# 3. Start the service
sudo gpu-agent start
```

## Agent Commands

| Command | Description |
|---------|-------------|
| `gpu-agent register --code <code>` | Register this host (latency test, key generation, listing) |
| `gpu-agent check` | Check whether this machine can host a rental, and show what a tenant is fenced off from (`--rules` prints the exact firewall rules) |
| `gpu-agent setup` | Finish this machine's setup now, in the foreground with its output: the same steps the agent takes by itself after linking (for support) |
| `gpu-agent setup --status` | Show the last setup attempt, and whether the automatic setup is on |
| `gpu-agent setup --off` / `--on` | Turn the automatic setup off (the agent then waits for the manual commands below) or back on |
| `gpu-agent setup --data-dir <dir>` | Keep the rental base image and the rentals' disks on another disk — an NVMe on a board whose eMMC is small, say (never an SD card); moves what is there already |
| `gpu-agent runtime prepare` | Install the microVM runtime (`--install-deps`) and build the rental base image with what this machine's GPUs need — the NVIDIA driver (by default the one matching this machine's own driver, the default branch when the host has none, and none on a machine without an NVIDIA GPU; `--driver 580-server` or `--driver none` picks), and the firmware AMD and Intel cards load — and the RDMA tools |
| `gpu-agent runtime prepare --headless` | Make a desktop machine (a workstation) start without its desktop, so the GPU is free to rent; closes the desktop now. `gpu-agent remove` brings the desktop back. A DGX Spark does not need it: its desktop closes only while it is rented or testing |
| `gpu-agent check --boot` | Boot a real test rental (the GPU passed through, when the machine has one), check what it can and cannot reach, tear it down, and record the result |
| `gpu-agent check --pair` | Check, without changing anything, whether this machine can be half of a [linked pair](#linked-pairs-two-dgx-sparks-rented-as-one): what it is, its ConnectX-7 ports, the agents heard on them, its last pair test, and everything to fix (`--json` for the raw report) |
| `gpu-agent check --boot --pair` | Run the pair test, on both machines of a pair within 10 minutes: each finds the other on the cable, boots a test rental with its GPU and ConnectX card, and measures every link (`--min-rdma-gbps N`, default 100; `--yes` skips the prompt) |
| `gpu-agent update` | Update the agent to the latest release (`--version v0.2.1` picks one), exactly as "Update agent" in the control panel does; Linux |
| `gpu-agent update --auto off` / `--auto on` | Pause the automatic updates, or turn them back on (an update the marketplace pushes still applies) |
| `gpu-agent install` | Install as a system service (systemd/launchd/Windows Service) |
| `gpu-agent remove` | Withdraw the listing, revoke relay access and delete the agent completely (`--yes` skips the prompt) |
| `gpu-agent uninstall` | Remove the system service only — keys, token and listing stay; use `remove` to take everything off |
| `gpu-agent start` | Start the service |
| `gpu-agent stop` | Stop the service |
| `gpu-agent status` | Check the service *and* the registration state |
| `gpu-agent speedtest` | Measure download, upload and latency to the speed test server in this machine's location, and post them to the listing (not while rented) |
| `gpu-agent test-stats` | Collect and display system stats as JSON |
| `gpu-agent -version` | Print version |

Running without arguments starts the agent interactively or as a managed service.

`status` reports the two halves separately, because they fail independently — a
freshly installed agent is a healthy service with nothing to do yet:

```
$ sudo gpu-agent status
Service:      running
Registration: not registered

not registered — run 'gpu-agent register --code <code>' with a one-time code from your dashboard, then restart the service
```

```
$ sudo gpu-agent status
Service:      running
Registration: listing L-1042, location nyc, tunnel configured
```

Run it with `sudo`. Both halves need it: the registration state is stored 0600
and root-owned, so an unprivileged `status` can see that a registration exists
but not read it — and on macOS the daemon lives in launchd's system domain,
which a normal user cannot query at all, so the service line reads `unknown`
rather than guessing.

Once registered, the agent measures this machine's network once, against the
speed test server in the location of its relay: download and upload in Mbps,
and latency in ms. The result is shown on the listing, so renters see what the
connection can do before they rent it. The test takes up to 40 seconds in the
background. It never runs while the machine is rented, and it stops if a rental
starts; if a rental is on the machine, it waits for the next agent start.
`sudo gpu-agent speedtest` measures again at any time the machine is not rented,
and updates the listing.

## Automatic setup after linking

Linking a machine (`sudo gpu-agent register --code <code>`) is the last command a
host types. The agent restarts, reports the machine to the marketplace, and — when
the only things between the machine and *ready* are steps it can take itself —
takes them in the background:

1. **Installs the microVM runtime** with apt: `qemu-system-x86` (or `qemu-system-arm`),
   `qemu-utils`, `ovmf` (or `qemu-efi-aarch64`), `cloud-image-utils`, `cryptsetup-bin`,
   `nftables`, `iproute2` and `kmod`, the same packages as
   `runtime prepare --install-deps`. Nothing else. A few minutes.
2. **Builds the rental image**: Ubuntu 26.04's official cloud image, checked against
   Canonical's published checksums, with the NVIDIA driver baked in. The driver
   matches this machine's own: the same branch, and the open kernel module when this
   machine runs the open one (`580-server-open` on a DGX Spark), the proprietary one
   otherwise (`580-server`). A machine without an NVIDIA GPU gets no driver. Every
   image also carries the RDMA tools a linked pair needs. About 20-30 minutes.
3. **Runs a test rental** with the GPU passed through (on a machine that has one),
   exactly as `sudo gpu-agent check --boot` does. About 5-15 minutes. It never stops
   anything of yours: while your own programs use the GPU it runs without the GPU
   (see [Using your machine while it is listed](#using-your-machine-while-it-is-listed)).

Only the steps a machine still needs run; a machine that already has its image only
runs the test. While it works, the control panel shows one line in place of the
reasons, for example:

```
Setting up automatically: building the rental image (step 2 of 3, started 14:05 UTC). Nothing to do; this takes about 20-30 minutes.
```

`sudo gpu-agent status` shows the same progress, and `sudo gpu-agent setup --status`
the last attempt in full. When it finishes, the machine reports ready. If a step
fails (a mirror is down, the network drops), the panel says why and the agent tries
again after its next restart or in 6 hours; `sudo gpu-agent setup` runs it at once,
with its output.

It never runs while a rental (or the leftover of one) is on the machine, and no rental
can start while it runs: the machine is not ready until it has finished. It runs once
per agent version, host driver and Ubuntu release; a new agent, a driver update or a
different GPU is a new setup. It does **not** try to fix what needs a person — no KVM,
the IOMMU off, a GPU sharing its IOMMU group, too little memory or disk, a desktop on
the GPU of a machine that is not a DGX Spark, a system without apt — those stay in
`gpu-agent check` for you to fix.

**After an update (v0.2.3 and later)** the machine keeps being offered on its last
passing test rental, as long as its GPUs, their driver on the host and the rental
image are the ones that test ran with. The new version runs its own full test by
itself when the GPU is free, and always right before a rental starts (see
[Using your machine while it is listed](#using-your-machine-while-it-is-listed)).
Up to v0.2.2 the test rental was recorded per agent version, so every update took the
machine off the market until its test passed again. A pair's own test (`check --boot
--pair`) is still recorded per version, and is run again on both machines.

**Turning it off:** `sudo gpu-agent setup --off` (it writes `off` to
`/etc/gpu-agent/auto-setup`); `--on` turns it back on. With it off, the machine waits
for the manual commands, which stay available for support:
`sudo gpu-agent runtime prepare --install-deps` (packages and image) and
`sudo gpu-agent check --boot` (the test rental).

## Updates

From v0.2.0 the agent **keeps itself up to date**. Every capability report it sends
(at start, whenever something changes, and every 30 minutes) is answered with the
release this machine should run, and the marketplace's own status check every 30
seconds carries the same offer. When a newer release is named, the agent installs it
**only when the machine is idle** — not rented, no leftover of a rental, no setup, test
boot or pair test running — and otherwise waits for the next offer. It installs it
exactly as "Update agent" does (below): from the release address it builds itself,
checked against `checksums.txt`, with the automatic rollback. After an update the machine
stays on the market on its last passing test boot, and the new version runs its own when
the GPU is free (and before a rental starts). A rental waiting for you to free the machine
holds an update back, as a running one does.

- `sudo gpu-agent update --auto off` pauses the automatic updates; `--auto on` turns
  them back on. `sudo gpu-agent status` says which. An update the marketplace
  **pushes** — Server Room staff, or your own "Update agent" in the control panel — is
  still applied when the machine is idle.
- A release that failed or was rolled back is not tried again automatically for a day
  (a pushed one: an hour), so a bad release cannot restart the agent over and over.
- The agent never asks GitHub which release is the latest; it downloads only from the
  release address compiled into it.

**Problems the marketplace sees.** The agent keeps its last 20 problems — anything that
stopped, or stops, this machine being listed: a hosting check that fails, an automatic
setup step or test boot that failed, an update that failed or was rolled back, a relay
connection that has been failing for over five minutes, reports that could not be sent,
a rental that did not start or whose cleanup did not verify, a rental cancelled because
the machine was still in use or its GPU could not be handed to it, the automatic setup
switched off while it has work to do, a speed test the listing asked for that failed — in
`/var/lib/gpu-agent/errors.json`, and sends them with every capability report (a new one
at once, at most once a minute). Each says when, what, and whether it still stops the
machine. `sudo gpu-agent status` lists the ones still open under "Problems".

## Updating the agent, and removing a machine, from the control panel

**Update agent.** When a newer agent is released, the control panel offers to update
a published Linux machine that is not rented. The agent checks the request at once —
a release version (`vX.Y.Z`) newer than the one running, a release build, no rental,
no setup or test boot and no other update under way — and answers; the rest happens in
the background, and the panel follows it:

1. **downloading** the release for this machine from
   `github.com/serverroom/gpu-marketplace/releases` — the agent builds that address
   itself; only the version comes from the panel;
2. **verifying** it against the release's `checksums.txt` (the installers' rule) and
   by running it: it must say it is the version asked for;
3. **restarting**: the running binary is kept as `gpu-agent.prev`, the new one is moved
   into its place in one step, and the agent restarts a few seconds later.

Three minutes after the restart, the previous binary checks the new agent from a
systemd timer of its own: if the service is not running, or is not running the new
binary, it puts itself back and restarts the agent. The outcome is **ok**,
**rolled_back** or **failed** (anything that fails before the new binary is moved into
place leaves the running agent untouched), recorded in `/var/lib/gpu-agent/update.json`
and shown by the panel and by `sudo gpu-agent status`. `sudo gpu-agent update` does the
same from the machine. Agents older than v0.2.0 cannot update themselves: run the
install command once.

**Remove from marketplace.** When the host removes a published machine in the control
panel (or its capability report is answered *410 Gone*), the agent stops hosting: it
refuses every rental, stops its automatic setup, its reports and its tunnel, and removes
a rental firewall table a crash may have left. It is not uninstalled — the binary and
`/etc/gpu-agent` stay, so the agent says on every start, and in `status` and `check`:

```
This machine was removed from the marketplace in the control panel. Run 'sudo gpu-agent remove' to uninstall the agent, or register again with a new code to list it again.
```

A rented machine is removed once its rental ends. Registering again with a new code
lists it again (the record, `/var/lib/gpu-agent/withdrawn.json`, is cleared).

## DGX Spark: the desktop comes and goes with a rental

A DGX Spark draws its desktop on its only GPU, the GB10. On a machine the agent
confirms as a DGX Spark (Linux on arm64, DMI vendor NVIDIA, product "DGX Spark", a GB10
GPU), the desktop is not a reason to refuse a rental: **the desktop closes while the
Spark is rented or running its test rental, and comes back after.** Nothing to do: the
desktop counts as its own programs only — the display server, the shell, the display
manager and its login screen, and (since v0.2.5) GNOME Shell's own helpers such as the
desktop icons (the agent asks systemd and logind where each program using
the GPU runs); since v0.2.3 the browsers and other programs you open on it are your own use,
which a rental waits for ([below](#using-your-machine-while-it-is-listed)). Someone logged in
is warned 15 minutes before the desktop closes. The agent stops the display manager (`display-manager.service`,
or `gdm3`), which ends those logins, waits up to 30 seconds for every program to let go of
the GPU, and starts the display manager again once the GPU is back with its driver — also
after an agent crash or a reboot in the middle of a rental. If something outside the
desktop holds the GPU (a container's job), the rental waits for it to stop (since v0.2.3;
see [Using your machine while it is listed](#using-your-machine-while-it-is-listed)) before
the desktop is touched; if something still holds it after the desktop closed, the display
manager is started again and the rental does not start, naming what holds it. The same goes for a
[pair rental](#linked-pairs-two-dgx-sparks-rented-as-one); the pair test is refused.

**Save open work on a listed Spark:** its desktop, with everything open on it, closes
when a rental starts (and for the full test rental right before it), and anything unsaved
is lost; you are told first, and given 15 minutes. Since v0.2.3 the agent's own
test rentals leave a desktop someone is logged in to alone (they run without the GPU
then) and close only the login screen; a test you run by hand (`check --boot`) closes the
desktop as before. The control panel says so while a test runs. A Spark made headless on purpose
(`runtime prepare --headless`) is left headless. The capability the agent reports carries
the raw DMI strings (`identity`) so the match can be checked.

## DGX Spark: rented as a hardened container (v0.2.4)

A microVM needs its GPU passed through over VFIO, and the GB10 cannot be yet: its IOMMU
group asks for a 1:1 mapping, which the kernel's generic `vfio-pci` refuses, and the signed
`nvgrace-gpu-vfio-pci` that handles such GPUs does not carry the GB10's id (10de:2e12) in
any DGX OS kernel so far (checked up to 7.0.0-1019-nvidia). Until it does, a machine whose
NVIDIA GPUs all have no VFIO path is rented as a **hardened container** instead. The agent
finds this by itself; there is nothing to set. What a rental gets stays the same:

- rootful podman with a user-namespace remap (`--userns=auto`): root in the container is an
  unprivileged uid on the host; every capability dropped except the few sshd needs,
  `no-new-privileges`, no host network, a process limit, the rental's memory and CPUs;
- the GPU shared in over CDI (the NVIDIA Container Toolkit) with the host's own driver; the
  renter installs the CUDA userspace they want;
- the same fence (the `gpurent0` bridge and the `inet gpu_rental` table), the same address
  for the renter's SSH, an encrypted home (`/home/renter`) whose key dies with the rental,
  and a teardown that fails closed;
- the renter logs in as `renter`, without sudo.

`sudo gpu-agent runtime prepare --install-deps` (or the automatic setup) installs podman and
the container toolkit, writes the CDI spec, adds the user-namespace id ranges and builds the
rental image; `sudo gpu-agent check --boot` runs the container test boot. The container
shares the GPU, so **a container test boot runs beside the desktop** and never closes it; a
rental closes it as above, with the 15-minute warning, and waits for your own programs on
the GPU first, as on any Spark. A linked pair needs microVMs, so a Spark in container mode
cannot be half of a pair. When the signed nvgrace carries the GB10, the agent goes back to
microVMs by itself.

## Using your machine while it is listed

From v0.2.3 you keep using your machine until it is rented. Your own programs on the GPU
(`llama-server`, a training job, a container) and the memory they take do not take the
machine off the market: it stays offered, and `sudo gpu-agent status` says so:

```
Hosting:      ready — this machine can host a rental
In use:       In use by you: llama-server; the marketplace still offers this machine, and you are told when it is rented.
```

What counts as your use: any program you started that holds a GPU rentals get — in your
desktop session or not: a browser's GPU process, a `llama-server` typed in a terminal, a
container — but not the desktop itself (its display server, shell, display manager and
login screen: a DGX Spark's closes when a rental starts; on any other machine a GPU the
desktop draws on is left out of rentals), not NVIDIA's own services, not a short-lived
`nvidia-smi` — and, on any machine (one without a GPU too), less free memory than a
rental's VM needs. The agent only looks; it never stops, kills or signals a program of
yours.

**When the machine is rented while you use it**, the rental waits for you for up to 24
hours from the moment it was approved. You are told at once — a message on every terminal
(`wall`) and a desktop notification to everyone logged in to a desktop — again every 2
hours, and once more an hour before the deadline:

```
This machine has been rented. Please stop your programs that use it (llama-server) by 2026-09-19 15:00 EEST (12:00 UTC); the rental starts as soon as they have stopped. If they are still running then, the rental is cancelled and the machine is paused.
```

On a DGX Spark it adds: "The desktop closes when the rental starts; save your work."
`sudo gpu-agent status` shows the same line while the rental waits. On a DGX Spark someone
is logged in to, once nothing of yours holds the GPU any more (or when the rental arrives
with nothing of yours on it), the screen says "This machine's rental starts in 15 minutes:
the desktop will close then; save your work", and the desktop closes 15 minutes later for
the rental (and comes back after it). With nobody logged in, the rental starts at once. The agent looks every
15 seconds; as soon as your programs have stopped the rental starts. If they are still
running at the deadline, the rental is cancelled (the renter is refunded in full), you are
told, and the machine is paused on the marketplace until you put it back on sale in
Marketplace > List a GPU. A waiting rental survives an agent restart. A linked
pair waits for both machines: when one is free and the other is not, the free one waits
too (both start together, or neither — the two agents agree on the cable when both are
free, within the same minute, so the two machines' clocks must be right).

**Test rentals never interrupt you.** The test rental a machine needs (after linking, a
GPU change, a new rental image or a driver update) runs at once even while you use the
machine: with your programs on the GPU, it runs without the GPU — the VM, its network
fence, the internet, your blocked LAN, the encrypted disk, the SSH port and a clean
teardown are all tested — and the machine is offered on that pass; with little memory free
it runs in a smaller VM. The GPU's handover is then tested by a full test rental when the
GPU is free (the agent looks every 5 minutes), and always right before a rental starts
while this agent version has not passed one. On a DGX Spark the agent's own tests leave a
desktop someone is logged in to alone (they run without the GPU then), and close only the
login screen. If the full test right before a rental fails, the rental is cancelled with
"its GPU could not be handed to the rental" (the renter is refunded in full), and the
machine is paused and stays off the market until a full test passes — tried again when
the GPU is free, 6 hours later or at the agent's next start, or by hand with
`sudo gpu-agent check --boot`.

The machine's specs list the processor's own GPU too (marked `integrated`), so the
marketplace can say it is not included; rentals never take it.

The capability the agent reports carries `host_busy` (what you are using, and since when),
`retest_pending` (the full test is still owed) and `selftest` (the last test: passed,
whether it took the GPU, when, with which agent). The control panel shows them.

## Troubleshooting

**The agent seems to do nothing after installing.** That is the expected state
until you register — there is no tunnel and nothing is listening. The agent says
so on every start; read it with `sudo gpu-agent status`, or in the daemon log:

| Platform | Log |
|---|---|
| macOS | `tail -f /var/log/gpu-agent.err.log` |
| Linux | `journalctl -u gpu-agent -f` |
| Windows | `services.msc` → GPU Marketplace Agent |

**`register` succeeded but the agent is still offline.** Location assignment is a
second step, and the one-time code is already spent by then. `gpu-agent status`
shows `registered, no tunnel configured`; run `sudo gpu-agent select-location` —
it does not need a new code — then restart the service.

## Can this machine host a rental?

`sudo gpu-agent check` answers that without changing anything, and `status`
and `register` print the same answer. The agent reports it to the marketplace at
register and on every start; a machine is only shown to renters, and can only be
ordered, while its latest report says ready. A machine that is not ready also
refuses a rental request itself, with the reasons — it never accepts one it
cannot deliver.

Every one of these must hold, and `check` lists every one that does not:

- **Linux with KVM** (`/dev/kvm`). Rentals run in a microVM; Windows and macOS hosts cannot host.
- **On a machine with a GPU: the IOMMU enabled** (VT-d / AMD-Vi, or the SMMU on Arm), in firmware and on the kernel command line — without it no GPU can be handed to a microVM. A machine without a GPU does not need it.
- **Its GPUs, if it has any, can be of any make.** The agent finds them on the PCI bus itself — every display-class device, plus NVIDIA and AMD accelerator-class devices such as the Instinct MI300 family — so NVIDIA, AMD, Intel and others all count, and the host needs no GPU driver or vendor tool: the rental's VM is where a GPU has to work, and the test boot proves it gets there (an NVIDIA GPU without a host driver is rented as the GPU it is, never as a machine without one). The server's own management display (a BMC's ASPEED or Matrox chip, an old ATI ES1000, or any other VGA chip with less than a 256 MB memory window), a VM's virtual display and SR-IOV virtual functions are not GPUs. The processor's own GPU (an Intel iGPU, an AMD APU's) draws the machine's screen and is never rented; a machine with only that one hosts like a machine without a GPU ([below](#hosting-a-machine-without-a-gpu)). A card is **left out** of rentals, and named by `gpu-agent check`, when it draws a desktop that does not close for rentals, when it is already bound to `vfio-pci` for a VM of the host's own, when it is the boot display (the console) and another card can be rented, or when its IOMMU group holds a device that is not part of a graphics card; the machine is refused only when it has cards and none can be rented. Discrete GPUs report their own memory where their driver says; `nvidia-smi` on the host, where installed, adds names. **Unified-memory GPUs are supported too:** the NVIDIA GB10 in a DGX Spark (and the other GB10 boxes) has no separate GPU memory — `nvidia-smi` shows `[N/A]` — because the machine's memory is one pool used by both the CPU and the GPU. The agent knows this: it reports that pool as the GPU's memory, marks it unified so it is not counted twice, and a rental gets the pool as both its system and its GPU memory. **Apple Silicon** cannot host: it has no way to pass its GPU through to a microVM.
- **The rental runtime installed and its base image built** — QEMU, UEFI firmware (OVMF/AAVMF), `cloud-image-utils`, `cryptsetup` and `nftables`, and Ubuntu 26.04's official cloud image, verified against Canonical's published checksums, with what its GPUs need baked in — the NVIDIA driver matching this machine's (the default branch when the host has no NVIDIA driver, none on a machine without an NVIDIA GPU), and the firmware AMD's and Intel's in-kernel drivers load — and the RDMA tools. A GPU of another make added later asks for a rebuild. The agent does this by itself after linking; by hand, `sudo gpu-agent runtime prepare --install-deps`.
- **No rented GPU in use on the host** while a rental starts — each GPU is handed to the microVM whole, so anything using it (a container, a training job) must be stopped first. Your use does not keep the machine off the market: a rental that arrives while your programs hold the GPU waits for you, up to 24 hours, and the agent tells you ([below](#using-your-machine-while-it-is-listed)). The agent never stops a program of yours. NVIDIA's own background services (`nvidia-persistenced`, `nvidia-powerd`, DCGM) are not a problem: the agent stops the running ones for the rental and starts them again afterwards. Short-lived tools (`nvidia-smi`, `dcgmi`, a bug report) are given up to 5 seconds to finish, and the agent pauses its own GPU queries while it takes the GPU.
- **No desktop on the GPU** — except on a DGX Spark, whose desktop closes while it is rented and comes back after ([above](#dgx-spark-the-desktop-comes-and-goes-with-a-rental)). Any other machine that shows a desktop on a GPU (a workstation) holds that GPU for as long as the desktop runs, so the GPU is left out (and a machine with no other card cannot host) until it runs without one. `sudo gpu-agent runtime prepare --headless` makes it start without a desktop from now on and closes the running one (anything open on its screen closes too, so run it over SSH or be ready to log in again on the text screen); `sudo gpu-agent remove` restores the desktop from the next start. `gpu-agent check` names the desktop processes while this is needed.
- **Enough CPUs, memory and disk** — a rental gets at least 2 vCPUs and 2 GB of memory after what the machine keeps for itself: it keeps a tenth of its memory, never under 4 GB (on a machine of less than 8 GB, half), so a machine needs at least 4 GB. The rentals' disks need 20 GB free beyond 20 GB the machine keeps (40 GB free under `/var/lib/gpu-agent`, or wherever `setup --data-dir` put them), on fixed storage: a rental's disk never goes on an SD card or other removable media. When the disk holding `/var/lib/gpu-agent` is too small (a small system partition beside a big data one, say), the automatic setup puts the rentals' disks on the biggest internal disk that has room, in a `gpu-agent` directory there, and remembers it (since v0.2.6; it never picks a USB drive, and never moves them from a place you chose with `setup --data-dir`). `gpu-agent check` names that disk; `sudo gpu-agent setup --data-dir <directory>` puts them somewhere else.
- **A Linux kernel with what the runtime needs** — KVM (`/dev/kvm`), TUN/TAP, bridges, nf_tables, dm-crypt and loop devices. Stock Ubuntu and Debian kernels have them all; `gpu-agent check` names each one a vendor kernel lacks ([ARM boards](#arm-boards-rk3588)).
- **A passing test boot on this machine.** The automatic setup runs it; `sudo gpu-agent check --boot` runs it by hand: a real rental for a few minutes with a key nobody holds: the VM reports the GPUs it sees (on a machine that has any), that it reaches the internet, and that your machine, its address and its gateway are unreachable; it is then torn down and the GPU checked. A GPU passes when the VM sees it by its PCI vendor and device ID with every memory window (BAR) mapped; whether a driver took it inside the VM is reported, not required. What the VM saw — model, and memory where the driver says — is reported to the marketplace. The result is recorded with the GPUs, their driver on the host and the rental image it ran with, and a pass keeps counting across agent versions until one of those changes. A test run while your programs use the GPU passes without the GPU; the GPU's handover is then tested before a rental starts. A failed test keeps the machine off the market until one passes.

## Hosting a machine without a GPU

Any Linux KVM machine can host, GPU or not. A rental on a machine without a GPU
is the same microVM behind the same fence, with the same encrypted disk and the
same teardown — it just gets no GPU: the machine's CPUs (less one, or two on a
larger machine, for the host), its memory less a tenth (never under 4 GB), and
its disk.

- It needs everything in the list above except the GPU and the IOMMU: KVM, the
  runtime's tools and firmware, a base image, 4 GB of memory, 40 GB free on fixed
  storage, and a passing test boot **on this machine** — it is not offered to
  renters before. ARM boards: see [below](#arm-boards-rk3588).
- The automatic setup does all of it after linking, exactly as on a GPU machine:
  the runtime, a base image **without** the NVIDIA driver, and a test rental
  that checks what matters without a GPU — the VM boots, reaches the internet,
  cannot reach your machine or your network, and is torn down cleanly. By hand:
  `sudo gpu-agent runtime prepare --install-deps`, then `sudo gpu-agent check --boot`.
- A machine whose only GPU is its processor's own (an Intel iGPU, an AMD APU's)
  is a machine without a GPU: that GPU draws the machine's screen and is never
  rented.
- A machine with a GPU card is **not** treated as a machine without a GPU, even
  when the host has no driver for it: the card is rented as the GPU it is, so a
  GPU is never rented out as missing.
- If you add a GPU later, the agent notices its image was built without the
  driver and its test rental without a GPU, and redoes both (the automatic
  setup, or `sudo gpu-agent runtime prepare` then `sudo gpu-agent check --boot`).
- The agent reports the machine with `gpu_count` 0, and the marketplace shows it
  as a machine without a GPU.

## Windows and macOS hosting

The agent hosts on Windows and macOS too, not only Linux. On both, a rental runs
as the same [hardened container](#dgx-spark-rented-as-a-hardened-container-v024) a
DGX Spark uses, inside a Linux environment the agent owns — so a rental is fenced
off your network, on an encrypted disk whose key dies with it, and the renter logs
in as an unprivileged user, exactly as on a Linux host. There is nothing to set up
by hand: link the machine and the agent does the rest, the same as everywhere.

**Windows** (Windows 10 version 21H2 or later, Windows 11, Windows Server 2022 or
2025 — any edition, Home included). The agent installs WSL 2 from Microsoft's
signed release (one restart, then the setup carries on by itself), creates a
locked-down Linux distribution under an account of its own, and runs rentals in
it. An NVIDIA GPU reaches the rental through WSL's GPU sharing from your own
Windows driver (CUDA on WSL, NVIDIA driver 470+); a machine without an NVIDIA GPU
hosts CPU-only. Older Windows is told exactly what to update. What the agent adds
is removed cleanly by `gpu-agent remove`; WSL 2 itself stays, since it is part of
Windows and you may use it.

**macOS** (macOS 12 Monterey or later, Intel or Apple Silicon). The agent runs a
Linux VM with QEMU on Apple's Hypervisor.framework and runs rentals in it. QEMU is
installed with Homebrew (install Homebrew first if it is absent; the agent then
installs QEMU itself). A Mac cannot pass its GPU through to a guest, so a Mac rents
its CPUs, memory and disk ([a machine without a GPU](#hosting-a-machine-without-a-gpu));
a physical Mac only — a Mac that is itself a VM cannot host. `gpu-agent remove`
stops and deletes the VM; QEMU and Homebrew stay.

Everything else in this document — the fence, the encrypted disk, the automatic
setup, updates, the control panel, `gpu-agent status`/`check`/`remove` — works the
same on all three. `gpu-agent check` names anything a machine still needs.

## ARM boards (RK3588)

Any 64-bit ARM Linux machine with KVM can host: Rockchip RK3588 and RK3588S boards
(Radxa ROCK 5B, Orange Pi 5 and the like), Ampere and other Neoverse servers, and
the DGX Spark. A board's Mali GPU and NPU are not GPUs a renter can be given, so an
RK3588 board is a [machine without a GPU](#hosting-a-machine-without-a-gpu).

**What it needs:**

- 64-bit Linux (arm64) with KVM: `/dev/kvm` present. Many Rockchip vendor (BSP)
  kernels ship without KVM; a mainline or Armbian "edge" kernel has it. If the
  board's firmware starts Linux without virtualisation (the kernel log says "HYP mode
  not available"), no kernel can help: use the board maker's or Armbian's current
  image. `gpu-agent check` says which it is.
- The kernel pieces a rental needs: TUN/TAP, bridges, nf_tables (the fence), dm-crypt
  (the encrypted disk) and loop devices. `gpu-agent check` names each missing one.
- At least 4 GB of memory.
- At least 40 GB free on non-removable storage (eMMC, NVMe or SATA): 20 GB for a
  rental's disk and 20 GB left for the board. A rental's disk never goes on an SD
  card. On a board whose eMMC is small, the automatic setup puts the rentals on its
  NVMe by itself (since v0.2.6: in `/mnt/nvme/gpu-agent`, say, with the base image,
  remembered; its own state stays in `/var/lib/gpu-agent`). It never picks a USB
  drive by itself; `sudo gpu-agent setup --data-dir /mnt/usb` puts them there if you
  want. `gpu-agent check` names the disk it uses.
- Ubuntu, Debian or Armbian, for the automatic setup: it installs
  `qemu-system-arm qemu-utils qemu-efi-aarch64 cloud-image-utils cryptsetup-bin
  nftables iproute2 kmod` with apt-get (the same names on all three). On another
  distribution, install QEMU for aarch64, its UEFI firmware (AAVMF), cloud-image-utils,
  cryptsetup and nftables yourself; `gpu-agent check` says so.

**What a renter gets.** A rental's vCPUs run on one core type: the fastest. An RK3588
has four Cortex-A76 and four Cortex-A55 cores; the rental gets the four Cortex-A76
cores (pinned), and the board keeps the Cortex-A55 cores for itself — a VM whose
vCPUs moved between core types would see its CPU change under it. On a DGX Spark it
is the Cortex-X925 cores; on a server with one core type, all but one or two. The VM
gets the memory the board does not keep, and the disk. The agent reports exactly that
to the marketplace (`guest`: vCPUs, memory, disk and the core type), and names the
machine by its SoC ("Rockchip RK3588") and board ("Radxa ROCK 5B").

**Steps:** install the agent, link it (`sudo gpu-agent register --code <code>`), and
wait: the automatic setup installs the runtime, builds the rental image (no NVIDIA
driver) and runs a test rental. `sudo gpu-agent status` shows how far it is, and
anything it cannot fix by itself — a kernel without KVM, a too-small disk — with what
to do about it.

## Linked pairs: two DGX Sparks rented as one

Two NVIDIA DGX Sparks cabled to each other over their ConnectX-7 ports can be
rented together, as one pair, to one renter: each machine runs a microVM with
its GB10 **and its whole ConnectX-7 card**, and the two VMs reach each other
over the cable — 9000-byte frames, RDMA — for multi-node work. Each machine
keeps its own listing (and is rented on its own whenever the pair is not).

**What the host does**, on both Sparks:

1. Install the agent and link each Spark with its own code, as for any machine;
   the automatic setup makes each one ready on its own (its desktop closes for
   the test rental and comes back — save open work first).
2. Cable the two Sparks straight to each other — one or two QSFP cables, no
   switch. Leave the ConnectX-7 ports unconfigured on the host (see below).
3. Install the pair tools on both: `sudo apt-get install -y iproute2 ethtool mstflint`.
4. Run `sudo gpu-agent check --pair` on each: it says whether that Spark can
   be half of a pair, and lists everything to fix first.
5. Run `sudo gpu-agent check --boot --pair` on **both** machines, the second
   within 10 minutes of the first. They find each other on the cable, each boots
   a test rental with its GPU and its ConnectX card (the desktops close for it),
   the two VMs check every link carries 9000-byte frames and measure RDMA over
   it (each link must reach 100 Gb/s), and each machine records the result.
6. In the control panel, link the two machines as a pair and set the pair's
   price. The marketplace checks the cable itself before it links them, before
   every rental and every day while the pair is free.

**What both machines must be:**

- **NVIDIA DGX Sparks, confirmed by the agent:** Linux on Arm, an NVIDIA GB10,
  and DMI saying NVIDIA and DGX Spark. Other GB10 machines are not paired.
  `gpu-agent check --pair` prints what the machine reports about itself.
- **Each able to host a rental on its own** (everything above, including its own
  test rental), under the same account and in the same location.
- **Cabled port to port**, with nothing else on the cable. The marketplace's cable
  check refuses a cable that reaches a switch, a third machine or anything else.
- **ConnectX-7 ports left alone on the host:** no address or route on them, not
  in a bond, bridge or VLAN, not the machine's way to the internet or to the
  relay, not managed by NetworkManager (make them unmanaged) and not in netplan,
  nothing using their RDMA devices, SR-IOV off, and each function of the card in
  an IOMMU group of its own (or with the GPU's).
- **A base image with the RDMA tools**, which every image built by v0.2.0 has
  (an image from an older agent: `sudo gpu-agent runtime prepare`).
- **A passing pair test** (step 5) for the agent version running, over the ports
  still in the machine.

**On the cable, the host never gets an address.** The agent brings a port up
with IPv6 off and no address, exchanges raw Ethernet frames (ethertype 0x88B5),
and puts the port back exactly as it was — even if it is killed halfway (it keeps
a record and restores it at its next start). It does that for three things: a few
seconds of announcements at agent start and at 00:00, 06:00, 12:00 and 18:00 UTC,
so the two agents hear each other; the pair test; and the authenticated cable
check the marketplace asks for. None of it runs while a rental, a test rental or
the automatic setup is on the machine.

**During a pair rental** the whole card belongs to the VM: it leaves the host
right before the VM boots, after the GPU (and, on a Spark, after the desktop has
closed). Inside, each link is `cx7p0`, `cx7p1` with a `10.200.<i>.0/30` address;
the two VMs know each other by name (`gpu-<id>-a`, `gpu-<id>-b`, resolved over
the cable), share a key to log in to each other from the cable only, and
`/etc/gpu-pair.json` describes the links. A pair is delivered only when both
machines are rented **and** each VM has reached the other over every cable. When
the rental ends the card is given back and checked — every port back with its
MAC, the same firmware, the same persistent configuration, and no address put on
it by the host — and a machine where any of that does not verify is quarantined.

## What the agent does on your machine

It runs as root, because booting a microVM with a GPU passed through, loading
firewall rules and creating an encrypted disk all need it. Concretely:

- **Network: outbound only.** One SSH connection to the relay you picked (port 2222). Its key on the relay is `restrict`ed to two reverse forwards — no shell, no command, nothing else. Nothing is opened on your router.
- **Listens on loopback only.** `127.0.0.1:9101` is the control channel; `127.0.0.1:9100` is the legacy stats endpoint (only when `config.yaml` exists). Nothing on your LAN can reach either.
- **The control channel does eight things:** provision (which may wait for you to free the machine), teardown (which cancels a rental still waiting), status, health, update (to a newer release of this agent, from GitHub, verified — see above), withdrawn (stop hosting), and for [linked pairs](#linked-pairs-two-dgx-sparks-rented-as-one) the cable check and a pair rental's start. Every call but health needs the bearer token minted for this machine at register. There is no command execution, no file access, no shell, and no way for anyone at the marketplace to log in to your machine — nobody asks for, or gets, an account on it.
- **Hosting checks are local.** `check` reads `/dev/kvm`, `/sys/kernel/iommu_groups`, the PCI devices, which processes have a GPU open (`/proc/*/fd`), `nvidia-smi` where it is installed, DMI and your `PATH`, and reports the result. It changes nothing.
- **ConnectX ports** (a DGX Spark's, or any NVIDIA/Mellanox card's) are only ever brought up briefly, with no address and IPv6 off, for the announcements and checks described under linked pairs — and, while a pair rental waits for its machines, for 20 seconds at the start of each minute once this machine is free, to agree with the other machine on the start — and put back as they were.
- **Telling you on the machine:** while a rental waits for you, the agent runs `wall` and, for each person logged in to a desktop, `notify-send` as that person (`runuser`, their own session bus). Nothing else touches your sessions or your programs.
- **Files:** the binary (`/usr/local/bin/gpu-agent`), `/etc/gpu-agent` (the agent's SSH key, control token, registration and tunnel config, all `0600`, and `auto-setup` if you turned the automatic setup off), `/var/lib/gpu-agent` (the rental base image, the test-boot and pair-test results, the last setup attempt, the agents heard on the ConnectX ports, a rental waiting for you to free the machine (`pending.json`, with a pair rental's inter-machine key until it starts) and, while rented, the encrypted rental disk), and the service unit. The installer adds no users, kernel modules or drivers and installs only the OpenSSH client if it is missing. After linking, the automatic setup (or `gpu-agent runtime prepare --install-deps`) additionally installs QEMU, UEFI firmware, `cloud-image-utils`, `cryptsetup-bin`, `nftables`, `iproute2` and `kmod` with apt — nothing else; `sudo gpu-agent setup --off` before linking keeps it from doing so. While a rental runs, the agent also creates the `gpurent0` bridge, one nftables table, and (only if Docker or a firewall has set iptables' FORWARD policy to DROP) two accept rules for that bridge; all of them are removed when the rental ends.

## What a tenant can reach

Before a microVM boots, the agent loads one nftables table (`inet gpu_rental`)
and **reads it back from the kernel**; if the rules cannot be verified, nothing
boots. The microVM is attached only to the `gpurent0` bridge, and the table:

- drops every packet from the tenant to private and special ranges — `10/8`, `172.16/12`, `192.168/16`, `100.64/10`, `169.254/16`, `127/8`, multicast, `fc00::/7`, `fe80::/10`;
- drops every packet from the tenant to **every network configured on your machine**, whatever its addressing — your LAN and your NAS are blocked even if they are numbered from public space;
- drops every connection from the tenant to your machine itself, except DHCP and IPv6 neighbour discovery;
- drops every connection into the tenant from your network — the renter's SSH arrives through the agent's tunnel, not from your LAN.

`sudo gpu-agent check --rules` prints the exact table for your machine. The
tenant's disk is a per-rental encrypted overlay whose key only ever exists in
memory; at teardown it is destroyed and the GPU is reset and checked, and a
machine whose wipe or reset does not verify is quarantined instead of re-let.

## Removing the agent

```bash
sudo gpu-agent remove
```

This withdraws your listing and revokes the agent's access on the relay (while
the token that proves who it is still exists), stops and uninstalls the service,
deletes the rental firewall table and any rental disks, deletes `/etc/gpu-agent`
— the SSH key, token and registration — and deletes the binary. Your operating
system, drivers, packages and data are untouched; there is nothing to reinstall.
If the marketplace cannot be reached, the local removal still completes: the
agent's private key is gone, so the machine can never connect again, and the
command tells you the listing id to give support. On Windows, run it from an
Administrator PowerShell and delete `gpu-agent.exe` afterwards (Windows keeps a
running program locked).

`gpu-agent uninstall` is not the same thing: it only removes the service
definition and leaves the key, token and live listing behind.

## One machine, one listing

The agent registers the machine it runs on. Two machines — even two connected
to each other — are two agents and two listings. A
[linked pair](#linked-pairs-two-dgx-sparks-rented-as-one) of DGX Sparks is rented
as one on top of its two listings; each machine still runs its own agent. Running
`register` again on the same machine creates a second listing, so do not, unless
you mean to replace the first.

## Stats Endpoint

When running with a `config.yaml`, the agent exposes an HTTP endpoint on `127.0.0.1:9100` (loopback only — it answers without authentication, so it is never bound to your LAN):

- `GET /stats` — Returns system stats as JSON
- `GET /health` — Health check

Example response:
```json
{
  "hostname": "gpu-rig-01",
  "os": "linux",
  "arch": "amd64",
  "cpu": {
    "model": "AMD EPYC 7763",
    "cores": 64,
    "threads": 128,
    "usage_pct": 12.5
  },
  "memory": {
    "total_gb": 256,
    "available_gb": 240
  },
  "gpus": [
    {
      "model": "NVIDIA A100",
      "vram_total_gb": 80,
      "vram_used_gb": 2,
      "temp_c": 45,
      "utilization_pct": 0
    }
  ],
  "disk": {
    "total_gb": 2000,
    "free_gb": 1800
  },
  "status": "free",
  "uptime_seconds": 86400,
  "collected_at": "2026-04-07T12:00:00Z"
}
```

## Architecture

```
Provider (behind NAT)              Hub Servers (5 locations)
┌─────────────────┐        ┌─────────────────────────┐
│  gpu-agent (Go)  │        │  5 hub regions          │
│  reverse SSH     │◄──────►│  slot-routed relay      │
│  control :9101   │  SSH   │  forwarding-only        │
└─────────────────┘ tunnel  └────────┬────────────────┘
                                     │ pull /stats
                                     ▼
                            ┌─────────────────────────┐
                            │  Website GPU Listing     │
                            └─────────────────────────┘
```

- **Agent**: Single Go binary using [kardianos/service](https://github.com/kardianos/service) for cross-platform service management
- **Tunnel**: persistent reverse SSH tunnel (autossh-style), NAT-friendly (outbound only), relay host key pinned
- **Relay selection**: the control plane returns the account's relay list at register time; the agent TCP-probes them and prompts the provider to pick a location (closest preselected), then reports the choice back for slot allocation
- **Isolation**: each rental runs in a QEMU/KVM microVM (`-nodefaults`, seccomp sandbox, its own transient systemd unit) with the GPU's whole IOMMU group passed through via VFIO, behind a verified nftables fence with NAT to the internet only; its disk is dm-crypt with a key that never leaves memory; on turnover the key is discarded, the GPU reset, returned to its driver and checked (with `nvidia-smi` or `rocm-smi` as well, where installed and the GPU went back to that vendor's driver), failing closed. Every step is recorded so a crash or reboot is torn down exactly. A machine that fails its hosting checks or its test boot refuses rentals and reports why
- **Control path**: the control plane reaches an agent only through the relay manager's `/agent/<listing_id>/{health,status,provision,teardown}` proxy (plus `link/verify` and `pair/provision` for linked pairs, which an agent before v0.2.0 answers with 404), which forwards to that listing's own loopback slot and passes the bearer token through untouched
- **GPU detection**: any PCI GPU, from sysfs, named from the PCI ID database (nvidia-smi's names and rocm-smi's figures where installed); Apple Silicon (system_profiler)

## Building from Source

```bash
# Requires Go 1.22+
go build -o gpu-agent ./cmd/gpu-agent/

# Cross-compile for Linux
GOOS=linux GOARCH=amd64 go build -o gpu-agent-linux-amd64 ./cmd/gpu-agent/

# Cross-compile for macOS ARM
GOOS=darwin GOARCH=arm64 go build -o gpu-agent-darwin-arm64 ./cmd/gpu-agent/

# Releases stamp the version (the automatic setup, and the full test boot a version owes, are per version)
go build -ldflags "-s -w -X main.version=v0.2.3" -o gpu-agent ./cmd/gpu-agent/
```

## License

MIT
