package vmrt

import (
	"fmt"
	"regexp"
	"strings"
)

// Telling the people on this machine something: a wall message to every
// terminal, and a desktop notification to everyone logged in to a desktop.
// Nothing here waits for anybody or touches their programs.

// notifyTimeout bounds each message (coreutils timeout): a terminal or a
// session bus that does not answer must not hold up the agent.
const notifyTimeout = "20"

// Login is a person logged in to a desktop on this machine.
type Login struct {
	Session string
	User    string // the login name
	UID     string
}

var (
	sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9]+$`)
	userNamePattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*\$?$`)
	uidPattern       = regexp.MustCompile(`^[0-9]+$`)
)

// sessionProps reads a login session's properties from logind.
func sessionProps(h Host, id string) map[string]string {
	out, err := h.Output("loginctl", "show-session", id, "-p", "Type", "-p", "Class", "-p", "Name", "-p", "User", "-p", "State")
	if err != nil {
		return nil
	}
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	return props
}

// GraphicalLogins are the people logged in to a desktop on this machine
// (x11, wayland or mir sessions of class user -- not the login screen), one
// entry per person.
func GraphicalLogins(h Host) []Login {
	out, err := h.Output("loginctl", "list-sessions", "--no-legend")
	if err != nil {
		return nil
	}
	var logins []Login
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !sessionIDPattern.MatchString(f[0]) {
			continue
		}
		p := sessionProps(h, f[0])
		if !graphicalTypes[p["Type"]] || p["Class"] != "user" || p["State"] == "closing" {
			continue
		}
		l := Login{Session: f[0], User: p["Name"], UID: p["User"]}
		if !userNamePattern.MatchString(l.User) || !uidPattern.MatchString(l.UID) || seen[l.User] {
			continue
		}
		seen[l.User] = true
		logins = append(logins, l)
	}
	return logins
}

// NotifyHost tells whoever is on this machine: title and body as a wall
// message to every terminal, and as a desktop notification to every person
// logged in to a desktop (notify-send over that person's own session bus).
// reached says who was told; problems what could not be sent.
func NotifyHost(h Host, title, body string) (reached, problems []string) {
	if _, err := h.LookPath("wall"); err == nil {
		if err := h.RunInput([]byte(title+"\n\n"+body+"\n"), "timeout", notifyTimeout, "wall"); err != nil {
			problems = append(problems, "wall: "+err.Error())
		} else {
			reached = append(reached, "every terminal")
		}
	}
	logins := GraphicalLogins(h)
	if len(logins) == 0 {
		return reached, problems
	}
	_, errSend := h.LookPath("notify-send")
	_, errRunuser := h.LookPath("runuser")
	if errSend != nil || errRunuser != nil {
		return reached, append(problems, "no desktop notification: notify-send or runuser is not installed")
	}
	for _, l := range logins {
		err := h.Run("timeout", notifyTimeout, "runuser", "-u", l.User, "--", "env", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/"+l.UID+"/bus",
			"notify-send", "-u", "critical", "-a", "GPU marketplace", title, body)
		if err != nil {
			problems = append(problems, fmt.Sprintf("desktop notification to %s: %v", l.User, err))
			continue
		}
		reached = append(reached, "the desktop of "+l.User)
	}
	return reached, problems
}
