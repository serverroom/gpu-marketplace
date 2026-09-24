//go:build windows

package wsl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// ErrRestart: Windows must restart before WSL 2 can run.
var ErrRestart = errors.New("Windows must restart to finish installing WSL 2; restart it and the setup carries on by itself")

// Options size the distribution's VM.
type Options struct {
	Arch   string
	MemMB  int // the utility VM's memory (VMMemoryMB)
	CPUs   int
	Log    func(format string, args ...interface{})
	Stderr io.Writer
}

func (o Options) log(format string, a ...interface{}) {
	if o.Log != nil {
		o.Log(format, a...)
	}
}

// Setup makes this Windows machine ready to host in WSL 2, as far as Windows
// goes: the Windows features and WSL 2.x, the agent's account, its VM's
// configuration and the distribution, locked down. The container stack inside
// is the same setup a Linux host runs, through the distribution (autosetup).
// Idempotent; ErrRestart when Windows must restart first.
func Setup(ctx context.Context, o Options) error {
	f := Facts()
	if f.Server && f.Build < provisioner.MinWindowsServerBuild || !f.Server && f.Build < provisioner.MinWindowsClientBuild {
		return fmt.Errorf("this Windows (build %d) is too old for WSL 2 rentals", f.Build)
	}
	if f.RestartPending {
		return ErrRestart
	}
	restart, err := installPlatform(ctx, o)
	if err != nil {
		return err
	}
	if restart {
		markRestart()
		return ErrRestart
	}
	o.log("creating the Windows account %s that runs the rentals' WSL 2 environment", Account)
	if err := EnsureAccount(); err != nil {
		return err
	}
	s, err := openSession()
	if err != nil {
		return err
	}
	cfg := filepath.Join(s.home, ".wslconfig")
	if err := os.WriteFile(cfg, []byte(WSLConfig(o.MemMB, o.CPUs)), 0644); err != nil {
		return fmt.Errorf("write %s: %w", cfg, err)
	}
	if _, ok := s.registered(); !ok {
		if err := importDistro(ctx, s, o); err != nil {
			return err
		}
	}
	return configure(o)
}

// powershell runs a command in Windows PowerShell and returns its output.
func powershell(ctx context.Context, script string) (string, error) {
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// installPlatform turns on the Windows features WSL 2 needs and installs WSL
// 2.x from Microsoft's release, checking its signature. restart: Windows must
// restart before any of it works.
func installPlatform(ctx context.Context, o Options) (restart bool, err error) {
	for _, feature := range []string{"VirtualMachinePlatform", "Microsoft-Windows-Subsystem-Linux"} {
		state, err := powershell(ctx, "(Get-WindowsOptionalFeature -Online -FeatureName "+feature+").State")
		if err != nil {
			return false, fmt.Errorf("read the Windows feature %s: %v: %s", feature, err, state)
		}
		switch state {
		case "Enabled":
			continue
		case "EnablePending":
			restart = true
			continue
		}
		o.log("turning on the Windows feature %s", feature)
		out, err := powershell(ctx, "(Enable-WindowsOptionalFeature -Online -FeatureName "+feature+" -All -NoRestart -WarningAction SilentlyContinue).RestartNeeded")
		if err != nil {
			return false, fmt.Errorf("turn on the Windows feature %s: %v: %s", feature, err, out)
		}
		if strings.EqualFold(out, "True") {
			restart = true
		}
	}
	if exists(Exe()) {
		return restart, nil
	}
	arch := "x64"
	if o.Arch == "arm64" {
		arch = "arm64"
	}
	o.log("downloading WSL 2 from Microsoft's releases on GitHub")
	url, err := latestWSL(ctx, arch)
	if err != nil {
		return false, err
	}
	msi := filepath.Join(config.ConfigDir(), "wsl-install.msi")
	if err := download(ctx, url, msi, ""); err != nil {
		return false, err
	}
	defer os.Remove(msi)
	sig, err := powershell(ctx, "$s = Get-AuthenticodeSignature -FilePath '"+msi+"'; \"$($s.Status)|$($s.SignerCertificate.Subject)\"")
	if err != nil || !strings.HasPrefix(sig, "Valid|") || !strings.Contains(sig, "O=Microsoft Corporation") {
		return false, fmt.Errorf("the WSL installer from %s is not signed by Microsoft (%s); not installing it", url, sig)
	}
	o.log("installing WSL 2")
	cmd := exec.CommandContext(ctx, "msiexec.exe", "/i", msi, "/qn", "/norestart")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee) && (ee.ExitCode() == 3010 || ee.ExitCode() == 1641):
		restart = true
	default:
		return false, fmt.Errorf("install WSL 2: %w", err)
	}
	return restart, nil
}

// latestWSL is the download URL of the newest WSL 2 installer for arch.
func latestWSL(ctx context.Context, arch string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, wslReleases, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		return "", fmt.Errorf("find the WSL 2 release: %w", err)
	}
	defer resp.Body.Close()
	var rel struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", fmt.Errorf("find the WSL 2 release: %w", err)
	}
	for _, a := range rel.Assets {
		if strings.HasPrefix(a.Name, "wsl.") && strings.HasSuffix(a.Name, "."+arch+".msi") {
			return a.URL, nil
		}
	}
	return "", fmt.Errorf("the WSL 2 release has no installer for %s", arch)
}

// download fetches url to path; with sum, it must have that SHA-256.
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

// importDistro creates the distribution from Canonical's Ubuntu 24.04 WSL
// image, in a folder only the agent, the account and administrators can open.
func importDistro(ctx context.Context, s *session, o Options) error {
	dir := chooseDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	grant := "*" + s.sid + ":(OI)(CI)F"
	cmd := exec.CommandContext(ctx, "icacls.exe", dir, "/inheritance:r", "/grant:r", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F", grant)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("set the permissions of %s: %v: %s", dir, err, out)
	}
	name := rootfsName(o.Arch)
	sums, err := fetch(ctx, rootfsBase+"SHA256SUMS")
	if err != nil {
		return fmt.Errorf("read the Ubuntu image's checksums: %w", err)
	}
	sum := sumFor(sums, name)
	if sum == "" {
		return fmt.Errorf("the Ubuntu image %s is not in its SHA256SUMS", name)
	}
	tar := filepath.Join(dir, "rootfs.tar.gz")
	o.log("downloading Ubuntu 24.04 for WSL (%s)", name)
	if err := download(ctx, rootfsBase+name, tar, sum); err != nil {
		return err
	}
	defer os.Remove(tar)
	o.log("creating the WSL distribution %s in %s", Distro, dir)
	if out, err := manage("--import", Distro, dir, tar, "--version", "2"); err != nil {
		if strings.Contains(out, "HCS") || strings.Contains(strings.ToLower(out), "virtualization") {
			return fmt.Errorf("WSL 2 cannot start a VM on this machine: turn on virtualization (Intel VT-x or AMD-V) in the BIOS/UEFI settings. %s", out)
		}
		return err
	}
	return nil
}

// configure locks the distribution down (/etc/wsl.conf), restarts it so that
// takes effect, and installs what the agent needs in it beyond the container
// stack.
func configure(o Options) error {
	h := vmrt.WSLHost{Exec: Exec}
	if cur, err := h.ReadFile("/etc/wsl.conf"); err != nil || string(cur) != wslConf {
		if err := h.WriteFile("/etc/wsl.conf", []byte(wslConf), 0644); err != nil {
			return err
		}
		if _, err := manage("--terminate", Distro); err != nil {
			return err
		}
	}
	if err := h.MkdirAll("/etc/containers", 0755); err != nil {
		return err
	}
	if err := h.WriteFile("/etc/containers/containers.conf", []byte(containersConf), 0644); err != nil {
		return err
	}
	if _, err := h.LookPath("socat"); err != nil {
		o.log("installing socat and kmod in the distribution")
		_ = h.Run("apt-get", "update", "-q")
		if err := h.Run("env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", "-q", "socat", "kmod", "ca-certificates"); err != nil {
			return err
		}
	}
	return nil
}

// keepalive is the command line of the process that holds the distribution up.
const keepalive = "gpu-agent-keepalive infinity"

// Keep holds the distribution up: one long-lived wsl.exe session running a
// sleep named gpu-agent-keepalive, shared by every holder. The last release
// ends it -- and any an earlier agent process left behind.
func Keep() (release func()) {
	keepMu.Lock()
	defer keepMu.Unlock()
	if keepN++; keepN == 1 {
		if s, err := openSession(); err == nil {
			go s.run(nil, "-d", Distro, "-u", "root", "--exec", "/bin/bash", "-c", "exec -a gpu-agent-keepalive sleep infinity")
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			keepMu.Lock()
			defer keepMu.Unlock()
			if keepN--; keepN == 0 {
				_, _, _, _ = Exec(nil, "pkill", "-f", "-x", keepalive)
			}
		})
	}
}

var (
	keepMu sync.Mutex
	keepN  int
)

// Remove unregisters the distribution (deleting its disk), deletes the
// account and the agent's WSL files (gpu-agent remove). WSL 2 itself stays: it
// is part of Windows, and the person may use it.
func Remove() error {
	var errs []string
	if accountExists() {
		if s, err := openSession(); err == nil {
			if base, ok := s.registered(); ok {
				if _, err := manage("--unregister", Distro); err != nil {
					errs = append(errs, err.Error())
				} else {
					_ = os.RemoveAll(base)
				}
			}
			_ = os.Remove(filepath.Join(s.home, ".wslconfig"))
		}
		if err := RemoveAccount(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	_ = os.Remove(restartPath())
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
