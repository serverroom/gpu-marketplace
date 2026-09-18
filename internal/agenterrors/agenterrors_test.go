package agenterrors

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
)

func testLog(t *testing.T) (*Log, *int64) {
	t.Helper()
	clock := int64(1789000000)
	l := New(filepath.Join(t.TempDir(), "errors.json"), func() time.Time { return time.Unix(clock, 0) })
	return l, &clock
}

// A condition is active while it lasts, reported once as new, and kept as
// resolved afterwards.
func TestRaiseAndResolve(t *testing.T) {
	l, clock := testLog(t)
	news := 0
	l.OnNew(func() { news++ })
	if !l.Raise(control.AreaTunnel, "the relay cannot be reached", "ssh: connect to host relay port 22: timed out") {
		t.Fatal("a first problem was not new")
	}
	*clock += 60
	if l.Raise(control.AreaTunnel, "the relay cannot be reached", "ssh: still timed out") {
		t.Error("the same active problem counted as new")
	}
	e := l.Entries()
	if len(e) != 1 || !e[0].Active || e[0].At != 1789000000 || e[0].Detail != "ssh: still timed out" {
		t.Fatalf("entries = %+v", e)
	}
	l.Resolve(control.AreaTunnel, "")
	if e := l.Entries(); len(e) != 1 || e[0].Active {
		t.Fatalf("after resolve = %+v", e)
	}
	// Back again: a new problem, next to the resolved one.
	*clock += 60
	if !l.Raise(control.AreaTunnel, "the relay cannot be reached", "") || len(l.Entries()) != 2 || news != 2 {
		t.Errorf("entries %+v news %d", l.Entries(), news)
	}
}

// Sync makes an area's active problems exactly the current ones.
func TestSync(t *testing.T) {
	l, clock := testLog(t)
	l.Sync(control.AreaPreflight, []Problem{{Message: "KVM is missing"}, {Message: "no base image"}})
	*clock += 10
	l.Sync(control.AreaPreflight, []Problem{{Message: "no base image"}, {Message: "not 40 GB free"}})
	active := map[string]bool{}
	all := map[string]bool{}
	for _, e := range l.Entries() {
		all[e.Message] = true
		if e.Active {
			active[e.Message] = true
		}
	}
	if !all["KVM is missing"] || active["KVM is missing"] || !active["no base image"] || !active["not 40 GB free"] {
		t.Errorf("entries = %+v", l.Entries())
	}
	if l.Entries()[0].Message != "not 40 GB free" {
		t.Errorf("the newest is not first: %+v", l.Entries())
	}
	l.Sync(control.AreaPreflight, nil)
	if len(l.Active()) != 0 {
		t.Errorf("active after a clean sync: %+v", l.Active())
	}
}

// Events are never active, and one read back again is not added twice.
func TestNoteAt(t *testing.T) {
	l, _ := testLog(t)
	if !l.NoteAt(1788990000, control.AreaUpdate, "the update to v0.2.1 was rolled back", "") {
		t.Fatal("not recorded")
	}
	if l.NoteAt(1788990000, control.AreaUpdate, "the update to v0.2.1 was rolled back", "") {
		t.Error("the same event recorded twice")
	}
	l.Raise(control.AreaSetup, "the base image did not build", "")
	l.NoteAt(1788980000, control.AreaRental, "an older event", "")
	e := l.Entries()
	if len(e) != 3 || e[0].Area != control.AreaSetup || e[2].Message != "an older event" || e[1].Active {
		t.Errorf("entries = %+v", e)
	}
}

// At most 20 are kept, newest first; the words are one printable line within
// bounds, and a long detail keeps its end.
func TestBounds(t *testing.T) {
	l, clock := testLog(t)
	for i := 0; i < 25; i++ {
		*clock++
		l.Note(control.AreaRental, fmt.Sprintf("rental %d did not start", i), "")
	}
	e := l.Entries()
	if len(e) != control.MaxAgentErrors || e[0].Message != "rental 24 did not start" {
		t.Fatalf("%d entries, first %q", len(e), e[0].Message)
	}
	long := strings.Repeat("x", 400) + "\x1b[31m\nsecond line\ttab"
	detail := strings.Repeat("early line\n", 300) + "the last line says why"
	l.Raise("no-such-area", long, detail)
	got := l.Entries()[0]
	if got.Area != control.AreaAgent || len([]rune(got.Message)) != MaxMessage || strings.ContainsAny(got.Message, "\n\t\x1b") {
		t.Errorf("message %q (area %s)", got.Message, got.Area)
	}
	if len([]rune(got.Detail)) != MaxDetail || !strings.HasSuffix(got.Detail, "the last line says why") {
		t.Errorf("detail ends %q (%d)", got.Detail[len(got.Detail)-30:], len([]rune(got.Detail)))
	}
}

// Another process's additions (a `check --boot` run) are in the list the
// daemon reads, and the file is private.
func TestSharedFile(t *testing.T) {
	dir := t.TempDir()
	a, b := Open(dir), Open(dir)
	a.Raise(control.AreaPreflight, "KVM is missing", "")
	b.Note(control.AreaTestBoot, "the test boot run by hand failed", "")
	if e := a.Entries(); len(e) != 2 {
		t.Errorf("entries = %+v", e)
	}
	if fi, err := os.Stat(Path(dir)); err != nil || (fi.Mode().Perm()&0077 != 0 && os.PathSeparator == '/') {
		t.Errorf("errors.json: %v %v", fi, err)
	}
}
