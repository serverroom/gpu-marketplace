package update

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/update/updatetest"
)

// The fingerprints of keys/release-1.pem (in use) and keys/release-2.pem
// (standby), as RELEASING.md names them.
var pinnedFingerprints = [2]string{"61facfb726465177", "c6d99a91c4fa4ac1"}

const (
	releaseJSON = "https://github.com/serverroom/gpu-marketplace/releases/download/v0.1.11/release.json"
	binaryURL   = "https://github.com/serverroom/gpu-marketplace/releases/download/v0.1.11/gpu-agent-linux-amd64"
	checksums   = "https://github.com/serverroom/gpu-marketplace/releases/download/v0.1.11/checksums.txt"
)

func runTo(r *rig, version string) Record {
	rec, err := r.u.Begin(version)
	if err != nil {
		panic(err)
	}
	return r.u.Run(rec)
}

// THE RULE: a release without a signature is never installed, however good
// its checksums.txt -- that is the whole point.
func TestAnUnsignedReleaseIsRefusedWhateverItsChecksums(t *testing.T) {
	r := newRig(t)
	delete(r.files, releaseJSON)
	r.refused(t, runTo(r, "v0.1.11"), "a release without a valid signature is never installed")
}

// Whoever took over the GitHub release replaces the binary AND its checksum:
// the old agents' rule passes, the signed manifest does not.
func TestABinaryAndChecksumBothReplacedAreRefused(t *testing.T) {
	r := newRig(t)
	evil := []byte("#!evil root code")
	r.files[binaryURL] = evil
	r.files[checksums] = []byte(sum(evil) + "  gpu-agent-linux-amd64\n")
	r.refused(t, runTo(r, "v0.1.11"), "is not the signed release")
}

func TestAReleaseSignedByAnotherKeyIsRefused(t *testing.T) {
	r := newRig(t)
	r.files[releaseJSON] = updatetest.Sign(updatetest.Manifest("v0.1.11", map[string][]byte{"gpu-agent-linux-amd64": newBinary}), updatetest.Key(3))
	r.refused(t, runTo(r, "v0.1.11"), "not signed by a release key")
}

// A test key is not a release key: an updater with no keys set uses the ones
// compiled in, and those refuse what the test keys sign.
func TestTheCompiledInKeysAreTheDefault(t *testing.T) {
	r := newRig(t)
	r.u.Keys = nil
	r.refused(t, runTo(r, "v0.1.11"), "not signed by a release key")
}

// Key rotation: either pinned key will do.
func TestEitherPinnedKeyWillDo(t *testing.T) {
	r := newRig(t)
	r.files[releaseJSON] = updatetest.Sign(updatetest.Manifest("v0.1.11", map[string][]byte{"gpu-agent-linux-amd64": newBinary}), updatetest.Key(2))
	if rec := runTo(r, "v0.1.11"); rec.Status != StatusRestarting {
		t.Fatalf("Run = %+v", rec)
	}
	if read(t, r.u.Binary) != string(newBinary) {
		t.Error("the release signed by the second key was not installed")
	}
}

// A signature is over the manifest's exact bytes: one changed byte (a digest
// swapped for the attacker's) breaks it.
func TestATamperedManifestIsRefused(t *testing.T) {
	r := newRig(t)
	var env Envelope
	if err := json.Unmarshal(r.files[releaseJSON], &env); err != nil {
		t.Fatal(err)
	}
	m, _ := base64.StdEncoding.DecodeString(env.Manifest)
	evil := []byte("#!evil root code")
	forged := strings.Replace(string(m), sum(newBinary), sum(evil), 1)
	if forged == string(m) {
		t.Fatal("the digest was not in the manifest")
	}
	env.Manifest = base64.StdEncoding.EncodeToString([]byte(forged))
	r.files[releaseJSON], _ = json.Marshal(env)
	r.files[binaryURL] = evil
	r.refused(t, runTo(r, "v0.1.11"), "not signed by a release key")
}

// An old signed release served under a new version's name: the manifest says
// which version it is for.
func TestASignedManifestForAnotherVersionIsRefused(t *testing.T) {
	r := newRig(t)
	r.files[releaseJSON] = updatetest.Release("v0.1.9", map[string][]byte{"gpu-agent-linux-amd64": newBinary})
	r.refused(t, runTo(r, "v0.1.11"), "is for v0.1.9, not v0.1.11")
}

func TestASignedManifestForAnotherAgentIsRefused(t *testing.T) {
	r := newRig(t)
	m := updatetest.Manifest("v0.1.11", map[string][]byte{"gpu-agent-linux-amd64": newBinary})
	m["agent"] = "servcast-snapshots"
	r.files[releaseJSON] = updatetest.Sign(m, updatetest.Key(1))
	r.refused(t, runTo(r, "v0.1.11"), "not for this agent")
}

func TestABinaryOfAnotherSizeIsRefused(t *testing.T) {
	r := newRig(t)
	m := updatetest.Manifest("v0.1.11", map[string][]byte{"gpu-agent-linux-amd64": newBinary})
	e := updatetest.Entry(newBinary)
	e["bytes"] = len(newBinary) + 1
	m["files"] = map[string]interface{}{"gpu-agent-linux-amd64": e}
	r.files[releaseJSON] = updatetest.Sign(m, updatetest.Key(1))
	r.refused(t, runTo(r, "v0.1.11"), "the signed manifest says")
}

func TestBrokenReleaseFilesAreRefused(t *testing.T) {
	sig := base64.StdEncoding.EncodeToString(make([]byte, 384))
	good := updatetest.Release("v0.1.11", map[string][]byte{"gpu-agent-linux-amd64": newBinary})
	var env Envelope
	_ = json.Unmarshal(good, &env)
	noSig, _ := json.Marshal(Envelope{Manifest: env.Manifest})
	zeroSig, _ := json.Marshal(Envelope{Manifest: env.Manifest, Signature: sig})
	badB64, _ := json.Marshal(Envelope{Manifest: env.Manifest, Signature: "!!" + env.Signature})
	for name, c := range map[string]struct {
		raw []byte
		why string
	}{
		"empty":            {nil, "empty"},
		"not json":         {[]byte("<html>404</html>"), "not readable"},
		"no signature":     {noSig, "no signature"},
		"zero signature":   {zeroSig, "not signed by a release key"},
		"signature base64": {badB64, "no signature"},
		"too large":        {append(append([]byte{}, good...), make([]byte, MaxReleaseBytes)...), "too large"},
	} {
		r := newRig(t)
		r.files[releaseJSON] = c.raw
		if c.raw == nil {
			r.files[releaseJSON] = []byte{}
		}
		rec, _ := r.u.Begin("v0.1.11")
		rec = r.u.Run(rec)
		if rec.Status != StatusFailed || !strings.Contains(rec.Error, c.why) {
			t.Errorf("%s: Run = %+v, want %q", name, rec, c.why)
		}
		if read(t, r.u.Binary) != "the running v0.1.10" {
			t.Errorf("%s: the running binary was replaced", name)
		}
	}
}

func TestAManifestNamingAPathIsRefused(t *testing.T) {
	for _, name := range []string{"../gpu-agent-linux-amd64", "gpu-agent-linux-amd64/../../x", "https://evil/gpu-agent", "GPU-agent-linux-amd64"} {
		m := updatetest.Manifest("v0.1.11", map[string][]byte{name: newBinary})
		if _, err := OpenRelease(updatetest.Sign(m, updatetest.Key(1)), []*rsa.PublicKey{updatetest.Public(1)}); err == nil || !strings.Contains(err.Error(), "unusable file") {
			t.Errorf("%q: %v", name, err)
		}
	}
}

// A key shorter than 3072 bits signs nothing the agent accepts, pinned or not.
func TestAShortKeyIsRefused(t *testing.T) {
	short, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	raw := updatetest.Sign(updatetest.Manifest("v0.1.11", map[string][]byte{"gpu-agent-linux-amd64": newBinary}), short)
	if _, err := OpenRelease(raw, []*rsa.PublicKey{&short.PublicKey}); err == nil {
		t.Error("a release signed by a 2048-bit key was accepted")
	}
}

// The keys compiled in: two, 3072-bit RSA, public halves only, and the
// fingerprints the release runbook names.
func TestThePinnedKeys(t *testing.T) {
	keys, err := PinnedKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("%d pinned keys, want 2 (in use + standby)", len(keys))
	}
	for i, k := range keys {
		if k.N.BitLen() != 3072 || k.E != 65537 {
			t.Errorf("key %d: %d bits, e=%d", i+1, k.N.BitLen(), k.E)
		}
		t.Logf("pinned key %d fingerprint %s", i+1, KeyFingerprint(k))
	}
	if got := []string{KeyFingerprint(keys[0]), KeyFingerprint(keys[1])}; got[0] != pinnedFingerprints[0] || got[1] != pinnedFingerprints[1] {
		t.Errorf("pinned fingerprints %v, the runbook says %v", got, pinnedFingerprints)
	}
	_ = fs.WalkDir(keyFiles, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, _ := keyFiles.ReadFile(p)
		if strings.Contains(string(data), "PRIVATE") || strings.Count(string(data), "-----BEGIN") != 1 {
			t.Errorf("%s is not exactly one public key", p)
		}
		return nil
	})
}

// What scripts/release/sign-release.py writes (python + `openssl dgst -sha256
// -sign`, with a throwaway key whose public half is in testdata) is what the
// agent reads: the two implementations agree.
func TestAReleaseSignedByTheSigningScriptOpens(t *testing.T) {
	raw, err := os.ReadFile("testdata/openssl-signed-release.json")
	if err != nil {
		t.Fatal(err)
	}
	pubFile := func(name string) *rsa.PublicKey {
		data, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		k, err := ParsePublicKey(data)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	test, other := pubFile("openssl-test-key.pem"), pubFile("openssl-other-key.pem")
	m, err := OpenRelease(raw, []*rsa.PublicKey{other, test})
	if err != nil {
		t.Fatalf("OpenRelease = %v", err)
	}
	entry, err := m.For("v0.3.13", "gpu-agent-linux-amd64")
	if err != nil || len(m.Files) != 6 {
		t.Fatalf("For = %+v, %v (%d files)", entry, err, len(m.Files))
	}
	if err := entry.Matches([]byte("bin gpu-agent-linux-amd64\n")); err != nil {
		t.Errorf("the file the script signed does not match: %v", err)
	}
	if _, err := OpenRelease(raw, []*rsa.PublicKey{other}); err == nil {
		t.Error("the script's release opened with a key that did not sign it")
	}
	// And never with the keys compiled in: a test key is not a release key.
	pinned, _ := PinnedKeys()
	if _, err := OpenRelease(raw, pinned); err == nil {
		t.Error("a test-signed release opened with the pinned keys")
	}
}
