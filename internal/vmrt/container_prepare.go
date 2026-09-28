package vmrt

import (
	"encoding/json"
	"fmt"
	"strings"
)

// EnsureContainerHost configures a host to run container rentals: it generates
// the NVIDIA CDI spec for the GPU (normalized to a spec version older podman
// accepts) and ensures the user-namespace id ranges --userns=auto needs. It is
// idempotent. It does NOT install packages -- that is InstallContainerPackages,
// behind --install-deps.
// gpu false (a machine renting its CPUs only) skips the NVIDIA CDI spec.
func EnsureContainerHost(h Host, gpu bool, log func(format string, args ...interface{})) error {
	if gpu {
		if err := generateContainerCDI(h, log); err != nil {
			return err
		}
	}
	return ensureContainersSubID(h, log)
}

// generateContainerCDI writes /etc/cdi/nvidia.json for the machine's GPUs, then
// normalizes it: podman before ~5.0 (Ubuntu 24.04 ships 4.9.3) cannot parse a
// CDI spec newer than 0.6.0, and rejects the whole spec on the 0.7.0-only
// `additionalGids` field. Pinning cdiVersion to 0.6.0 and stripping that field
// makes the spec load on old and new podman alike.
func generateContainerCDI(h Host, log func(format string, args ...interface{})) error {
	if _, err := h.LookPath("nvidia-ctk"); err != nil {
		return fmt.Errorf("nvidia-ctk (NVIDIA Container Toolkit) is not installed; run 'sudo gpu-agent runtime prepare --install-deps'")
	}
	_ = h.MkdirAll("/etc/cdi", 0755)
	const path = containerCDIPath
	if err := h.Run("nvidia-ctk", "cdi", "generate", "--format=json", "--output="+path); err != nil {
		return fmt.Errorf("generate CDI spec: %w", err)
	}
	data, err := h.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read CDI spec: %w", err)
	}
	var spec map[string]interface{}
	if err := json.Unmarshal(data, &spec); err != nil {
		return fmt.Errorf("parse CDI spec: %w", err)
	}
	spec["cdiVersion"] = "0.6.0"
	stripJSONKey(spec, "additionalGids")
	out, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("rewrite CDI spec: %w", err)
	}
	if err := h.WriteFile(path, out, 0644); err != nil {
		return fmt.Errorf("write CDI spec: %w", err)
	}
	// A stale YAML spec beside the JSON would be loaded too and could carry the
	// 0.7.0 fields; remove it.
	_ = h.Remove("/etc/cdi/nvidia.yaml")
	if log != nil {
		log("wrote %s (CDI 0.6.0)", path)
	}
	return nil
}

// containerCDIPath is the spec the agent writes and podman reads for the GPU.
const containerCDIPath = "/etc/cdi/nvidia.json"

// RefreshContainerCDI writes the GPU's CDI spec again and checks it: the spec
// names DRM device nodes by number (/dev/dri/card1), and a GB10's number is not
// stable across boots -- the firmware framebuffer takes card0 on some boots and
// not others. So it is written at every agent start and before every rental,
// not only at setup, and a spec whose nodes are not the NVIDIA GPU's is an error.
func RefreshContainerCDI(h Host, log func(format string, args ...interface{})) error {
	if err := generateContainerCDI(h, log); err != nil {
		return err
	}
	if problem := ContainerCDIProblem(h); problem != "" {
		return fmt.Errorf("%s", problem)
	}
	return nil
}

// ContainerCDIProblem is what is wrong with the GPU's CDI spec, or "": every
// /dev/dri node it hands a rental must exist and belong to an NVIDIA device.
// nvidia-cdi-refresh's own /var/run/cdi spec is left alone (podman before 5.0
// cannot read its 0.7.0 fields); the agent's spec is the one it checks.
func ContainerCDIProblem(h Host) string {
	data, err := h.ReadFile(containerCDIPath)
	if err != nil {
		return "the GPU's CDI spec (" + containerCDIPath + ") is missing; run 'sudo gpu-agent runtime prepare'"
	}
	var spec cdiSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return "the GPU's CDI spec (" + containerCDIPath + ") cannot be read (" + err.Error() + "); run 'sudo gpu-agent runtime prepare'"
	}
	nodes := spec.ContainerEdits.DeviceNodes
	for _, d := range spec.Devices {
		nodes = append(nodes, d.ContainerEdits.DeviceNodes...)
	}
	for _, n := range nodes {
		p := n.HostPath
		if p == "" {
			p = n.Path
		}
		if !strings.HasPrefix(p, "/dev/dri/") {
			continue
		}
		if !h.Exists(p) {
			return fmt.Sprintf("the GPU's CDI spec names %s, which does not exist on this boot, so a rental would not start", p)
		}
		vendor, verr := h.ReadFile("/sys/class/drm/" + strings.TrimPrefix(p, "/dev/dri/") + "/device/vendor")
		if verr != nil || !strings.EqualFold(strings.TrimSpace(string(vendor)), "0x10de") {
			return fmt.Sprintf("the GPU's CDI spec names %s, which is not the NVIDIA GPU's on this boot, so a rental would get another device", p)
		}
	}
	return ""
}

// cdiSpec is the part of a CDI spec ContainerCDIProblem reads.
type cdiSpec struct {
	ContainerEdits cdiEdits `json:"containerEdits"`
	Devices        []struct {
		ContainerEdits cdiEdits `json:"containerEdits"`
	} `json:"devices"`
}

type cdiEdits struct {
	DeviceNodes []struct {
		Path     string `json:"path"`
		HostPath string `json:"hostPath"`
	} `json:"deviceNodes"`
}

// stripJSONKey deletes key from every object in a decoded JSON tree.
func stripJSONKey(o interface{}, key string) {
	switch v := o.(type) {
	case map[string]interface{}:
		delete(v, key)
		for _, child := range v {
			stripJSONKey(child, key)
		}
	case []interface{}:
		for _, child := range v {
			stripJSONKey(child, key)
		}
	}
}

// ensureContainersSubID makes sure /etc/subuid and /etc/subgid grant a range to
// the "containers" user, which rootful `podman run --userns=auto` allocates
// each container's remapped ids from. The range is chosen not to overlap the
// usual 100000-based ranges. Idempotent: a host that already has one is left be.
func ensureContainersSubID(h Host, log func(format string, args ...interface{})) error {
	const entry = "containers:2000000000:1000000\n"
	for _, f := range []string{"/etc/subuid", "/etc/subgid"} {
		data, err := h.ReadFile(f)
		if err == nil && strings.Contains(string(data), "containers:") {
			continue
		}
		body := string(data)
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		if err := h.WriteFile(f, []byte(body+entry), 0644); err != nil {
			return fmt.Errorf("configure %s: %w", f, err)
		}
		if log != nil {
			log("added a 'containers' range to %s", f)
		}
	}
	return nil
}

// InstallContainerPackages installs the container runtime stack with apt:
// podman, and the NVIDIA Container Toolkit (adding its repo). Ubuntu/Debian
// only; on anything else the preflight tells the operator what to install.
func InstallContainerPackages(h Host, log func(format string, args ...interface{})) error {
	pkgs := []string{"podman", "cryptsetup", "nftables", "e2fsprogs", "iproute2", "util-linux", "curl", "gnupg"}
	if log != nil {
		log("installing the container runtime and tools (%s)", strings.Join(pkgs, ", "))
	}
	install := func() error {
		return h.Run("apt-get", append([]string{"install", "-y", "-q"}, pkgs...)...)
	}
	if err := install(); err != nil {
		_ = h.Run("apt-get", "update", "-q")
		if err := install(); err != nil {
			return fmt.Errorf("install container packages: %w", err)
		}
	}
	if _, err := h.LookPath("nvidia-ctk"); err != nil {
		if log != nil {
			log("adding the NVIDIA Container Toolkit repository")
		}
		const keyring = "/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg"
		if err := h.Run("bash", "-c", "curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | gpg --dearmor -o "+keyring); err != nil {
			return fmt.Errorf("add toolkit key: %w", err)
		}
		if err := h.Run("bash", "-c", "curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list | sed 's#deb https://#deb [signed-by="+keyring+"] https://#g' > /etc/apt/sources.list.d/nvidia-container-toolkit.list"); err != nil {
			return fmt.Errorf("add toolkit repo: %w", err)
		}
		if err := h.Run("apt-get", "update", "-q"); err != nil {
			return fmt.Errorf("apt-get update: %w", err)
		}
		if err := h.Run("apt-get", "install", "-y", "-q", "nvidia-container-toolkit"); err != nil {
			return fmt.Errorf("install nvidia-container-toolkit: %w", err)
		}
	}
	return nil
}
