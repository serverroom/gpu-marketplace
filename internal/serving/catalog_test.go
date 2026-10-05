package serving

import (
	"strings"
	"testing"
)

func TestCatalogEntriesAreValid(t *testing.T) {
	ids := map[string]bool{}
	for _, m := range Catalog {
		if err := m.Validate(); err != nil {
			t.Errorf("catalog entry: %v", err)
		}
		if ids[m.ID] {
			t.Errorf("model %s is listed twice", m.ID)
		}
		ids[m.ID] = true
	}
	if len(Catalog) == 0 {
		t.Fatal("the catalog is empty")
	}
}

func TestGptOss120bIsPinned(t *testing.T) {
	m, ok := Lookup("gpt-oss-120b")
	if !ok {
		t.Fatal("gpt-oss-120b is not in the catalog")
	}
	if m.TotalBytes() != 65276850729 {
		t.Errorf("gpt-oss-120b weighs %d bytes; the pinned revision weighs 65276850729", m.TotalBytes())
	}
	shards := 0
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, "original/") || strings.HasPrefix(f.Path, "metal/") {
			t.Errorf("%s is the same weights in another format and must not be downloaded", f.Path)
		}
		if strings.HasSuffix(f.Path, ".safetensors") {
			shards++
		}
	}
	if shards != 15 {
		t.Errorf("want the 15 safetensors shards, got %d", shards)
	}
	// No image yet: it comes with the release that publishes the serving image.
	if m.Servable() {
		t.Errorf("gpt-oss-120b names an image (%s) before one was published", m.Image)
	}
}

func TestLookupUnknown(t *testing.T) {
	if _, ok := Lookup("gpt-4o-mini"); ok {
		t.Fatal("a model the catalog does not list was found")
	}
}

func TestValidFilePath(t *testing.T) {
	good := []string{"config.json", "model-00001-of-00014.safetensors", "sub/tokenizer.json", "_x"}
	bad := []string{"", "/etc/passwd", "../x", "a/../b", "a//b", "./a", ".hidden", "a\\b", "C:x", "a/", "-flag",
		"a\x00b", strings.Repeat("a", 513)}
	for _, p := range good {
		if !ValidFilePath(p) {
			t.Errorf("%q should be a valid path", p)
		}
	}
	for _, p := range bad {
		if ValidFilePath(p) {
			t.Errorf("%q should not be a valid path", p)
		}
	}
}

func TestValidateRefuses(t *testing.T) {
	base := func() Model {
		return Model{ID: "m", Repo: "o/r", Revision: strings.Repeat("a", 40), Engine: "vllm", MemoryGB: 1,
			Kinds: []string{kindContainerNV}, Files: []File{{"a.json", 1, strings.Repeat("b", 64)}}}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("a good entry was refused: %v", err)
	}
	cases := map[string]func(*Model){
		"id":           func(m *Model) { m.ID = "GPT 4" },
		"repo":         func(m *Model) { m.Repo = "no-slash" },
		"revision":     func(m *Model) { m.Revision = "main" },
		"image tag":    func(m *Model) { m.Image = "ghcr.io/x/y:latest" },
		"no memory":    func(m *Model) { m.MemoryGB = 0 },
		"no kinds":     func(m *Model) { m.Kinds = nil },
		"no files":     func(m *Model) { m.Files = nil },
		"unsafe path":  func(m *Model) { m.Files[0].Path = "../a.json" },
		"no size":      func(m *Model) { m.Files[0].Size = 0 },
		"no sha":       func(m *Model) { m.Files[0].SHA256 = "abc" },
		"listed twice": func(m *Model) { m.Files = append(m.Files, File{"A.json", 1, strings.Repeat("c", 64)}) },
	}
	for name, change := range cases {
		m := base()
		change(&m)
		if m.Validate() == nil {
			t.Errorf("%s: a bad entry was accepted", name)
		}
	}
	m := base()
	m.Image = "ghcr.io/serverroom/gpu-serving@sha256:" + strings.Repeat("d", 64)
	if err := m.Validate(); err != nil || !m.Servable() {
		t.Errorf("an image pinned by digest was refused: %v", err)
	}
}

func TestFits(t *testing.T) {
	m, _ := Lookup("gpt-oss-120b")
	if ok, why := m.Fits(kindContainerNV, 122); !ok {
		t.Errorf("a GB10 should fit gpt-oss-120b: %s", why)
	}
	if ok, _ := m.Fits(kindContainerNV, 64); ok {
		t.Error("64 GB should not fit gpt-oss-120b")
	}
	if ok, _ := m.Fits("qemu", 200); ok {
		t.Error("a machine without a GPU should not serve gpt-oss-120b")
	}
}
