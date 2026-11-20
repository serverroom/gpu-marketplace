// Package dl downloads one large file (an Ubuntu rootfs for WSL or the macOS
// VM) so that a slow connection still finishes.
//
// The first version gave each download an hour in total and started again
// from nothing on every try. A host in Bucharest reached
// cloud-images.ubuntu.com at 15-60 KB/s on 2026-11-20 while the rest of the
// internet came at 90 MB/s; the 357 MB rootfs would have needed about seven
// hours, so every attempt died at the hour and threw away what it had. Here a
// download fails only when nothing arrives for StallTimeout, keeps what did
// arrive in a .part file, and continues it with a Range request -- within one
// call, and on the next setup run if the agent restarts in between.
package dl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var (
	// StallTimeout is how long a download may receive nothing before this
	// attempt gives up (what arrived is kept for the next).
	StallTimeout = 3 * time.Minute
	// Attempts is how many times one call starts or resumes the download.
	Attempts = 6
	// Backoff is the wait before a retry; it doubles each time.
	Backoff = 10 * time.Second
	// ProgressEvery is how often a long download says how far it has come.
	ProgressEvery = time.Minute
)

// permanent marks an answer that a retry cannot change (a 404, a 403).
type permanent struct{ error }

// File downloads url to path. With sum (hex SHA-256) the result must match it,
// and a file already at path that matches is kept without downloading. Partial
// data waits in path+".part" between attempts. log may be nil.
func File(ctx context.Context, url, path, sum string, log func(format string, a ...interface{})) error {
	if log == nil {
		log = func(string, ...interface{}) {}
	}
	if sum != "" {
		if got, err := fileSum(path); err == nil && got == sum {
			return nil
		}
	}
	part := path + ".part"
	wait := Backoff
	var err error
	for attempt := 1; attempt <= Attempts; attempt++ {
		offset := size(part)
		if attempt > 1 {
			log("resuming the download of %s at %s (attempt %d of %d)", url, mb(offset), attempt, Attempts)
		}
		err = once(ctx, url, part, offset, log)
		if err == nil {
			break
		}
		var p permanent
		if ctx.Err() != nil || errors.As(err, &p) {
			return err
		}
		if attempt < Attempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			wait *= 2
		}
	}
	if err != nil {
		return err
	}
	if sum != "" {
		got, err := fileSum(part)
		if err != nil {
			return err
		}
		if got != sum {
			os.Remove(part)
			return fmt.Errorf("download %s: SHA-256 %s, published %s", url, got, sum)
		}
	}
	os.Remove(path)
	return os.Rename(part, path)
}

// once fetches url into part from offset: appending when the server resumes
// (206), starting over when it sends the whole file (200).
func once(ctx context.Context, url, part string, offset int64, log func(string, ...interface{})) error {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stalled atomic.Bool
	watchdog := time.AfterFunc(StallTimeout, func() {
		stalled.Store(true)
		cancel()
	})
	defer watchdog.Stop()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return permanent{err}
	}
	req.Header.Set("User-Agent", "gpu-agent")
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if stalled.Load() && ctx.Err() == nil {
			return fmt.Errorf("download %s: no answer for %s", url, StallTimeout)
		}
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()

	flags := os.O_WRONLY | os.O_CREATE
	switch {
	case resp.StatusCode == http.StatusPartialContent && offset > 0:
		if !strings.HasPrefix(resp.Header.Get("Content-Range"), "bytes "+strconv.FormatInt(offset, 10)+"-") {
			// Resumed from somewhere else: start the file again.
			os.Remove(part)
			return fmt.Errorf("download %s: the server resumed from the wrong place (%s)", url, resp.Header.Get("Content-Range"))
		}
		flags |= os.O_APPEND
	case resp.StatusCode == http.StatusOK:
		offset = 0
		flags |= os.O_TRUNC
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && offset > 0:
		// Nothing after offset: the .part is whole, or wrong -- the checksum decides.
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests:
		return permanent{fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)}
	default:
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	total := int64(-1)
	if resp.ContentLength >= 0 {
		total = offset + resp.ContentLength
	}

	out, err := os.OpenFile(part, flags, 0644)
	if err != nil {
		return permanent{err}
	}
	var got atomic.Int64
	got.Store(offset)
	start, startAt := time.Now(), offset
	ticker := time.NewTicker(ProgressEvery)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				n := got.Load()
				rate := float64(n-startAt) / time.Since(start).Seconds()
				of := ""
				if total > 0 {
					of = " of " + mb(total)
				}
				log("downloaded %s%s (%.0f KB/s)", mb(n), of, rate/1024)
			}
		}
	}()
	_, copyErr := io.Copy(out, &progressReader{r: resp.Body, watchdog: watchdog, n: &got})
	ticker.Stop()
	close(done)
	closeErr := out.Close()
	if copyErr != nil {
		if stalled.Load() && ctx.Err() == nil {
			return fmt.Errorf("download %s: nothing received for %s at %s (kept, to resume)", url, StallTimeout, mb(got.Load()))
		}
		return fmt.Errorf("download %s: %w", url, copyErr)
	}
	if closeErr != nil {
		return permanent{closeErr}
	}
	if total > 0 && got.Load() < total {
		return fmt.Errorf("download %s: the connection ended at %s of %s", url, mb(got.Load()), mb(total))
	}
	return nil
}

// progressReader counts what arrives and feeds the stall watchdog.
type progressReader struct {
	r        io.Reader
	watchdog *time.Timer
	n        *atomic.Int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.watchdog.Reset(StallTimeout)
		p.n.Add(int64(n))
	}
	return n, err
}

func size(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func mb(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1<<20)) }
