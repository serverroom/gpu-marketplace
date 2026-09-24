package vmrt

import (
	"fmt"
	"path"
	"strings"
)

// ContainerImageRef is the base image a container rental runs -- the container
// analog of the golden disk. Bumped when the entrypoint below changes, so a
// stale image is rebuilt.
const ContainerImageRef = "localhost/gpu-agent-rental:5"

// containerBaseImage is what the rental image is built from. The NVIDIA driver
// and nvidia-smi come from the host at run time over CDI, so a plain Ubuntu
// base is enough; the renter installs whatever CUDA userspace they want on
// their encrypted home.
const containerBaseImage = "docker.io/library/ubuntu:24.04"

// containerEntrypoint runs as PID 1 in a rental container: it installs the
// renter's authorized_keys on the encrypted /home/renter volume, generates
// per-rental host keys on the /run tmpfs, and runs sshd. The self-test probe is
// NOT run here -- the host runs it with `podman exec` once the container and its
// network are up (see ContainerRuntime.SelfTest), which captures its output
// reliably instead of racing sshd for the container's stdout.
const containerEntrypoint = `#!/bin/bash
set -u
# The encrypted volume is mounted (idmap) owned by container-root; make it the
# renter's so they can write their own home.
chown renter:renter /home/renter 2>/dev/null || true
install -d -m 700 -o renter -g renter /home/renter/.ssh 2>/dev/null || true
if [ -f /run/renter/authorized_keys ]; then
  install -m 600 -o renter -g renter /run/renter/authorized_keys /home/renter/.ssh/authorized_keys
fi
mkdir -p /run/sshd /run/hostkeys
for t in rsa ecdsa ed25519; do
  [ -f /run/hostkeys/ssh_host_${t}_key ] || ssh-keygen -q -t "$t" -N "" -f /run/hostkeys/ssh_host_${t}_key </dev/null
done
exec /usr/sbin/sshd -D -e \
  -o AuthorizedKeysFile=/home/renter/.ssh/authorized_keys \
  -h /run/hostkeys/ssh_host_rsa_key \
  -h /run/hostkeys/ssh_host_ecdsa_key \
  -h /run/hostkeys/ssh_host_ed25519_key
`

// containerDockerfile builds the rental image: sshd, the renter user, and the
// entrypoint. Host-key generation and authorized_keys happen at run time (a
// read-only rootfs, per-rental keys), so nothing tenant-specific is baked in.
func containerDockerfile() string {
	return "FROM " + containerBaseImage + "\n" +
		"ENV DEBIAN_FRONTEND=noninteractive\n" +
		"RUN apt-get update && apt-get install -y --no-install-recommends " +
		"openssh-server ca-certificates iproute2 iputils-ping curl && " +
		"rm -rf /var/lib/apt/lists/* && " +
		// -p '*' unlocks the account (a bare useradd leaves the shadow field '!',
		// which sshd refuses even for pubkey auth) without setting a usable
		// password; login stays key-only.
		"useradd -m -s /bin/bash -p '*' renter && " +
		"printf 'PermitRootLogin no\\nPasswordAuthentication no\\nAllowUsers renter\\nX11Forwarding no\\nUsePAM no\\nLogLevel VERBOSE\\n' > /etc/ssh/sshd_config.d/gpu-agent.conf\n" +
		"COPY gpuagent-entrypoint /usr/local/sbin/gpuagent-entrypoint\n" +
		"RUN chmod 0755 /usr/local/sbin/gpuagent-entrypoint\n" +
		"ENTRYPOINT [\"/usr/local/sbin/gpuagent-entrypoint\"]\n"
}

// ContainerImagePresent reports whether the rental image has been built.
func ContainerImagePresent(h Host) bool {
	return h.Run("podman", "image", "exists", ContainerImageRef) == nil
}

// ContainerImageProblem is the reason the rental image cannot be used, or "".
func ContainerImageProblem(h Host) string {
	if _, err := h.LookPath("podman"); err != nil {
		return "podman is not installed, so a container rental cannot run; run 'sudo gpu-agent runtime prepare --install-deps'"
	}
	if !ContainerImagePresent(h) {
		return "the container rental image has not been built; run 'sudo gpu-agent runtime prepare'"
	}
	return ""
}

// PrepareContainerImage builds the rental image with podman from the embedded
// Dockerfile and entrypoint. The build context lives under the data dir.
func PrepareContainerImage(h Host, dataDir string, log func(format string, args ...interface{})) error {
	dir := path.Join(dataDir, "container-build")
	if err := h.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("build context: %w", err)
	}
	if err := h.WriteFile(path.Join(dir, "Dockerfile"), []byte(containerDockerfile()), 0600); err != nil {
		return fmt.Errorf("write Dockerfile: %w", err)
	}
	if err := h.WriteFile(path.Join(dir, "gpuagent-entrypoint"), []byte(containerEntrypoint), 0700); err != nil {
		return fmt.Errorf("write entrypoint: %w", err)
	}
	if log != nil {
		log("building the container rental image %s (this pulls %s the first time)", ContainerImageRef, containerBaseImage)
	}
	if err := h.Run("podman", "build", "-t", ContainerImageRef, dir); err != nil {
		return fmt.Errorf("build container image: %w", err)
	}
	return nil
}

// cdiDeviceRefs are the CDI references for a machine's rentable GPUs, from their
// nvidia-smi UUIDs ("nvidia.com/gpu=GPU-<uuid>"). Empty when nvidia-smi cannot
// be read; the preflight then reports the GPU is not usable in a container.
func CDIDeviceRefs(h Host) []string {
	out, err := h.Output("nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader")
	if err != nil {
		return nil
	}
	var refs []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		uuid := strings.TrimSpace(line)
		if strings.HasPrefix(uuid, "GPU-") || strings.HasPrefix(uuid, "MIG-") {
			refs = append(refs, "nvidia.com/gpu="+uuid)
		}
	}
	return refs
}
