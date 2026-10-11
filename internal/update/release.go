package update

// Signed releases (from v0.3.13). A release is trusted only when its
// release.json is signed by one of the release keys compiled into this
// binary; checksums.txt, which sits in the same GitHub release as the
// binaries, vouches for nothing on its own -- whoever could replace a binary
// there could replace its checksum too.
//
// WHO CAN SIGN: whoever holds a release key's private half. They live on
// DEVEL61 only, root-only (0600) -- /root/.servcast/gpu-agent-release.pem
// (key 1, the one in use) and /root/.servcast/gpu-agent-release-standby.pem
// (key 2, for rotation) -- and are never in this repository, in CI or on
// GitHub. The design is the backup agent's (ServCast snapshots/release.py):
// RSA-3072, PKCS#1 v1.5 over SHA-256 of the manifest's exact bytes -- what
// `openssl dgst -sha256 -sign` makes, so signing needs nothing but openssl.
//
// release.json is ONE file, so nobody reads a new manifest beside an old
// signature:
//
//	{"manifest": <base64>, "signature": <base64>, "key": <fingerprint>}
//
// and the manifest it carries is:
//
//	{"agent": "gpu-agent", "version": "v0.3.13", "commit": "<sha>",
//	 "released_at": <epoch>,
//	 "files": {"gpu-agent-linux-amd64": {"sha256": "<hex>", "bytes": <n>}, ...}}
//
// `key` is for the humans reading the file; nothing trusts it -- every pinned
// key is tried. A manifest is good for its own version only, so an old signed
// release can never be served as a new one, and Check refuses anything not
// newer than the running agent.
//
// KEY ROTATION: two keys are pinned (keys/release-1.pem, keys/release-2.pem).
// To retire key 1: ship a release, signed with key 1, that pins key 2 and a
// new key 3; once the fleet runs it, sign with key 2. Changing the pinned keys
// any other way strands every installed agent on its current version until
// its host reinstalls it -- which is the point of pinning them.

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"path"
	"regexp"
	"sort"
	"sync"
)

// AgentName is what a manifest for this agent says it is for.
const AgentName = "gpu-agent"

// ReleaseFile is the signed release file, published beside the binaries.
const ReleaseFile = "release.json"

// MaxReleaseBytes is the most a release.json may be.
const MaxReleaseBytes = 64 << 10

// MinKeyBits is the smallest RSA key a release may be signed with.
const MinKeyBits = 3072

//go:embed keys/*.pem
var keyFiles embed.FS

var (
	pinnedOnce sync.Once
	pinned     []*rsa.PublicKey
	pinnedErr  error
)

// PinnedKeys are the release keys compiled into this binary.
func PinnedKeys() ([]*rsa.PublicKey, error) {
	pinnedOnce.Do(func() {
		entries, err := keyFiles.ReadDir("keys")
		if err != nil {
			pinnedErr = err
			return
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, name := range names {
			data, err := keyFiles.ReadFile(path.Join("keys", name))
			if err != nil {
				pinnedErr = err
				return
			}
			key, err := ParsePublicKey(data)
			if err != nil {
				pinnedErr = fmt.Errorf("pinned release key %s: %v", name, err)
				return
			}
			pinned = append(pinned, key)
		}
		if len(pinned) == 0 {
			pinnedErr = fmt.Errorf("no release key is pinned")
		}
	})
	return pinned, pinnedErr
}

// ParsePublicKey reads a PEM "PUBLIC KEY" (what `openssl pkey -pubout`
// writes) holding an RSA key of at least MinKeyBits.
func ParsePublicKey(data []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("not a PEM public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("not an RSA key")
	}
	if key.N.BitLen() < MinKeyBits {
		return nil, fmt.Errorf("an RSA key of %d bits; at least %d are needed", key.N.BitLen(), MinKeyBits)
	}
	return key, nil
}

// KeyFingerprint is the first 16 hex digits of SHA-256 over the key's modulus
// bytes -- the backup agent's fingerprint, for the humans reading a file.
func KeyFingerprint(key *rsa.PublicKey) string {
	sum := sha256.Sum256(key.N.Bytes())
	return hex.EncodeToString(sum[:])[:16]
}

// FileEntry is one file a manifest vouches for.
type FileEntry struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// Manifest is what a release's signature covers.
type Manifest struct {
	Agent      string               `json:"agent"`
	Version    string               `json:"version"`
	Commit     string               `json:"commit,omitempty"`
	ReleasedAt int64                `json:"released_at,omitempty"`
	Files      map[string]FileEntry `json:"files"`
}

// Envelope is release.json.
type Envelope struct {
	Manifest  string `json:"manifest"`
	Signature string `json:"signature"`
	Key       string `json:"key,omitempty"`
}

var (
	digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// What a file a manifest names may be called: a release asset, never a path.
	assetPattern = regexp.MustCompile(`^gpu-agent-[a-z0-9]+-[a-z0-9]+(\.exe)?$`)
)

// maxAssetBytes matches what the updater reads of a download.
const maxAssetBytes = 200 << 20

// VerifySignature reports which of keys signed data (RSA PKCS#1 v1.5 over
// SHA-256), or -1 when none did.
func VerifySignature(data, signature []byte, keys []*rsa.PublicKey) int {
	digest := sha256.Sum256(data)
	for i, key := range keys {
		if key == nil || key.N.BitLen() < MinKeyBits {
			continue
		}
		if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) == nil {
			return i
		}
	}
	return -1
}

// OpenRelease checks a release.json as the agent does before it trusts a byte
// of a release: the signature by one of keys over the manifest's exact bytes,
// the agent's name, the version's shape, and every file entry's shape.
// Whether it is the release asked for is the caller's to check (Manifest.For).
func OpenRelease(raw []byte, keys []*rsa.PublicKey) (*Manifest, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("the release file is empty")
	}
	if len(raw) > MaxReleaseBytes {
		return nil, fmt.Errorf("the release file is too large (%d bytes)", len(raw))
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("the release file is not readable")
	}
	manifest, err := base64.StdEncoding.Strict().DecodeString(env.Manifest)
	if err != nil || len(manifest) == 0 {
		return nil, fmt.Errorf("the release file carries no readable manifest")
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(env.Signature)
	if err != nil || len(signature) == 0 {
		return nil, fmt.Errorf("the release file carries no signature")
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no release key to check it with")
	}
	if VerifySignature(manifest, signature, keys) < 0 {
		return nil, fmt.Errorf("the release is not signed by a release key")
	}
	var m Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return nil, fmt.Errorf("the signed manifest is not readable")
	}
	if m.Agent != AgentName {
		return nil, fmt.Errorf("the signed manifest is not for this agent (%q)", m.Agent)
	}
	if _, ok := Parse(m.Version); !ok {
		return nil, fmt.Errorf("the signed manifest has no usable version (%q)", m.Version)
	}
	if len(m.Files) == 0 {
		return nil, fmt.Errorf("the signed manifest names no files")
	}
	for name, f := range m.Files {
		if !assetPattern.MatchString(name) {
			return nil, fmt.Errorf("the signed manifest names an unusable file %q", name)
		}
		if !digestPattern.MatchString(f.SHA256) {
			return nil, fmt.Errorf("the signed manifest has no digest for %s", name)
		}
		if f.Bytes <= 0 || f.Bytes > maxAssetBytes {
			return nil, fmt.Errorf("the signed manifest has no usable size for %s", name)
		}
	}
	return &m, nil
}

// For checks the manifest is release `version`'s and returns its entry for
// asset.
func (m *Manifest) For(version, asset string) (FileEntry, error) {
	if m.Version != version {
		return FileEntry{}, fmt.Errorf("the signed manifest is for %s, not %s", m.Version, version)
	}
	f, ok := m.Files[asset]
	if !ok {
		return FileEntry{}, fmt.Errorf("the signed manifest for %s names no %s", version, asset)
	}
	return f, nil
}

// Matches reports why data is not the file f vouches for, or nil.
func (f FileEntry) Matches(data []byte) error {
	if int64(len(data)) != f.Bytes {
		return fmt.Errorf("it is %d bytes, the signed manifest says %d", len(data), f.Bytes)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != f.SHA256 {
		return fmt.Errorf("its SHA-256 is %s, the signed manifest says %s", got, f.SHA256)
	}
	return nil
}
