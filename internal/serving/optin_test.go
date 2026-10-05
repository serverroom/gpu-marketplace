package serving

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseOffer(t *testing.T) {
	good := map[string]Offer{
		`{"enabled":true,"model":"gpt-oss-120b"}`:  {Enabled: true, Model: "gpt-oss-120b"},
		`{"enabled":false}`:                        {},
		`{"enabled":false,"model":"gpt-oss-120b"}`: {},
	}
	for raw, want := range good {
		got := ParseOffer(json.RawMessage(raw))
		if got == nil || *got != want {
			t.Errorf("%s: got %+v, want %+v", raw, got, want)
		}
	}
	bad := []string{
		``, `null`, `[]`, `"yes"`, `{}`,
		`{"enabled":"true","model":"gpt-oss-120b"}`,
		`{"enabled":true}`,
		`{"enabled":true,"model":"GPT 4o"}`,
		`{"enabled":true,"model":"gpt-oss-120b","image":"x"}`,
		`{"enabled":true,"model":"` + strings.Repeat("a", 600) + `"}`,
	}
	for _, raw := range bad {
		if got := ParseOffer(json.RawMessage(raw)); got != nil {
			t.Errorf("%s: want no offer, got %+v", raw, got)
		}
	}
}

func TestOfferKeptAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	if LoadOffer(dir) != nil {
		t.Fatal("an offer before any was saved")
	}
	if err := SaveOffer(dir, Offer{Enabled: true, Model: "gpt-oss-120b"}); err != nil {
		t.Fatal(err)
	}
	if o := LoadOffer(dir); o == nil || !o.Enabled || o.Model != "gpt-oss-120b" {
		t.Fatalf("loaded %+v", o)
	}
}

func TestVeto(t *testing.T) {
	dir := t.TempDir()
	if Vetoed(dir) {
		t.Fatal("vetoed before anything was set")
	}
	if err := SetVeto(dir, true); err != nil || !Vetoed(dir) {
		t.Fatalf("serve off did not stick: %v", err)
	}
	if err := SetVeto(dir, false); err != nil || Vetoed(dir) {
		t.Fatalf("serve on did not lift it: %v", err)
	}
	if err := SetVeto(dir, false); err != nil {
		t.Fatalf("serve on twice: %v", err)
	}
}

func TestWanted(t *testing.T) {
	on := &Offer{Enabled: true, Model: "gpt-oss-120b"}
	if m, why := Wanted(on, false); m.ID != "gpt-oss-120b" || why != "" {
		t.Errorf("an opted-in machine: %q %q", m.ID, why)
	}
	if m, why := Wanted(on, true); m.ID != "" || !strings.Contains(why, "switched off on this machine") {
		t.Errorf("the host's veto must win over the panel: %q %q", m.ID, why)
	}
	if m, why := Wanted(nil, false); m.ID != "" || !strings.Contains(why, "control panel") {
		t.Errorf("no offer: %q %q", m.ID, why)
	}
	if m, why := Wanted(&Offer{Enabled: true, Model: "llama-9"}, false); m.ID != "" || !strings.Contains(why, "does not serve") {
		t.Errorf("a model this release does not list: %q %q", m.ID, why)
	}
}
