# v0.1.10: where the build decided differently from the brief

Everything here is a decision taken while building release-v0.1.10, with the reason.
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
