package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeProv struct {
	provisioned string
	tornDown    string
	capability  Capability
}

func (f *fakeProv) Provision(rentalID, renterPubkey string) error {
	f.provisioned = rentalID
	return nil
}
func (f *fakeProv) Teardown(rentalID string) error {
	f.tornDown = rentalID
	return nil
}
func (f *fakeProv) Status() string         { return "free" }
func (f *fakeProv) Capability() Capability { return f.capability }

func readyProv() *fakeProv {
	return &fakeProv{capability: Capability{Ready: true, Kind: "kata-vfio"}}
}

func serve(s *Server, method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.httpServer.Handler.ServeHTTP(w, req)
	return w
}

func TestProvisionRejectedWithoutToken(t *testing.T) {
	s := New("127.0.0.1:0", "secret", readyProv())
	w := serve(s, "POST", "/provision", `{"rental_id":"R1"}`, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without token, got %d", w.Code)
	}
}

func TestProvisionRejectedWithWrongToken(t *testing.T) {
	s := New("127.0.0.1:0", "secret", readyProv())
	w := serve(s, "POST", "/provision", `{"rental_id":"R1"}`, "nope")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 with wrong token, got %d", w.Code)
	}
}

func TestProvisionCallsProvisioner(t *testing.T) {
	fp := readyProv()
	s := New("127.0.0.1:0", "secret", fp)
	w := serve(s, "POST", "/provision", `{"rental_id":"R1","renter_pubkey":"k"}`, "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if fp.provisioned != "R1" {
		t.Errorf("provisioner not called with R1, got %q", fp.provisioned)
	}
}

// A machine that cannot host must answer with the reason, never with success.
func TestProvisionRefusedWhenNotReady(t *testing.T) {
	fp := &fakeProv{capability: Capability{Kind: "kata-vfio", Reasons: []string{"the IOMMU is off", "no GPU"}}}
	s := New("127.0.0.1:0", "secret", fp)
	w := serve(s, "POST", "/provision", `{"rental_id":"R1","renter_pubkey":"k"}`, "secret")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 from a machine that is not ready, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "the IOMMU is off; no GPU") {
		t.Errorf("refusal does not carry the reasons: %q", w.Body.String())
	}
	if fp.provisioned != "" {
		t.Errorf("provisioner was called on a machine that is not ready")
	}
}

func TestProvisionRejectsBadRequests(t *testing.T) {
	for _, body := range []string{
		`{"rental_id":"../x","renter_pubkey":"k"}`,
		`{"rental_id":"R1"}`,
		`{"renter_pubkey":"k"}`,
	} {
		fp := readyProv()
		w := serve(New("127.0.0.1:0", "secret", fp), "POST", "/provision", body, "secret")
		if w.Code != http.StatusBadRequest || fp.provisioned != "" {
			t.Errorf("%s: code=%d provisioned=%q, want 400 and no call", body, w.Code, fp.provisioned)
		}
	}
}

func TestProvisionIsPostOnly(t *testing.T) {
	w := serve(New("127.0.0.1:0", "secret", readyProv()), "GET", "/provision", "", "secret")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

func TestTeardownCallsProvisioner(t *testing.T) {
	fp := readyProv()
	s := New("127.0.0.1:0", "secret", fp)
	w := serve(s, "POST", "/teardown", `{"rental_id":"R2"}`, "secret")
	if w.Code != http.StatusOK || fp.tornDown != "R2" {
		t.Fatalf("teardown failed: code=%d tornDown=%q", w.Code, fp.tornDown)
	}
}

// Status is how the control plane tells a machine that can host from one that
// cannot -- the distinction v0.1.5 had no way to make.
func TestStatusCarriesCapability(t *testing.T) {
	fp := &fakeProv{capability: Capability{Kind: "kata-vfio", Reasons: []string{"no GPU"}}}
	w := serve(New("127.0.0.1:0", "secret", fp), "GET", "/status", "", "secret")
	var got struct {
		Status  string   `json:"status"`
		Ready   bool     `json:"ready"`
		Kind    string   `json:"kind"`
		Reasons []string `json:"reasons"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "free" || got.Ready || got.Kind != "kata-vfio" || len(got.Reasons) != 1 {
		t.Errorf("status = %+v", got)
	}
}

func TestValidRentalID(t *testing.T) {
	for id, want := range map[string]bool{
		"R1": true, "3f2b9c1e-8d7a-4b6f-9e2d-1a2b3c4d5e6f": true,
		"": false, "../etc": false, "a b": false, "-lead": false,
		strings.Repeat("a", 65): false,
	} {
		if got := ValidRentalID(id); got != want {
			t.Errorf("ValidRentalID(%q) = %v, want %v", id, got, want)
		}
	}
}
