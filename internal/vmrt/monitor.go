package vmrt

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Monitor is a Host that can talk to a VM over its QMP socket. Only the wider
// layout (layout.go) needs it: it starts the VM paused, and QMP is how the
// agent learns each vCPU's thread and lets the guest run.
type Monitor interface {
	// QMP opens the VM's QMP socket, sends each command in turn and returns
	// each one's answer (the JSON of its "return"). A command "event:NAME" is
	// not sent: it waits for the VM to say that event.
	QMP(socket string, commands ...string) ([]string, error)
}

// qmpTimeout bounds one conversation with a VM's monitor.
const qmpTimeout = 20 * time.Second

// qmpLine is one line a QMP server says: its greeting, an event, or the answer
// to a command.
type qmpLine struct {
	QMP    json.RawMessage `json:"QMP"`
	Event  string          `json:"event"`
	Return json.RawMessage `json:"return"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error"`
}

// QMP talks to a VM's monitor (Monitor).
func (OSHost) QMP(socket string, commands ...string) ([]string, error) {
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(qmpTimeout))
	lines := bufio.NewReader(conn)
	read := func() (qmpLine, error) {
		var l qmpLine
		raw, err := lines.ReadBytes('\n')
		if err != nil {
			return l, err
		}
		if err := json.Unmarshal(raw, &l); err != nil {
			return l, fmt.Errorf("the monitor said %q", strings.TrimSpace(string(raw)))
		}
		return l, nil
	}
	// answer reads up to a command's answer. What the VM says by itself
	// meanwhile is kept: the event a command causes may come before the
	// command's own answer (a reset's RESET does, since QEMU answers commands
	// from a coroutine) or after it, and a wait for it has to hold either way.
	said := map[string]int{}
	answer := func(cmd string) (string, error) {
		for {
			l, err := read()
			switch {
			case err != nil:
				return "", fmt.Errorf("%s: %w", cmd, err)
			case l.Error != nil:
				return "", fmt.Errorf("%s: %s", cmd, l.Error.Desc)
			case l.Return != nil:
				return string(l.Return), nil
			case l.Event != "":
				said[l.Event]++
			}
		}
	}
	if l, err := read(); err != nil {
		return nil, err
	} else if l.QMP == nil {
		return nil, errors.New("the socket did not greet as a QMP monitor")
	}
	if _, err := fmt.Fprintf(conn, "{\"execute\":\"qmp_capabilities\"}\n"); err != nil {
		return nil, err
	}
	if _, err := answer("qmp_capabilities"); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(commands))
	for _, cmd := range commands {
		if event, ok := strings.CutPrefix(cmd, "event:"); ok {
			for said[event] == 0 {
				l, err := read()
				if err != nil {
					return out, fmt.Errorf("waiting for the VM's %s event: %w", event, err)
				}
				if l.Event != "" {
					said[l.Event]++
				}
			}
			said[event]--
			out = append(out, "")
			continue
		}
		req, _ := json.Marshal(map[string]string{"execute": cmd})
		if _, err := conn.Write(append(req, '\n')); err != nil {
			return out, fmt.Errorf("%s: %w", cmd, err)
		}
		ans, err := answer(cmd)
		if err != nil {
			return out, err
		}
		out = append(out, ans)
	}
	return out, nil
}
