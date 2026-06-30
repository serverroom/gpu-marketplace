package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeProv struct {
	provisioned string
	tornDown    string
}

func (f *fakeProv) Provision(rentalID, renterPubkey string) error {
	f.provisioned = rentalID
	return nil
}
func (f *fakeProv) Teardown(rentalID string) error {
	f.tornDown = rentalID
	return nil
}
func (f *fakeProv) Status() string { return "free" }

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
	s := New("127.0.0.1:0", "secret", &fakeProv{})
	w := serve(s, "POST", "/provision", `{"rental_id":"R1"}`, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without token, got %d", w.Code)
	}
}

func TestProvisionRejectedWithWrongToken(t *testing.T) {
	s := New("127.0.0.1:0", "secret", &fakeProv{})
	w := serve(s, "POST", "/provision", `{"rental_id":"R1"}`, "nope")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 with wrong token, got %d", w.Code)
	}
}

func TestProvisionCallsProvisioner(t *testing.T) {
	fp := &fakeProv{}
	s := New("127.0.0.1:0", "secret", fp)
	w := serve(s, "POST", "/provision", `{"rental_id":"R1","renter_pubkey":"k"}`, "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if fp.provisioned != "R1" {
		t.Errorf("provisioner not called with R1, got %q", fp.provisioned)
	}
}

func TestTeardownCallsProvisioner(t *testing.T) {
	fp := &fakeProv{}
	s := New("127.0.0.1:0", "secret", fp)
	w := serve(s, "POST", "/teardown", `{"rental_id":"R2"}`, "secret")
	if w.Code != http.StatusOK || fp.tornDown != "R2" {
		t.Fatalf("teardown failed: code=%d tornDown=%q", w.Code, fp.tornDown)
	}
}
