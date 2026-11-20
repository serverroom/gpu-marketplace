package sshtunnel

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestProtectKeyWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ProtectKey(path); err != nil {
		// Taking ownership for Administrators needs an elevated process.
		t.Skipf("not elevated: %v", err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	got := sd.String()
	if got != "O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)" {
		t.Errorf("key security = %s", got)
	}
}
