// release-check is the release runbook's checker (RELEASING.md); it is not
// shipped. It checks a release exactly as an installed agent does -- with the
// release keys compiled into the agent (internal/update) -- before anyone is
// offered it.
//
//	release-check compare -version vX.Y.Z -local DIR
//	    GitHub's binaries and checksums.txt for the version are byte for byte
//	    the ones in DIR (our own build, scripts/release/build.sh).
//	release-check check -version vX.Y.Z [-release FILE] [-local DIR] [-offline]
//	    release.json (FILE, or the one on GitHub) is signed by a pinned key,
//	    is for this version, names every platform, and every binary -- in DIR
//	    and, unless -offline, on GitHub, with its checksums.txt -- is the one
//	    it vouches for.
//
// Exit status 0 only when everything checked out.
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/update"
)

// Assets are the binaries every release carries (.github/workflows/release.yml).
var Assets = []string{
	"gpu-agent-linux-amd64", "gpu-agent-linux-arm64",
	"gpu-agent-windows-amd64.exe", "gpu-agent-windows-arm64.exe",
	"gpu-agent-darwin-amd64", "gpu-agent-darwin-arm64",
}

func releaseURL(version, file string) string {
	return "https://github.com/" + update.Repo + "/releases/download/" + version + "/" + file
}

func get(url string) ([]byte, error) {
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// sums reads a checksums.txt.
func sums(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			out[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	return out
}

type checker struct{ problems int }

func (c *checker) ok(format string, a ...interface{}) { fmt.Printf("  ok    "+format+"\n", a...) }
func (c *checker) bad(format string, a ...interface{}) {
	c.problems++
	fmt.Printf("  FAIL  "+format+"\n", a...)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	version := fs.String("version", "", "release version, vX.Y.Z")
	local := fs.String("local", "", "directory holding our own build of the release")
	releaseFile := fs.String("release", "", "a local release.json (default: the one on GitHub)")
	offline := fs.Bool("offline", false, "check only local files; fetch nothing from GitHub")
	_ = fs.Parse(os.Args[2:])
	if _, ok := update.Parse(*version); !ok {
		fmt.Fprintf(os.Stderr, "-version %q is not a release version (vX.Y.Z)\n", *version)
		os.Exit(2)
	}
	c := &checker{}
	switch os.Args[1] {
	case "compare":
		if *local == "" {
			usage()
		}
		c.compare(*version, *local)
	case "check":
		c.check(*version, *releaseFile, *local, *offline)
	default:
		usage()
	}
	if c.problems > 0 {
		fmt.Printf("%d problem(s): NOT good to go\n", c.problems)
		os.Exit(1)
	}
	fmt.Println("all good")
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: release-check compare -version vX.Y.Z -local DIR")
	fmt.Fprintln(os.Stderr, "       release-check check -version vX.Y.Z [-release FILE] [-local DIR] [-offline]")
	os.Exit(2)
}

func (c *checker) compare(version, dir string) {
	fmt.Printf("GitHub's %s against our build in %s:\n", version, dir)
	published := map[string]string{}
	if raw, err := get(releaseURL(version, "checksums.txt")); err != nil {
		c.bad("checksums.txt: %v", err)
	} else {
		published = sums(string(raw))
	}
	for _, name := range Assets {
		mine, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			c.bad("%s: our build: %v", name, err)
			continue
		}
		theirs, err := get(releaseURL(version, name))
		if err != nil {
			c.bad("%s: GitHub: %v", name, err)
			continue
		}
		switch {
		case digest(mine) != digest(theirs):
			c.bad("%s: GitHub's is %s, ours %s", name, digest(theirs), digest(mine))
		case published[name] != digest(mine):
			c.bad("%s: checksums.txt says %q, the binary is %s", name, published[name], digest(mine))
		default:
			c.ok("%s %s (%d bytes)", name, digest(mine), len(mine))
		}
	}
}

func (c *checker) check(version, releaseFile, dir string, offline bool) {
	var raw []byte
	var err error
	if releaseFile != "" {
		raw, err = os.ReadFile(releaseFile)
	} else if offline {
		err = fmt.Errorf("-offline needs -release FILE")
	} else {
		raw, err = get(releaseURL(version, update.ReleaseFile))
	}
	if err != nil {
		c.bad("release.json: %v", err)
		return
	}
	keys, err := update.PinnedKeys()
	if err != nil {
		c.bad("pinned keys: %v", err)
		return
	}
	fmt.Printf("release.json for %s, with the %d release keys compiled into the agent:\n", version, len(keys))
	m, err := update.OpenRelease(raw, keys)
	if err != nil {
		c.bad("%v", err)
		return
	}
	var env update.Envelope
	_ = json.Unmarshal(raw, &env)
	manifest, _ := base64.StdEncoding.DecodeString(env.Manifest)
	signature, _ := base64.StdEncoding.DecodeString(env.Signature)
	if i := update.VerifySignature(manifest, signature, keys); i >= 0 {
		c.ok("signed by pinned key %d (%s); commit %s, released_at %d", i+1, update.KeyFingerprint(keys[i]), m.Commit, m.ReleasedAt)
	}
	if m.Version != version {
		c.bad("the manifest is for %s, not %s", m.Version, version)
	}
	names := make([]string, 0, len(m.Files))
	for name := range m.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	if want := append([]string(nil), Assets...); strings.Join(names, ",") != strings.Join(sortStrings(want), ",") {
		c.bad("the manifest names %v, every release carries %v", names, Assets)
	}
	published := map[string]string{}
	if !offline {
		if raw, err := get(releaseURL(version, "checksums.txt")); err != nil {
			c.bad("checksums.txt: %v", err)
		} else {
			published = sums(string(raw))
		}
	}
	for _, name := range Assets {
		entry, err := m.For(version, name)
		if err != nil {
			c.bad("%v", err)
			continue
		}
		if dir != "" {
			if mine, err := os.ReadFile(filepath.Join(dir, name)); err != nil {
				c.bad("%s: our build: %v", name, err)
			} else if err := entry.Matches(mine); err != nil {
				c.bad("%s: our build: %v", name, err)
			} else {
				c.ok("%s: our build is the signed one", name)
			}
		}
		if offline {
			continue
		}
		if theirs, err := get(releaseURL(version, name)); err != nil {
			c.bad("%s: GitHub: %v", name, err)
		} else if err := entry.Matches(theirs); err != nil {
			c.bad("%s: GitHub's: %v", name, err)
		} else {
			c.ok("%s: GitHub's is the signed one", name)
		}
		// The agents before v0.3.13 still go by checksums.txt: it must agree.
		if published[name] != entry.SHA256 {
			c.bad("%s: checksums.txt says %q, the signed manifest %s", name, published[name], entry.SHA256)
		}
	}
}

func sortStrings(s []string) []string { sort.Strings(s); return s }
