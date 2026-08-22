package knowledge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

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
//
// The example file is used in tests and when no real pinned_models.json is
// present. Callers should treat the result as display metadata; the Python
// worker is the source of truth for runtime artifact validation.
func PinnedModels() ([]ModelInfo, error) {
	candidates := pinnedCandidates()
	var lastErr error
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err != nil {
			lastErr = err
			continue
		}
		var pf pinnedFile
		if err := json.Unmarshal(b, &pf); err != nil {
			lastErr = fmt.Errorf("%s: %w", p, err)
			continue
		}
		var out []ModelInfo
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
		if len(out) == 0 {
			lastErr = fmt.Errorf("%s: no models", p)
			continue
		}
		return out, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no candidate")
	}
	return nil, fmt.Errorf("pinned models: not found (tried %v): %w", candidates, lastErr)
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
	var pf pinnedFile
	if err := json.Unmarshal(b, &pf); err != nil {
		return nil, err
	}
	var out []ModelInfo
	for alias, m := range pf.Models {
		out = append(out, ModelInfo{
			Alias:            alias,
			Name:             alias,
			ModelID:          m.ModelID,
			License:          m.License,
			SizeBytes:        m.SizeBytes,
			ExpectedMemoryMB: m.ExpectedMemoryMB,
			Dimensions:       m.Dimensions,
			ArtifactPath:     m.ArtifactPath,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out, nil
}
