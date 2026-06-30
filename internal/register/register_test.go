package register

import (
	"encoding/base64"
	"encoding/json"
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
