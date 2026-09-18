package vmrt

import (
	"encoding/json"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

const headlessDir = "/var/lib/gpu-agent"

func desktopMachine() *fakehost.Host {
	h := fakehost.New()
	h.Outputs["systemctl get-default"] = "graphical.target\n"
	return h
}

func TestMakeHeadlessRecordsTheOldTargetAndSwitches(t *testing.T) {
	h := desktopMachine()
	prev, err := MakeHeadless(h, headlessDir)
	if err != nil || prev != "graphical.target" {
		t.Fatalf("MakeHeadless = %q, %v", prev, err)
	}
	if !h.Ran("run systemctl set-default multi-user.target") {
		t.Error("the default target was not changed")
	}
	if h.Ran("run systemctl isolate") {
		t.Error("MakeHeadless closed the desktop itself; CloseDesktop does that, last")
	}
	var rec HeadlessRecord
	if err := json.Unmarshal(h.Files[HeadlessPath(headlessDir)], &rec); err != nil || rec.PreviousDefault != "graphical.target" {
		t.Errorf("record = %+v, %v", rec, err)
	}
}

func TestMakeHeadlessTwiceStillRemembersTheDesktop(t *testing.T) {
	h := desktopMachine()
	if _, err := MakeHeadless(h, headlessDir); err != nil {
		t.Fatal(err)
	}
	h.Outputs["systemctl get-default"] = "multi-user.target\n"
	if _, err := MakeHeadless(h, headlessDir); err != nil {
		t.Fatal(err)
	}
	rec, err := LoadHeadless(h, headlessDir)
	if err != nil || rec.PreviousDefault != "graphical.target" {
		t.Errorf("a second run overwrote the original target: %+v, %v", rec, err)
	}
}

func TestCloseDesktopIsolatesTheHeadlessTarget(t *testing.T) {
	h := desktopMachine()
	if err := CloseDesktop(h); err != nil {
		t.Fatal(err)
	}
	if !h.Ran("run systemctl isolate multi-user.target") {
		t.Error("the desktop was not closed")
	}
}

func TestRestoreDesktopUndoesMakeHeadless(t *testing.T) {
	h := desktopMachine()
	if _, err := MakeHeadless(h, headlessDir); err != nil {
		t.Fatal(err)
	}
	restored, target, err := RestoreDesktop(h, headlessDir)
	if err != nil || !restored || target != "graphical.target" {
		t.Fatalf("RestoreDesktop = %v, %q, %v", restored, target, err)
	}
	if !h.Ran("run systemctl set-default graphical.target") {
		t.Error("the desktop target was not restored")
	}
	if _, ok := h.Files[HeadlessPath(headlessDir)]; ok {
		t.Error("the record was left behind")
	}
}

func TestRestoreDesktopIsANoOpWhenTheAgentNeverChangedIt(t *testing.T) {
	h := desktopMachine()
	restored, _, err := RestoreDesktop(h, headlessDir)
	if err != nil || restored || h.Ran("run systemctl set-default") {
		t.Errorf("RestoreDesktop on an untouched machine = %v, %v", restored, err)
	}
	// A machine that was already headless is left headless.
	h.Outputs["systemctl get-default"] = "multi-user.target\n"
	if _, err := MakeHeadless(h, headlessDir); err != nil {
		t.Fatal(err)
	}
	restored, _, err = RestoreDesktop(h, headlessDir)
	if err != nil || restored || h.Ran("run systemctl set-default") {
		t.Errorf("a machine that was headless before was changed: %v, %v, calls %v", restored, err, h.Calls)
	}
}
