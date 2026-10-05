package serving

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
)

// The weight cache: a model's files, downloaded once by the agent itself on the
// host's network (never from inside the serving container) and kept beside the
// rental image, outside the per-rental encrypted disk, so a rental's wipe does
// not take them. Public weights need no encryption.
//
// Layout: <Dir>/<model id>/<revision>/<the catalog's paths>. A file is written
// to "<path>.part", resumed from where it stopped, checked against the
// catalog's SHA-256 and only then renamed into place, so a file under its own
// name has always passed its check. ".verified.json" in the revision directory
// records which files did; a model is verified when every file the catalog
// lists is recorded there with the hash the catalog gives and is on disk at its
// size. Another revision of the same model is deleted once the new one is
// complete.

// DefaultBaseURL is where weights come from: Hugging Face, at the commit the
// catalog pins (owner's decision, 2026-10-05).
const DefaultBaseURL = "https://huggingface.co"

// Weights states, in control.ServingWeights.State.
const (
	WeightsAbsent      = "absent"
	WeightsPartial     = "partial"
	WeightsDownloading = "downloading"
	WeightsVerified    = "verified"
)

const markerName = ".verified.json"

// ErrBusy is returned by Fetch while another download is running: one at a
// time, so two models never split the host's bandwidth or race for its disk.
var ErrBusy = errors.New("a model download is already running")

// SpaceError is a download refused because it would leave less free disk than
// the machine needs for its rentals.
type SpaceError struct {
	Model    string
	Need     uint64
	Free     uint64
	Reserved uint64
}

func (e *SpaceError) Error() string {
	return fmt.Sprintf("not enough disk for %s: it needs %d GB more and %d GB must stay free for rentals; %d GB is free",
		e.Model, gb(e.Need), gb(e.Reserved), gb(e.Free))
}

func gb(b uint64) uint64 { return (b + (1<<30 - 1)) >> 30 }

// Cache is the weight cache under Dir.
type Cache struct {
	Dir string
	// BaseURL is DefaultBaseURL unless a test says otherwise.
	BaseURL string
	// Client is the HTTP client downloads use; nil: one with sane timeouts that
	// follows redirects only to https (Hugging Face sends its large files on to
	// its CDN).
	Client *http.Client
	// RateBytesPerSec caps the download (0: no cap), so a host's connection is
	// not taken over for an hour.
	RateBytesPerSec int64
	// FreeBytes is the free disk under a directory; nil: the filesystem's.
	FreeBytes func(dir string) (uint64, error)
	// Now and Sleep are the clock, for tests.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	fetchMu sync.Mutex

	mu       sync.Mutex
	active   string // the model being downloaded
	progress *int64 // its bytes on disk so far
	lastErr  map[string]string
}

// ModelsDir is the cache's directory under the agent's storage directory
// (config.StorageDir: the disk the rental image and disks live on).
func ModelsDir(storageDir string) string { return filepath.Join(storageDir, "models") }

type marker struct {
	Repo     string            `json:"repo"`
	Revision string            `json:"revision"`
	Files    map[string]string `json:"files"`
	Complete bool              `json:"complete"`
}

func (c *Cache) revisionDir(m Model) string { return filepath.Join(c.Dir, m.ID, m.Revision) }

// filePath is a catalog path under the revision directory, refused when it is
// not one ValidFilePath accepts: Validate already checked it, and this is the
// one place a path from the catalog meets the filesystem.
func (c *Cache) filePath(m Model, f File) (string, error) {
	if !ValidFilePath(f.Path) {
		return "", fmt.Errorf("%s: file path %q is not safe", m.ID, f.Path)
	}
	return filepath.Join(c.revisionDir(m), filepath.FromSlash(f.Path)), nil
}

func (c *Cache) loadMarker(m Model) marker {
	data, err := os.ReadFile(filepath.Join(c.revisionDir(m), markerName))
	if err == nil {
		var mk marker
		if json.Unmarshal(data, &mk) == nil && mk.Repo == m.Repo && mk.Revision == m.Revision && mk.Files != nil {
			return mk
		}
	}
	return marker{Repo: m.Repo, Revision: m.Revision, Files: map[string]string{}}
}

func (c *Cache) saveMarker(m Model, mk marker) error {
	data, err := json.MarshalIndent(mk, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(c.revisionDir(m), markerName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// done reports whether a file has passed its check and is on disk at its size.
func (c *Cache) done(m Model, mk marker, f File) bool {
	if mk.Files[f.Path] != f.SHA256 {
		return false
	}
	path, err := c.filePath(m, f)
	if err != nil {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Size() == f.Size
}

// present is how much of a file is on disk: all of it once done, else what its
// .part holds (never more than the file's size).
func (c *Cache) present(m Model, mk marker, f File) int64 {
	if c.done(m, mk, f) {
		return f.Size
	}
	path, err := c.filePath(m, f)
	if err != nil {
		return 0
	}
	st, err := os.Stat(path + ".part")
	if err != nil || !st.Mode().IsRegular() {
		return 0
	}
	if st.Size() > f.Size {
		return 0
	}
	return st.Size()
}

// Status is a model's weights on this machine.
func (c *Cache) Status(m Model) control.ServingWeights {
	st := control.ServingWeights{Model: m.ID, Total: m.TotalBytes()}
	c.mu.Lock()
	if c.lastErr != nil {
		st.Error = c.lastErr[m.ID]
	}
	if c.active == m.ID && c.progress != nil {
		st.State = WeightsDownloading
		st.Bytes = atomic.LoadInt64(c.progress)
		c.mu.Unlock()
		return st
	}
	c.mu.Unlock()

	if m.Validate() != nil {
		st.State = WeightsAbsent
		return st
	}
	mk := c.loadMarker(m)
	all := true
	for _, f := range m.Files {
		st.Bytes += c.present(m, mk, f)
		if !c.done(m, mk, f) {
			all = false
		}
	}
	switch {
	case all:
		st.State = WeightsVerified
		st.Error = ""
	case st.Bytes > 0:
		st.State = WeightsPartial
	default:
		st.State = WeightsAbsent
	}
	return st
}

// Verified reports whether the model's weights are complete and checked.
func (c *Cache) Verified(m Model) bool { return c.Status(m).State == WeightsVerified }

// Fetch downloads whatever of the model is not on disk yet, checks every file
// and records it. reserve is what must stay free on the disk afterwards: the
// disk the listing promises a rental, and the room the host keeps for itself.
// Safe to call again after a failure or a restart: it resumes.
func (c *Cache) Fetch(ctx context.Context, m Model, reserve uint64) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if !c.fetchMu.TryLock() {
		return ErrBusy
	}
	defer c.fetchMu.Unlock()

	err := c.fetch(ctx, m, reserve)
	c.mu.Lock()
	c.active, c.progress = "", nil
	if c.lastErr == nil {
		c.lastErr = map[string]string{}
	}
	if err != nil {
		c.lastErr[m.ID] = err.Error()
	} else {
		delete(c.lastErr, m.ID)
	}
	c.mu.Unlock()
	return err
}

func (c *Cache) fetch(ctx context.Context, m Model, reserve uint64) error {
	dir := c.revisionDir(m)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	mk := c.loadMarker(m)

	var have, need int64
	for _, f := range m.Files {
		p := c.present(m, mk, f)
		have += p
		need += f.Size - p
	}
	progress := have
	c.mu.Lock()
	c.active, c.progress = m.ID, &progress
	c.mu.Unlock()

	if need > 0 {
		free, err := c.freeBytes(dir)
		if err != nil {
			return fmt.Errorf("read free disk under %s: %w", dir, err)
		}
		if free < uint64(need)+reserve {
			return &SpaceError{Model: m.ID, Need: uint64(need), Free: free, Reserved: reserve}
		}
	}

	for _, f := range m.Files {
		if c.done(m, mk, f) {
			continue
		}
		if err := c.fetchFile(ctx, m, f, &progress); err != nil {
			return err
		}
		mk.Files[f.Path] = f.SHA256
		if err := c.saveMarker(m, mk); err != nil {
			return fmt.Errorf("record %s: %w", f.Path, err)
		}
	}
	mk.Complete = true
	if err := c.saveMarker(m, mk); err != nil {
		return fmt.Errorf("record %s: %w", m.ID, err)
	}
	c.pruneRevisions(m)
	return nil
}

// pruneRevisions deletes the model's other revisions: weights an older release
// pinned, which this one will never serve.
func (c *Cache) pruneRevisions(m Model) {
	entries, err := os.ReadDir(filepath.Join(c.Dir, m.ID))
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() != m.Revision && revisionPattern.MatchString(e.Name()) {
			os.RemoveAll(filepath.Join(c.Dir, m.ID, e.Name()))
		}
	}
}

// URL is where one file of the model comes from: the repository's file at the
// pinned commit, every path segment escaped.
func (c *Cache) URL(m Model, f File) string {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	var parts []string
	for _, s := range strings.Split(m.Repo, "/") {
		parts = append(parts, url.PathEscape(s))
	}
	parts = append(parts, "resolve", url.PathEscape(m.Revision))
	for _, s := range strings.Split(f.Path, "/") {
		parts = append(parts, url.PathEscape(s))
	}
	return strings.TrimRight(base, "/") + "/" + strings.Join(parts, "/")
}

func (c *Cache) fetchFile(ctx context.Context, m Model, f File, progress *int64) error {
	final, err := c.filePath(m, f)
	if err != nil {
		return err
	}
	part := final + ".part"
	if err := os.MkdirAll(filepath.Dir(final), 0755); err != nil {
		return err
	}

	var offset int64
	if st, err := os.Stat(part); err == nil && st.Mode().IsRegular() {
		offset = st.Size()
	}
	if offset > f.Size {
		// Longer than the file can be: present() counted none of it.
		os.Remove(part)
		offset = 0
	}

	if offset < f.Size {
		if err := c.download(ctx, m, f, part, offset, progress); err != nil {
			return err
		}
	}

	sum, err := hashFile(ctx, part)
	if err != nil {
		return fmt.Errorf("check %s: %w", f.Path, err)
	}
	if sum != f.SHA256 {
		if st, err := os.Stat(part); err == nil {
			atomic.AddInt64(progress, -st.Size())
		}
		os.Remove(part)
		return fmt.Errorf("%s of %s did not match its SHA-256 and was deleted: got %s, want %s",
			f.Path, m.ID, sum, f.SHA256)
	}
	// A file left under its own name with no record of its check (the agent
	// stopped between the rename and the record) is replaced, not trusted.
	if err := os.Remove(final); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("replace %s: %w", f.Path, err)
	}
	if err := os.Rename(part, final); err != nil {
		return fmt.Errorf("move %s into place: %w", f.Path, err)
	}
	return nil
}

// StallTimeout is how long a download may receive nothing before it is given
// up (and resumed on the next try): a connection that hangs must not hold the
// one download slot for ever.
var StallTimeout = 2 * time.Minute

func (c *Cache) download(ctx context.Context, m Model, f File, part string, offset int64, progress *int64) error {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stalled atomic.Bool
	watchdog := time.AfterFunc(StallTimeout, func() {
		stalled.Store(true)
		cancel()
	})
	defer watchdog.Stop()

	err := c.downloadBody(reqCtx, m, f, part, offset, progress, watchdog)
	if err != nil && stalled.Load() && ctx.Err() == nil {
		return fmt.Errorf("download %s: nothing received for %s (kept what arrived, to resume)", f.Path, StallTimeout)
	}
	return err
}

func (c *Cache) downloadBody(ctx context.Context, m Model, f File, part string, offset int64, progress *int64,
	watchdog *time.Timer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL(m, f), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "gpu-agent")
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", f.Path, err)
	}
	defer resp.Body.Close()

	flags := os.O_WRONLY | os.O_CREATE
	switch {
	case resp.StatusCode == http.StatusPartialContent && offset > 0:
		if !strings.HasPrefix(resp.Header.Get("Content-Range"), "bytes "+strconv.FormatInt(offset, 10)+"-") {
			return fmt.Errorf("download %s: the server resumed from the wrong place (%s)", f.Path,
				resp.Header.Get("Content-Range"))
		}
		flags |= os.O_APPEND
	case resp.StatusCode == http.StatusOK:
		// A full answer, asked for or not: start the file again.
		atomic.AddInt64(progress, -offset)
		offset = 0
		flags |= os.O_TRUNC
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && offset > 0:
		// Nothing after offset: the .part is either whole (the hash decides) or
		// longer than the server's file, which the hash also refuses.
		return nil
	default:
		return fmt.Errorf("download %s: HTTP %d", f.Path, resp.StatusCode)
	}

	out, err := os.OpenFile(part, flags, 0644)
	if err != nil {
		return err
	}
	want := f.Size - offset
	body := c.limit(ctx, &stallReader{r: resp.Body, watchdog: watchdog})
	// One byte more than the file can hold, to tell a longer file from a whole one.
	n, copyErr := io.Copy(&counter{w: out, n: progress}, io.LimitReader(body, want+1))
	closeErr := out.Close()
	switch {
	case n > want:
		os.Remove(part)
		atomic.AddInt64(progress, -(offset + n))
		return fmt.Errorf("download %s: the server sent more than the catalog's %d bytes; deleted", f.Path, f.Size)
	case copyErr != nil:
		return fmt.Errorf("download %s: %w (kept %d of %d bytes to resume)", f.Path, copyErr, offset+n, f.Size)
	case closeErr != nil:
		return fmt.Errorf("write %s: %w", f.Path, closeErr)
	case n < want:
		return fmt.Errorf("download %s: the connection ended after %d of %d bytes (kept, to resume)", f.Path, offset+n, f.Size)
	}
	return nil
}

func (c *Cache) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	scheme := "https"
	if u, err := url.Parse(c.BaseURL); err == nil && u.Scheme != "" {
		scheme = u.Scheme
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = time.Minute
	return &http.Client{Transport: transport, CheckRedirect: redirectPolicy(scheme)}
}

// redirectPolicy follows at most 10 redirects, each to https or to the scheme
// the cache was given (a test's plain http server): never down from https.
func redirectPolicy(scheme string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" && req.URL.Scheme != scheme {
			return fmt.Errorf("refused a redirect to %s", req.URL.Scheme)
		}
		return nil
	}
}

func (c *Cache) freeBytes(dir string) (uint64, error) {
	if c.FreeBytes != nil {
		return c.FreeBytes(dir)
	}
	return freeBytes(dir)
}

// limit caps the body's rate at RateBytesPerSec.
func (c *Cache) limit(ctx context.Context, r io.Reader) io.Reader {
	if c.RateBytesPerSec <= 0 {
		return r
	}
	now := c.Now
	if now == nil {
		now = time.Now
	}
	sleep := c.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	return &rateReader{ctx: ctx, r: r, rate: c.RateBytesPerSec, start: now(), now: now, sleep: sleep}
}

type rateReader struct {
	ctx   context.Context
	r     io.Reader
	rate  int64
	start time.Time
	n     int64
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

func (rr *rateReader) Read(p []byte) (int, error) {
	// A quarter of a second's worth at a time keeps the pace even.
	if chunk := rr.rate / 4; chunk > 0 && int64(len(p)) > chunk {
		p = p[:chunk]
	}
	n, err := rr.r.Read(p)
	rr.n += int64(n)
	due := time.Duration(float64(rr.n) / float64(rr.rate) * float64(time.Second))
	if elapsed := rr.now().Sub(rr.start); elapsed < due {
		if serr := rr.sleep(rr.ctx, due-elapsed); serr != nil && err == nil {
			err = serr
		}
	}
	return n, err
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// stallReader restarts the download's watchdog whenever bytes arrive.
type stallReader struct {
	r        io.Reader
	watchdog *time.Timer
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.watchdog.Reset(StallTimeout)
	}
	return n, err
}

type counter struct {
	w io.Writer
	n *int64
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	atomic.AddInt64(c.n, int64(n))
	return n, err
}

func hashFile(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 4<<20)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buf)
		h.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Recheck hashes every file of the model again and forgets any that no longer
// match (deleting it), so the next Fetch downloads it again. Status trusts the
// record and the sizes, which is cheap; this reads all 65 GB, and is what
// serving runs before the engine loads the weights. Returns the paths that
// failed.
func (c *Cache) Recheck(ctx context.Context, m Model) ([]string, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if !c.fetchMu.TryLock() {
		return nil, ErrBusy
	}
	defer c.fetchMu.Unlock()

	mk := c.loadMarker(m)
	var failed []string
	for _, f := range m.Files {
		if !c.done(m, mk, f) {
			continue
		}
		path, err := c.filePath(m, f)
		if err != nil {
			return failed, err
		}
		sum, err := hashFile(ctx, path)
		if err != nil {
			return failed, fmt.Errorf("check %s: %w", f.Path, err)
		}
		if sum != f.SHA256 {
			failed = append(failed, f.Path)
			delete(mk.Files, f.Path)
			mk.Complete = false
			os.Remove(path)
		}
	}
	if len(failed) > 0 {
		if err := c.saveMarker(m, mk); err != nil {
			return failed, err
		}
	}
	return failed, nil
}

// Remove deletes a model's weights, every revision. Refused while that model is
// downloading.
func (c *Cache) Remove(id string) error {
	if !ValidModelID(id) {
		return fmt.Errorf("model id %q is not valid", id)
	}
	c.mu.Lock()
	busy := c.active == id
	c.mu.Unlock()
	if busy {
		return ErrBusy
	}
	return os.RemoveAll(filepath.Join(c.Dir, id))
}

// Models are the ids with anything in the cache, catalog or not, sorted: what
// `gpu-agent serve status` lists and `remove` offers to delete.
func (c *Cache) Models() []string {
	entries, err := os.ReadDir(c.Dir)
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && ValidModelID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids
}
