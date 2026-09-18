package control

import (
	"encoding/json"
	"errors"
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

type fakeHost struct {
	version   string
	record    interface{}
	updateTo  string
	updateErr error
	withdrawn int
	withErr   error
}

func (f *fakeHost) AgentVersion() string      { return f.version }
func (f *fakeHost) UpdateRecord() interface{} { return f.record }
func (f *fakeHost) Update(v string) (string, string, error) {
	f.updateTo = v
	if f.updateErr != nil {
		return "", "", f.updateErr
	}
	return f.version, v, nil
}
func (f *fakeHost) Withdraw() error {
	f.withdrawn++
	return f.withErr
}

type codedErr struct{ code int }

func (e codedErr) Error() string   { return "a rental is on this machine" }
func (e codedErr) HTTPStatus() int { return e.code }

func TestUpdateAnswers202AtOnce(t *testing.T) {
	s := New("127.0.0.1:0", "secret", readyProv())
	h := &fakeHost{version: "v0.1.10"}
	s.SetHost(h)
	w := serve(s, "POST", "/update", `{"version":"v0.1.11"}`, "secret")
	if w.Code != http.StatusAccepted {
		t.Fatalf("code %d: %s", w.Code, w.Body)
	}
	var body map[string]string
	json.NewDecoder(w.Body).Decode(&body)
	if body["status"] != "updating" || body["from"] != "v0.1.10" || body["to"] != "v0.1.11" || h.updateTo != "v0.1.11" {
		t.Errorf("body = %v, asked %q", body, h.updateTo)
	}
}

func TestUpdateRefusalsKeepTheirStatus(t *testing.T) {
	for _, c := range []struct {
		err  error
		code int
	}{
		{&StatusError{Code: http.StatusConflict, Msg: "this is a development build"}, http.StatusConflict},
		{&StatusError{Code: http.StatusNotImplemented, Msg: "updating from the panel works on Linux hosts"}, http.StatusNotImplemented},
		{codedErr{http.StatusConflict}, http.StatusConflict},
		{errors.New("boom"), http.StatusInternalServerError},
	} {
		s := New("127.0.0.1:0", "secret", readyProv())
		s.SetHost(&fakeHost{version: "v0.1.10", updateErr: c.err})
		if w := serve(s, "POST", "/update", `{"version":"v0.1.11"}`, "secret"); w.Code != c.code || !strings.Contains(w.Body.String(), c.err.Error()) {
			t.Errorf("%v: %d %s", c.err, w.Code, w.Body)
		}
	}
}

func TestUpdateAndWithdrawnNeedTheToken(t *testing.T) {
	s := New("127.0.0.1:0", "secret", readyProv())
	h := &fakeHost{version: "v0.1.10"}
	s.SetHost(h)
	for _, path := range []string{"/update", "/withdrawn"} {
		if w := serve(s, "POST", path, `{"version":"v0.1.11"}`, "nope"); w.Code != http.StatusUnauthorized {
			t.Errorf("%s without the token: %d", path, w.Code)
		}
	}
	if h.updateTo != "" || h.withdrawn != 0 {
		t.Error("an unauthenticated request reached the agent")
	}
}

func TestWithoutAHostUpdateIs501(t *testing.T) {
	s := New("127.0.0.1:0", "secret", readyProv())
	if w := serve(s, "POST", "/update", `{"version":"v0.1.11"}`, "secret"); w.Code != http.StatusNotImplemented {
		t.Errorf("code %d", w.Code)
	}
}

func TestWithdrawn(t *testing.T) {
	s := New("127.0.0.1:0", "secret", readyProv())
	h := &fakeHost{version: "v0.1.10"}
	s.SetHost(h)
	w := serve(s, "POST", "/withdrawn", "", "secret")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"withdrawn"`) || h.withdrawn != 1 {
		t.Errorf("%d %s (withdrawn %d)", w.Code, w.Body, h.withdrawn)
	}
	h.withErr = &StatusError{Code: http.StatusConflict, Msg: "this machine is rented"}
	if w := serve(s, "POST", "/withdrawn", "", "secret"); w.Code != http.StatusConflict {
		t.Errorf("while rented: %d", w.Code)
	}
}

func TestStatusShowsVersionAndUpdate(t *testing.T) {
	fp := readyProv()
	fp.capability.AgentVersion = "v0.1.10"
	s := New("127.0.0.1:0", "secret", fp)
	w := serve(s, "GET", "/status", "", "secret")
	if !strings.Contains(w.Body.String(), `"agent_version":"v0.1.10"`) || !strings.Contains(w.Body.String(), `"update":null`) {
		t.Errorf("status without a host = %s", w.Body)
	}
	s.SetHost(&fakeHost{version: "v0.1.10", record: map[string]string{"status": "downloading", "to": "v0.1.11"}})
	w = serve(s, "GET", "/status", "", "secret")
	if !strings.Contains(w.Body.String(), `"update":{"status":"downloading","to":"v0.1.11"}`) {
		t.Errorf("status = %s", w.Body)
	}
}
