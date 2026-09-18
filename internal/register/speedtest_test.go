package register

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"

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
		_ = json.NewDecoder(r.Body).Decode(&got)
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
