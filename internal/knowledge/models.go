package knowledge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// embeddedPinnedModelsJSON mirrors
// workers/embedding/pinned_models.json.example. go:embed patterns cannot
// reference parent directories, so the example content is duplicated here
// as the final fallback for production (no cwd, no env). models_test.go
// asserts it stays in sync with the on-disk example.
const embeddedPinnedModelsJSON = `{
  "models": {
    "omahab-embed-english": {
      "model_id": "nomic-ai/nomic-embed-text-v1.5",
      "revision": "e5a65b3c5f5a61234f1234567890abcd",
      "artifact_path": "/var/lib/omahab/models/nomic-embed-text-v1.5",
      "artifact_sha256": "0000000000000000000000000000000000000000000000000000000000000000",
      "dimensions": 768,
      "max_sequence_length": 8192,
      "license": "Apache-2.0",
      "size_bytes": 548000000,
      "expected_memory_mb": 512
    },
    "omahab-embed-worldwide": {
      "model_id": "Qwen/Qwen3-Embedding-0.6B",
      "revision": "pinned-revision-abc123",
      "artifact_path": "/var/lib/omahab/models/qwen3-embedding-0.6b",
      "artifact_sha256": "1111111111111111111111111111111111111111111111111111111111111111",
      "dimensions": 1024,
      "max_sequence_length": 8192,
      "license": "Apache-2.0",
      "size_bytes": 1200000000,
      "expected_memory_mb": 1024
    }
  },
  "models_base_dir": "/var/lib/omahab/models",
  "allow_test_adapter": false
}`

var embeddedPinnedModels = []byte(embeddedPinnedModelsJSON)
// ModelInfo describes a pinned embedding model for UI display.
type ModelInfo struct {
	Alias              string `json:"alias"`
	Name               string `json:"name"` // alias alias == name for UI
	ModelID            string `json:"model_id"`
	License            string `json:"license"`
	SizeBytes          int64  `json:"size_bytes"`
	ExpectedMemoryMB   int    `json:"expected_memory_mb"`
	Dimensions         int    `json:"dimensions,omitempty"`
	MaxSequenceLength  int    `json:"max_sequence_length,omitempty"`
	ArtifactPath       string `json:"artifact_path,omitempty"`
}

// pinnedModelsFile mirrors workers/embedding/pinned_models.json.
type pinnedFile struct {
	Models map[string]struct {
		ModelID           string `json:"model_id"`
		Revision          string `json:"revision"`
		ArtifactPath      string `json:"artifact_path"`
		ArtifactSHA256    string `json:"artifact_sha256"`
		Dimensions        int    `json:"dimensions"`
		MaxSequenceLength int    `json:"max_sequence_length"`
		License           string `json:"license"`
		SizeBytes         int64  `json:"size_bytes"`
		ExpectedMemoryMB  int    `json:"expected_memory_mb"`
	} `json:"models"`
}

// PinnedModels locates pinned_models.json in the repo and returns model metadata
// for UI display: name, license, download size, expected memory. It searches
// candidate paths in order:
//
//  1. $PINNED_MODELS_PATH / $EMBEDDING_WORKER_CONFIG if set
//  2. workers/embedding/pinned_models.json relative to cwd
//  3. workers/embedding/pinned_models.json.example fallback (repo always has this)
//  4. compiled-in copy of the example (final fallback: production has no
//     cwd and no env, so this never returns 'not found')
//
// The example file is used in tests and when no real pinned_models.json is
// present. An embedded parse that yields zero models returns an empty slice
// with a nil error — display metadata may be empty. Callers should treat the
// result as display metadata; the Python worker is the source of truth for
// runtime artifact validation.
func PinnedModels() ([]ModelInfo, error) {
	for _, p := range pinnedCandidates() {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		out, err := pinnedFromBytes(b)
		if err != nil {
			continue
		}
		if len(out) == 0 {
			continue
		}
		return out, nil
	}
	// Final fallback: embedded example so production (no cwd, no env)
	// never returns 'not found'. Display metadata may be empty: zero
	// models (or even unparseable bytes, defensively) yield an empty
	// slice with a nil error rather than a hard failure.
	if out, err := pinnedFromBytes(embeddedPinnedModels); err == nil {
		if out == nil {
			out = []ModelInfo{}
		}
		return out, nil
	}
	return []ModelInfo{}, nil
}
func pinnedFromBytes(b []byte) ([]ModelInfo, error) {
	var pf pinnedFile
	if err := json.Unmarshal(b, &pf); err != nil {
		return nil, err
	}
	out := []ModelInfo{}
	for alias, m := range pf.Models {
		out = append(out, ModelInfo{
			Alias:             alias,
			Name:              alias,
			ModelID:           m.ModelID,
			License:           m.License,
			SizeBytes:         m.SizeBytes,
			ExpectedMemoryMB:  m.ExpectedMemoryMB,
			Dimensions:        m.Dimensions,
			MaxSequenceLength: m.MaxSequenceLength,
			ArtifactPath:      m.ArtifactPath,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out, nil
}
func pinnedCandidates() []string {
	var out []string
	if v := os.Getenv("PINNED_MODELS_PATH"); v != "" {
		out = append(out, v)
	}
	if v := os.Getenv("EMBEDDING_WORKER_CONFIG"); v != "" {
		out = append(out, v)
	}
	if cwd, err := os.Getwd(); err == nil {
		dir := cwd
		for range 10 {
			out = append(out, filepath.Join(dir, "workers", "embedding", "pinned_models.json"))
			out = append(out, filepath.Join(dir, "workers", "embedding", "pinned_models.json.example"))
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	out = append(out, "workers/embedding/pinned_models.json")
	out = append(out, "workers/embedding/pinned_models.json.example")
	return dedup(out)
}

func dedup(in []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
// PinnedModelsFromPath is a test helper to load from an explicit path.
func PinnedModelsFromPath(path string) ([]ModelInfo, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return pinnedFromBytes(b)
}
