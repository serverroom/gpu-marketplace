package interconnect

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// Everything that puts frames on a ConnectX-7 cable -- the cable check, the
// peer announcements, the pair test's handshake -- goes through the same
// routine: record each port's link state and IPv6 setting, turn IPv6 off (so
// the kernel gives the port no link-local address and sends no neighbour
// discovery), bring the link up with no address at all, wait for carrier,
// exchange frames, and put both settings back exactly as they were.

// Timings, overridable by tests.
var (
	FrameInterval = 250 * time.Millisecond
	// SecondUnit is what one "second" of a check lasts; tests shrink it.
	SecondUnit  = time.Second
	CarrierWait = 5 * time.Second
	carrierPoll = 100 * time.Millisecond
)

// FramePort is one port frames are exchanged on.
type FramePort struct {
	Netdev string
	BDF    string
	MAC    string // lower case
}

// portState is what bringUp changed on a port, for restore.
type portState struct {
	port        FramePort
	wasUp       bool
	raised      bool
	ipv6Path    string
	ipv6Orig    string
	ipv6Changed bool
}

const iffUp = 0x1

func adminUp(h vmrt.Host, netdev string) (bool, error) {
	raw := strings.TrimSpace(vmrt.NetAttr(h, netdev, "flags"))
	if raw == "" {
		return false, fmt.Errorf("the link state of %s cannot be read", netdev)
	}
	flags, err := strconv.ParseUint(raw, 0, 64)
	if err != nil {
		return false, fmt.Errorf("the link state of %s cannot be read (%q)", netdev, raw)
	}
	return flags&iffUp != 0, nil
}

// bringUp prepares one port. Whatever it changed before an error is recorded
// in the state it returns, so restore undoes exactly that.
func bringUp(h vmrt.Host, p FramePort) (*portState, error) {
	st := &portState{port: p, ipv6Path: "/proc/sys/net/ipv6/conf/" + p.Netdev + "/disable_ipv6"}
	up, err := adminUp(h, p.Netdev)
	if err != nil {
		return st, err
	}
	st.wasUp = up
	if data, err := h.ReadFile(st.ipv6Path); err == nil {
		st.ipv6Orig = strings.TrimSpace(string(data))
		if st.ipv6Orig != "1" {
			if err := h.WriteFile(st.ipv6Path, []byte("1"), 0644); err != nil {
				return st, fmt.Errorf("turn IPv6 off on %s: %w", p.Netdev, err)
			}
			st.ipv6Changed = true
		}
	}
	if !st.wasUp {
		if err := h.Run("ip", "link", "set", "dev", p.Netdev, "up"); err != nil {
			return st, fmt.Errorf("bring %s up: %w", p.Netdev, err)
		}
		st.raised = true
	}
	return st, nil
}

// restore puts a port back: down again if it was down, IPv6 as it was. The
// link goes down first, so re-enabling IPv6 adds no address to a port that
// had none.
func (st *portState) restore(h vmrt.Host) []string {
	var problems []string
	if st.raised {
		if err := h.Run("ip", "link", "set", "dev", st.port.Netdev, "down"); err != nil {
			problems = append(problems, fmt.Sprintf("bring %s back down: %v", st.port.Netdev, err))
		}
	}
	if st.ipv6Changed {
		if err := h.WriteFile(st.ipv6Path, []byte(st.ipv6Orig), 0644); err != nil {
			problems = append(problems, fmt.Sprintf("restore IPv6 on %s: %v", st.port.Netdev, err))
		}
	}
	return problems
}

func carrier(h vmrt.Host, netdev string) bool { return vmrt.NetAttr(h, netdev, "carrier") == "1" }

func speedMbps(h vmrt.Host, netdev string) int {
	s, err := strconv.Atoi(vmrt.NetAttr(h, netdev, "speed"))
	if err != nil || s < 0 {
		return 0
	}
	return s
}

// linkResult is what the routine read off one port at the end of its window.
type linkResult struct {
	Carrier   bool
	SpeedMbps int
}

// Session is a set of ports brought up for frames.
type Session struct {
	h       vmrt.Host
	open    Opener
	journal string
	// wrote: this session wrote the journal, so it is this session's to
	// remove. A journal left by an earlier run is never touched here.
	wrote  bool
	states []*portState
	conns  []PacketConn
}

// journalEntry is one port's original state, written to disk before anything
// changes, so an agent killed mid-run puts the port back at its next start
// (RecoverPorts) instead of leaving it up with IPv6 off.
type journalEntry struct {
	Netdev   string `json:"netdev"`
	WasUp    bool   `json:"was_up"`
	IPv6Path string `json:"ipv6_path,omitempty"`
	IPv6Orig string `json:"ipv6_orig,omitempty"`
}

// journal is the record on disk: the ports' original state, and the boot it
// belongs to -- a restart puts every port back by itself, so a record from an
// earlier boot has nothing left to restore.
type journal struct {
	BootID string         `json:"boot_id,omitempty"`
	Ports  []journalEntry `json:"ports"`
}

// JournalPath is where a run in progress records the ports' original state.
func JournalPath(dataDir string) string { return filepath.Join(dataDir, "frames-journal.json") }

// ErrUnfinishedRun: an earlier cable check's record of the ports is still on
// disk, so their original state is not what they show now; nothing may change
// them until that record has been played back (RecoverPorts).
var ErrUnfinishedRun = errors.New("a cable check that did not finish left this machine's ConnectX-7 ports changed, and they could not all be put back; restart this machine to reset them")

// bootID is this boot's id ("" when it cannot be read).
func bootID(h vmrt.Host) string {
	data, err := h.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func writeJournal(h vmrt.Host, path string, entries []journalEntry) error {
	data, err := json.Marshal(journal{BootID: bootID(h), Ports: entries})
	if err != nil {
		return err
	}
	if err := h.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return h.WriteFile(path, data, 0600)
}

// readJournal reads the record; ok is false when it cannot be read.
func readJournal(data []byte) (journal, bool) {
	var j journal
	if json.Unmarshal(data, &j) == nil {
		return j, true
	}
	// The first form of the record: the port list alone.
	if json.Unmarshal(data, &j.Ports) == nil {
		return j, true
	}
	return journal{}, false
}

// RecoverPorts puts back ports a run that never finished left changed, and
// removes the record once everything is back. Call it only while holding the
// frame lock (TryLock), so no run is in progress: it restores recorded
// originals. A record from an earlier boot is dropped -- the restart reset the
// ports, and whatever has set them up since is left alone.
func RecoverPorts(h vmrt.Host, dataDir string) []string {
	path := JournalPath(dataDir)
	data, err := h.ReadFile(path)
	if err != nil {
		return nil
	}
	j, ok := readJournal(data)
	if !ok {
		_ = h.Remove(path)
		return []string{"an unreadable record of a cable check was discarded"}
	}
	if now := bootID(h); j.BootID != "" && now != "" && j.BootID != now {
		_ = h.Remove(path)
		return nil
	}
	var problems []string
	for _, e := range j.Ports {
		if !ValidNetdev(e.Netdev) {
			continue
		}
		st := &portState{port: FramePort{Netdev: e.Netdev}, raised: !e.WasUp, ipv6Path: e.IPv6Path, ipv6Orig: e.IPv6Orig,
			ipv6Changed: e.IPv6Path != "" && e.IPv6Orig != "" && e.IPv6Path == "/proc/sys/net/ipv6/conf/"+e.Netdev+"/disable_ipv6"}
		if !h.Exists("/sys/class/net/" + e.Netdev) {
			continue
		}
		problems = append(problems, st.restore(h)...)
	}
	if len(problems) == 0 {
		_ = h.Remove(path)
	}
	return problems
}

// ValidNetdev reports whether s can be an interface name.
func ValidNetdev(s string) bool { return netdevPattern.MatchString(s) }

var netdevPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)

// Open brings every port up (none left changed on error) and opens a raw
// socket on each, then waits up to CarrierWait for carrier on all of them.
// With a journal path, the ports' original state is on disk before anything
// changes and is removed once Close has put everything back; a record an
// earlier run left there means the ports are not in their original state, so
// Open refuses (ErrUnfinishedRun) rather than record the changed state as the
// original. The caller plays such a record back first (RecoverPorts).
func Open(h vmrt.Host, open Opener, ports []FramePort, journal string) (*Session, error) {
	s := &Session{h: h, open: open, journal: journal}
	fail := func(err error) (*Session, error) {
		if problems := s.Close(); len(problems) > 0 {
			err = fmt.Errorf("%w; and %s", err, strings.Join(problems, "; "))
		}
		return nil, err
	}
	for _, p := range ports {
		if !ValidNetdev(p.Netdev) {
			return fail(fmt.Errorf("%q is not an interface name", p.Netdev))
		}
	}
	if journal != "" {
		if h.Exists(journal) {
			return nil, ErrUnfinishedRun
		}
		var entries []journalEntry
		for _, p := range ports {
			up, err := adminUp(h, p.Netdev)
			if err != nil {
				return fail(err)
			}
			e := journalEntry{Netdev: p.Netdev, WasUp: up}
			path := "/proc/sys/net/ipv6/conf/" + p.Netdev + "/disable_ipv6"
			if data, err := h.ReadFile(path); err == nil {
				e.IPv6Path, e.IPv6Orig = path, strings.TrimSpace(string(data))
			}
			entries = append(entries, e)
		}
		if err := writeJournal(h, journal, entries); err != nil {
			_ = h.Remove(journal)
			return fail(fmt.Errorf("record the ports' state: %w", err))
		}
		s.wrote = true
	}
	for _, p := range ports {
		st, err := bringUp(h, p)
		s.states = append(s.states, st)
		if err != nil {
			return fail(err)
		}
	}
	for _, p := range ports {
		c, err := open(p.Netdev)
		if err != nil {
			return fail(fmt.Errorf("open %s for raw frames: %w", p.Netdev, err))
		}
		s.conns = append(s.conns, c)
	}
	for waited := time.Duration(0); waited < CarrierWait; waited += carrierPoll {
		all := true
		for _, p := range ports {
			if !carrier(h, p.Netdev) {
				all = false
				break
			}
		}
		if all {
			break
		}
		h.Sleep(carrierPoll)
	}
	return s, nil
}

// Close closes the sockets and restores every port, returning what could not
// be restored.
func (s *Session) Close() []string {
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.conns = nil
	var problems []string
	for _, st := range s.states {
		problems = append(problems, st.restore(s.h)...)
	}
	s.states = nil
	if s.wrote && len(problems) == 0 {
		_ = s.h.Remove(s.journal)
		s.wrote = false
	}
	return problems
}

// Exchange runs every port at once until window has passed or stop (checked
// at least every FrameInterval) says to. On each port, send(port, seq) is
// broadcast every FrameInterval (nil sends nothing) and every frame received
// from elsewhere goes to recv. The answer is each port's carrier and speed at
// the end.
func (s *Session) Exchange(window time.Duration, send func(p FramePort, seq int) []byte, recv func(p FramePort, f Frame), stop func() bool) []linkResult {
	deadline := time.Now().Add(window)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := range s.conns {
		wg.Add(1)
		go func(p FramePort, c PacketConn) {
			defer wg.Done()
			buf := make([]byte, 16384)
			next := time.Now()
			for seq := 0; ; {
				now := time.Now()
				if !now.Before(deadline) || (stop != nil && stop()) {
					return
				}
				if !now.Before(next) {
					if frame := send(p, seq); frame != nil {
						_ = c.WriteFrame(frame) // a port without carrier cannot send; its silence is the answer
					}
					seq++
					next = next.Add(FrameInterval)
				}
				until := next
				if deadline.Before(until) {
					until = deadline
				}
				n, outgoing, err := c.ReadFrame(buf, until)
				if errors.Is(err, ErrTimeout) || outgoing {
					continue
				}
				if err != nil {
					return
				}
				if f, ok := ParseFrame(buf[:n]); ok {
					mu.Lock()
					recv(p, f)
					mu.Unlock()
				}
			}
		}(s.states[i].port, s.conns[i])
	}
	wg.Wait()
	out := make([]linkResult, len(s.states))
	for i, st := range s.states {
		out[i] = linkResult{Carrier: carrier(s.h, st.port.Netdev), SpeedMbps: speedMbps(s.h, st.port.Netdev)}
	}
	return out
}
