package knowledge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F-embedding-worker-no-models: the stored setup choice must materialize a
// real pinned model so the worker serves the chosen embedding after restart.
func TestEmbeddingWorkerConfigEnglish768(t *testing.T) {
	raw, err := EmbeddingWorkerConfigForChoice("english")
	if err != nil {
		t.Fatalf("config for english: %v", err)
	}
	var doc struct {
		Models map[string]struct {
			ModelID      string `json:"model_id"`
			ArtifactPath string `json:"artifact_path"`
			Dimensions   int    `json:"dimensions"`
		} `json:"models"`
		ModelsBaseDir    string `json:"models_base_dir"`
		AllowTestAdapter bool   `json:"allow_test_adapter"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse rendered config: %v", err)
	}
	pin, ok := doc.Models["omahab-embed-english"]
	if !ok {
		t.Fatalf("rendered models = %v, want omahab-embed-english", doc.Models)
	}
	if len(doc.Models) != 1 {
		t.Fatalf("rendered %d models, want exactly 1", len(doc.Models))
	}
	if pin.ModelID != "nomic-ai/nomic-embed-text-v1.5" {
		t.Fatalf("model_id = %q, want nomic-ai/nomic-embed-text-v1.5", pin.ModelID)
	}
	if pin.Dimensions != 768 {
		t.Fatalf("dimensions = %d, want 768", pin.Dimensions)
	}
	if !strings.HasPrefix(pin.ArtifactPath, "/var/lib/omahab/models/") {
		t.Fatalf("artifact_path = %q, want inside /var/lib/omahab/models", pin.ArtifactPath)
	}
	if doc.AllowTestAdapter {
		t.Fatal("production config must not enable the test adapter")
	}
	alias, err := ChoiceModelAlias("english")
	if err != nil || alias != "omahab-embed-english" {
		t.Fatalf("alias = %q, err = %v", alias, err)
	}
}

func TestEmbeddingWorkerConfigWorldwideBoth(t *testing.T) {
	raw, err := EmbeddingWorkerConfigForChoice("worldwide")
	if err != nil {
		t.Fatalf("config for worldwide: %v", err)
	}
	var doc struct {
		Models map[string]struct {
			ModelID    string `json:"model_id"`
			Dimensions int    `json:"dimensions"`
		} `json:"models"`
		AllowTestAdapter bool `json:"allow_test_adapter"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse rendered config: %v", err)
	}
	if len(doc.Models) != 2 {
		t.Fatalf("worldwide renders %d models, want English (memory) + worldwide (documents)", len(doc.Models))
	}
	eng, ok := doc.Models["omahab-embed-english"]
	if !ok || eng.Dimensions != 768 {
		t.Fatalf("english entry = %+v, want 768-dim memory model", doc.Models)
	}
	world, ok := doc.Models["omahab-embed-worldwide"]
	if !ok || world.Dimensions != 1024 {
		t.Fatalf("worldwide entry = %+v, want 1024-dim document model", doc.Models)
	}
	if world.ModelID != "Xenova/multilingual-e5-large" {
		t.Fatalf("worldwide model_id = %q, want pinned production source", world.ModelID)
	}
	if doc.AllowTestAdapter {
		t.Fatal("production config must not enable the test adapter")
	}
}

func TestEmbeddingWorkerConfigFullTextMemoryEnglish(t *testing.T) {
	raw, err := EmbeddingWorkerConfigForChoice("full_text")
	if err != nil {
		t.Fatalf("config for full_text: %v", err)
	}
	var doc struct {
		Models map[string]struct {
			Dimensions int `json:"dimensions"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse rendered config: %v", err)
	}
	// Full-text document search stages no document model, but memory still
	// requires English: exactly the English entry is rendered.
	if len(doc.Models) != 1 {
		t.Fatalf("full-text config models = %v, want exactly English (memory)", doc.Models)
	}
	if d := doc.Models["omahab-embed-english"].Dimensions; d != 768 {
		t.Fatalf("memory dimensions = %d, want 768", d)
	}
	if alias, err := ChoiceModelAlias("full_text"); err != nil || alias != "" {
		t.Fatalf("full-text document alias = %q, err = %v", alias, err)
	}
	got, err := ChoiceModelAliases("full_text")
	if err != nil || len(got) != 1 || got[0] != "omahab-embed-english" {
		t.Fatalf("full-text required models = %v, err = %v", got, err)
	}
	got, err = ChoiceModelAliases("worldwide")
	if err != nil || len(got) != 2 || got[0] != "omahab-embed-english" || got[1] != "omahab-embed-worldwide" {
		t.Fatalf("worldwide required models = %v, err = %v", got, err)
	}
}

// No silent default: setup must require an explicit choice.
func TestEmbeddingWorkerConfigRequiresChoice(t *testing.T) {
	if _, err := EmbeddingWorkerConfigForChoice(""); err == nil {
		t.Fatal("empty choice must fail")
	}
	if _, err := EmbeddingWorkerConfigForChoice("german"); err == nil {
		t.Fatal("unknown choice must fail")
	}
	if _, err := ChoiceModelAlias(""); err == nil {
		t.Fatal("empty choice alias must fail")
	}
}

func TestWriteEmbeddingWorkerConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embedding", "pinned_models.json")
	alias, err := WriteEmbeddingWorkerConfig(path, "english")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if alias != "omahab-embed-english" {
		t.Fatalf("alias = %q, want omahab-embed-english", alias)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var doc struct {
		Models map[string]struct {
			Dimensions int `json:"dimensions"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse written config: %v", err)
	}
	if d := doc.Models["omahab-embed-english"].Dimensions; d != 768 {
		t.Fatalf("written dimensions = %d, want 768", d)
	}
	if _, err := WriteEmbeddingWorkerConfig(path, "full_text"); err != nil {
		t.Fatalf("rewrite full_text: %v", err)
	}
	raw, _ = os.ReadFile(path)
	var mem struct {
		Models map[string]struct {
			Dimensions int `json:"dimensions"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &mem); err != nil || len(mem.Models) != 1 {
		t.Fatalf("rewritten full_text = %s, want exactly English (memory)", raw)
	}
	if d := mem.Models["omahab-embed-english"].Dimensions; d != 768 {
		t.Fatalf("rewritten memory dimensions = %d, want 768", d)
	}
}
