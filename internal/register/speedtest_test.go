package register

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/speedtest"
	"github.com/serverroom/gpu-marketplace/internal/stats"
)

var testTarget = speedtest.Target{
	Server:      "speedtest-ny.example.net",
	DownloadURL: "https://speedtest-ny.example.net/backend/garbage",
	UploadURL:   "https://speedtest-ny.example.net/backend/empty",
	LatencyHost: "console-nyc.example.net",
	LatencyPort: 2222,
}

// registeredAt makes this test's scratch config dir a registered agent whose
// capability endpoint is capabilityURL.
func registeredAt(t *testing.T, capabilityURL string) {
	t.Helper()
	scratchConfigDir(t)
	writeRegistration(t, Registration{ListingID: "L-42", CapabilityURL: capabilityURL})
	if err := saveControlToken("tok-9"); err != nil {
		t.Fatalf("save token: %v", err)
	}
}

func TestReportCapabilityReturnsTheSpeedtestTarget(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		io.WriteString(w, `{"status":"ok",
			"speedtest":{"server":"speedtest-ny.example.net",
				"download_url":"https://speedtest-ny.example.net/backend/garbage",
				"upload_url":"https://speedtest-ny.example.net/backend/empty",
				"latency_host":"console-nyc.example.net","latency_port":2222},
			"measure_url":"https://example.net/api/marketplace/measure"}`)
	}))
	defer srv.Close()
	registeredAt(t, srv.URL+"/api/marketplace/capability")

	resp, err := ReportCapability(control.Capability{Ready: true})
	if err != nil {
		t.Fatalf("ReportCapability: %v", err)
	}
	if auth != "Bearer tok-9" {
		t.Errorf("Authorization = %q", auth)
	}
	if resp.Speedtest == nil || *resp.Speedtest != testTarget {
		t.Errorf("speedtest = %+v, want %+v", resp.Speedtest, testTarget)
	}
	if resp.MeasureURL != "https://example.net/api/marketplace/measure" {
		t.Errorf("measure_url = %q", resp.MeasureURL)
	}

	saved := SavedSpeedtest()
	if saved.Speedtest == nil || *saved.Speedtest != testTarget || saved.MeasureURL != resp.MeasureURL {
		t.Errorf("saved = %+v, want the answer kept for the speedtest command", saved)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(SpeedtestPath())
		if err != nil || fi.Mode().Perm() != 0600 {
			t.Errorf("speedtest.json: %v, mode %v, want 0600", err, fi.Mode().Perm())
		}
	}
}

// Control planes older than the speed test answer without it, or with no JSON
// at all; the report still succeeded.
func TestReportCapabilityWithoutSpeedtest(t *testing.T) {
	bodies := []string{
		`{"status":"ok"}`,
		`{"status":"ok","speedtest":null,"measure_url":null}`,
		``,
		`ok`,
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, body)
			}))
			defer srv.Close()
			registeredAt(t, srv.URL+"/capability")
			target := testTarget
			if err := rememberSpeedtest(&CapabilityResponse{Speedtest: &target, MeasureURL: "https://example.net/measure"}); err != nil {
				t.Fatalf("remember: %v", err)
			}

			resp, err := ReportCapability(control.Capability{Ready: true})
			if err != nil {
				t.Fatalf("ReportCapability: %v", err)
			}
			if resp == nil || resp.Speedtest != nil || resp.MeasureURL != "" {
				t.Errorf("answer = %+v, want no speed test", resp)
			}
			// A measured listing is no longer offered a target; the last one
			// stays for `gpu-agent speedtest`.
			if saved := SavedSpeedtest(); saved.Speedtest == nil || saved.MeasureURL != "https://example.net/measure" {
				t.Errorf("saved = %+v, want the earlier target kept", saved)
			}
		})
	}
}

func TestMeasureEndpoint(t *testing.T) {
	capability := Registration{CapabilityURL: "https://example.net/api/marketplace/capability"}
	cases := []struct {
		name, explicit, saved string
		reg                   Registration
		want                  string
	}{
		{"the URL the control plane just named", "https://a.example.net/measure", "https://b.example.net/measure", capability, "https://a.example.net/measure"},
		{"else the last one it named", "", "https://b.example.net/measure", capability, "https://b.example.net/measure"},
		{"else the capability endpoint's sibling", "", "", capability, "https://example.net/api/marketplace/measure"},
		{"an older registration, through select-relay", "", "", Registration{SelectURL: "https://example.net/api/marketplace/select-relay"}, "https://example.net/api/marketplace/measure"},
		{"nothing to derive it from", "", "", Registration{CapabilityURL: "https://example.net/api/other"}, ""},
	}
	for _, c := range cases {
		reg := c.reg
		if got := measureEndpoint(c.explicit, c.saved, &reg); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPostMeasurement(t *testing.T) {
	var got Measurement
	var auth, path string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()
	registeredAt(t, srv.URL+"/api/marketplace/capability")

	result := speedtest.Result{Server: "speedtest-ny.example.net", DownMbps: 940.2, UpMbps: 512.7, LatencyMs: 18.4}
	if err := PostMeasurement("", result); err != nil {
		t.Fatalf("PostMeasurement: %v", err)
	}
	if path != "/api/marketplace/measure" {
		t.Errorf("posted to %s, want the capability endpoint's sibling", path)
	}
	if auth != "Bearer tok-9" {
		t.Errorf("Authorization = %q", auth)
	}
	want := Measurement{ListingID: "L-42", DownMbps: 940.2, UpMbps: 512.7, LatencyMs: 18.4, Server: "speedtest-ny.example.net"}
	if got != want {
		t.Errorf("posted %+v, want %+v", got, want)
	}

	status = 219
	err := PostMeasurement("", result)
	var ee *EndpointError
	if !errors.As(err, &ee) || ee.Code != 219 {
		t.Errorf("a 219 answer: got %v, want an EndpointError with code 219", err)
	}
}

// Every capability report carries the machine's specs as they are now, so a
// board registered by an older agent (with an empty CPU model) is described
// properly from its next report.
func TestReportCapabilityCarriesTheSpecs(t *testing.T) {
	var got map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var wire map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&wire)
		if len(wire) != 2 || wire["report_b64"] == nil || wire["capability"] != nil {
			t.Errorf("on the wire = %v, want only listing_id and report_b64", wire)
		}
		got = openReport(wire)
		io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()
	registeredAt(t, srv.URL+"/api/marketplace/capability")
	orig := collectSpecs
	collectSpecs = func() (*stats.SystemStats, error) {
		return &stats.SystemStats{Arch: "arm64", Board: "Radxa ROCK 5B",
			CPU: stats.CPUInfo{Model: "Rockchip RK3588", CoresDetail: "4× Cortex-A76 + 4× Cortex-A55"}}, nil
	}
	t.Cleanup(func() { collectSpecs = orig })

	if _, err := ReportCapability(control.Capability{Ready: true}); err != nil {
		t.Fatal(err)
	}
	var specs stats.SystemStats
	if err := json.Unmarshal(got["specs"], &specs); err != nil || specs.Board != "Radxa ROCK 5B" ||
		specs.CPU.Model != "Rockchip RK3588" || specs.CPU.CoresDetail != "4× Cortex-A76 + 4× Cortex-A55" {
		t.Errorf("specs = %s (%v)", got["specs"], err)
	}
	if len(got["capability"]) == 0 || string(got["listing_id"]) != `"L-42"` {
		t.Errorf("body = %v", got)
	}
}

// openReport opens a report's base64 envelope the way the marketplace does,
// keeping the outer listing_id; a clear body is returned as it is.
func openReport(body map[string]json.RawMessage) map[string]json.RawMessage {
	var wrapped string
	if json.Unmarshal(body["report_b64"], &wrapped) != nil {
		return body
	}
	raw, err := base64.StdEncoding.DecodeString(wrapped)
	if err != nil {
		return body
	}
	var inner map[string]json.RawMessage
	if json.Unmarshal(raw, &inner) != nil {
		return body
	}
	inner["listing_id"] = body["listing_id"]
	return inner
}

// A marketplace from before the envelope refuses it with its 400 for a missing
// capability; the agent then sends the same report in the clear, once.
func TestReportCapabilityFallsBackToTheClearForm(t *testing.T) {
	var bodies []map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		if b["capability"] == nil {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, "capability must be an object")
			return
		}
		io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()
	registeredAt(t, srv.URL+"/api/marketplace/capability")
	if _, err := ReportCapability(control.Capability{Ready: true}); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || bodies[0]["report_b64"] == nil || bodies[1]["capability"] == nil ||
		string(bodies[1]["listing_id"]) != `"L-42"` {
		t.Errorf("bodies = %v", bodies)
	}
}

func TestPlainBodyKeepsNoMarkup(t *testing.T) {
	page := "<html>\r\n<head><title>403 Forbidden</title></head>\r\n<body>\r\n<center><h1>403 Forbidden</h1></center></body></html>"
	for in, want := range map[string]string{
		page:                            "an error page from a web server: 403 Forbidden",
		"<!DOCTYPE html><body>x</body>": "an error page from a web server",
		"  auth \n failed  ":            "auth failed",
	} {
		if got := PlainBody(in); got != want {
			t.Errorf("PlainBody(%q) = %q, want %q", in, got, want)
		}
	}
	if got := PlainBody(strings.Repeat("a", 400)); len([]rune(got)) != 300 {
		t.Errorf("long body kept %d characters", len([]rune(got)))
	}
}

// From v0.3.1 the control plane names the server even for a listing that is
// already measured, with the interval and the last measurement; the file keeps
// all three for the daily re-measurement and the speedtest command.
func TestAMeasuredListingStillNamesItsServer(t *testing.T) {
	answer := `{"measure_url":"https://example.net/api/marketplace/measure","measured_at":1789792246,"speedtest_every_hours":24,
		"speedtest_target":{"server":"speedtest-ny.example.net",
			"download_url":"https://speedtest-ny.example.net/backend/garbage",
			"upload_url":"https://speedtest-ny.example.net/backend/empty",
			"latency_host":"console-nyc.example.net","latency_port":2222}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, answer)
	}))
	defer srv.Close()
	registeredAt(t, srv.URL+"/api/marketplace/capability")

	resp, err := ReportCapability(control.Capability{Ready: true})
	if err != nil {
		t.Fatalf("ReportCapability: %v", err)
	}
	if resp.Speedtest != nil {
		t.Errorf("speedtest = %+v; a measured listing is not asked to measure", resp.Speedtest)
	}
	saved := SavedSpeedtest()
	if saved.Speedtest == nil || *saved.Speedtest != testTarget {
		t.Errorf("saved target = %+v, want %+v", saved.Speedtest, testTarget)
	}
	if saved.SpeedtestTarget != nil || saved.AgentUpdate != nil {
		t.Errorf("saved = %+v; the target is kept once, as speedtest, and no update offer", saved)
	}
	if saved.MeasuredAt != 1789792246 || saved.SpeedtestEveryHours == nil || *saved.SpeedtestEveryHours != 24 {
		t.Errorf("saved measured_at %d, every %v; want the marketplace's", saved.MeasuredAt, saved.SpeedtestEveryHours)
	}

	// An older control plane's answer names no target: what is saved stays.
	answer = `{"measure_url":"https://example.net/api/marketplace/measure"}`
	if _, err := ReportCapability(control.Capability{Ready: true}); err != nil {
		t.Fatalf("ReportCapability: %v", err)
	}
	if again := SavedSpeedtest(); again.Speedtest == nil || again.MeasuredAt != 1789792246 {
		t.Errorf("after an answer without a target: %+v, want the saved target and measurement kept", again)
	}
}

// A result this machine posts is when its next daily measurement counts from.
func TestPostingAMeasurementRemembersWhen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	registeredAt(t, srv.URL+"/api/marketplace/capability")
	before := time.Now().Unix()
	if err := PostMeasurement(srv.URL+"/api/marketplace/measure", speedtest.Result{Server: "speedtest-ny.example.net", DownMbps: 300, UpMbps: 100}); err != nil {
		t.Fatalf("PostMeasurement: %v", err)
	}
	if at := SavedSpeedtest().MeasuredAt; at < before || at > time.Now().Unix() {
		t.Errorf("measured_at %d, want the moment it was posted", at)
	}
}
