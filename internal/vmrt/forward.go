package vmrt

import (
	"io"
	"net"
	"sync"
	"time"
)

// Forwarder carries the renter's SSH from the agent's loopback port -- which the
// relay tunnel exposes as the listing's ssh slot -- to the guest's port 22. It
// is the renter's only way in: the guest has no route from the LAN.
type Forwarder struct {
	ln     net.Listener
	target string
	dial   func() (net.Conn, error)
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	done   chan struct{}
	// The renter's connections, for the staff's view of a live rental: see
	// Sessions. Kept in memory, so an agent restart starts them afresh.
	stats SessionStats
	open  map[net.Conn]time.Time // client side, since when
}

// SessionMin is how long a connection must stay open to count as a session: a
// port probe or a bare banner check is shorter, a person's SSH login is not.
var SessionMin = 10 * time.Second

// SessionStats is what the forwarder has seen of the renter's SSH. It sees
// connections, not logins: whether a key was accepted is the guest's sshd's
// business. A connection counts once it has been open SessionMin.
type SessionStats struct {
	Active         int   `json:"active"`                     // sessions open now
	Sessions       int   `json:"sessions"`                   // sessions since the forward opened
	LastStartedAt  int64 `json:"last_started_at,omitempty"`  // the latest session's start
	LastEndedAt    int64 `json:"last_ended_at,omitempty"`    // the latest session's end
	BytesToGuest   int64 `json:"bytes_to_guest,omitempty"`   // through the forward, finished sessions
	BytesFromGuest int64 `json:"bytes_from_guest,omitempty"` // and the other way
}

// StartForward listens on listen (a loopback address) and forwards to target.
func StartForward(listen, target string) (*Forwarder, error) {
	return StartForwardVia(listen, target, nil)
}

// StartForwardVia is StartForward with the connection to target opened by
// dial: on Windows the guest is inside WSL, which the agent reaches through
// wsl.exe, not over a route. nil dials target over TCP.
func StartForwardVia(listen, target string, dial func(addr string) (net.Conn, error)) (*Forwarder, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	f := &Forwarder{ln: ln, target: target, conns: map[net.Conn]struct{}{}, open: map[net.Conn]time.Time{}, done: make(chan struct{})}
	if dial != nil {
		f.dial = func() (net.Conn, error) { return dial(target) }
	}
	go f.serve()
	return f, nil
}

func (f *Forwarder) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			select {
			case <-f.done:
				return
			default:
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}
		go f.pipe(c)
	}
}

func (f *Forwarder) track(c net.Conn, add bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if add {
		f.conns[c] = struct{}{}
	} else {
		delete(f.conns, c)
	}
}

func (f *Forwarder) pipe(client net.Conn) {
	var server net.Conn
	var err error
	if f.dial != nil {
		server, err = f.dial()
	} else {
		server, err = net.DialTimeout("tcp", f.target, 10*time.Second)
	}
	if err != nil {
		client.Close()
		return
	}
	f.track(client, true)
	f.track(server, true)
	start := time.Now()
	f.mu.Lock()
	f.open[client] = start
	f.mu.Unlock()
	var toGuest, fromGuest int64
	defer func() {
		client.Close()
		server.Close()
		f.track(client, false)
		f.track(server, false)
		f.ended(client, start, toGuest, fromGuest)
	}()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { toGuest, _ = io.Copy(server, client); server.Close(); wg.Done() }()
	go func() { fromGuest, _ = io.Copy(client, server); client.Close(); wg.Done() }()
	wg.Wait()
}

// ended records a finished connection: a session when it lasted SessionMin.
func (f *Forwarder) ended(client net.Conn, start time.Time, toGuest, fromGuest int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.open, client)
	end := time.Now()
	if end.Sub(start) < SessionMin {
		return
	}
	f.stats.Sessions++
	f.stats.BytesToGuest += toGuest
	f.stats.BytesFromGuest += fromGuest
	if s := start.Unix(); s > f.stats.LastStartedAt {
		f.stats.LastStartedAt = s
	}
	f.stats.LastEndedAt = end.Unix()
}

// Sessions is the renter's SSH as the forwarder has seen it: sessions open now
// (connections open at least SessionMin) and the ones that ended.
func (f *Forwarder) Sessions() SessionStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.stats
	now := time.Now()
	for _, since := range f.open {
		if now.Sub(since) < SessionMin {
			continue
		}
		s.Active++
		if st := since.Unix(); st > s.LastStartedAt {
			s.LastStartedAt = st
		}
	}
	return s
}

// Stop closes the listener and every open connection: a rental that has ended
// leaves no session running into a VM that no longer exists.
func (f *Forwarder) Stop() {
	close(f.done)
	f.ln.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.conns {
		c.Close()
	}
}
