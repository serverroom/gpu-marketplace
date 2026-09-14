// Package speedtest measures a host's network against the marketplace's speed
// test server in the location of its relay: TCP connect latency, a download
// from a LibreSpeed garbage endpoint and an upload to its empty endpoint.
//
// A run is bounded: one overall deadline, a time and a byte limit on each
// transfer, one reusable buffer per transfer, and everything stops when its
// context is cancelled. Standard library only.
package speedtest

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// MiB is a LibreSpeed chunk, and the size of each request of the upload
// fallback.
const MiB = 1 << 20

const (
	defaultTimeout        = 40 * time.Second
	defaultLatencyProbes  = 5
	defaultLatencyTimeout = 3 * time.Second
	defaultDownloadFor    = 15 * time.Second
	defaultDownloadChunks = 100
	defaultUploadFor      = 10 * time.Second
	defaultUploadMax      = 100 * MiB

	// minDownload is the least a download must bring for its speed to mean
	// anything.
	minDownload = MiB
	// responseReserve is kept back from the overall deadline for the upload's
	// reply.
	responseReserve = 2 * time.Second
	dialTimeout     = 10 * time.Second
	readBuffer      = 64 << 10
	// uploadChunk is one chunk of the streamed upload's chunked encoding.
	uploadChunk = 64 << 10
)

// Target is where to measure, exactly as the control plane hands it out.
type Target struct {
	Server      string `json:"server"`
	DownloadURL string `json:"download_url"`
	UploadURL   string `json:"upload_url"`
	LatencyHost string `json:"latency_host"`
	LatencyPort int    `json:"latency_port"`
}

// Validate says what is unusable in a target before any traffic is sent.
func (t Target) Validate() error {
	for _, u := range []struct{ name, value string }{
		{"download_url", t.DownloadURL},
		{"upload_url", t.UploadURL},
	} {
		p, err := url.Parse(u.value)
		if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			return fmt.Errorf("speed test target: %s %q is not an http(s) URL", u.name, u.value)
		}
	}
	if t.LatencyHost == "" || t.LatencyPort < 1 || t.LatencyPort > 65535 {
		return fmt.Errorf("speed test target: no usable latency host (%q, port %d)", t.LatencyHost, t.LatencyPort)
	}
	return nil
}

// How the upload was measured.
const (
	// UploadStreamed is one POST streaming random bytes: the normal method.
	UploadStreamed = "streamed"
	// UploadChunked is repeated 1 MiB POSTs, used when the streamed POST was
	// refused by a request-body-size or WAF limit.
	UploadChunked = "1 MiB POSTs"
)

// Result is one measurement.
type Result struct {
	Server    string
	LatencyMs float64
	DownMbps  float64
	UpMbps    float64
	DownBytes int64
	UpBytes   int64
	// UploadMethod is UploadStreamed or UploadChunked.
	UploadMethod string
	// UploadRefused is the HTTP status that refused the streamed upload when
	// the fallback was used, 0 otherwise.
	UploadRefused int
}

// Summary is the one-line form, for the daemon log.
func (r Result) Summary() string {
	s := fmt.Sprintf("Speed test to %s: %.1f Mbps down, %.1f Mbps up, %.1f ms latency", r.Server, r.DownMbps, r.UpMbps, r.LatencyMs)
	if r.UploadMethod == UploadChunked {
		s += " (upload " + r.UploadNote() + ")"
	}
	return s
}

// UploadNote says how the upload was measured.
func (r Result) UploadNote() string {
	if r.UploadMethod == UploadChunked {
		return fmt.Sprintf("in 1 MiB POSTs: a streamed upload was refused with HTTP %d", r.UploadRefused)
	}
	return UploadStreamed
}

// Runner runs speed tests. The zero Runner measures the way the marketplace
// expects; the fields exist so tests can shorten it and point it at local
// servers.
type Runner struct {
	// Dial opens every TCP connection: the latency probes and the transfers.
	// nil is a net.Dialer.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)

	Timeout        time.Duration // the whole run; 40 s
	LatencyProbes  int           // TCP connects whose median is the latency; 5
	LatencyTimeout time.Duration // per connect; 3 s
	DownloadFor    time.Duration // reading the body, from its first byte; 15 s
	DownloadChunks int           // ckSize: 1 MiB chunks asked for; 100
	UploadFor      time.Duration // writing the body; 10 s
	UploadMax      int64         // bytes streamed at most; 100 MiB
}

func (r Runner) withDefaults() Runner {
	if r.Timeout <= 0 {
		r.Timeout = defaultTimeout
	}
	if r.LatencyProbes <= 0 {
		r.LatencyProbes = defaultLatencyProbes
	}
	if r.LatencyTimeout <= 0 {
		r.LatencyTimeout = defaultLatencyTimeout
	}
	if r.DownloadFor <= 0 {
		r.DownloadFor = defaultDownloadFor
	}
	if r.DownloadChunks <= 0 {
		r.DownloadChunks = defaultDownloadChunks
	}
	if r.UploadFor <= 0 {
		r.UploadFor = defaultUploadFor
	}
	if r.UploadMax <= 0 {
		r.UploadMax = defaultUploadMax
	}
	return r
}

// Run measures latency, then download, then upload. Any phase failing fails
// the run: a listing is better shown with no figures than with a wrong one.
//
// Every connection is direct, never through an HTTP proxy from the
// environment: what is being measured is the path renters and the relay
// tunnel use.
func (r Runner) Run(ctx context.Context, t Target) (Result, error) {
	r = r.withDefaults()
	if err := t.Validate(); err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	res := Result{Server: t.Server}
	if res.Server == "" {
		if u, err := url.Parse(t.DownloadURL); err == nil {
			res.Server = u.Hostname()
		}
	}

	var err error
	if res.LatencyMs, err = r.latency(ctx, t.LatencyHost, t.LatencyPort); err != nil {
		return res, err
	}
	if res.DownMbps, res.DownBytes, err = r.download(ctx, t.DownloadURL); err != nil {
		return res, err
	}
	up, err := r.upload(ctx, t.UploadURL)
	if err != nil {
		return res, err
	}
	res.UpMbps, res.UpBytes, res.UploadMethod, res.UploadRefused = up.mbps, up.bytes, up.method, up.refused
	return res, nil
}

// latency is the median of the TCP connect times to host:port, in ms rounded
// to 0.1. Probes that fail are left out; only all of them failing is an error.
func (r Runner) latency(ctx context.Context, host string, port int) (float64, error) {
	ip, err := resolve(ctx, host)
	if err != nil {
		return 0, fmt.Errorf("latency: look up %s: %w", host, err)
	}
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	dial := r.dialer()

	var ms []float64
	var lastErr error
	for i := 0; i < r.LatencyProbes; i++ {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("latency: %w", err)
		}
		pctx, cancel := context.WithTimeout(ctx, r.LatencyTimeout)
		start := time.Now()
		conn, err := dial(pctx, "tcp", addr)
		elapsed := time.Since(start)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		conn.Close()
		ms = append(ms, float64(elapsed.Microseconds())/1000)
	}
	if len(ms) == 0 {
		return 0, fmt.Errorf("latency: no TCP connection to %s:%d in %d attempts: %v", host, port, r.LatencyProbes, lastErr)
	}
	return round1(median(ms)), nil
}

// resolve looks the latency host up once, so the connect times are the
// network's and not the same DNS query five times over. IPv4 first: nearly
// every host can reach it.
func resolve(ctx context.Context, host string) (string, error) {
	if net.ParseIP(host) != nil {
		return host, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return "", err
	}
	if len(addrs) == 0 {
		return "", errors.New("no addresses")
	}
	for _, a := range addrs {
		if a.IP.To4() != nil {
			return a.IP.String(), nil
		}
	}
	return addrs[0].IP.String(), nil
}

// download reads download_url?ckSize=N for at most DownloadFor or until EOF.
// The clock starts at the first byte of the body, so connection setup, TLS
// and the server's time to answer are not counted as network speed.
func (r Runner) download(ctx context.Context, rawURL string) (mbpsOut float64, received int64, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, 0, fmt.Errorf("download: %w", err)
	}
	q := u.Query()
	q.Set("ckSize", strconv.Itoa(r.DownloadChunks))
	u.RawQuery = q.Encode()

	reserve := r.UploadFor + responseReserve
	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var expired atomic.Bool
	stop := phaseEnd(ctx, time.Now(), r.DownloadFor, reserve)
	watchdog := time.AfterFunc(time.Until(stop), func() {
		expired.Store(true)
		cancel()
	})
	defer watchdog.Stop()

	req, err := http.NewRequestWithContext(dctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, 0, fmt.Errorf("download: %w", err)
	}
	req.Header.Set("Cache-Control", "no-store")
	client, done := r.client()
	defer done()
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, failure(ctx, &expired, "download", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("download: GET %s: HTTP %d", u, resp.StatusCode)
	}

	buf := make([]byte, readBuffer)
	var first, last time.Time
	var timed int64
	for {
		n, rerr := resp.Body.Read(buf)
		now := time.Now()
		if n > 0 {
			received += int64(n)
			if first.IsZero() {
				first, last = now, now
				stop = phaseEnd(ctx, now, r.DownloadFor, reserve)
				watchdog.Reset(time.Until(stop))
			} else {
				// The first read's bytes arrived before the clock started.
				timed += int64(n)
				last = now
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if expired.Load() && !first.IsZero() && ctx.Err() == nil {
				break // the time limit, not a failure
			}
			return 0, received, failure(ctx, &expired, "download", rerr)
		}
		if !first.IsZero() && !now.Before(stop) {
			break
		}
	}
	if received < minDownload {
		return 0, received, fmt.Errorf("download: only %d bytes arrived; at least 1 MiB is needed", received)
	}
	return mbps(timed, last.Sub(first)), received, nil
}

type uploadResult struct {
	mbps    float64
	bytes   int64
	method  string
	refused int
}

// upload streams random bytes in one POST. When that POST is refused with 413
// or 403 -- a request-body-size or WAF limit in front of the server -- it
// measures again with repeated 1 MiB POSTs instead.
//
// Both write their requests by hand on their own connection rather than
// through net/http, because a speed test server can answer before it has read
// the body: nginx's `return 204` replies as soon as the headers are in and
// discards the body as it arrives. net/http stops sending a body once the
// reply is in, which times the first few hundred KiB into a socket buffer, not
// the link.
func (r Runner) upload(ctx context.Context, rawURL string) (uploadResult, error) {
	up, status, err := r.streamUpload(ctx, rawURL)
	if status == http.StatusRequestEntityTooLarge || status == http.StatusForbidden {
		up, err = r.chunkedUpload(ctx, rawURL)
		up.refused = status
	}
	return up, err
}

// streamUpload writes one chunked POST of random bytes until UploadFor or
// UploadMax. It counts the bytes written to the connection itself, from the
// first byte of the body to the last write, and requires a 2xx. The reply is
// read as soon as it comes: a refusal stops the upload at once, while a 2xx
// sent before the body was read does not.
//
// The returned status is the server's, so the caller can tell a refusal.
func (r Runner) streamUpload(ctx context.Context, rawURL string) (uploadResult, int, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return uploadResult{}, 0, fmt.Errorf("upload: %w", err)
	}
	m := &meter{}
	conn, err := r.openConn(ctx, u, m)
	if err != nil {
		return uploadResult{}, 0, fmt.Errorf("upload: %w", err)
	}
	defer conn.Close()
	defer context.AfterFunc(ctx, func() { conn.Close() })()

	replies := make(chan reply, 1)
	go func() { replies <- readReply(bufio.NewReader(conn)) }()

	head := "POST " + u.RequestURI() + " HTTP/1.1\r\nHost: " + u.Host +
		"\r\nUser-Agent: gpu-agent\r\nContent-Type: application/octet-stream\r\nTransfer-Encoding: chunked\r\n\r\n"
	if _, err := io.WriteString(conn, head); err != nil {
		return uploadResult{}, 0, fmt.Errorf("upload: %w", err)
	}

	start := m.begin()
	stop := phaseEnd(ctx, start, r.UploadFor, responseReserve)
	conn.SetWriteDeadline(stop.Add(responseReserve / 2))
	src := newRandom(MiB)
	frame := make([]byte, 0, uploadChunk+16)

	var early *reply
	var werr error
	incoming := replies
	for left := r.UploadMax; left > 0 && time.Now().Before(stop); {
		select {
		case rep := <-incoming:
			if rep.err != nil || !ok2xx(rep.status) {
				return uploadResult{}, rep.status, rep.failure("upload", rawURL)
			}
			early, incoming = &rep, nil
		default:
		}
		n := int64(uploadChunk)
		if left < n {
			n = left
		}
		frame = strconv.AppendInt(frame[:0], n, 16)
		frame = append(frame, '\r', '\n')
		frame = append(frame, src.next(int(n))...)
		frame = append(frame, '\r', '\n')
		if _, werr = conn.Write(frame); werr != nil {
			break
		}
		left -= n
	}
	if werr == nil {
		_, werr = io.WriteString(conn, "0\r\n\r\n")
	}
	sent, elapsed := m.span()

	rep := early
	if rep == nil {
		wait := time.Until(stop.Add(responseReserve))
		if werr != nil {
			// A refusal can close the connection under the writes; its reply
			// may be in all the same.
			wait = responseReserve
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case got := <-replies:
			rep = &got
		case <-timer.C:
		case <-ctx.Done():
		}
	}
	switch {
	case ctx.Err() != nil:
		return uploadResult{}, 0, fmt.Errorf("upload: %w", ctx.Err())
	case rep != nil && rep.err == nil && !ok2xx(rep.status):
		return uploadResult{}, rep.status, rep.failure("upload", rawURL)
	case werr != nil:
		// Even after a 2xx: a body cut short by the server closing is timed
		// on a socket buffer, not the link.
		return uploadResult{}, 0, fmt.Errorf("upload: the connection failed after %d bytes: %w", sent, werr)
	case rep == nil:
		return uploadResult{}, 0, errors.New("upload: the server did not answer")
	case rep.err != nil:
		return uploadResult{}, 0, rep.failure("upload", rawURL)
	case sent <= 0:
		return uploadResult{}, rep.status, errors.New("upload: nothing was sent")
	}
	return uploadResult{mbps: mbps(sent, elapsed), bytes: sent, method: UploadStreamed}, rep.status, nil
}

// chunkedUpload POSTs 1 MiB of random bytes at a time, one after another on a
// kept-alive connection, until UploadFor is up, and counts only what the
// server accepted. Each request waits for its reply before the next, so on a
// fast, distant link this reads somewhat below the truth.
func (r Runner) chunkedUpload(ctx context.Context, rawURL string) (uploadResult, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return uploadResult{}, fmt.Errorf("upload: %w", err)
	}
	head := "POST " + u.RequestURI() + " HTTP/1.1\r\nHost: " + u.Host +
		"\r\nUser-Agent: gpu-agent\r\nContent-Type: text/plain\r\nContent-Length: " + strconv.Itoa(MiB) + "\r\n\r\n"
	src := newRandom(MiB)

	start := time.Now()
	stop := phaseEnd(ctx, start, r.UploadFor, responseReserve)

	var conn net.Conn
	var br *bufio.Reader
	release := func() {}
	defer func() { release() }()

	var accepted int64
	var last time.Time
posts:
	for time.Now().Before(stop) {
		if conn == nil {
			c, err := r.openConn(ctx, u, nil)
			if err != nil {
				return uploadResult{}, fmt.Errorf("upload in 1 MiB POSTs: %w", err)
			}
			c.SetDeadline(stop)
			unwatch := context.AfterFunc(ctx, func() { c.Close() })
			conn, br = c, bufio.NewReader(c)
			release = func() { unwatch(); c.Close() }
		}
		_, werr := io.WriteString(conn, head)
		if werr == nil {
			_, werr = conn.Write(src.next(MiB))
		}
		// An early reply is already buffered, and a refusal that closed the
		// connection may still be readable.
		rep := readReply(br)
		switch {
		case ctx.Err() != nil:
			return uploadResult{}, fmt.Errorf("upload: %w", ctx.Err())
		case isTimeout(werr) || (werr == nil && isTimeout(rep.err)):
			break posts // the time limit cut the last request short
		case rep.err == nil && !ok2xx(rep.status):
			return uploadResult{}, rep.failure("upload in 1 MiB POSTs", rawURL)
		case werr != nil:
			return uploadResult{}, fmt.Errorf("upload in 1 MiB POSTs: %w", werr)
		case rep.err != nil:
			return uploadResult{}, rep.failure("upload in 1 MiB POSTs", rawURL)
		}
		accepted += MiB
		last = time.Now()
		if rep.close {
			release()
			release, conn = func() {}, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return uploadResult{}, fmt.Errorf("upload: %w", err)
	}
	if accepted == 0 {
		return uploadResult{}, errors.New("upload in 1 MiB POSTs: no request finished within the time limit")
	}
	return uploadResult{mbps: mbps(accepted, last.Sub(start)), bytes: accepted, method: UploadChunked}, nil
}

// openConn dials the server of u for a hand-written request, TLS included.
// With a meter, every byte written to the TCP connection is counted.
func (r Runner) openConn(ctx context.Context, u *url.URL, m *meter) (net.Conn, error) {
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	raw, err := r.dialer()(dctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return nil, err
	}
	conn := raw
	if m != nil {
		conn = &countingConn{Conn: raw, m: m}
	}
	if u.Scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: u.Hostname(), NextProtos: []string{"http/1.1"}})
		if err := tc.HandshakeContext(dctx); err != nil {
			raw.Close()
			return nil, err
		}
		conn = tc
	}
	return conn, nil
}

// reply is the status line of an answer, or why none could be read.
type reply struct {
	status int
	close  bool
	err    error
}

func readReply(br *bufio.Reader) reply {
	for {
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			return reply{err: err}
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, readBuffer))
		resp.Body.Close()
		if resp.StatusCode >= 100 && resp.StatusCode < 200 {
			continue // 100 Continue and the like: the answer is still to come
		}
		return reply{status: resp.StatusCode, close: resp.Close}
	}
}

func (rep reply) failure(phase, rawURL string) error {
	if rep.err != nil {
		return fmt.Errorf("%s: no answer: %w", phase, rep.err)
	}
	return fmt.Errorf("%s: POST %s: HTTP %d", phase, rawURL, rep.status)
}

func ok2xx(status int) bool { return status >= 200 && status <= 299 }

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// client is a fresh, direct HTTP/1.1 client for the download.
func (r Runner) client() (*http.Client, func()) {
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         r.dialer(),
		TLSHandshakeTimeout: dialTimeout,
		// A transparently gunzipped body would be timed on what it inflated to.
		DisableCompression:  true,
		MaxIdleConnsPerHost: 1,
		ReadBufferSize:      readBuffer,
	}
	return &http.Client{Transport: tr}, tr.CloseIdleConnections
}

func (r Runner) dialer() func(ctx context.Context, network, address string) (net.Conn, error) {
	if r.Dial != nil {
		return r.Dial
	}
	return (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
}

// phaseEnd is start+d, pulled in so that reserve is still left before ctx's
// deadline.
func phaseEnd(ctx context.Context, start time.Time, d, reserve time.Duration) time.Time {
	end := start.Add(d)
	if dl, ok := ctx.Deadline(); ok && dl.Add(-reserve).Before(end) {
		end = dl.Add(-reserve)
	}
	return end
}

// failure names why the download stopped: the caller cancelled, the phase ran
// out of time before any data, or the network error itself.
func failure(ctx context.Context, expired *atomic.Bool, phase string, err error) error {
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("%s: %w", phase, ctx.Err())
	case expired.Load():
		return fmt.Errorf("%s: no data within the time limit", phase)
	default:
		return fmt.Errorf("%s: %w", phase, err)
	}
}

// meter counts the bytes written to a connection and when.
type meter struct {
	mu      sync.Mutex
	written int64
	base    int64     // written when the body began: the TLS handshake and headers are not upload
	start   time.Time // the body's first byte
	last    time.Time // the last write to the connection
}

func (m *meter) wrote(n int) {
	if n <= 0 {
		return
	}
	now := time.Now()
	m.mu.Lock()
	m.written += int64(n)
	m.last = now
	m.mu.Unlock()
}

func (m *meter) begin() time.Time {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.start.IsZero() {
		m.start = now
		m.base = m.written
	}
	return m.start
}

func (m *meter) span() (int64, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.start.IsZero() || m.last.Before(m.start) {
		return 0, 0
	}
	return m.written - m.base, m.last.Sub(m.start)
}

type countingConn struct {
	net.Conn
	m *meter
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.m.wrote(n)
	return n, err
}

// random is incompressible upload data: one reusable buffer, refilled from
// crypto/rand each time it has been used up.
type random struct {
	buf []byte
	pos int
}

func newRandom(size int) *random { return &random{buf: make([]byte, size), pos: size} }

// next returns the next n random bytes, n at most the buffer's size. They are
// valid until the next call.
func (g *random) next(n int) []byte {
	if len(g.buf)-g.pos < n {
		rand.Read(g.buf)
		g.pos = 0
	}
	b := g.buf[g.pos : g.pos+n]
	g.pos += n
	return b
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// mbps is megabits per second, rounded to 0.1. A transfer too quick for the
// clock is timed as one microsecond rather than divided by zero.
func mbps(bytes int64, d time.Duration) float64 {
	if d < time.Microsecond {
		d = time.Microsecond
	}
	return round1(float64(bytes) * 8 / d.Seconds() / 1e6)
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }
