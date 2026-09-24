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
	f := &Forwarder{ln: ln, target: target, conns: map[net.Conn]struct{}{}, done: make(chan struct{})}
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
	defer func() {
		client.Close()
		server.Close()
		f.track(client, false)
		f.track(server, false)
	}()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { io.Copy(server, client); server.Close(); wg.Done() }()
	go func() { io.Copy(client, server); client.Close(); wg.Done() }()
	wg.Wait()
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
