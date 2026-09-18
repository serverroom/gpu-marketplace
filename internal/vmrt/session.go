package vmrt

import (
	"regexp"
	"strings"
)

// A desktop is more than its display server and shell: every program a person
// runs in a graphical login -- a browser's GPU process, a video player, a
// notebook started from a terminal on the desktop -- may hold the GPU, and a
// DGX Spark used as a desktop nearly always has some. Naming processes is not
// enough, so a holder also counts as the desktop when systemd places it in a
// graphical login session, or when it descends from the display manager.

var (
	// sessionScopePattern is a login session's scope: user-<uid>.slice/session-<id>.scope.
	sessionScopePattern = regexp.MustCompile(`/user\.slice/user-(\d+)\.slice/session-([A-Za-z0-9]+)\.scope(/|$)`)
	// userManagerPattern is a unit of a user's own systemd, where desktops start
	// their apps: user-<uid>.slice/user@<uid>.service/app.slice/...
	userManagerPattern = regexp.MustCompile(`/user\.slice/user-(\d+)\.slice/user@(\d+)\.service/`)
)

// graphicalTypes are the session types logind gives a desktop login.
var graphicalTypes = map[string]bool{"x11": true, "wayland": true, "mir": true}

// cgroupPath is where systemd placed a process: the unified (cgroup v2)
// hierarchy's path, or the name=systemd one on a cgroup v1 machine.
func cgroupPath(h Host, pid string) string {
	data, err := h.ReadFile("/proc/" + pid + "/cgroup")
	if err != nil {
		return ""
	}
	var v1 string
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 {
			continue
		}
		switch {
		case parts[0] == "0" && parts[1] == "":
			return parts[2]
		case parts[1] == "name=systemd":
			v1 = parts[2]
		}
	}
	return v1
}

// sessions answers "is this process part of the desktop?" for one look at the
// GPU's holders, asking logind and systemd at most once per session, user and
// display manager.
type sessions struct {
	h         Host
	graphical map[string]bool // session id -> graphical
	users     map[string]bool // uid -> has a graphical session
	dmPID     string
	dmRead    bool
}

func newSessions(h Host) *sessions {
	return &sessions{h: h, graphical: map[string]bool{}, users: map[string]bool{}}
}

func (s *sessions) value(args ...string) string {
	out, err := s.h.Output("loginctl", args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// graphicalSession: a desktop login (x11, wayland, mir) or a login screen
// (class greeter).
func (s *sessions) graphicalSession(id string) bool {
	if g, ok := s.graphical[id]; ok {
		return g
	}
	g := graphicalTypes[s.value("show-session", id, "-p", "Type", "--value")] ||
		s.value("show-session", id, "-p", "Class", "--value") == "greeter"
	s.graphical[id] = g
	return g
}

// graphicalUser: a user with a graphical session open.
func (s *sessions) graphicalUser(uid string) bool {
	if g, ok := s.users[uid]; ok {
		return g
	}
	g := false
	for _, id := range strings.Fields(s.value("show-user", uid, "-p", "Sessions", "--value")) {
		if s.graphicalSession(id) {
			g = true
			break
		}
	}
	s.users[uid] = g
	return g
}

// displayManagerPID is the display manager's main process, or "".
func (s *sessions) displayManagerPID() string {
	if !s.dmRead {
		s.dmRead = true
		if unit := DisplayManager(s.h); unit != "" {
			if out, err := s.h.Output("systemctl", "show", "-p", "MainPID", "--value", unit); err == nil {
				if pid := strings.TrimSpace(out); pid != "0" {
					s.dmPID = pid
				}
			}
		}
	}
	return s.dmPID
}

// underDisplayManager reports whether pid is the display manager or one of
// its descendants.
func (s *sessions) underDisplayManager(pid string) bool {
	dm := s.displayManagerPID()
	if dm == "" {
		return false
	}
	for i := 0; i < 64 && pid != "" && pid != "0" && pid != "1"; i++ {
		if pid == dm {
			return true
		}
		pid = parentPID(s.h, pid)
	}
	return false
}

// desktop reports whether a process is part of the machine's desktop by
// where it runs: a graphical login session, an app unit of a user who has
// one, or anything the display manager started.
func (s *sessions) desktop(pid string) bool {
	path := cgroupPath(s.h, pid)
	if m := sessionScopePattern.FindStringSubmatch(path); m != nil && s.graphicalSession(m[2]) {
		return true
	}
	if m := userManagerPattern.FindStringSubmatch(path); m != nil && s.graphicalUser(m[1]) {
		return true
	}
	return s.underDisplayManager(pid)
}
