package register

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/config"
)

// scratchConfigDir points the register package at a throwaway directory so the
// state helpers can be exercised without touching a real install.
func scratchConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(config.ConfigDirEnv, dir)
	return dir
}

func writeRegistration(t *testing.T, reg Registration) {
	t.Helper()
	data, err := json.Marshal(reg)
	if err != nil {
		t.Fatalf("marshal registration: %v", err)
	}
	if err := os.WriteFile(RegistrationPath(), data, 0600); err != nil {
		t.Fatalf("write registration: %v", err)
	}
}

func TestLoadStateFreshInstall(t *testing.T) {
	scratchConfigDir(t)

	st := LoadState()
	if st.Registered || st.HasTunnel || st.Unreadable {
		t.Fatalf("fresh install should be unregistered with no tunnel, got %+v", st)
	}
}

func TestLoadStateRegisteredWithoutTunnel(t *testing.T) {
	scratchConfigDir(t)
	writeRegistration(t, Registration{ListingID: "L-7", Hub: "nyc"})

	st := LoadState()
	if !st.Registered {
		t.Error("Registered = false, want true")
	}
	if st.HasTunnel {
		t.Error("HasTunnel = true, want false: no tunnel.json was written")
	}
	if st.ListingID != "L-7" || st.Location != "nyc" {
		t.Errorf("listing/location = %q/%q, want L-7/nyc", st.ListingID, st.Location)
	}
}

func TestLoadStateRegisteredWithTunnel(t *testing.T) {
	dir := scratchConfigDir(t)
	writeRegistration(t, Registration{ListingID: "L-8", Hub: "ams"})
	if err := os.WriteFile(filepath.Join(dir, "tunnel.json"), []byte("{}"), 0600); err != nil {
		t.Fatalf("write tunnel config: %v", err)
	}

	st := LoadState()
	if !st.Registered || !st.HasTunnel {
		t.Fatalf("want registered with tunnel, got %+v", st)
	}
}

// A registration that cannot be parsed must not read as "never registered" —
// that would send a provider off to burn a fresh one-time code.
func TestLoadStateMalformedRegistrationIsUnreadable(t *testing.T) {
	dir := scratchConfigDir(t)
	if err := os.WriteFile(filepath.Join(dir, "registration.json"), []byte("{not json"), 0600); err != nil {
		t.Fatalf("write registration: %v", err)
	}

	st := LoadState()
	if !st.Registered {
		t.Error("Registered = false, want true: the file is there")
	}
	if !st.Unreadable {
		t.Error("Unreadable = false, want true")
	}
}

// The real-world case: `gpu-agent status` run without sudo against 0600 state.
func TestLoadStateUnreadableWithoutPermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file modes do not restrict reads")
	}
	dir := scratchConfigDir(t)
	writeRegistration(t, Registration{ListingID: "L-9"})
	if err := os.Chmod(filepath.Join(dir, "registration.json"), 0000); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	st := LoadState()
	if !st.Registered || !st.Unreadable {
		t.Fatalf("want registered-but-unreadable, got %+v", st)
	}
}
