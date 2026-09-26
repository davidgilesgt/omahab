package knowledge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// Embedding worker config materialization (F-embedding-worker-no-models).
//
// The appliance boots the worker with an empty models map; it serves no
// adapter until setup records an explicit document-search choice
// (english|worldwide|full_text via SetIndexSetupChoice) and omahabd writes
// the corresponding pinned_models.json, then restarts
// omahab-embedding-worker. This file maps the stored choice to exact worker
// config bytes so setup writes real pinned models — never an empty default
// masquerading as configured. English is always included for Hindsight
// memory (even full-text-only, which adds no document model). The worker's
// /health then reflects the loaded models (ok with the required aliases
// ready, degraded otherwise).

// EmbeddingWorkerConfigPath is the worker config omahabd renders. The
// systemd unit reads exactly this path (see nix/apps.nix
// omahab-embedding-worker --config).
const EmbeddingWorkerConfigPath = "/var/lib/omahab/embedding/pinned_models.json"

// EmbeddingModelsBaseDir is where supervised setup staging lands. It lives
// outside backup sources (/var/lib, never /srv/omahab).
const EmbeddingModelsBaseDir = "/var/lib/omahab/models"

// EmbeddingWorkerUser owns the rendered config alongside root (0640
// root:omahab-embedding, mirroring the nix embedding-init ownership) so the
// sandboxed worker (ProtectSystem=strict + ReadOnlyPaths) can read it.
const EmbeddingWorkerUser = "omahab-embedding"

// EnglishAlias is staged for every choice: Hindsight memory embeddings
// always run on the English model, even when document search is worldwide
// or full-text-only.
const EnglishAlias = "omahab-embed-english"

// WorldwideAlias is the document-search model for mixed-language collections.
const WorldwideAlias = "omahab-embed-worldwide"

// ChoiceModelAlias maps a document-search choice to its embedding alias.
// Empty means full-text only (no document model; memory still uses English).
func ChoiceModelAlias(choice string) (string, error) {
	switch normalizedChoice(choice) {
	case "english":
		return EnglishAlias, nil
	case "worldwide":
		return WorldwideAlias, nil
	case "full_text":
		return "", nil
	case "":
		return "", fmt.Errorf("document search mode not chosen: pick english, worldwide, or full-text only")
	default:
		return "", fmt.Errorf("unknown document search mode %q: want english, worldwide, or full_text", choice)
	}
}

// ChoiceModelAliases returns every worker model a choice requires: English
// is mandatory for Hindsight memory alongside the selected document index
// (english -> English; worldwide -> English + worldwide; full_text ->
// English only for memory, no document embeddings).
func ChoiceModelAliases(choice string) ([]string, error) {
	switch normalizedChoice(choice) {
	case "english":
		return []string{EnglishAlias}, nil
	case "worldwide":
		return []string{EnglishAlias, WorldwideAlias}, nil
	case "full_text":
		return []string{EnglishAlias}, nil
	case "":
		return nil, fmt.Errorf("document search mode not chosen: pick english, worldwide, or full-text only")
	default:
		return nil, fmt.Errorf("unknown document search mode %q: want english, worldwide, or full_text", choice)
	}
}

func normalizedChoice(choice string) string {
	n := strings.TrimSpace(strings.ToLower(choice))
	if n == "fulltext" {
		n = "full_text"
	}
	return n
}

// EmbeddingWorkerConfigForChoice renders pinned_models.json bytes for the
// stored setup choice, embedding every model ChoiceModelAliases requires
// (English always for memory, plus worldwide for mixed-language documents).
// Entries come verbatim from the compiled-in pinned reference (same source
// as PinnedModels display metadata). Empty/invalid choices fail — setup
// must require an explicit choice, never default silently.
func EmbeddingWorkerConfigForChoice(choice string) ([]byte, error) {
	aliases, err := ChoiceModelAliases(choice)
	if err != nil {
		return nil, err
	}
	return multiModelWorkerConfig(aliases)
}

func multiModelWorkerConfig(aliases []string) ([]byte, error) {
	var doc struct {
		Models           map[string]json.RawMessage `json:"models"`
		ModelsBaseDir    string                     `json:"models_base_dir"`
		AllowTestAdapter bool                       `json:"allow_test_adapter"`
	}
	if err := json.Unmarshal(embeddedPinnedModels, &doc); err != nil {
		return nil, fmt.Errorf("parse pinned reference: %w", err)
	}
	models := make(map[string]json.RawMessage, len(aliases))
	for _, alias := range aliases {
		raw, ok := doc.Models[alias]
		if !ok || len(bytes.TrimSpace(raw)) == 0 {
			return nil, fmt.Errorf("pinned reference has no model %q", alias)
		}
		models[alias] = raw
	}
	base := doc.ModelsBaseDir
	if strings.TrimSpace(base) == "" {
		base = EmbeddingModelsBaseDir
	}
	out := map[string]any{
		"models":             models,
		"models_base_dir":    base,
		"allow_test_adapter": false,
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render worker config: %w", err)
	}
	return append(b, '\n'), nil
}

// WriteEmbeddingWorkerConfig atomically writes the rendered choice config to
// path with 0640 perms and root:omahab-embedding ownership (the worker is
// sandboxed; without group ownership it cannot read the file). Ownership is
// enforced when running as root (production omahabd): lookup or chown
// failure fails the write rather than leaving an unreadable config behind.
// Non-root callers (unit tests) skip the chown silently. It returns the
// model alias selected ("" for full-text only).
func WriteEmbeddingWorkerConfig(path, choice string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("worker config path is required")
	}
	alias, err := ChoiceModelAlias(choice)
	if err != nil {
		return "", err
	}
	content, err := EmbeddingWorkerConfigForChoice(choice)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create worker config dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".pinned_models.*.tmp")
	if err != nil {
		return "", fmt.Errorf("create worker config temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write worker config: %w", err)
	}
	if err := tmp.Chmod(0o640); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("chmod worker config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close worker config: %w", err)
	}
	if err := chownWorkerConfig(tmpName); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("install worker config: %w", err)
	}
	return alias, nil
}

// chownWorkerConfig points the staged config at root:EmbeddingWorkerUser,
// mirroring the nix embedding-init ownership. Non-root callers (unit tests)
// skip silently; root callers get a hard error on lookup/chown failure so a
// runtime worker can never be left with an unreadable config.
func chownWorkerConfig(tmpName string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	u, err := user.Lookup(EmbeddingWorkerUser)
	if err != nil {
		return fmt.Errorf("lookup %s: %w", EmbeddingWorkerUser, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("parse uid for %s: %w", EmbeddingWorkerUser, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return fmt.Errorf("parse gid for %s: %w", EmbeddingWorkerUser, err)
	}
	if err := os.Chown(tmpName, uid, gid); err != nil {
		return fmt.Errorf("chown worker config: %w", err)
	}
	return nil
}
