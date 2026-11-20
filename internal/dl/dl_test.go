package dl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func blob() ([]byte, string) {
	b := bytes.Repeat([]byte("gpu-agent rootfs "), 64*1024) // ~1 MB
	s := sha256.Sum256(b)
	return b, hex.EncodeToString(s[:])
}

func fast(t *testing.T) {
	t.Helper()
	saved := []time.Duration{StallTimeout, Backoff, ProgressEvery}
	StallTimeout, Backoff, ProgressEvery = 300*time.Millisecond, 10*time.Millisecond, time.Hour
	t.Cleanup(func() { StallTimeout, Backoff, ProgressEvery = saved[0], saved[1], saved[2] })
}

func serve(w http.ResponseWriter, r *http.Request, b []byte) {
	http.ServeContent(w, r, "rootfs.tar.gz", time.Time{}, bytes.NewReader(b))
}

func TestWholeDownload(t *testing.T) {
	fast(t)
	b, sum := blob()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serve(w, r, b) }))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "rootfs.tar.gz")
	if err := File(context.Background(), srv.URL, path, sum, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, b) {
		t.Fatal("content differs")
	}
	if _, err := os.Stat(path + ".part"); !os.IsNotExist(err) {
		t.Fatal(".part left behind")
	}
}

// The first answer is cut off half way; the next request must ask for the rest.
func TestResumesAfterACutConnection(t *testing.T) {
	fast(t)
	b, sum := blob()
	var calls atomic.Int32
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.Header.Get("Range"))
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", "1048576000")
			w.WriteHeader(http.StatusOK)
			w.Write(b[:len(b)/2])
			return // the handler ends: the client sees a short body
		}
		serve(w, r, b)
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "rootfs.tar.gz")
	if err := File(context.Background(), srv.URL, path, sum, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, b) {
		t.Fatal("content differs")
	}
	if len(ranges) < 2 || !strings.HasPrefix(ranges[1], "bytes=") || ranges[1] == "bytes=0-" {
		t.Fatalf("second request did not resume: %q", ranges)
	}
}

// A server that goes quiet is given up on after StallTimeout and resumed,
// however long the whole download takes.
func TestStallIsResumed(t *testing.T) {
	fast(t)
	b, sum := blob()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", "1048576")
			w.WriteHeader(http.StatusOK)
			w.Write(b[:1000])
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		serve(w, r, b)
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "rootfs.tar.gz")
	var logs []string
	err := File(context.Background(), srv.URL, path, sum, func(f string, a ...interface{}) { logs = append(logs, f) })
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() < 2 {
		t.Fatalf("calls = %d, want a resumed second request", calls.Load())
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "resuming") {
		t.Fatalf("no resume logged: %q", logs)
	}
}

func TestExistingGoodFileIsKept(t *testing.T) {
	fast(t)
	b, sum := blob()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); serve(w, r, b) }))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "rootfs.tar.gz")
	os.WriteFile(path, b, 0644)
	if err := File(context.Background(), srv.URL, path, sum, nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("downloaded again (%d requests)", calls.Load())
	}
}

func TestWrongChecksumIsRefused(t *testing.T) {
	fast(t)
	b, _ := blob()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serve(w, r, b) }))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "rootfs.tar.gz")
	err := File(context.Background(), srv.URL, path, strings.Repeat("0", 64), nil)
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("err = %v, want a checksum refusal", err)
	}
	for _, p := range []string{path, path + ".part"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s kept after a bad checksum", p)
		}
	}
}

func TestNotFoundIsNotRetried(t *testing.T) {
	fast(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	err := File(context.Background(), srv.URL, filepath.Join(t.TempDir(), "x"), "", nil)
	if err == nil || calls.Load() != 1 {
		t.Fatalf("err = %v after %d requests, want one failed request", err, calls.Load())
	}
}

// A .part left by an earlier run (the agent restarted) is continued, not thrown away.
func TestPartFromAnEarlierRunIsContinued(t *testing.T) {
	fast(t)
	b, sum := blob()
	var first string
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			first = r.Header.Get("Range")
		}
		serve(w, r, b)
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "rootfs.tar.gz")
	os.WriteFile(path+".part", b[:300000], 0644)
	if err := File(context.Background(), srv.URL, path, sum, nil); err != nil {
		t.Fatal(err)
	}
	if first != "bytes=300000-" {
		t.Fatalf("first Range = %q, want bytes=300000-", first)
	}
}
