package vmrt

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"sort"
	"strings"
	"time"
)

// A rental's own SSH host keys, so a renter can check the machine before
// their first connection. Every rental makes new ones (a container's
// entrypoint in /run/hostkeys, a microVM's cloud-init on first boot), so they
// are read once the rental's sshd answers, before the agent opens the
// renter's way in, kept in the rental's state and reported on /status.
//
// Only PUBLIC halves are ever read: a container is asked for the .pub files
// by name, a microVM is asked by the SSH protocol itself (ssh-keyscan), which
// carries nothing else. ParseHostKeys keeps only a line that is a public key
// of a host key type, whose data has exactly that key's shape, so nothing
// else -- a private key above all -- can come out of it.

// HostKeys are a rental's SSH host keys as read when it started: each
// "type base64" (no comment), ed25519 first, then ecdsa, then rsa. Problem
// says why none could be read.
type HostKeys struct {
	RentalID string   `json:"rental_id"`
	Keys     []string `json:"keys,omitempty"`
	At       int64    `json:"at"`
	Problem  string   `json:"problem,omitempty"`
}

// hostKeyTypes is the order a renter's ssh prefers a server's host keys in,
// which is the order they are reported in.
var hostKeyTypes = []string{"ssh-ed25519", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521", "ssh-rsa"}

// containerHostKeyFiles are the public halves of the keys a rental
// container's sshd is started with (containerEntrypoint's -h), and the only
// files the agent reads there.
var containerHostKeyFiles = []string{
	"/run/hostkeys/ssh_host_ed25519_key.pub",
	"/run/hostkeys/ssh_host_ecdsa_key.pub",
	"/run/hostkeys/ssh_host_rsa_key.pub",
}

// hostKeyReadLimit bounds a read: it never holds a rental up for longer.
const hostKeyReadLimit = 20 * time.Second

// maxHostKeys is the most keys reported; an sshd offers three.
const maxHostKeys = 5

func hostKeyRank(t string) int {
	for i, k := range hostKeyTypes {
		if k == t {
			return i
		}
	}
	return -1
}

// ParseHostKeys returns the public host keys in text: a .pub file's lines
// ("type base64 comment") or ssh-keyscan's ("host type base64"). Each comes
// back as "type base64", ed25519 first, each once.
func ParseHostKeys(text string) []string {
	var keys []string
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		for i := 0; i+1 < len(fields) && i < 2; i++ {
			if hostKeyRank(fields[i]) < 0 {
				continue
			}
			if key, ok := publicHostKey(fields[i], fields[i+1]); ok && !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
			break
		}
	}
	sort.SliceStable(keys, func(a, b int) bool {
		return hostKeyRank(strings.Fields(keys[a])[0]) < hostKeyRank(strings.Fields(keys[b])[0])
	})
	if len(keys) > maxHostKeys {
		keys = keys[:maxHostKeys]
	}
	return keys
}

// publicHostKey is "type base64" when data is exactly a public key of that
// type in SSH's wire form (RFC 4253 6.6, RFC 5656 3.1, RFC 8709 4): the type,
// then an ed25519 key's 32 bytes, an ecdsa key's curve and point, or an rsa
// key's exponent and modulus, and nothing after them.
func publicHostKey(keyType, data string) (string, bool) {
	blob, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(blob) > 4096 {
		return "", false
	}
	parts, ok := sshStrings(blob)
	if !ok || len(parts) == 0 || string(parts[0]) != keyType {
		return "", false
	}
	switch {
	case keyType == "ssh-ed25519":
		ok = len(parts) == 2 && len(parts[1]) == 32
	case strings.HasPrefix(keyType, "ecdsa-sha2-"):
		ok = len(parts) == 3 && string(parts[1]) == strings.TrimPrefix(keyType, "ecdsa-sha2-") &&
			len(parts[2]) > 0 && parts[2][0] == 4
	case keyType == "ssh-rsa":
		ok = len(parts) == 3 && len(parts[1]) > 0 && len(parts[2]) >= 128
	default:
		ok = false
	}
	if !ok {
		return "", false
	}
	return keyType + " " + data, true
}

// sshStrings splits blob into its length-prefixed strings; false when it is
// not made of them exactly.
func sshStrings(blob []byte) ([][]byte, bool) {
	var parts [][]byte
	for len(blob) > 0 {
		if len(blob) < 4 {
			return nil, false
		}
		n := binary.BigEndian.Uint32(blob)
		if uint64(n) > uint64(len(blob)-4) {
			return nil, false
		}
		parts = append(parts, blob[4:4+n])
		blob = blob[4+n:]
	}
	return parts, true
}

// HostKeyFingerprint is what ssh shows for a key ("SHA256:..."), or "" for a
// line that is not a key.
func HostKeyFingerprint(key string) string {
	fields := strings.Fields(key)
	if len(fields) < 2 {
		return ""
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// readHostKeys is the container's host keys: its .pub files, read inside it.
func (rt *ContainerRuntime) readHostKeys(name string) ([]string, error) {
	out, err := rt.h.RunLimited(hostKeyReadLimit, nil, nil, "podman",
		append([]string{"exec", name, "cat"}, containerHostKeyFiles...)...)
	return foundHostKeys(out, err)
}

// readHostKeys is the microVM's host keys, as its sshd offers them.
func (rt *Runtime) readHostKeys() ([]string, error) {
	if _, err := rt.h.LookPath("ssh-keyscan"); err != nil {
		return nil, errors.New("ssh-keyscan is not installed (it comes with the OpenSSH client)")
	}
	out, err := rt.h.RunLimited(hostKeyReadLimit, nil, nil, "ssh-keyscan", "-T", "5", "-t", "ed25519,ecdsa,rsa", GuestIP)
	return foundHostKeys(out, err)
}

// foundHostKeys is what a read found. A command that also failed (one file
// missing) still counts for the keys it printed.
func foundHostKeys(out string, err error) ([]string, error) {
	if keys := ParseHostKeys(out); len(keys) > 0 {
		return keys, nil
	}
	if err == nil {
		err = errors.New("no public host key was found")
	}
	return nil, err
}

// recordHostKeys reads the rental's host keys and keeps them in its state. A
// read that fails costs the rental nothing (the renter can still log in, they
// are only not shown the key); its reason is kept instead.
func recordHostKeys(st *State, save func() error, read func() ([]string, error)) {
	hk := &HostKeys{RentalID: st.RentalID, At: time.Now().Unix()}
	keys, err := read()
	if err != nil {
		hk.Problem = err.Error()
		if len(hk.Problem) > 300 {
			hk.Problem = hk.Problem[:300]
		}
	}
	hk.Keys = keys
	st.HostKeys = hk
	_ = save()
}

// hostKeysOf is the running rental's host keys from its state, or nil.
func hostKeysOf(h Host, dataDir string) *HostKeys {
	st, err := LoadState(h, dataDir)
	if err != nil || st == nil || st.Dirty || st.HostKeys == nil {
		return nil
	}
	hk := *st.HostKeys
	hk.Keys = append([]string(nil), hk.Keys...)
	return &hk
}

// HostKeys are the running container rental's SSH host keys, or nil.
func (rt *ContainerRuntime) HostKeys() *HostKeys { return hostKeysOf(rt.h, rt.spec.DataDir) }

// HostKeys are the running microVM rental's SSH host keys, or nil.
func (rt *Runtime) HostKeys() *HostKeys { return hostKeysOf(rt.h, rt.spec.DataDir) }
