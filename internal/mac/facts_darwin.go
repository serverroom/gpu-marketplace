//go:build darwin

package mac

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// Facts reads this Mac and the agent's VM on it (provisioner.ReadMac).
func Facts() provisioner.MacFacts {
	var f provisioner.MacFacts
	f.Arch = runtime.GOARCH
	f.MacOS = parseSWVers(run("sw_vers", "-productVersion"))
	f.Name = "macOS " + strings.TrimSpace(run("sw_vers", "-productVersion"))
	f.HVF = hvfSupported(run("sysctl", "-n", "kern.hv_support"))
	if v, err := unix.SysctlUint64("hw.memsize"); err == nil {
		f.TotalMemMB = int(v >> 20)
	}
	f.CPUs = runtime.NumCPU()
	_, f.QEMU = qemuPath()
	_, f.Brew = brewPath()
	p := paths()
	f.StorageFreeGB = freeGB(p.dir)
	f.Ready = ready(p)
	if f.Ready {
		f.Machine = vmrt.ExecHost{Exec: Exec, Dial: Dial, Keep: Keep}
		f.Dial = Dial
	}
	return f
}

// MemAvailableMB is the memory macOS has free-ish now (free + inactive +
// speculative pages), from vm_stat.
func MemAvailableMB() int {
	out := run("vm_stat")
	if out == "" {
		return -1
	}
	pageSize := 4096
	var freePages int64
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimRight(v, ".")), 10, 64)
		switch {
		case strings.Contains(k, "page size of"):
			for _, w := range strings.Fields(k) {
				if p, err := strconv.Atoi(w); err == nil {
					pageSize = p
				}
			}
		case strings.HasPrefix(k, "Pages free"), strings.HasPrefix(k, "Pages inactive"), strings.HasPrefix(k, "Pages speculative"):
			freePages += n
		}
	}
	return int(freePages * int64(pageSize) >> 20)
}

// ready reports whether the agent's VM exists (its key is generated and its
// disk built); it may not be running yet.
func ready(p vmPaths) bool {
	_, err1 := os.Stat(p.keyPath())
	_, err2 := os.Stat(p.diskPath())
	return err1 == nil && err2 == nil
}

func run(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// qemuPath finds the QEMU for this Mac's architecture (Homebrew on Apple
// Silicon installs under /opt/homebrew, on Intel under /usr/local).
func qemuPath() (string, bool) {
	bin := qemuBinary(runtime.GOARCH)
	if p, err := exec.LookPath(bin); err == nil {
		return p, true
	}
	for _, d := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		if _, err := os.Stat(d + "/" + bin); err == nil {
			return d + "/" + bin, true
		}
	}
	return "", false
}

func brewPath() (string, bool) {
	if p, err := exec.LookPath("brew"); err == nil {
		return p, true
	}
	for _, d := range []string{"/opt/homebrew/bin/brew", "/usr/local/bin/brew"} {
		if _, err := os.Stat(d); err == nil {
			return d, true
		}
	}
	return "", false
}

func freeGB(dir string) int {
	d := dir
	for d != "" {
		if _, err := os.Stat(d); err == nil {
			break
		}
		parent := parentDir(d)
		if parent == d {
			break
		}
		d = parent
	}
	var st unix.Statfs_t
	if err := unix.Statfs(d, &st); err != nil {
		return 0
	}
	return int(uint64(st.Bavail) * uint64(st.Bsize) >> 30)
}

func parentDir(d string) string {
	i := strings.LastIndexByte(d, '/')
	if i <= 0 {
		return "/"
	}
	return d[:i]
}
