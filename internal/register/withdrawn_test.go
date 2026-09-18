package register

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/control"
)

func TestWithdrawnRecord(t *testing.T) {
	t.Setenv(config.ConfigDirEnv, t.TempDir())
	if LoadWithdrawn() != nil {
		t.Fatal("withdrawn before anything happened")
	}
	if err := MarkWithdrawn("control panel"); err != nil {
		t.Fatal(err)
	}
	w := LoadWithdrawn()
	if w == nil || w.By != "control panel" || w.At == 0 {
		t.Fatalf("record = %+v", w)
	}
	if err := ClearWithdrawn(); err != nil || LoadWithdrawn() != nil {
		t.Errorf("ClearWithdrawn = %v; still %+v", err, LoadWithdrawn())
	}
	if err := ClearWithdrawn(); err != nil {
		t.Errorf("clearing twice = %v", err)
	}
}

// A capability report answered 410 is an EndpointError the agent can tell
// apart: the host removed the machine.
func TestACapabilityReportAnswered410IsGone(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.ConfigDirEnv, dir)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "this listing was removed", http.StatusGone)
	}))
	defer srv.Close()
	reg, _ := json.Marshal(Registration{ListingID: "L-1", CapabilityURL: srv.URL + "/capability"})
	if err := os.WriteFile(RegistrationPath(), reg, 0600); err != nil {
		t.Fatal(err)
	}
	if err := saveControlToken("tok"); err != nil {
		t.Fatal(err)
	}
	_, err := ReportCapability(control.Capability{})
	var ee *EndpointError
	if !errors.As(err, &ee) || ee.Code != http.StatusGone {
		t.Errorf("ReportCapability = %v", err)
	}
}
