package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

type servingProv struct {
	fakeProv
	started  *ServeStartRequest
	stopped  string
	drain    time.Duration
	current  *ServingStatus
	startErr error
}

func (f *servingProv) ServeStart(req ServeStartRequest) (*ServingStatus, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.started = &req
	f.current = &ServingStatus{ServingID: req.ServingID, Model: req.Model, State: ServingLoading, Since: 1700000000}
	return f.current, nil
}

func (f *servingProv) ServeStop(id string, drain time.Duration) (*ServingStatus, error) {
	f.stopped, f.drain = id, drain
	if drain == 0 {
		f.current = nil
		return nil, nil
	}
	f.current.State = ServingDraining
	return f.current, nil
}

func (f *servingProv) Serving() *ServingStatus { return f.current }

func newServingProv() *servingProv {
	return &servingProv{fakeProv: fakeProv{capability: Capability{Ready: true, Kind: "container-nv"}}}
}

var goodCredential = strings.Repeat("c", 40)

func startBody(id, model, cred string) string {
	b, _ := json.Marshal(map[string]string{"serving_id": id, "model": model, "credential": cred})
	return string(b)
}

func TestServeRoutesAbsentWithoutController(t *testing.T) {
	s := New("127.0.0.1:0", "secret", readyProv())
	for _, path := range []string{"/serve/start", "/serve/stop"} {
		if w := serve(s, "POST", path, `{}`, "secret"); w.Code != http.StatusNotFound {
			t.Errorf("%s on an agent that cannot serve: want 404, got %d", path, w.Code)
		}
	}
}

func TestServeStartNeedsToken(t *testing.T) {
	s := New("127.0.0.1:0", "secret", newServingProv())
	if w := serve(s, "POST", "/serve/start", startBody("S1", "gpt-oss-120b", goodCredential), ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without token, got %d", w.Code)
	}
}

func TestServeStartAccepted(t *testing.T) {
	fp := newServingProv()
	s := New("127.0.0.1:0", "secret", fp)
	w := serve(s, "POST", "/serve/start", startBody("S1", "gpt-oss-120b", goodCredential), "secret")
	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", w.Code, w.Body.String())
	}
	if fp.started == nil || fp.started.Credential != goodCredential || fp.started.Model != "gpt-oss-120b" {
		t.Fatalf("the controller did not get the request: %+v", fp.started)
	}
	var body map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != ServingLoading || body["serving_id"] != "S1" {
		t.Errorf("unexpected answer: %v", body)
	}
	if strings.Contains(w.Body.String(), goodCredential) {
		t.Error("the answer echoes the credential")
	}
}

func TestServeStartRefusesBadRequests(t *testing.T) {
	cases := map[string]string{
		"no serving id":  startBody("", "gpt-oss-120b", goodCredential),
		"bad serving id": startBody("../x", "gpt-oss-120b", goodCredential),
		"no model":       startBody("S1", "", goodCredential),
		"bad model":      startBody("S1", "GPT 4o", goodCredential),
		"short secret":   startBody("S1", "gpt-oss-120b", "short"),
		"spaced secret":  startBody("S1", "gpt-oss-120b", strings.Repeat("a", 20)+" "+strings.Repeat("b", 20)),
		"unknown field":  `{"serving_id":"S1","model":"gpt-oss-120b","credential":"` + goodCredential + `","image":"x"}`,
		"not json":       `serve please`,
		"two objects":    startBody("S1", "gpt-oss-120b", goodCredential) + `{}`,
	}
	for name, body := range cases {
		fp := newServingProv()
		s := New("127.0.0.1:0", "secret", fp)
		w := serve(s, "POST", "/serve/start", body, "secret")
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", name, w.Code)
		}
		if fp.started != nil {
			t.Errorf("%s: the controller was called", name)
		}
	}
}

func TestServeStartRefusedWhenMachineCannotHost(t *testing.T) {
	fp := newServingProv()
	fp.capability = Capability{Ready: false, Reasons: []string{"no KVM"}}
	s := New("127.0.0.1:0", "secret", fp)
	w := serve(s, "POST", "/serve/start", startBody("S1", "gpt-oss-120b", goodCredential), "secret")
	if w.Code != http.StatusServiceUnavailable || fp.started != nil {
		t.Fatalf("want 503 and no start, got %d", w.Code)
	}
}

func TestServeStartPassesRefusals(t *testing.T) {
	fp := newServingProv()
	fp.startErr = Conflict("this machine is rented")
	s := New("127.0.0.1:0", "secret", fp)
	w := serve(s, "POST", "/serve/start", startBody("S1", "gpt-oss-120b", goodCredential), "secret")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "rented") {
		t.Fatalf("want the controller's 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestServeStopDrain(t *testing.T) {
	fp := newServingProv()
	s := New("127.0.0.1:0", "secret", fp)
	serve(s, "POST", "/serve/start", startBody("S1", "gpt-oss-120b", goodCredential), "secret")

	w := serve(s, "POST", "/serve/stop", `{"serving_id":"S1"}`, "secret")
	if w.Code != http.StatusOK || fp.drain != MaxDrainSeconds*time.Second {
		t.Fatalf("want 200 and the default drain, got %d and %v", w.Code, fp.drain)
	}
	if !strings.Contains(w.Body.String(), ServingDraining) {
		t.Errorf("want draining, got %s", w.Body.String())
	}

	w = serve(s, "POST", "/serve/stop", `{"serving_id":"S1","drain_seconds":0}`, "secret")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "stopped") {
		t.Fatalf("want stopped, got %d: %s", w.Code, w.Body.String())
	}

	for _, d := range []string{"-1", "61", "600"} {
		w = serve(s, "POST", "/serve/stop", `{"serving_id":"S1","drain_seconds":`+d+`}`, "secret")
		if w.Code != http.StatusBadRequest {
			t.Errorf("drain_seconds %s: want 400, got %d", d, w.Code)
		}
	}
}

func TestStatusShowsServingAndStaysFree(t *testing.T) {
	fp := newServingProv()
	s := New("127.0.0.1:0", "secret", fp)
	w := serve(s, "GET", "/status", "", "secret")
	if strings.Contains(w.Body.String(), `"serving"`) {
		t.Fatalf("serving reported while nothing serves: %s", w.Body.String())
	}
	serve(s, "POST", "/serve/start", startBody("S1", "gpt-oss-120b", goodCredential), "secret")
	w = serve(s, "GET", "/status", "", "secret")
	var body map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != "free" {
		t.Errorf("a serving machine must stay free for rentals, got %v", body["status"])
	}
	serving, _ := body["serving"].(map[string]interface{})
	if serving == nil || serving["model"] != "gpt-oss-120b" {
		t.Errorf("want the serving session in /status, got %v", body["serving"])
	}
	if strings.Contains(w.Body.String(), goodCredential) {
		t.Error("/status shows the credential")
	}
}
