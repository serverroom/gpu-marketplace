package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocationLabelMapsKnownRelays(t *testing.T) {
	for key, want := range map[string]string{
		"nyc":       "New York",
		"miami":     "Miami",
		"sf":        "San Francisco",
		"amsterdam": "Amsterdam",
		"bucharest": "Bucharest",
	} {
		if got := LocationLabel(key); got != want {
			t.Errorf("LocationLabel(%q) = %q, want %q", key, got, want)
		}
	}
}

// Every relay in the built-in list must have a real label — a key that slipped
// through would show a provider "sf" in the location picker.
func TestEveryDefaultHubHasALabel(t *testing.T) {
	for _, hub := range DefaultHubs() {
		if _, ok := locationLabels[hub.Name]; !ok {
			t.Errorf("default hub %q has no display label", hub.Name)
		}
	}
}

// A relay the control plane adds later must still read properly without a new
// agent release, so an unknown key is title-cased rather than shown raw.
func TestLocationLabelTitleCasesUnknownKeys(t *testing.T) {
	for key, want := range map[string]string{
		"frankfurt": "Frankfurt",
		"sao-paulo": "Sao Paulo",
		"cape_town": "Cape Town",
		"TOKYO":     "Tokyo",
	} {
		if got := LocationLabel(key); got != want {
			t.Errorf("LocationLabel(%q) = %q, want %q", key, got, want)
		}
	}
}

// Registration.Hub holds a bare hostname on the pre-assigned-tunnel path, so
// anything host-shaped has to pass through untouched.
func TestLocationLabelPassesThroughHosts(t *testing.T) {
	for _, in := range []string{"162.244.81.236", "relay-nyc.example.net", ""} {
		if got := LocationLabel(in); got != in {
			t.Errorf("LocationLabel(%q) = %q, want it unchanged", in, got)
		}
	}
}

func TestStorageDirIsRecordedApartFromTheConfig(t *testing.T) {
	t.Setenv(ConfigDirEnv, t.TempDir())
	if StorageDir() != DataDir() {
		t.Fatalf("default storage = %s, want the data dir", StorageDir())
	}
	dir := filepath.Join(t.TempDir(), "gpu-agent")
	if err := SetStorageDir(dir); err != nil {
		t.Fatal(err)
	}
	if StorageDir() != dir {
		t.Errorf("storage = %s, want %s", StorageDir(), dir)
	}
	if _, err := os.Stat(ConfigPath()); !os.IsNotExist(err) {
		t.Errorf("config.yaml was written (it starts the legacy stats server)")
	}
	if err := SetStorageDir(""); err != nil || StorageDir() != DataDir() {
		t.Errorf("forgetting the storage dir: %v, %s", err, StorageDir())
	}
}
