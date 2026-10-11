// Package updatetest signs releases for tests with keys made on the spot.
// Nothing outside _test.go files imports it, so no test key reaches a build.
package updatetest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"sync"
)

var (
	mu   sync.Mutex
	keys = map[int]*rsa.PrivateKey{}
)

// Key is test key n (3072-bit RSA), made once per test binary.
func Key(n int) *rsa.PrivateKey {
	mu.Lock()
	defer mu.Unlock()
	if k, ok := keys[n]; ok {
		return k
	}
	k, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		panic(err)
	}
	keys[n] = k
	return k
}

// Public is test key n's public half.
func Public(n int) *rsa.PublicKey { return &Key(n).PublicKey }

// Entry is a manifest's entry for data.
func Entry(data []byte) map[string]interface{} {
	sum := sha256.Sum256(data)
	return map[string]interface{}{"sha256": hex.EncodeToString(sum[:]), "bytes": len(data)}
}

// Manifest is a gpu-agent manifest for version vouching for files.
func Manifest(version string, files map[string][]byte) map[string]interface{} {
	entries := map[string]interface{}{}
	for name, data := range files {
		entries[name] = Entry(data)
	}
	return map[string]interface{}{"agent": "gpu-agent", "version": version, "commit": "0123456789abcdef", "released_at": 1760000000, "files": entries}
}

// SignBytes is a release.json carrying manifest signed by key.
func SignBytes(manifest []byte, key *rsa.PrivateKey) []byte {
	digest := sha256.Sum256(manifest)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	out, _ := json.Marshal(map[string]string{
		"manifest":  base64.StdEncoding.EncodeToString(manifest),
		"signature": base64.StdEncoding.EncodeToString(sig),
		"key":       "test",
	})
	return out
}

// Sign is a release.json for manifest signed by key.
func Sign(manifest map[string]interface{}, key *rsa.PrivateKey) []byte {
	raw, err := json.Marshal(manifest)
	if err != nil {
		panic(err)
	}
	return SignBytes(raw, key)
}

// Release is a release.json for version vouching for files, signed by test key 1.
func Release(version string, files map[string][]byte) []byte {
	return Sign(Manifest(version, files), Key(1))
}
