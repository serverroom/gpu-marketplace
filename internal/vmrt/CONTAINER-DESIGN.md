# Hardened container rental mode — design

Interim GPU rental path for machines whose GPU **cannot be passed through over VFIO** (the DGX Spark GB10,
until NVIDIA ships GB10 in the signed `nvgrace-gpu-vfio-pci`). Sits beside the QEMU/VFIO microVM as a second
`Machine`. Owner decisions (2026-09-19): **rootful podman + userns remap**, **VFIO-fallback only**, validate
on internal box **SID 2457** then a real Spark.

## Why this is a small, safe addition
`provisioner.Machine` is `Start(vmrt.StartOptions) / Stop() vmrt.StopResult / Alive() / Present() / Dirty()`.
`*vmrt.Runtime` (QEMU) is one implementation; the container runtime is another. **ServCast needs no change** —
nothing branches on `capability.kind`; delivery/billing/relay/watcher key off `status`+`ready`. The renter
path is unchanged because the container sits on the **same netguard bridge at the same `GuestIP` (10.254.254.2)**
with an sshd on :22, so `provisioner.start`'s `startForward(SSHListen, GuestIP:22)` works verbatim.

## Reuse (unchanged)
- **Encrypted disk** — `CreateDisk`/`DestroyDisk` dm-crypt. Container variant: same loop+cryptsetup, then
  `mkfs.ext4` + mount instead of writing a golden qcow2; mounted into the container as the renter's writable
  space. Teardown closes the mapping (key gone = wipe) after unmount. Reuses `DestroyDisk`.
- **Fence** — `netguard` bridge fence, `fence.Apply()/Remove()`, applied first, removed last. Identical.
- **Network** — the netguard bridge, `HostIP`/`GuestIP` /30, `forwardRules` (DOCKER-USER/FORWARD accepts),
  ip_forward. Container gets a **veth** into its netns at `GuestIP` (not a tap). `NetState` gains `VethHost`.
- **Renter tunnel** — `provisioner.startForward` → `GuestIP:22`, unchanged.
- **Self-test verdict engine** — `ParseSerial`/`Evaluate`/`SelfTestResult`; container reports over `podman
  logs` stdout instead of a serial console (same marker lines from `selfTestScript`).
- **State machine + fail-closed teardown** — `State` (+ `ContainerID`, `VolumeMount`), `SaveState`/`LoadState`,
  `StopResult.Clean()`.
- **Billing/heartbeat/status/control** — entirely unchanged.

## Container-specific (new)
- **Kind/vendor** — `KindContainer = "container-nv"`. New `GPUVendor` `VendorContainerNV` that `CanHost()` but
  is not VFIO (`CanIsolate()` stays VFIO-only). Chosen in `Detect` only when: NVIDIA GPU present, VFIO
  passthrough impossible for it (IOMMU group has a `direct`/RMR reserved region the signed nvgrace can't take,
  i.e. the GB10 case), AND the container stack is present. Every VFIO-capable host stays `qemu-vfio`.
- **Run core** — rootful `podman run` (daemonless) with: `--userns=auto` (container-root ≠ host-root),
  `--security-opt=no-new-privileges`, `--cap-drop=ALL`, default seccomp + apparmor, `--read-only` rootfs +
  tmpfs for /tmp,/run, the encrypted volume mounted rw at the renter's home, **no host bind-mounts, no host
  net**, `--network=none` (veth wired in after), GPU via **CDI** (`--device nvidia.com/gpu=<uuid>`, generated
  with `nvidia-ctk cdi generate`) — never `--privileged`, never all GPUs. Resource caps: `--cpus`, `--memory`
  to the guest sizing. Runs an entrypoint that creates user `renter`, installs the renter pubkey, starts sshd.
- **GPU handover** — none. The GPU stays on the host `nvidia` driver, shared read-only into the container via
  CDI. Before a rental the host desktop is closed (reuse `CloseDesktop`, DGX Spark on-demand) and NVIDIA host
  users are stopped so the renter has the GPU to themselves; restored on teardown.
- **Clean-verify** — after `podman rm`, the GPU has no renter compute apps and VRAM/pool is clear
  (reuse `nvidiaClear` over the GPU BDFs). No driver rebind to verify (GPU never left the host).
- **Preflight** — drop KVM/IOMMU/VFIO/firmware/QEMU/golden checks. Require: podman (rootful) + userns
  subuid/subgid, `nvidia-container-toolkit` + a generated CDI spec listing the GPU, `nvidia-smi` sees the GPU,
  the base container image present, disk/cryptsetup/nft tooling, memory/disk floors. Gate on a passing
  container self-test for this agent version + GPU (same `SelfTestProblem` model).

## Files
- `internal/vmrt/container.go` — `ContainerRuntime` (Start/Stop/Alive/Present/Dirty + SelfTest), the veth
  wiring, the encrypted-volume helper, the podman argv builder, the entrypoint/probe.
- `internal/vmrt/container_image.go` — prepare/pull + verify the base container image (the container analog of
  the golden image).
- `internal/provisioner/container_preflight.go` — container preflight + the VFIO-impossible detection + kind.
- `internal/provisioner/preflight.go` / `provisioner.go` — `Detect` chooses the runtime; `KindContainer`;
  `VendorContainerNV`.
- Tests: `container_test.go` (fakehost) — argv hardening flags present, veth+fence+disk order, teardown wipes +
  GPU-clean gate, selftest verdict, and that a VFIO-capable host is untouched (still `qemu-vfio`).

## Validate
`go build ./... && go vet ./... && go test ./...` + cross-builds. Then force container mode on SID 2457
(x86 + CMP 170HX) for a real provision→ssh→fence-check→teardown→wipe E2E, then a real DGX Spark.

## Non-goals (v1)
Multi-tenant on one GPU (single rental per machine, as today). MIG/vGPU. Windows/macOS. Panel copy that
labels the listing "container-isolated" is a later, optional ServCast tweak (honors the no-over-promising rule).
</content>
