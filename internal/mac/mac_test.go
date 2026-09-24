package mac

import (
	"strings"
	"testing"
)

func TestRootfsAndSums(t *testing.T) {
	if rootfsName("arm64") != "ubuntu-24.04-server-cloudimg-arm64.tar.gz" || rootfsName("amd64") != "ubuntu-24.04-server-cloudimg-amd64.tar.gz" {
		t.Fatal(rootfsName("arm64"), rootfsName("amd64"))
	}
	sums := "42fc6c6f3d5965e1c2e230704938a76e9d0d932b4ebb8ea5c2cb26a9992bd96f *ubuntu-24.04-server-cloudimg-amd64.tar.gz\n" +
		"82d61182744e8a4f3d388a1965208abb351daa47a689c49165bfb0c41471af35 *ubuntu-24.04-server-cloudimg-arm64.tar.gz\n"
	if got := sumFor(sums, rootfsName("arm64")); got != "82d61182744e8a4f3d388a1965208abb351daa47a689c49165bfb0c41471af35" {
		t.Errorf("arm64 sum = %q", got)
	}
	if sumFor(sums, "not-listed.tar.gz") != "" {
		t.Error("a file not listed has no sum")
	}
}

func TestVMMemory(t *testing.T) {
	if got := VMMemoryMB(65536, 58982); got != 60006 {
		t.Errorf("64 GB Mac: %d", got)
	}
	if got := VMMemoryMB(8192, 7000); got != 6144 {
		t.Errorf("capped at the machine less 2 GB: %d", got)
	}
}

// The VM boots the Mac's own architecture on Apple's hypervisor, with the
// rental disk, the cloud-init seed, one loopback SSH forward, and no window.
func TestQEMUArgs(t *testing.T) {
	for _, arch := range []string{"arm64", "amd64"} {
		o := VMConfig{Arch: arch, CPUs: 8, MemMB: 60006, Disk: "/d/disk.qcow2", Seed: "/d/seed.img",
			Firmware: "/d/uefi.fd", SerialLog: "/d/serial.log", SSHPort: GuestSSHPort}
		got := strings.Join(QEMUArgs(o), " ")
		for _, want := range []string{"-accel hvf", "-cpu host", "-smp 8", "-m 60006M",
			"hostfwd=tcp:127.0.0.1:52422-:22", "if=virtio,format=qcow2,file=/d/disk.qcow2", "-nographic"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: args miss %q\n  %s", arch, want, got)
			}
		}
		hasBIOS := strings.Contains(got, "-bios /d/uefi.fd")
		if arch == "arm64" && !hasBIOS {
			t.Errorf("arm64 needs UEFI firmware: %s", got)
		}
		if arch == "amd64" && hasBIOS {
			t.Errorf("amd64 uses its own SeaBIOS, not -bios: %s", got)
		}
	}
}

func TestSeed(t *testing.T) {
	ud := SeedUserData("ssh-ed25519 AAAAC3Nz key")
	for _, want := range []string{"#cloud-config", `- "ssh-ed25519 AAAAC3Nz key"`, "disable_root: false", "growpart:", "PasswordAuthentication no"} {
		if !strings.Contains(ud, want) {
			t.Errorf("user-data lacks %q:\n%s", want, ud)
		}
	}
	if !strings.Contains(SeedMetaData(), "instance-id: "+VMName) {
		t.Error("meta-data needs a stable instance id")
	}
}

func TestParseFacts(t *testing.T) {
	if parseSWVers("15.6.1\n") != 15 || parseSWVers("12") != 12 || parseSWVers("") != 0 {
		t.Errorf("sw_vers parse: %d %d %d", parseSWVers("15.6.1"), parseSWVers("12"), parseSWVers(""))
	}
	if !hvfSupported("1\n") || hvfSupported("0\n") {
		t.Error("hv_support")
	}
}
