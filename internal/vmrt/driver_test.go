package vmrt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

const (
	openVersionFile = "NVRM version: NVIDIA UNIX Open Kernel Module for aarch64  580.95.05  Release Build  (dvs-builder@U16-I1-N07-05-2)  Thu Sep 25 2025\n"
	propVersionFile = "NVRM version: NVIDIA UNIX x86_64 Kernel Module  580.82.07  Wed Aug 13 2025\n"
)

func TestChooseDriverMatchesTheHost(t *testing.T) {
	for name, c := range map[string]struct {
		smi, versionFile, license string
		want, source              string
		open                      bool
	}{
		// A DGX Spark ships NVIDIA's open kernel module.
		"open, from the version file": {"580.95.05\n", openVersionFile, "", "580-server-open", DriverFromHost, true},
		"open, from modinfo":          {"580.95.05\n", "", "Dual MIT/GPL\n", "580-server-open", DriverFromHost, true},
		// A CMP 170HX needed the proprietary one.
		"proprietary":           {"580.82.07\n580.82.07\n", propVersionFile, "NVIDIA\n", "580-server", DriverFromHost, false},
		"another branch":        {"570.172.08\n", propVersionFile, "NVIDIA\n", "570-server", DriverFromHost, false},
		"unreadable: default":   {"", openVersionFile, "", DefaultDriver, DriverDefault, false},
		"nonsense: default":     {"[N/A]\n", "", "", DefaultDriver, DriverDefault, false},
		"four digits: default":  {"1001.2.3\n", "", "", DefaultDriver, DriverDefault, false},
		"proprietary, no files": {"580.82.07\n", "", "", "580-server", DriverFromHost, false},
	} {
		h := fakehost.New()
		if c.smi != "" {
			h.Outputs["nvidia-smi --query-gpu=driver_version"] = c.smi
		}
		if c.versionFile != "" {
			h.Files[NVIDIAVersionFile] = []byte(c.versionFile)
		}
		if c.license != "" {
			h.Outputs["modinfo -F license nvidia"] = c.license
		}
		got := ChooseDriver(h)
		if got.Driver != c.want || got.Source != c.source || got.Open != c.open {
			t.Errorf("%s: ChooseDriver = %+v, want %s from %s (open=%v)", name, got, c.want, c.source, c.open)
		}
	}
}

func TestChooseDriverWhenNvidiaSmiFails(t *testing.T) {
	h := fakehost.New()
	h.Fail["nvidia-smi"] = errors.New("NVIDIA-SMI has failed")
	if got := ChooseDriver(h); got.Driver != DefaultDriver || got.Source != DriverDefault || got.Describe() != "unknown" {
		t.Errorf("ChooseDriver = %+v", got)
	}
}

func writeGolden(h *fakehost.Host, info GoldenInfo) {
	data, _ := json.Marshal(info)
	h.Files[testSpec().GoldenImage+".json"] = data
}

func TestGoldenDriverProblem(t *testing.T) {
	host := DriverChoice{Driver: "580-server", HostVersion: "580.82.07", Source: DriverFromHost}
	h := fakehost.New()

	writeGolden(h, GoldenInfo{Base: "x", Driver: "580-server-open"}) // an older agent's default
	if p := GoldenDriverProblem(h, testSpec(), host); !strings.Contains(p, "has NVIDIA driver 580-server-open, and this machine runs 580-server") {
		t.Errorf("a mismatched image passed: %q", p)
	}
	writeGolden(h, GoldenInfo{Base: "x", Driver: "580-server-open", DriverSource: DriverFromFlag})
	if p := GoldenDriverProblem(h, testSpec(), host); p != "" {
		t.Errorf("a driver a person chose was second-guessed: %q", p)
	}
	writeGolden(h, GoldenInfo{Base: "x", Driver: "580-server", DriverSource: DriverFromHost})
	if p := GoldenDriverProblem(h, testSpec(), host); p != "" {
		t.Errorf("a matching image was flagged: %q", p)
	}
	writeGolden(h, GoldenInfo{Base: "x", Driver: "570-server"})
	if p := GoldenDriverProblem(h, testSpec(), DriverChoice{Driver: DefaultDriver, Source: DriverDefault}); p != "" {
		t.Errorf("flagged against a host driver nobody could read: %q", p)
	}
}

// prepareHost is a machine on which Prepare can bake: a verified cloud image,
// and a bake VM that installs the driver and powers off at once.
func prepareHost(t *testing.T) *fakehost.Host {
	t.Helper()
	h := newHost()
	delete(h.OnRun, "systemd-run --unit=") // the bake VM installs the driver and powers off at once
	h.Files["/usr/share/OVMF/OVMF_CODE_4M.fd"] = []byte("code")
	base, name := cloudImageBase(), cloudImageName("amd64")
	img := []byte("ubuntu cloud image")
	h.Downloads[base+name] = img
	h.Downloads[base+"SHA256SUMS"] = []byte(sha(img) + " *" + name + "\n")
	h.OnRun["systemd-run --unit=gpu-rental-bake"] = func(h *fakehost.Host, _ string) {
		h.SetFile(NewRental(dataDir, BakeID).SerialLog, []byte("GPUAGENT-BAKE DONE\n"))
	}
	h.OnRun["qemu-img convert"] = func(h *fakehost.Host, cmd string) { h.SetFile(fakehost.LastField(cmd), []byte("golden")) }
	return h
}

func TestPrepareBakesTheHostsDriverAndRecordsTheChoice(t *testing.T) {
	h := prepareHost(t)
	h.Outputs["nvidia-smi --query-gpu=driver_version"] = "580.82.07\n"
	h.Files[NVIDIAVersionFile] = []byte(propVersionFile)
	if err := Prepare(h, testSpec(), &fakeFence{h: h}, "v0.1.10", PrepareOptions{}); err != nil {
		t.Fatalf("Prepare = %v", err)
	}
	if ud := h.Call("write " + NewRental(dataDir, BakeID).Dir + "/user-data"); !strings.Contains(ud, "nvidia-driver-580-server nvidia-utils-580-server") {
		t.Errorf("bake user-data does not install the host's driver: %s", ud)
	}
	info, err := LoadGoldenInfo(h, testSpec())
	if err != nil || info.Driver != "580-server" || info.DriverSource != DriverFromHost || !strings.Contains(info.HostDriver, "580.82.07, proprietary") {
		t.Errorf("GoldenInfo = %+v, %v", info, err)
	}
}

func TestPrepareWithAnExplicitDriverRecordsItAsChosen(t *testing.T) {
	h := prepareHost(t)
	h.Outputs["nvidia-smi --query-gpu=driver_version"] = "580.95.05\n"
	h.Files[NVIDIAVersionFile] = []byte(openVersionFile)
	if err := Prepare(h, testSpec(), &fakeFence{h: h}, "v0.1.10", PrepareOptions{Driver: "580-server"}); err != nil {
		t.Fatalf("Prepare = %v", err)
	}
	info, _ := LoadGoldenInfo(h, testSpec())
	if info.Driver != "580-server" || info.DriverSource != DriverFromFlag {
		t.Errorf("GoldenInfo = %+v", info)
	}
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
