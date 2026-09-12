package knowledge

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Compiled-in fallback: with no env pointers and a cwd that contains no
// workers/embedding tree, PinnedModels must still resolve via the
// compiled-in copy of pinned_models.json.example — production has no cwd
// and no env, so this path must never return 'not found'.
func TestPinnedModelsEmbeddedFallback(t *testing.T) {
	oldCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(oldCwd); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	}()
	t.Setenv("PINNED_MODELS_PATH", "")
	t.Setenv("EMBEDDING_WORKER_CONFIG", "")

	// Sanity: tmp really has no candidate tree.
	if _, err := os.Stat(filepath.Join(tmp, "workers", "embedding", "pinned_models.json.example")); !os.IsNotExist(err) {
		t.Fatalf("tmp dir unexpectedly contains pinned models file")
	}

	models, err := PinnedModels()
	if err != nil {
		t.Fatalf("PinnedModels via embedded fallback: %v", err)
	}
	if len(models) < 2 {
		t.Fatalf("want at least 2 embedded models, got %d", len(models))
	}
	for i := 1; i < len(models); i++ {
		if models[i-1].Alias > models[i].Alias {
			t.Fatalf("models not sorted: %+v", models)
		}
	}
}

// An embedded parse that yields zero models is display-metadata-empty,
// not an error: callers get an empty slice with nil error.
func TestPinnedFromBytesEmpty(t *testing.T) {
	out, err := pinnedFromBytes([]byte(`{"models":{}}`))
	if err != nil {
		t.Fatalf("pinnedFromBytes empty: %v", err)
	}
	if out == nil {
		t.Fatalf("want non-nil empty slice, got nil")
	}
	if len(out) != 0 {
		t.Fatalf("want 0 models, got %d", len(out))
	}
}

// The compiled-in copy must stay in sync with the on-disk example
// (workers/embedding/pinned_models.json.example is the read-only
// reference). Test cwd is the package dir, so a relative read works here
// even though production cannot rely on cwd.
func TestEmbeddedPinnedModelsInSync(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "workers", "embedding", "pinned_models.json.example"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	want, err := pinnedFromBytes(raw)
	if err != nil {
		t.Fatalf("parse example: %v", err)
	}
	got, err := pinnedFromBytes(embeddedPinnedModels)
	if err != nil {
		t.Fatalf("parse embedded: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("embedded copy out of sync with workers/embedding/pinned_models.json.example:\ngot  %+v\nwant %+v", got, want)
	}
}
