package speedtest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMedian(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
	}{
		{[]float64{5}, 5},
		{[]float64{30, 5, 20, 10, 40}, 20},
		{[]float64{4, 1, 3, 2}, 2.5},
	}
	for _, c := range cases {
		if got := median(c.in); got != c.want {
			t.Errorf("median(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestMbps(t *testing.T) {
	// 125 MB in 10 s is 1 Gbit over 10 s: 100 Mbps.
	if got := mbps(125_000_000, 10*time.Second); got != 100 {
		t.Errorf("mbps = %v, want 100", got)
	}
	if got := mbps(1234567, time.Second); got != 9.9 {
		t.Errorf("mbps = %v, want 9.9 (rounded to 0.1)", got)
	}
}

func pipeConn() net.Conn {
	a, b := net.Pipe()
	b.Close()
	return a
}

// The median, not the mean: one slow connect (a retransmitted SYN) must not
// become the listing's latency.
func TestLatencyIsTheMedianConnectTime(t *testing.T) {
	delays := []time.Duration{120, 10, 60, 30, 200}
	calls := 0
	r := Runner{Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := delays[calls] * time.Millisecond
		calls++
		time.Sleep(d)
		return pipeConn(), nil
	}}.withDefaults()

	ms, err := r.latency(context.Background(), "127.0.0.1", 2222)
	if err != nil {
		t.Fatalf("latency: %v", err)
	}
	if calls != 5 {
		t.Errorf("%d connects, want 5", calls)
	}
	if ms < 60 || ms >= 80 {
		t.Errorf("latency = %.1f ms, want the median probe (60 ms), not the mean (84 ms)", ms)
	}
	if ms*10 != math.Round(ms*10) {
		t.Errorf("latency = %v, want it rounded to 0.1 ms", ms)
	}
}

func TestLatencyFailsOnlyWhenEveryProbeFails(t *testing.T) {
	calls := 0
	r := Runner{Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		calls++
		if calls <= 4 {
			return nil, errors.New("connection refused")
		}
		return pipeConn(), nil
	}}.withDefaults()
	if _, err := r.latency(context.Background(), "127.0.0.1", 2222); err != nil {
		t.Fatalf("one probe of five connected, but latency failed: %v", err)
	}

	calls = 0
	r.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		calls++
		return nil, errors.New("connection refused")
	}
	_, err := r.latency(context.Background(), "127.0.0.1", 2222)
	if err == nil {
		t.Fatal("every probe failed, want an error")
	}
	if calls != 5 {
		t.Errorf("%d connects, want 5", calls)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error %q does not say why", err)
	}
}

func TestLatencyProbeTimesOut(t *testing.T) {
	r := Runner{
		LatencyTimeout: 20 * time.Millisecond,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			<-ctx.Done() // a filtered port: the SYN goes nowhere
			return nil, ctx.Err()
		},
	}.withDefaults()
	start := time.Now()
	if _, err := r.latency(context.Background(), "127.0.0.1", 2222); err == nil {
		t.Fatal("want an error when no probe connects")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("five 20 ms probes took %s", elapsed)
	}
}

func TestDownloadMbpsFromTheFirstByte(t *testing.T) {
	const chunk = 256 << 10
	const chunks = 16
	ckSize := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ckSize <- r.URL.Query().Get("ckSize")
		w.Header().Set("Content-Type", "application/octet-stream")
		// Server think time before the body: not network speed, not timed.
		time.Sleep(300 * time.Millisecond)
		buf := make([]byte, chunk)
		for i := 0; i < chunks; i++ {
			if i > 0 {
				time.Sleep(50 * time.Millisecond)
			}
			w.Write(buf)
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	got, received, err := Runner{}.withDefaults().download(context.Background(), srv.URL+"/backend/garbage")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if received != chunk*chunks {
		t.Errorf("received %d bytes, want %d", received, chunk*chunks)
	}
	if q := <-ckSize; q != "100" {
		t.Errorf("ckSize = %q, want 100", q)
	}
	// 4 MiB less the first read (64 KiB) over 15 x 50 ms is 44 Mbps; counting
	// the 300 ms before the first byte would make it 31.
	if got < 36 || got > 47 {
		t.Errorf("download = %.1f Mbps, want about 44", got)
	}
}

func TestDownloadStopsAtItsTimeLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 256<<10)
		for r.Context().Err() == nil {
			if _, err := w.Write(buf); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer srv.Close()

	r := Runner{DownloadFor: 300 * time.Millisecond}.withDefaults()
	start := time.Now()
	got, received, err := r.download(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("an endless body that is cut at the time limit is a measurement, not a failure: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("a 300 ms download took %s", elapsed)
	}
	if received < MiB || got <= 0 {
		t.Errorf("received %d bytes at %.1f Mbps", received, got)
	}
}

func TestDownloadNeedsOneMiB(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 100<<10))
	}))
	defer srv.Close()
	if _, _, err := (Runner{}).withDefaults().download(context.Background(), srv.URL); err == nil {
		t.Fatal("100 KiB is too little to time, want an error")
	}
}

func TestUploadStreamsAndCountsWhatWasWritten(t *testing.T) {
	var received atomic.Int64
	type seen struct {
		contentType string
		length      int64
	}
	seenc := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenc <- seen{r.Header.Get("Content-Type"), r.ContentLength}
		n, _ := io.Copy(io.Discard, r.Body)
		received.Store(n)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	r := Runner{UploadMax: 8 * MiB}.withDefaults()
	up, err := r.upload(context.Background(), srv.URL+"/backend/empty")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	s := <-seenc
	if s.contentType != "application/octet-stream" {
		t.Errorf("Content-Type = %q", s.contentType)
	}
	if s.length != -1 {
		t.Errorf("Content-Length = %d, want a streamed body of no declared size", s.length)
	}
	if got := received.Load(); got != 8*MiB {
		t.Errorf("server received %d bytes, want the 8 MiB limit", got)
	}
	// What the connection carries beyond the body is the request line, headers
	// and chunk framing: a few KiB, not a buffer's worth of bytes never sent.
	if up.bytes < received.Load() || up.bytes > received.Load()+64<<10 {
		t.Errorf("counted %d bytes written, server received %d", up.bytes, received.Load())
	}
	if up.method != UploadStreamed || up.refused != 0 || up.mbps <= 0 {
		t.Errorf("upload = %+v", up)
	}
}

func TestUploadStopsAtItsTimeLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 64<<10)
		for {
			if _, err := r.Body.Read(buf); err != nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	r := Runner{UploadFor: 200 * time.Millisecond}.withDefaults()
	start := time.Now()
	up, err := r.upload(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("a 200 ms upload took %s", elapsed)
	}
	if up.bytes <= 0 || up.bytes >= defaultUploadMax {
		t.Errorf("sent %d bytes: want the time limit, not the byte limit, to end it", up.bytes)
	}
}

// A proxy's request-body limit (413) or a WAF (403) refuses a big streamed
// POST; 1 MiB POSTs still measure the link.
func TestUploadFallsBackToOneMiBPostsWhenRefused(t *testing.T) {
	for _, status := range []int{http.StatusRequestEntityTooLarge, http.StatusForbidden} {
		var accepted atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Header.Get("Content-Type") {
			case "application/octet-stream":
				w.Header().Set("Connection", "close")
				http.Error(w, "refused", status)
			case "text/plain":
				n, _ := io.Copy(io.Discard, r.Body)
				if n != MiB {
					http.Error(w, "not 1 MiB", http.StatusBadRequest)
					return
				}
				accepted.Add(n)
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "unexpected type", http.StatusUnsupportedMediaType)
			}
		}))

		r := Runner{UploadFor: 300 * time.Millisecond}.withDefaults()
		up, err := r.upload(context.Background(), srv.URL)
		srv.Close()
		if err != nil {
			t.Fatalf("HTTP %d: upload: %v", status, err)
		}
		if up.method != UploadChunked || up.refused != status {
			t.Errorf("HTTP %d: method %q refused %d", status, up.method, up.refused)
		}
		// The server may have taken one more POST whose reply came after the
		// time limit; that one is not counted.
		if got := accepted.Load(); up.bytes <= 0 || up.bytes%MiB != 0 || got < up.bytes || got > up.bytes+MiB {
			t.Errorf("HTTP %d: counted %d bytes, server accepted %d", status, up.bytes, got)
		}
		if up.mbps <= 0 {
			t.Errorf("HTTP %d: %.1f Mbps", status, up.mbps)
		}
	}
}

// earlyServer answers every request as soon as its headers are in and only
// then reads and discards the body: what nginx's `return 204` does, and what
// the marketplace's speed test server was found doing. It counts the body
// bytes it received per Content-Type.
type earlyServer struct {
	ln  net.Listener
	mu  sync.Mutex
	got map[string]int64
}

func newEarlyServer(t *testing.T, status func(contentType string) int) *earlyServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &earlyServer{ln: ln, got: map[string]int64{}}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					ct := req.Header.Get("Content-Type")
					code := status(ct)
					fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\n\r\n", code, http.StatusText(code))
					n, err := io.Copy(io.Discard, req.Body)
					s.mu.Lock()
					s.got[ct] += n
					s.mu.Unlock()
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return s
}

func (s *earlyServer) url() string { return "http://" + s.ln.Addr().String() + "/backend/empty" }

// received waits briefly for the server to finish discarding, then reports
// what it got.
func (s *earlyServer) received(contentType string, want int64) int64 {
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.mu.Lock()
		got := s.got[contentType]
		s.mu.Unlock()
		if got >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The real server answers 204 before reading the body. net/http stops sending
// once that answer is in, which measured 200 KiB in 60 ms; the whole body must
// still go out and be timed.
func TestUploadKeepsSendingAfterAnEarlyReply(t *testing.T) {
	s := newEarlyServer(t, func(string) int { return http.StatusNoContent })
	r := Runner{UploadMax: 8 * MiB}.withDefaults()
	up, err := r.upload(context.Background(), s.url())
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if got := s.received("application/octet-stream", 8*MiB); got != 8*MiB {
		t.Errorf("server received %d bytes, want the whole 8 MiB", got)
	}
	if up.method != UploadStreamed || up.bytes < 8*MiB || up.bytes > 8*MiB+64<<10 {
		t.Errorf("upload = %+v, want 8 MiB streamed", up)
	}
}

func TestUploadFallsBackWhenAnEarlyReplyRefuses(t *testing.T) {
	s := newEarlyServer(t, func(ct string) int {
		if ct == "application/octet-stream" {
			return http.StatusRequestEntityTooLarge
		}
		return http.StatusNoContent
	})
	r := Runner{UploadFor: 300 * time.Millisecond}.withDefaults()
	start := time.Now()
	up, err := r.upload(context.Background(), s.url())
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("a refused stream plus 300 ms of fallback took %s: the refusal did not stop the stream", elapsed)
	}
	if up.method != UploadChunked || up.refused != http.StatusRequestEntityTooLarge {
		t.Errorf("method %q refused %d", up.method, up.refused)
	}
	if up.bytes <= 0 || up.bytes%MiB != 0 {
		t.Fatalf("counted %d bytes", up.bytes)
	}
	// One more POST may have reached the server after the time limit.
	if got := s.received("text/plain", up.bytes); got < up.bytes || got > up.bytes+MiB {
		t.Errorf("counted %d bytes accepted, server received %d", up.bytes, got)
	}
}

func TestUploadOtherRefusalIsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "broken", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := (Runner{}).withDefaults().upload(context.Background(), srv.URL); err == nil {
		t.Fatal("a 500 is a failed upload, not a fallback")
	}
}

func TestRunMeasuresAllThree(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/backend/garbage", func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 256<<10)
		for i := 0; i < 8; i++ {
			w.Write(buf)
		}
	})
	mux.HandleFunc("/backend/empty", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	target := Target{
		Server:      "speedtest-ny.example.net",
		DownloadURL: srv.URL + "/backend/garbage",
		UploadURL:   srv.URL + "/backend/empty",
		LatencyHost: "127.0.0.1",
		LatencyPort: ln.Addr().(*net.TCPAddr).Port,
	}
	res, err := Runner{UploadMax: 4 * MiB}.Run(context.Background(), target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Server != target.Server || res.DownBytes != 2*MiB || res.UpBytes < 4*MiB {
		t.Errorf("result = %+v", res)
	}
	if res.DownMbps <= 0 || res.UpMbps <= 0 || res.LatencyMs < 0 || res.UploadMethod != UploadStreamed {
		t.Errorf("result = %+v", res)
	}
	if !strings.HasPrefix(res.Summary(), "Speed test to speedtest-ny.example.net: ") {
		t.Errorf("summary = %q", res.Summary())
	}
}

func TestRunRefusesAnIncompleteTarget(t *testing.T) {
	good := Target{
		DownloadURL: "https://speedtest.example.net/backend/garbage",
		UploadURL:   "https://speedtest.example.net/backend/empty",
		LatencyHost: "console.example.net",
		LatencyPort: 2222,
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("a complete target: %v", err)
	}
	bad := []func(*Target){
		func(t *Target) { t.DownloadURL = "" },
		func(t *Target) { t.UploadURL = "ftp://speedtest.example.net/empty" },
		func(t *Target) { t.LatencyHost = "" },
		func(t *Target) { t.LatencyPort = 0 },
	}
	for i, mutate := range bad {
		tt := good
		mutate(&tt)
		if _, err := (Runner{}).Run(context.Background(), tt); err == nil {
			t.Errorf("case %d: Run accepted %+v", i, tt)
		}
	}
}

func TestSummary(t *testing.T) {
	r := Result{Server: "speedtest-ny.example.net", DownMbps: 940.2, UpMbps: 512.7, LatencyMs: 18.4, UploadMethod: UploadStreamed}
	want := "Speed test to speedtest-ny.example.net: 940.2 Mbps down, 512.7 Mbps up, 18.4 ms latency"
	if got := r.Summary(); got != want {
		t.Errorf("Summary() = %q, want %q", got, want)
	}
	r.UploadMethod, r.UploadRefused = UploadChunked, 413
	if got := r.Summary(); !strings.Contains(got, "1 MiB POSTs") || !strings.Contains(got, "413") {
		t.Errorf("a fallback upload does not say so: %q", got)
	}
}
