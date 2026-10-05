// Package serving is the inference API's side of a machine: the models it may
// serve, and their weights on its disk.
//
// A machine serves only while it is listed, idle and opted in by its host, and a
// rental always comes first. The serving engine runs in the same hardened
// runtime a rental does, and a worker beside it connects out to the
// marketplace's gateway over HTTPS on 443, so nothing on the host's network is
// opened and the relay tunnel is unchanged.
//
// WHAT MAY BE SERVED IS COMPILED INTO THE RELEASE. The control plane names a
// model by its id and can only pick one this release lists in Catalog, with the
// weights pinned to one commit and every file pinned to its SHA-256: the same
// rule internal/update follows for the agent's own binary. A new model, or new
// weights for one, is a new release, which machines install by themselves when
// idle.
package serving

import (
	"fmt"
	"regexp"
	"strings"
)

// File is one file of a model's weights, as the catalog pins it.
type File struct {
	// Path is relative to the model's revision, with forward slashes.
	Path   string
	Size   int64
	SHA256 string
}

// Model is one model a machine may serve.
type Model struct {
	// ID is the name a client asks the inference API for.
	ID string
	// Repo and Revision say where the weights come from: a Hugging Face
	// repository, at one commit.
	Repo     string
	Revision string
	License  string
	Files    []File
	// Engine is the inference engine the serving image runs.
	Engine string
	// MemoryGB is the GPU memory the model needs with room for its context
	// cache: on a machine whose GPU shares the system's memory (a GB10), of that
	// pool.
	MemoryGB int
	// Kinds are the machine kinds (control.Capability.Kind) that may serve it.
	Kinds []string
	// Image is the serving image (the engine and the worker) by digest:
	// "<registry>/<name>@sha256:<64 hex>". Empty while no image has been
	// published for this model: its weights can be downloaded, but it cannot be
	// served.
	Image string
}

// kindContainerNV is provisioner.KindContainer, the hardened container a DGX
// Spark (GB10) is rented as. Named here rather than imported, because the
// provisioner will import this package; a test in internal/provisioner checks
// that every kind the catalog names is one the provisioner knows.
const kindContainerNV = "container-nv"

// Catalog is every model this release can serve.
//
// gpt-oss-120b: OpenAI's open-weight mixture-of-experts model, Apache 2.0, in
// its published MXFP4 weights (65.3 GB). The `original/` and `metal/` folders
// of the repository hold the same weights in other formats and are not
// downloaded. Hashes read from the Hugging Face API on 2026-10-05; the small
// files' SHA-256 were computed from their bytes at this revision, which matched
// the git ids the API lists. Served with vLLM on GB10s (owner's decision,
// 2026-10-05); its image and the engine's arguments come with the release that
// publishes the serving image, measured on a real GB10.
var Catalog = []Model{
	{
		ID:       "gpt-oss-120b",
		Repo:     "openai/gpt-oss-120b",
		Revision: "b5c939de8f754692c1647ca79fbf85e8c1e70f8a",
		License:  "Apache-2.0",
		Engine:   "vllm",
		MemoryGB: 96,
		Kinds:    []string{kindContainerNV},
		Files: []File{
			{"LICENSE", 11357, "58d1e17ffe5109a7ae296caafcadfdbe6a7d176f0bc4ab01e12a689b0499d8bd"},
			{"USAGE_POLICY", 201, "fc48d386a7a7ff8b066f743cfe62df683ab16892450f5bb7357bb4de261cd037"},
			{"chat_template.jinja", 16738, "a4c9919cbbd4acdd51ccffe22da049264b1b73e59055fa58811a99efbd7c8146"},
			{"config.json", 2089, "933aeb666a3fd851133ddd7686414f369bc564c4185fb5704416550879f10566"},
			{"generation_config.json", 177, "f9970ada892d2d1f72e3ed0a6535ccebadd11897318794ca671d8c7014c957da"},
			{"model-00000-of-00014.safetensors", 4625017896, "695218884684c611fe08a74751ee443f971e9bd9bc062edba822da3fe45969b7"},
			{"model-00001-of-00014.safetensors", 4115586736, "a881aa5f561b26a22b14a8262aa61849ace349ffd73d74769e030ac90a1fcf8a"},
			{"model-00002-of-00014.safetensors", 4625017888, "022478dd04398c5bdb545a5be0a6437ecc2eb53d1dbd29edafcfff4b3ddf0a41"},
			{"model-00003-of-00014.safetensors", 4115586752, "47aee9e7b9d5bedb215042c01ccededd9bd9c30b0dddea862dc2506b9d6c74de"},
			{"model-00004-of-00014.safetensors", 4625017896, "f6c2752acda607b1d5ca52df9e75c1b9b2761e6875ff10c9bd6ddac473c0262e"},
			{"model-00005-of-00014.safetensors", 4115586696, "0c8dd401544c31cb93b8459eee7da20ea2a07626a59455d7d92b85257df9b46c"},
			{"model-00006-of-00014.safetensors", 4625017856, "28d839f2e027985a8b14e45f2323798862eddb7770ee9800ea6b7c803abee489"},
			{"model-00007-of-00014.safetensors", 4060267176, "c8958c5f183c04f6ea959cfd90562b5128124154b2bbf979b8a22b9405b30ed8"},
			{"model-00008-of-00014.safetensors", 4625017896, "bf1f2a88868ffc37d520dcf77d26f0e823710b5e682d473ff10f6974fa3b7517"},
			{"model-00009-of-00014.safetensors", 4170906304, "f72d34a4004241b45c332b61f8ffa124e9a913bc1ab442b66e717d3e94e741ce"},
			{"model-00010-of-00014.safetensors", 4625017896, "f48c867c2cb0a44bfc2f8768cb98e4aec9a350946fceacfebdcad5d32ad4a471"},
			{"model-00011-of-00014.safetensors", 4115586752, "a06851b2cfd35f48722f823bc1ab8f7bcb4a878a5b8e975f4d3544f230454eeb"},
			{"model-00012-of-00014.safetensors", 4064660808, "3af33667c307e20ae2a7648ea52653de46dd0171601ec5c696e47a2f5d5bf1e4"},
			{"model-00013-of-00014.safetensors", 4625017896, "bcbcb74b043e071d1e05471d500d74dcf661175e00878ed302ccdf1801a75aef"},
			{"model-00014-of-00014.safetensors", 4115586736, "54b1be1609696c307cc5ca117b1fa54feaddebffa04e9c2db117652a01964230"},
			{"model.safetensors.index.json", 54511, "ede2655fdc05008561983b6e0829c600727c28d591e071077377059f03a6c00e"},
			{"special_tokens_map.json", 98, "dd5e191d20c12d2fee1da5bae14ca1db0f5f4215300af691f23cdee97120a293"},
			{"tokenizer.json", 27868174, "0614fe83cadab421296e664e1f48f4261fa8fef6e03e63bb75c20f38e37d07d3"},
			{"tokenizer_config.json", 4200, "9279e942392b742d633c7adbb89ebe002c98399db8926a7af5125c726f404070"},
		},
	},
}

var (
	modelIDPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)
	repoPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	imagePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9./_-]{0,254}@sha256:[0-9a-f]{64}$`)
	filePartPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
)

// ValidModelID reports whether id can name a model: the control channel refuses
// anything else before looking it up.
func ValidModelID(id string) bool { return modelIDPattern.MatchString(id) }

// Lookup is the catalog's model with this id.
func Lookup(id string) (Model, bool) {
	for _, m := range Catalog {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// TotalBytes is the size of the model's weights.
func (m Model) TotalBytes() int64 {
	var total int64
	for _, f := range m.Files {
		total += f.Size
	}
	return total
}

// Servable reports whether this release can serve the model at all: it names
// the image to serve it with.
func (m Model) Servable() bool { return m.Image != "" }

// Fits reports whether a machine of this kind, with this much GPU memory (the
// shared pool on a machine with unified memory), can serve the model, and if
// not, why.
func (m Model) Fits(kind string, memoryGB int) (bool, string) {
	kindOK := false
	for _, k := range m.Kinds {
		if k == kind {
			kindOK = true
			break
		}
	}
	if !kindOK {
		return false, fmt.Sprintf("%s is not served on this kind of machine (%s)", m.ID, kind)
	}
	if memoryGB < m.MemoryGB {
		return false, fmt.Sprintf("%s needs %d GB of GPU memory; this machine has %d GB", m.ID, m.MemoryGB, memoryGB)
	}
	return true, ""
}

// ValidFilePath reports whether a catalog path is safe to join under a
// model's directory: relative, forward slashes, and no part that is empty,
// "." or "..", or starts with a dot.
func ValidFilePath(p string) bool {
	if p == "" || len(p) > 512 || strings.ContainsAny(p, "\\:\x00") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if !filePartPattern.MatchString(part) {
			return false
		}
	}
	return true
}

// Validate checks one catalog entry. Every entry of Catalog passes it (a test
// says so), and the weight cache checks it again before it writes anything.
func (m Model) Validate() error {
	if !ValidModelID(m.ID) {
		return fmt.Errorf("model id %q is not valid", m.ID)
	}
	if !repoPattern.MatchString(m.Repo) {
		return fmt.Errorf("%s: repository %q is not valid", m.ID, m.Repo)
	}
	if !revisionPattern.MatchString(m.Revision) {
		return fmt.Errorf("%s: revision %q is not a commit id", m.ID, m.Revision)
	}
	if m.Image != "" && !imagePattern.MatchString(m.Image) {
		return fmt.Errorf("%s: image %q is not pinned by digest", m.ID, m.Image)
	}
	if m.MemoryGB <= 0 || len(m.Kinds) == 0 {
		return fmt.Errorf("%s: says neither the memory it needs nor the machines it runs on", m.ID)
	}
	if len(m.Files) == 0 {
		return fmt.Errorf("%s: lists no files", m.ID)
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		if !ValidFilePath(f.Path) {
			return fmt.Errorf("%s: file path %q is not safe", m.ID, f.Path)
		}
		// The weight cache writes "<path>.part" and "<file>.tmp" beside a file.
		if strings.HasSuffix(f.Path, ".part") || strings.HasSuffix(f.Path, ".tmp") {
			return fmt.Errorf("%s: file path %q ends like the cache's own working files", m.ID, f.Path)
		}
		key := strings.ToLower(f.Path)
		if seen[key] {
			return fmt.Errorf("%s: file %q is listed twice", m.ID, f.Path)
		}
		seen[key] = true
		if f.Size <= 0 {
			return fmt.Errorf("%s: file %q has no size", m.ID, f.Path)
		}
		if !sha256Pattern.MatchString(f.SHA256) {
			return fmt.Errorf("%s: file %q has no SHA-256", m.ID, f.Path)
		}
	}
	return nil
}
