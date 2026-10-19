package vmrt

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// Keys made with ssh-keygen (OpenSSH 10), and the fingerprints ssh-keygen -l
// printed for them: what a renter's ssh shows on the first connection.
const (
	testHostEd25519 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIWb+8kLhkXZihbzXPPshu/QxRvjiDppwdhxOMg9nZLC"
	testHostECDSA   = "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBB3YWkpDZUnZZtPVuQQylfBwU8vSeb9zBH93bKtSli4L7ozP15JEtGXQgOvsGm6Bh4bbrLDygHUVMpVNd/uMoHM="
	testHostRSA     = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQDeIEo0jiJY+kX3KnWaBiglYk+d23fMcG6kaGwRpOGS31s9ohLakWuAwBcGAw/5vGXiynA+A8Jx8LJQUPShaqlOrqcP69F8tbezCHvc1mu5yMwApjfaO8vpF/Yr2kdi+B7CzPW5Gc2rWNGsAzLigHaRv6bdx20qNqLQbwhqmOx5LXY+fNT2IZT34fWei4OnrY/+XvJa4aAGfq1YLj6RHTQu6Fuo7YFT0uEGqgbvO9HOod6GKpyeEUZ4lxGZpPbiJfz6uwOVr5E/FkInJw+1LMoMxdXafB6ehwSl5Bw00zTIUsV4E4iqXJ/koUSyTD3XY16iIlodmkPACKuG2/kR/4+5iuliwjTrRPZVxmRnv1vFoIMYe4/Pf8FTG5A0rR0oPOCFFD7u30nv2wnJhG1Vbag38EZX1l60jCyXD38e5F/WiMJjzPKu+zatzVxmPV99DDOzcZ2qKe7n7bpNpDMWOhVkwZUqQbc/mat/Erdx51AmiQJ7eFPbol3MhzdE6OOHfO8="
)

// What `cat` prints for the container's three .pub files: in file order
// (ed25519, ecdsa, rsa), each with the comment ssh-keygen gave it.
var testPubFiles = testHostEd25519 + " root@gpu-test\n" + testHostECDSA + " root@gpu-test\n" + testHostRSA + " root@gpu-test\n"

func TestHostKeysAreReadFromPubFilesInTheOrderSSHPrefersThem(t *testing.T) {
	// Out of order, with a blank line: ed25519 still comes first.
	got := ParseHostKeys(testHostRSA + " root@x\n\n" + testHostECDSA + " root@x\n" + testHostEd25519 + " root@x\n")
	want := []string{testHostEd25519, testHostECDSA, testHostRSA}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("keys = %q, want %q", got, want)
	}
}

func TestHostKeysAreReadFromKeyscan(t *testing.T) {
	out := "# 10.200.0.2:22 SSH-2.0-OpenSSH_9.6p1 Ubuntu-3ubuntu13.14\n" +
		"10.200.0.2 " + testHostRSA + "\n" +
		"# 10.200.0.2:22 SSH-2.0-OpenSSH_9.6p1 Ubuntu-3ubuntu13.14\n" +
		"10.200.0.2 " + testHostEd25519 + "\n" +
		"10.200.0.2 " + testHostEd25519 + "\n" +
		"10.200.0.2 " + testHostECDSA + "\n"
	got := ParseHostKeys(out)
	if len(got) != 3 || got[0] != testHostEd25519 || got[1] != testHostECDSA || got[2] != testHostRSA {
		t.Fatalf("keys = %q", got)
	}
}

func TestFingerprintIsWhatSSHShows(t *testing.T) {
	for key, want := range map[string]string{
		testHostEd25519: "SHA256:65Mdx0dgwPp1j+G8SIpjaalA5vq6bmw30VCFy9Asxo0",
		testHostECDSA:   "SHA256:kWurcX2Du4qQX3eB+BmrBXBqMlTl98DtN006MWTZhaI",
		testHostRSA:     "SHA256:tUrYupDZWFK/leXZqI1XTjjgpKUp/WQrUmj9v6Yh1kI",
	} {
		if got := HostKeyFingerprint(key); got != want {
			t.Errorf("%s: fingerprint %s, want %s", strings.Fields(key)[0], got, want)
		}
	}
}

// Nothing but a public host key comes out: a private key, a key whose data
// is not its type's, data with anything after the key, a renter's kind of key
// that no sshd serves as a host key, and text that only looks like a key.
func TestOnlyPublicHostKeysComeOut(t *testing.T) {
	ed := strings.Fields(testHostEd25519)[1]
	blob, _ := base64.StdEncoding.DecodeString(ed)
	trailing := base64.StdEncoding.EncodeToString(append(append([]byte{}, blob...), 0, 0, 0, 1, 'x'))
	short := base64.StdEncoding.EncodeToString(blob[:len(blob)-1])
	// The binary body of an OpenSSH private key file, as if it were a key.
	private := base64.StdEncoding.EncodeToString([]byte("openssh-key-v1\x00\x00\x00\x00\x04none\x00\x00\x00\x04none"))
	for name, text := range map[string]string{
		"private key file": "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n-----END OPENSSH PRIVATE KEY-----\n",
		"private body":     "ssh-ed25519 " + private,
		"wrong type":       "ssh-rsa " + ed,
		"curve mismatch":   "ecdsa-sha2-nistp384 " + strings.Fields(testHostECDSA)[1],
		"trailing data":    "ssh-ed25519 " + trailing,
		"cut short":        "ssh-ed25519 " + short,
		"security key":     "sk-ssh-ed25519@openssh.com " + ed,
		"not base64":       "ssh-ed25519 not*base64",
		"type only":        "ssh-ed25519",
		"key third":        "a b " + testHostEd25519,
	} {
		if got := ParseHostKeys(text); len(got) != 0 {
			t.Errorf("%s: got %q, want nothing", name, got)
		}
	}
}

func TestContainerRentalKeepsItsHostKeys(t *testing.T) {
	h := newContainerHost()
	h.Outputs["podman exec "+containerName("R1")+" cat"] = testPubFiles
	rt, _ := newContainerRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Read inside the container once its sshd answered, naming the .pub files
	// and nothing else, within a time limit.
	read := h.Call("run podman exec " + containerName("R1") + " cat")
	if read == "" {
		t.Fatal("the host keys were not read")
	}
	for _, f := range strings.Fields(read)[5:] {
		if !strings.HasPrefix(f, "/run/hostkeys/") || !strings.HasSuffix(f, ".pub") {
			t.Errorf("the read names %q, which is not a public key file", f)
		}
	}
	if h.Limits["podman exec "+containerName("R1")+" cat "+strings.Join(containerHostKeyFiles, " ")] == 0 {
		t.Error("the read has no time limit")
	}
	hk := rt.HostKeys()
	if hk == nil || hk.RentalID != "R1" || hk.Problem != "" || hk.At == 0 ||
		strings.Join(hk.Keys, "|") != testHostEd25519+"|"+testHostECDSA+"|"+testHostRSA {
		t.Fatalf("host keys = %+v", hk)
	}
	// Kept in the rental's state: an agent restarted mid-rental still has them.
	if st, _ := LoadState(h, dataDir); st == nil || st.HostKeys == nil || len(st.HostKeys.Keys) != 3 {
		t.Fatalf("state = %+v", st)
	}
	// Gone with the rental.
	if res := rt.Stop(); !res.Clean() {
		t.Fatalf("Stop: %+v", res)
	}
	if rt.HostKeys() != nil {
		t.Error("an ended rental still reports host keys")
	}
}

// A read that fails costs the rental nothing: it starts, and says why there
// is no key.
func TestContainerRentalStartsWhenItsHostKeysCannotBeRead(t *testing.T) {
	h := newContainerHost()
	h.Fail["podman exec "+containerName("R1")+" cat"] = errors.New("exit status 1: cat: No such file or directory")
	rt, _ := newContainerRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	hk := rt.HostKeys()
	if hk == nil || len(hk.Keys) != 0 || !strings.Contains(hk.Problem, "No such file") {
		t.Fatalf("host keys = %+v", hk)
	}
}

func TestVMRentalKeepsTheHostKeysItsSSHDOffers(t *testing.T) {
	h := newHost()
	h.Outputs["ssh-keyscan"] = "# " + GuestIP + ":22 SSH-2.0-OpenSSH_9.6p1\n" + GuestIP + " " + testHostECDSA + "\n" +
		GuestIP + " " + testHostEd25519 + "\n" + GuestIP + " " + testHostRSA + "\n"
	rt, _ := newRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if h.Call("run ssh-keyscan -T 5 -t ed25519,ecdsa,rsa "+GuestIP) == "" {
		t.Fatal("the guest was not asked for its host keys")
	}
	// Asked once the guest's SSH answered: after the boot.
	before(t, h, "run systemd-run", "run ssh-keyscan")
	hk := rt.HostKeys()
	if hk == nil || hk.RentalID != "R1" || len(hk.Keys) != 3 || hk.Keys[0] != testHostEd25519 {
		t.Fatalf("host keys = %+v", hk)
	}
}

func TestVMRentalWithoutKeyscanSaysSo(t *testing.T) {
	h := newHost()
	h.Tools = map[string]bool{"ssh-keyscan": false}
	rt, _ := newRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: "R1", Pubkey: key(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	hk := rt.HostKeys()
	if hk == nil || len(hk.Keys) != 0 || !strings.Contains(hk.Problem, "ssh-keyscan is not installed") {
		t.Fatalf("host keys = %+v", hk)
	}
}

// A test boot's VM reports to its serial console and is never waited for:
// nothing is read from it.
func TestTestBootVMIsNotAskedForHostKeys(t *testing.T) {
	h := newHost()
	rt, _ := newRuntime(h, nil)
	if err := rt.Start(StartOptions{ID: SelfTestPrefix + "1", Pubkey: key(t), Probes: []string{}, NoWait: true}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if h.Call("run ssh-keyscan") != "" {
		t.Error("a test boot was asked for its host keys")
	}
}
