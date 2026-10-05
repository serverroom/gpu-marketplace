package serving

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testRevision = "0123456789abcdef0123456789abcdef01234567"

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// weightServer serves files as Hugging Face does: /<repo>/resolve/<revision>/<path>,
// with Range support (http.ServeContent).
type weightServer struct {
	*httptest.Server
	mu     sync.Mutex
	files  map[string][]byte
	ranges []string
	hits   int
	// override, when set, answers instead of the file.
	override func(w http.ResponseWriter, r *http.Request) bool
}

func newWeightServer(t *testing.T, files map[string][]byte) *weightServer {
	ws := &weightServer{files: files}
	ws.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws.mu.Lock()
		ws.hits++
		ws.ranges = append(ws.ranges, r.Header.Get("Range"))
		override := ws.override
		ws.mu.Unlock()
		if override != nil && override(w, r) {
			return
		}
		prefix := "/org/model/resolve/" + testRevision + "/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		data, ok := ws.files[strings.TrimPrefix(r.URL.Path, prefix)]
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(ws.Close)
	return ws
}

func testModel(files map[string][]byte) Model {
	m := Model{ID: "test-model", Repo: "org/model", Revision: testRevision, Engine: "vllm", MemoryGB: 1,
		Kinds: []string{kindContainerNV}}
	for _, p := range []string{"config.json", "model-00001-of-00002.safetensors", "sub/tokenizer.json"} {
		if data, ok := files[p]; ok {
			m.Files = append(m.Files, File{Path: p, Size: int64(len(data)), SHA256: sum(data)})
		}
	}
	return m
}

func testFiles() map[string][]byte {
	return map[string][]byte{
		"config.json":                      []byte(`{"model_type":"test"}`),
		"model-00001-of-00002.safetensors": bytes.Repeat([]byte("weights!"), 4096),
		"sub/tokenizer.json":               []byte(`{"vocab":{}}`),
	}
}

func newCache(t *testing.T, base string) *Cache {
	return &Cache{Dir: t.TempDir(), BaseURL: base, FreeBytes: func(string) (uint64, error) { return 1 << 40, nil }}
}

func TestFetchDownloadsAndVerifies(t *testing.T) {
	files := testFiles()
	ws := newWeightServer(t, files)
	c := newCache(t, ws.URL)
	m := testModel(files)

	if st := c.Status(m); st.State != WeightsAbsent || st.Total != m.TotalBytes() {
		t.Fatalf("before: %+v", st)
	}
	if err := c.Fetch(context.Background(), m, 0); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	st := c.Status(m)
	if st.State != WeightsVerified || st.Bytes != m.TotalBytes() || st.Error != "" {
		t.Fatalf("after: %+v", st)
	}
	for p, data := range files {
		got, err := os.ReadFile(filepath.Join(c.Dir, m.ID, m.Revision, filepath.FromSlash(p)))
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("%s on disk differs: %v", p, err)
		}
	}
	// A second fetch has nothing to do.
	hits := ws.hits
	if err := c.Fetch(context.Background(), m, 0); err != nil || ws.hits != hits {
		t.Errorf("a verified model was fetched again (%v, %d requests)", err, ws.hits-hits)
	}
}

func TestFetchResumesFromPart(t *testing.T) {
	files := testFiles()
	ws := newWeightServer(t, files)
	c := newCache(t, ws.URL)
	m := testModel(files)

	big := "model-00001-of-00002.safetensors"
	dir := filepath.Join(c.Dir, m.ID, m.Revision)
	os.MkdirAll(dir, 0755)
	half := len(files[big]) / 2
	os.WriteFile(filepath.Join(dir, big+".part"), files[big][:half], 0644)

	if st := c.Status(m); st.State != WeightsPartial || st.Bytes != int64(half) {
		t.Fatalf("a half-downloaded file: %+v", st)
	}
	if err := c.Fetch(context.Background(), m, 0); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	found := false
	for _, r := range ws.ranges {
		if r == "bytes="+strconv.Itoa(half)+"-" {
			found = true
		}
	}
	if !found {
		t.Errorf("the download did not resume from byte %d: ranges %q", half, ws.ranges)
	}
	if !c.Verified(m) {
		t.Fatalf("not verified after resuming: %+v", c.Status(m))
	}
}

func TestFetchRefusesWrongBytes(t *testing.T) {
	files := testFiles()
	m := testModel(files)
	served := testFiles()
	served["config.json"] = []byte(`{"model_type":"evil"}`) // same length, other bytes
	ws := newWeightServer(t, served)
	c := newCache(t, ws.URL)

	err := c.Fetch(context.Background(), m, 0)
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("want a hash mismatch, got %v", err)
	}
	path := filepath.Join(c.Dir, m.ID, m.Revision, "config.json")
	if _, err := os.Stat(path); err == nil {
		t.Error("a file that failed its check is under its own name")
	}
	if _, err := os.Stat(path + ".part"); err == nil {
		t.Error("a file that failed its check was kept")
	}
	st := c.Status(m)
	if st.State == WeightsVerified || !strings.Contains(st.Error, "SHA-256") {
		t.Errorf("status after a failed check: %+v", st)
	}
}

func TestFetchRefusesLongerFile(t *testing.T) {
	files := testFiles()
	m := testModel(files)
	served := testFiles()
	served["config.json"] = append(append([]byte{}, files["config.json"]...), []byte("extra")...)
	ws := newWeightServer(t, served)
	c := newCache(t, ws.URL)
	err := c.Fetch(context.Background(), m, 0)
	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("want a refusal of a longer file, got %v", err)
	}
}

func TestFetchKeepsShortDownloadToResume(t *testing.T) {
	files := testFiles()
	m := testModel(files)
	big := "model-00001-of-00002.safetensors"
	ws := newWeightServer(t, files)
	var cut atomic.Bool
	cut.Store(true)
	ws.override = func(w http.ResponseWriter, r *http.Request) bool {
		if !cut.Load() || !strings.HasSuffix(r.URL.Path, big) {
			return false
		}
		// Promise the whole file, send a third, and hang up.
		data := files[big]
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		w.Write(data[:len(data)/3])
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			conn.Close()
		}
		return true
	}
	c := newCache(t, ws.URL)
	if err := c.Fetch(context.Background(), m, 0); err == nil {
		t.Fatal("a cut download was accepted")
	}
	if st := c.Status(m); st.State != WeightsPartial || st.Bytes == 0 {
		t.Fatalf("what arrived was not kept: %+v", st)
	}
	cut.Store(false)
	if err := c.Fetch(context.Background(), m, 0); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !c.Verified(m) {
		t.Fatalf("not verified after the resume: %+v", c.Status(m))
	}
}

func TestFetchRefusesWithoutRoomForRentals(t *testing.T) {
	files := testFiles()
	ws := newWeightServer(t, files)
	c := newCache(t, ws.URL)
	m := testModel(files)
	c.FreeBytes = func(string) (uint64, error) { return uint64(m.TotalBytes()) + 100, nil }

	err := c.Fetch(context.Background(), m, 1000)
	var se *SpaceError
	if !errors.As(err, &se) {
		t.Fatalf("want a SpaceError, got %v", err)
	}
	if ws.hits != 0 {
		t.Errorf("%d requests were made before the space check refused", ws.hits)
	}
	if err := c.Fetch(context.Background(), m, 100); err != nil {
		t.Fatalf("with exactly enough room: %v", err)
	}
}

func TestFetchOneAtATime(t *testing.T) {
	files := testFiles()
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	ws := newWeightServer(t, files)
	ws.override = func(w http.ResponseWriter, r *http.Request) bool {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return false
	}
	c := newCache(t, ws.URL)
	m := testModel(files)
	done := make(chan error, 1)
	go func() { done <- c.Fetch(context.Background(), m, 0) }()
	<-entered
	if st := c.Status(m); st.State != WeightsDownloading {
		t.Errorf("while downloading: %+v", st)
	}
	if err := c.Fetch(context.Background(), m, 0); !errors.Is(err, ErrBusy) {
		t.Errorf("a second download started: %v", err)
	}
	if err := c.Remove(m.ID); !errors.Is(err, ErrBusy) {
		t.Errorf("weights were removed while downloading: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("fetch: %v", err)
	}
}

func TestFetchGivesUpOnAStall(t *testing.T) {
	old := StallTimeout
	StallTimeout = 200 * time.Millisecond
	defer func() { StallTimeout = old }()

	files := testFiles()
	ws := newWeightServer(t, files)
	stop := make(chan struct{})
	defer close(stop)
	ws.override = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-stop:
		case <-r.Context().Done():
		}
		return true
	}
	c := newCache(t, ws.URL)
	err := c.Fetch(context.Background(), testModel(files), 0)
	if err == nil || !strings.Contains(err.Error(), "nothing received") {
		t.Fatalf("want a stall, got %v", err)
	}
}

func TestRecheckForgetsCorruptFiles(t *testing.T) {
	files := testFiles()
	ws := newWeightServer(t, files)
	c := newCache(t, ws.URL)
	m := testModel(files)
	if err := c.Fetch(context.Background(), m, 0); err != nil {
		t.Fatal(err)
	}
	if failed, err := c.Recheck(context.Background(), m); err != nil || len(failed) != 0 {
		t.Fatalf("a clean cache failed its recheck: %v %v", failed, err)
	}
	path := filepath.Join(c.Dir, m.ID, m.Revision, "config.json")
	os.WriteFile(path, []byte(`{"model_type":"rot!"}`), 0644) // same size
	if !c.Verified(m) {
		t.Fatal("status should trust the record until a recheck")
	}
	failed, err := c.Recheck(context.Background(), m)
	if err != nil || len(failed) != 1 || failed[0] != "config.json" {
		t.Fatalf("recheck: %v %v", failed, err)
	}
	if c.Verified(m) {
		t.Fatal("a corrupt file still counts as verified")
	}
	if err := c.Fetch(context.Background(), m, 0); err != nil || !c.Verified(m) {
		t.Fatalf("the corrupt file was not fetched again: %v", err)
	}
}

func TestOldRevisionsArePruned(t *testing.T) {
	files := testFiles()
	ws := newWeightServer(t, files)
	c := newCache(t, ws.URL)
	m := testModel(files)
	old := filepath.Join(c.Dir, m.ID, strings.Repeat("f", 40))
	os.MkdirAll(old, 0755)
	os.WriteFile(filepath.Join(old, "config.json"), []byte("old"), 0644)
	keep := filepath.Join(c.Dir, m.ID, "notes")
	os.MkdirAll(keep, 0755)

	if err := c.Fetch(context.Background(), m, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("an older revision was kept")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("a directory that is not a revision was deleted")
	}
}

func TestRemoveAndModels(t *testing.T) {
	files := testFiles()
	ws := newWeightServer(t, files)
	c := newCache(t, ws.URL)
	m := testModel(files)
	if err := c.Fetch(context.Background(), m, 0); err != nil {
		t.Fatal(err)
	}
	if got := c.Models(); len(got) != 1 || got[0] != m.ID {
		t.Fatalf("models: %v", got)
	}
	if err := c.Remove("../etc"); err == nil {
		t.Error("an unsafe id was accepted")
	}
	if err := c.Remove(m.ID); err != nil {
		t.Fatal(err)
	}
	if st := c.Status(m); st.State != WeightsAbsent {
		t.Errorf("after remove: %+v", st)
	}
}

func TestURLEscapesEverySegment(t *testing.T) {
	c := &Cache{}
	m := Model{Repo: "openai/gpt-oss-120b", Revision: testRevision}
	got := c.URL(m, File{Path: "sub/model-00001-of-00014.safetensors"})
	want := "https://huggingface.co/openai/gpt-oss-120b/resolve/" + testRevision + "/sub/model-00001-of-00014.safetensors"
	if got != want {
		t.Fatalf("url:\n got %s\nwant %s", got, want)
	}
}

func TestRedirectPolicy(t *testing.T) {
	policy := redirectPolicy("https")
	to := func(raw string) *http.Request {
		u, _ := url.Parse(raw)
		return &http.Request{URL: u}
	}
	if err := policy(to("https://cdn-lfs.example/x"), nil); err != nil {
		t.Errorf("a redirect to https was refused: %v", err)
	}
	if err := policy(to("http://cdn-lfs.example/x"), nil); err == nil {
		t.Error("a redirect from https down to http was followed")
	}
	if err := policy(to("https://x/"), make([]*http.Request, 10)); err == nil {
		t.Error("an eleventh redirect was followed")
	}
}

func TestRateLimit(t *testing.T) {
	var slept time.Duration
	clock := time.Unix(0, 0)
	c := &Cache{RateBytesPerSec: 1000,
		Now: func() time.Time { return clock },
		Sleep: func(_ context.Context, d time.Duration) error {
			slept += d
			clock = clock.Add(d)
			return nil
		}}
	r := c.limit(context.Background(), bytes.NewReader(make([]byte, 5000)))
	buf := make([]byte, 64<<10)
	var n int
	for {
		k, err := r.Read(buf)
		n += k
		if err != nil {
			break
		}
	}
	if n != 5000 {
		t.Fatalf("read %d bytes", n)
	}
	if slept < 4900*time.Millisecond || slept > 5100*time.Millisecond {
		t.Errorf("5000 bytes at 1000 B/s took %v", slept)
	}
}
