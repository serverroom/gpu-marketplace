//go:build darwin

package mac

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Options size the agent's VM.
type Options struct {
	Arch  string
	MemMB int
	CPUs  int
	Log   func(format string, args ...interface{})
}

func (o Options) log(format string, a ...interface{}) {
	if o.Log != nil {
		o.Log(format, a...)
	}
}

// Setup makes this Mac ready to host, as far as macOS goes: QEMU (via
// Homebrew), the agent's VM key, the Ubuntu base image, the overlay disk, the
// cloud-init seed, the UEFI firmware on Apple Silicon, and the VM booted and
// reachable over ssh. The container stack inside is installed afterwards by the
// setup running apt-get in the VM (the same InstallContainerPackages a Linux
// host runs). Idempotent.
func Setup(ctx context.Context, o Options) error {
	p := paths()
	if err := os.MkdirAll(p.dir, 0700); err != nil {
		return err
	}
	if err := installQEMU(ctx, o); err != nil {
		return err
	}
	if err := ensureKey(p); err != nil {
		return err
	}
	if err := ensureFirmware(ctx, p, o); err != nil {
		return err
	}
	if err := ensureDisk(ctx, p, o); err != nil {
		return err
	}
	if err := ensureSeed(ctx, p); err != nil {
		return err
	}
	if err := Boot(ctx, o); err != nil {
		return err
	}
	return waitSSH(ctx, p, 3*time.Minute)
}

// installQEMU installs QEMU with Homebrew when it is missing. Homebrew itself
// is a person's to install (it needs the Xcode command-line tools and its own
// confirmation); detectMac reports that as a human step.
func installQEMU(ctx context.Context, o Options) error {
	if _, ok := qemuPath(); ok {
		return nil
	}
	brew, ok := brewPath()
	if !ok {
		return fmt.Errorf("QEMU is not installed and Homebrew is not present to install it; install Homebrew from https://brew.sh then run the setup again")
	}
	o.log("installing QEMU with Homebrew")
	cmd := exec.CommandContext(ctx, brew, "install", "qemu")
	cmd.Env = append(os.Environ(), "HOMEBREW_NO_AUTO_UPDATE=1", "NONINTERACTIVE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("brew install qemu: %w: %s", err, tail(string(out), 400))
	}
	if _, ok := qemuPath(); !ok {
		return fmt.Errorf("QEMU still not found after brew install")
	}
	return nil
}

// ensureKey generates the agent's ed25519 key to the VM (once).
func ensureKey(p vmPaths) error {
	if _, err := os.Stat(p.keyPath()); err == nil {
		return nil
	}
	cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "gpu-agent", "-f", p.keyPath())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("generate the VM key: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// pubkey reads the generated public key.
func pubkey(p vmPaths) (string, error) {
	b, err := os.ReadFile(p.keyPath() + ".pub")
	return strings.TrimSpace(string(b)), err
}

// ensureFirmware puts UEFI firmware beside the VM (Apple Silicon has no legacy
// BIOS; QEMU's edk2-aarch64 comes with the Homebrew qemu). Intel uses SeaBIOS.
func ensureFirmware(ctx context.Context, p vmPaths, o Options) error {
	if o.Arch != "arm64" {
		return nil
	}
	if _, err := os.Stat(p.firmwarePath()); err == nil {
		return nil
	}
	for _, src := range []string{
		"/opt/homebrew/share/qemu/edk2-aarch64-code.fd",
		"/usr/local/share/qemu/edk2-aarch64-code.fd",
	} {
		if data, err := os.ReadFile(src); err == nil {
			// A flat 64 MiB pflash image is what -bios wants.
			return os.WriteFile(p.firmwarePath(), data, 0600)
		}
	}
	return fmt.Errorf("UEFI firmware (edk2-aarch64-code.fd) not found beside QEMU")
}

// ensureDisk downloads the Ubuntu cloud image (checked against SHA256SUMS) and
// makes a qcow2 overlay on it for the VM to write to, sized for a rental.
func ensureDisk(ctx context.Context, p vmPaths, o Options) error {
	if _, err := os.Stat(p.diskPath()); err == nil {
		return nil
	}
	qemuImg, err := qemuImgPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.basePath()); err != nil {
		name := rootfsName(o.Arch)
		sums, err := fetch(ctx, rootfsBase+"SHA256SUMS")
		if err != nil {
			return fmt.Errorf("read the Ubuntu image checksums: %w", err)
		}
		sum := sumFor(sums, name)
		if sum == "" {
			return fmt.Errorf("the Ubuntu image %s is not in its SHA256SUMS", name)
		}
		o.log("downloading Ubuntu 24.04 for the VM (%s)", name)
		tarball := filepath.Join(p.dir, name)
		if err := download(ctx, rootfsBase+name, tarball, sum); err != nil {
			return err
		}
		defer os.Remove(tarball)
		o.log("unpacking the VM base image")
		if out, err := exec.CommandContext(ctx, "tar", "-xzf", tarball, "-C", p.dir).CombinedOutput(); err != nil {
			return fmt.Errorf("unpack %s: %w: %s", name, err, strings.TrimSpace(string(out)))
		}
		// The tarball holds one .img (the raw disk); rename it to base.img.
		imgs, _ := filepath.Glob(filepath.Join(p.dir, "*.img"))
		var raw string
		for _, m := range imgs {
			if m != p.basePath() && m != p.diskPath() && m != p.seedPath() {
				raw = m
			}
		}
		if raw == "" {
			return fmt.Errorf("no disk image inside %s", name)
		}
		if err := os.Rename(raw, p.basePath()); err != nil {
			return err
		}
	}
	// A qcow2 overlay: the base stays read-only and shared, the VM writes here.
	disk := 20 + o.diskGB()
	cmd := exec.CommandContext(ctx, qemuImg, "create", "-q", "-f", "qcow2", "-F", "raw", "-b", p.basePath(), p.diskPath(), fmt.Sprintf("%dG", disk))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create the VM disk: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (o Options) diskGB() int { return 40 } // the base plus headroom; the rental's own disk is a file inside

// ensureSeed builds the cloud-init NoCloud seed image (a FAT image labelled
// cidata holding user-data and meta-data), with hdiutil (built into macOS).
func ensureSeed(ctx context.Context, p vmPaths) error {
	if _, err := os.Stat(p.seedPath()); err == nil {
		return nil
	}
	key, err := pubkey(p)
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp(p.dir, "seed-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := os.WriteFile(filepath.Join(stage, "user-data"), []byte(SeedUserData(key)), 0600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, "meta-data"), []byte(SeedMetaData()), 0600); err != nil {
		return err
	}
	// A raw FAT image labelled cidata is what cloud-init's NoCloud reads.
	iso := filepath.Join(p.dir, "seed.iso")
	cmd := exec.CommandContext(ctx, "hdiutil", "makehybrid", "-iso", "-joliet", "-default-volume-name", "cidata", "-o", iso, stage)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build the cloud-init seed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return os.Rename(iso, p.seedPath())
}

// Boot starts the VM's QEMU if it is not already running, recording its pid.
func Boot(ctx context.Context, o Options) error {
	p := paths()
	if running(p) {
		return nil
	}
	qemu, ok := qemuPath()
	if !ok {
		return fmt.Errorf("QEMU is not installed")
	}
	cfg := VMConfig{
		Arch: o.Arch, CPUs: o.CPUs, MemMB: o.MemMB,
		Disk: p.diskPath(), Seed: p.seedPath(), Firmware: p.firmwarePath(),
		SerialLog: p.serialPath(), SSHPort: GuestSSHPort,
	}
	cmd := exec.Command(qemu, QEMUArgs(cfg)...)
	// Detach: the VM outlives the command that started it (the setup, a check).
	cmd.SysProcAttr = detached()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the VM: %w", err)
	}
	if err := os.WriteFile(p.pidPath(), []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	return nil
}

// Stop shuts the VM down (gpu-agent remove).
func Stop() {
	p := paths()
	data, err := os.ReadFile(p.pidPath())
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Kill()
		}
	}
	_ = os.Remove(p.pidPath())
}

// Remove stops the VM and deletes everything the agent made for it.
func Remove() error {
	Stop()
	return os.RemoveAll(vmDir())
}

// waitSSH waits until the VM answers ssh (cloud-init has installed the key).
func waitSSH(ctx context.Context, p vmPaths, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, _, code, err := Exec(nil, "true"); err == nil && code == 0 {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("the VM did not answer ssh within %s (its console log is %s)", timeout, p.serialPath())
}

func qemuImgPath() (string, error) {
	if p, err := exec.LookPath("qemu-img"); err == nil {
		return p, nil
	}
	for _, d := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		if _, err := os.Stat(d + "/qemu-img"); err == nil {
			return d + "/qemu-img", nil
		}
	}
	return "", fmt.Errorf("qemu-img not found")
}

func fetch(ctx context.Context, url string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), err
}

func download(ctx context.Context, url, path, sum string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := (&http.Client{Timeout: 60 * time.Minute}).Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("download %s: %w", url, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); sum != "" && got != sum {
		os.Remove(path)
		return fmt.Errorf("download %s: SHA-256 %s, published %s", url, got, sum)
	}
	return nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
