// Package wiretest is an in-process Ethernet for tests: named ends
// ("machine/netdev") joined into segments -- two ends are a cable, more are a
// switch -- with interconnect.PacketConn sockets on them. A frame written on
// one end reaches every other open end of its segment (and its own socket, as
// an outgoing copy, like a raw socket), unless that direction was cut.
package wiretest

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/interconnect"
)

// Wire is the whole fake network.
type Wire struct {
	mu   sync.Mutex
	segs map[string]*segment // end -> its segment
	cut  map[[2]string]bool  // from, to
	open map[string]*conn    // end -> its open socket
}

type segment struct{ ends []string }

// New returns an empty wire.
func New() *Wire {
	return &Wire{segs: map[string]*segment{}, cut: map[[2]string]bool{}, open: map[string]*conn{}}
}

// Join puts ends on one segment: two ends are a cable, more a switch.
func (w *Wire) Join(ends ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	seg := &segment{ends: append([]string(nil), ends...)}
	for _, e := range ends {
		w.segs[e] = seg
	}
}

// Cut stops frames from one end reaching another (a one-way cable).
func (w *Wire) Cut(from, to string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cut[[2]string{from, to}] = true
}

// Opener is machine's interconnect.Opener: netdev n opens end "machine/n".
func (w *Wire) Opener(machine string) interconnect.Opener {
	return func(netdev string) (interconnect.PacketConn, error) { return w.Open(machine + "/" + netdev) }
}

// Open opens a socket on an end. An end on no segment is a port with nothing
// plugged in: it opens, and hears nothing.
func (w *Wire) Open(end string) (interconnect.PacketConn, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, busy := w.open[end]; busy {
		return nil, errors.New("wiretest: " + end + " is already open")
	}
	c := &conn{w: w, end: end, in: make(chan packet, 4096)}
	w.open[end] = c
	return c, nil
}

// Opened lists the ends with a socket open now.
func (w *Wire) Opened() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for e := range w.open {
		out = append(out, e)
	}
	return out
}

type packet struct {
	data     []byte
	outgoing bool
}

type conn struct {
	w      *Wire
	end    string
	in     chan packet
	closed bool
}

func (c *conn) WriteFrame(frame []byte) error {
	w := c.w
	w.mu.Lock()
	defer w.mu.Unlock()
	if c.closed {
		return errors.New("wiretest: closed")
	}
	data := append([]byte(nil), frame...)
	select {
	case c.in <- packet{data: data, outgoing: true}:
	default:
	}
	seg := w.segs[c.end]
	if seg == nil {
		return nil
	}
	for _, e := range seg.ends {
		if e == c.end || w.cut[[2]string{c.end, e}] {
			continue
		}
		if o := w.open[e]; o != nil && !o.closed {
			select {
			case o.in <- packet{data: data}:
			default:
			}
		}
	}
	return nil
}

func (c *conn) ReadFrame(buf []byte, deadline time.Time) (int, bool, error) {
	wait := time.Until(deadline)
	if wait <= 0 {
		select {
		case p := <-c.in:
			return copy(buf, p.data), p.outgoing, nil
		default:
			return 0, false, interconnect.ErrTimeout
		}
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case p := <-c.in:
		return copy(buf, p.data), p.outgoing, nil
	case <-t.C:
		return 0, false, interconnect.ErrTimeout
	}
}

func (c *conn) Close() error {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	c.closed = true
	delete(c.w.open, c.end)
	return nil
}

// Machine is the name part of an end.
func Machine(end string) string {
	m, _, _ := strings.Cut(end, "/")
	return m
}
