package register

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSubmitSendsCodeAndReturnsListing(t *testing.T) {
	var got RegisterRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/marketplace/register" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(RegisterResponse{ListingID: "L-123", Status: "free"})
	}))
	defer srv.Close()

	resp, err := Submit(srv.URL+"/marketplace/register", RegisterRequest{Code: "ABC", AgentPubkey: "ssh-ed25519 KKK"})
	if err != nil {
		t.Fatalf("Submit error: %v", err)
	}
	if resp.ListingID != "L-123" {
		t.Errorf("listing_id = %q, want L-123", resp.ListingID)
	}
	if got.Code != "ABC" || got.AgentPubkey != "ssh-ed25519 KKK" {
		t.Errorf("server received wrong payload: %+v", got)
	}
}

func TestSubmitErrorsOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid, used, or expired code", 201)
	}))
	defer srv.Close()

	_, err := Submit(srv.URL, RegisterRequest{Code: "BAD"})
	if err == nil {
		t.Fatal("expected an error on non-200, got nil")
	}
}

func TestSubmitParsesRelaysAndSelectURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(RegisterResponse{
			ListingID: "L-9",
			Status:    "pending",
			Relays: []RelayOption{
				{Name: "nyc", Host: "1.2.3.4", Port: 443},
				{Name: "ams", Host: "5.6.7.8", Port: 443},
			},
			SelectURL:    srvURLPlaceholder,
			ControlToken: "tok-1",
		})
	}))
	defer srv.Close()

	resp, err := Submit(srv.URL, RegisterRequest{Code: "ABC"})
	if err != nil {
		t.Fatalf("Submit error: %v", err)
	}
	if len(resp.Relays) != 2 || resp.Relays[0].Name != "nyc" || resp.Relays[1].Port != 443 {
		t.Errorf("relays parsed wrong: %+v", resp.Relays)
	}
	if resp.SelectURL != srvURLPlaceholder || resp.ControlToken != "tok-1" {
		t.Errorf("select_url/control_token parsed wrong: %+v", resp)
	}
}

const srvURLPlaceholder = "https://example.net/select-relay"

func TestSelectRelaySendsChoiceAndBearer(t *testing.T) {
	var got SelectRelayRequest
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(SelectRelayResponse{
			Status: "assigned",
			Tunnel: &TunnelInfo{RelayHost: "1.2.3.4", RelayPort: 22, RelayUser: "gpu-tunnel", ControlSlot: 20001, SSHSlot: 20002},
		})
	}))
	defer srv.Close()

	resp, err := SelectRelay(srv.URL, "tok-1", "L-9", "nyc")
	if err != nil {
		t.Fatalf("SelectRelay error: %v", err)
	}
	if auth != "Bearer tok-1" {
		t.Errorf("Authorization = %q, want Bearer tok-1", auth)
	}
	if got.ListingID != "L-9" || got.Relay != "nyc" {
		t.Errorf("server received wrong payload: %+v", got)
	}
	if resp.Tunnel == nil || resp.Tunnel.ControlSlot != 20001 {
		t.Errorf("tunnel parsed wrong: %+v", resp.Tunnel)
	}
}

func TestSelectRelayErrorsOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unknown relay", 400)
	}))
	defer srv.Close()

	_, err := SelectRelay(srv.URL, "", "L-9", "nope")
	if err == nil {
		t.Fatal("expected an error on non-200, got nil")
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 400 {
		t.Errorf("expected StatusError with code 400, got %v", err)
	}
}

func TestSelectRelayOmitsAuthHeaderWithoutToken(t *testing.T) {
	var hasAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasAuth = r.Header["Authorization"]
		json.NewEncoder(w).Encode(SelectRelayResponse{Status: "assigned"})
	}))
	defer srv.Close()

	if _, err := SelectRelay(srv.URL, "", "L-9", "nyc"); err != nil {
		t.Fatalf("SelectRelay error: %v", err)
	}
	if hasAuth {
		t.Error("Authorization header sent despite empty bearer")
	}
}

func TestRelayOptionsMapsServerList(t *testing.T) {
	resp := &RegisterResponse{Relays: []RelayOption{
		{Name: "nyc", Host: "1.2.3.4", Port: 443},
		{Name: "ams", Host: "5.6.7.8", Port: 443},
	}}
	hubs := relayOptions(resp)
	if len(hubs) != 2 {
		t.Fatalf("got %d hubs, want 2", len(hubs))
	}
	if hubs[0].Name != "nyc" || hubs[0].Host != "1.2.3.4" || hubs[0].Port != 443 {
		t.Errorf("hub[0] mapped wrong: %+v", hubs[0])
	}
	if hubs[1].Name != "ams" || hubs[1].Host != "5.6.7.8" {
		t.Errorf("hub[1] mapped wrong: %+v", hubs[1])
	}
}

func TestRelayOptionsFallsBackWhenEmpty(t *testing.T) {
	hubs := relayOptions(&RegisterResponse{})
	if len(hubs) == 0 {
		t.Fatal("expected fallback hubs, got none")
	}
}

func TestDecodeCode(t *testing.T) {
	payload, _ := json.Marshal(map[string]string{
		"u": "https://example.com/api/marketplace/register",
		"c": "secret123",
	})
	encoded := base64.RawURLEncoding.EncodeToString(payload)

	url, secret, err := DecodeCode(encoded)
	if err != nil {
		t.Fatalf("DecodeCode error: %v", err)
	}
	if url != "https://example.com/api/marketplace/register" {
		t.Errorf("url = %q", url)
	}
	if secret != "secret123" {
		t.Errorf("secret = %q", secret)
	}
}

func TestDecodeCodeRejectsGarbage(t *testing.T) {
	if _, _, err := DecodeCode("!!! not base64 !!!"); err == nil {
		t.Fatal("expected an error on a garbage code")
	}
}
