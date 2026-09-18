// Package agenterrors keeps the agent's recent problems -- everything that
// stopped, or stops, its machine being listed -- in errors.json in the data
// directory, newest first and at most control.MaxAgentErrors of them, and
// hands them to every capability report so the marketplace (and its staff)
// see why a machine is not listed without asking the host.
//
// A problem is either a condition (Raise ... Resolve: active while it lasts,
// kept as resolved until newer ones push it out) or an event (Note: a failed
// update, a rental that did not start), which is never active. The file is
// read and written on every change, so the agent's own command-line runs
// (`check --boot`) add to the same list the daemon reports.
package agenterrors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/serverroom/gpu-marketplace/internal/control"
)

// Bounds on what one problem carries (CONTRACT-ops A1).
const (
	MaxMessage = 300
	MaxDetail  = 2000
)

// Path is where the problems are kept.
func Path(dataDir string) string { return filepath.Join(dataDir, "errors.json") }

// Problem is one active problem of an area, for Sync.
type Problem struct {
	Message string
	Detail  string
}

// Log is the problem list on disk.
type Log struct {
	path string
	now  func() time.Time

	mu    sync.Mutex
	onNew func()
}

// Open is the log in dataDir.
func Open(dataDir string) *Log { return New(Path(dataDir), nil) }

// New is the log kept at path; now is the clock (nil: time.Now).
func New(path string, now func() time.Time) *Log {
	if now == nil {
		now = time.Now
	}
	return &Log{path: path, now: now}
}

// OnNew sets what is told when a problem is new -- not one that was already
// active -- so the agent can report it at once. It runs outside the log's
// lock.
func (l *Log) OnNew(f func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onNew = f
}

var areas = map[string]bool{
	control.AreaSetup: true, control.AreaPreflight: true, control.AreaTestBoot: true, control.AreaUpdate: true,
	control.AreaTunnel: true, control.AreaReport: true, control.AreaRental: true, control.AreaRegister: true,
	control.AreaAgent: true,
}

// clean makes s one printable line of at most max characters.
func cleanLine(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || (!unicode.IsPrint(r) && r != ' ') {
			if unicode.IsSpace(r) {
				return ' '
			}
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max-3]) + "..."
	}
	return s
}

// cleanDetail keeps lines but drops other control characters, and keeps the
// END of a long detail: the last lines of a failing command are the ones
// that say why.
func cleanDetail(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t' || r == '\r':
			return ' '
		case r == utf8.RuneError || !unicode.IsPrint(r):
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n > MaxDetail {
		s = "..." + string([]rune(s)[n-MaxDetail+3:])
	}
	return s
}

func (l *Log) load() []control.AgentError {
	data, err := os.ReadFile(l.path)
	if err != nil {
		return nil
	}
	var entries []control.AgentError
	if json.Unmarshal(data, &entries) != nil {
		return nil
	}
	return entries
}

func (l *Log) save(entries []control.AgentError) {
	if len(entries) > control.MaxAgentErrors {
		entries = entries[:control.MaxAgentErrors]
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0700); err != nil {
		return
	}
	tmp := l.path + ".tmp"
	if os.WriteFile(tmp, data, 0600) != nil {
		return
	}
	if os.Rename(tmp, l.path) != nil {
		_ = os.Remove(tmp)
	}
}

// change applies f to the list under the lock and saves it; fresh says a new
// problem went in, and onNew is told after the lock is released.
func (l *Log) change(f func(entries []control.AgentError) ([]control.AgentError, bool)) bool {
	l.mu.Lock()
	before := l.load()
	was, _ := json.Marshal(before)
	entries, fresh := f(before)
	if now, _ := json.Marshal(entries); string(now) != string(was) {
		l.save(entries)
	}
	notify := l.onNew
	l.mu.Unlock()
	if fresh && notify != nil {
		notify()
	}
	return fresh
}

func entry(at int64, area, message, detail string, active bool) control.AgentError {
	if !areas[area] {
		area = control.AreaAgent
	}
	return control.AgentError{At: at, Area: area, Message: cleanLine(message, MaxMessage), Detail: cleanDetail(detail), Active: active}
}

// Raise records a condition that stops the machine now. One already active
// with the same area and message keeps its time (and takes the new detail);
// otherwise it goes in first, and fresh says so.
func (l *Log) Raise(area, message, detail string) (fresh bool) {
	e := entry(l.now().Unix(), area, message, detail, true)
	if e.Message == "" {
		return false
	}
	return l.change(func(entries []control.AgentError) ([]control.AgentError, bool) {
		for i := range entries {
			if entries[i].Active && entries[i].Area == e.Area && entries[i].Message == e.Message {
				entries[i].Detail = e.Detail
				return entries, false
			}
		}
		return append([]control.AgentError{e}, entries...), true
	})
}

// Note records an event that happened now: never active.
func (l *Log) Note(area, message, detail string) bool {
	return l.NoteAt(l.now().Unix(), area, message, detail)
}

// NoteAt records an event that happened at at. The same event (time, area and
// message) is recorded once, so an event read back from elsewhere -- the last
// update's record, at every start -- is not added again.
func (l *Log) NoteAt(at int64, area, message, detail string) bool {
	e := entry(at, area, message, detail, false)
	if e.Message == "" {
		return false
	}
	return l.change(func(entries []control.AgentError) ([]control.AgentError, bool) {
		for _, x := range entries {
			if x.At == e.At && x.Area == e.Area && x.Message == e.Message {
				return entries, false
			}
		}
		out := append([]control.AgentError{e}, entries...)
		// Newest first, by time: an event read back late goes where it belongs.
		for i := 0; i+1 < len(out) && out[i].At < out[i+1].At; i++ {
			out[i], out[i+1] = out[i+1], out[i]
		}
		return out, true
	})
}

// Resolve marks an area's active problems resolved: all of them, or only the
// one with message when it is not "".
func (l *Log) Resolve(area, message string) {
	if message != "" {
		message = cleanLine(message, MaxMessage)
	}
	l.change(func(entries []control.AgentError) ([]control.AgentError, bool) {
		for i := range entries {
			if entries[i].Active && entries[i].Area == area && (message == "" || entries[i].Message == message) {
				entries[i].Active = false
			}
		}
		return entries, false
	})
}

// Sync makes active exactly an area's current problems: each is raised (a new
// one goes in first), and every other active problem of the area is resolved.
func (l *Log) Sync(area string, active []Problem) {
	now := l.now().Unix()
	l.change(func(entries []control.AgentError) ([]control.AgentError, bool) {
		want := map[string]control.AgentError{}
		var order []string
		for _, p := range active {
			e := entry(now, area, p.Message, p.Detail, true)
			if e.Message == "" {
				continue
			}
			if _, dup := want[e.Message]; !dup {
				order = append(order, e.Message)
			}
			want[e.Message] = e
		}
		for i := range entries {
			if !entries[i].Active || entries[i].Area != area {
				continue
			}
			if e, ok := want[entries[i].Message]; ok {
				entries[i].Detail = e.Detail
				delete(want, entries[i].Message)
			} else {
				entries[i].Active = false
			}
		}
		var fresh []control.AgentError
		for _, m := range order {
			if e, ok := want[m]; ok {
				fresh = append(fresh, e)
			}
		}
		return append(fresh, entries...), len(fresh) > 0
	})
}

// Entries are the problems, newest first.
func (l *Log) Entries() []control.AgentError {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := l.load()
	if len(entries) > control.MaxAgentErrors {
		entries = entries[:control.MaxAgentErrors]
	}
	return entries
}

// Active are the problems that still stop the machine.
func (l *Log) Active() []control.AgentError {
	var out []control.AgentError
	for _, e := range l.Entries() {
		if e.Active {
			out = append(out, e)
		}
	}
	return out
}
