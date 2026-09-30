package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/knowledge"
)

// wireKnowledgeTestService migrates the knowledge tables into the test
// backend's store and attaches a real knowledge service, mirroring
// production wiring in backend.go.
func wireKnowledgeTestService(t *testing.T, b *Backend) {
	t.Helper()
	ctx := context.Background()
	if err := b.store.Migrate(ctx, knowledge.Migrations()...); err != nil {
		t.Fatal(err)
	}
	b.knowledge = knowledge.New(b.db, knowledge.ServiceOption{})
}

func knowledgeCheckStatus(t *testing.T, b *Backend) string {
	t.Helper()
	st, err := b.GetSetupStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return checkByID(t, st.Checks, "knowledge_index_setup").Status
}

func TestKnowledgeIndexSetupPendingWhenEmpty(t *testing.T) {
	b, _ := newSetupBackend(t, nil)
	wireKnowledgeTestService(t, b)
	st, err := b.GetSetupStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(t, st.Checks, "knowledge_index_setup")
	if c.Status != "pending" {
		t.Fatalf("status = %q want pending", c.Status)
	}
	if c.Label != "Document search" || c.Owner != "operator" {
		t.Fatalf("label/owner = %q/%q", c.Label, c.Owner)
	}
	if c.Action != "Choose a document search mode." {
		t.Fatalf("action = %q", c.Action)
	}
}

func TestKnowledgeIndexSetupOKChoices(t *testing.T) {
	ctx := context.Background()
	for _, choice := range []string{"english", "worldwide", "full_text"} {
		b, _ := newSetupBackend(t, nil)
		wireKnowledgeTestService(t, b)
		// Hermetic staging: tiny verified fixtures under TempDir + local
		// download origin. The persisted choice must survive without
		// touching /var/lib/omahab or the internet.
		fx := setupTinyEmbeddingFixtureWithDownload(t, nil)
		if err := b.KnowledgeSetIndexSetup(ctx, choice); err != nil {
			t.Fatalf("set %q: %v", choice, err)
		}
		// A fresh read must observe the persisted choice (Setup/AI reload).
		got, err := b.KnowledgeGetIndexSetup(ctx)
		if err != nil || got != choice {
			t.Fatalf("get after set %q = %q, %v", choice, got, err)
		}
		if status := knowledgeCheckStatus(t, b); status != "ok" {
			t.Fatalf("choice %q status = %q want ok", choice, status)
		}
		fx.assertNoVarLib(t)
	}
}

func TestKnowledgeIndexSetupFailedOnInvalid(t *testing.T) {
	ctx := context.Background()
	b, _ := newSetupBackend(t, nil)
	wireKnowledgeTestService(t, b)
	if _, err := b.db.ExecContext(ctx, `INSERT INTO knowledge_settings (key, value, updated_at) VALUES ('index_setup_choice', 'german', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	st, err := b.GetSetupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(t, st.Checks, "knowledge_index_setup")
	if c.Status != "failed" {
		t.Fatalf("invalid choice status = %q want failed", c.Status)
	}
	if !strings.Contains(c.Detail, "german") {
		t.Fatalf("detail should name the bad value, got %q", c.Detail)
	}
}

func TestKnowledgeIndexSetupFailedOnStorageError(t *testing.T) {
	ctx := context.Background()
	b, _ := newSetupBackend(t, nil)
	wireKnowledgeTestService(t, b)
	if _, err := b.db.ExecContext(ctx, `DROP TABLE knowledge_settings`); err != nil {
		t.Fatal(err)
	}
	st, err := b.GetSetupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(t, st.Checks, "knowledge_index_setup")
	// A failed read must never report complete.
	if c.Status != "failed" {
		t.Fatalf("dropped table status = %q want failed", c.Status)
	}
}

func TestKnowledgeIndexSetupFailedWhenUnconfigured(t *testing.T) {
	b, _ := newSetupBackend(t, nil)
	st, err := b.GetSetupStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(t, st.Checks, "knowledge_index_setup")
	if c.Status != "failed" {
		t.Fatalf("nil knowledge status = %q want failed", c.Status)
	}
}

func TestKnowledgeIndexSetupMetaAndOrder(t *testing.T) {
	pending := applySetupCheckMeta(apitypes.SetupCheck{ID: "knowledge_index_setup", Status: "pending"})
	if pending.Action != "Choose a document search mode." {
		t.Fatalf("pending action = %q", pending.Action)
	}
	ok := applySetupCheckMeta(apitypes.SetupCheck{ID: "knowledge_index_setup", Status: "ok"})
	if ok.Action != "" {
		t.Fatalf("ok action should be cleared, got %q", ok.Action)
	}
	got := orderSetupChecks([]apitypes.SetupCheck{
		{ID: "backups_configured"}, {ID: "knowledge_index_setup"}, {ID: "admin_passkeys"}, {ID: "restore"},
	})
	pos := map[string]int{}
	for i, c := range got {
		pos[c.ID] = i
	}
	// Backups lead the checklist (DESIGN §5.4): restore, then the
	// repository, then everything downstream of enrollment.
	if !(pos["restore"] < pos["backups_configured"] && pos["backups_configured"] < pos["admin_passkeys"] && pos["admin_passkeys"] < pos["knowledge_index_setup"]) {
		t.Fatalf("order = %v, want restore < backups_configured < admin_passkeys < knowledge_index_setup", got)
	}
}

// tinyEmbeddingFixture stages deterministic verified model files under
// t.TempDir so setup exercises the real PrepareChoiceModel verify+download
// path without huge production weights, /var/lib writes, or internet.
type tinyEmbeddingFixture struct {
	modelsDir      string
	configPath     string
	hits           *atomic.Int64
	englishFiles   map[string]string
	worldwideFiles map[string]string
}

func (f *tinyEmbeddingFixture) assertNoVarLib(t *testing.T) {
	t.Helper()
	for _, p := range []string{embeddingWorkerConfigPath, embeddingModelsBaseDir, f.modelsDir, f.configPath} {
		if strings.HasPrefix(p, "/var/lib/omahab") {
			t.Fatalf("test path must not live under /var/lib/omahab, got %q", p)
		}
	}
}

func tinyModelContents() (english, worldwide map[string]string) {
	english = map[string]string{
		"onnx/model.bin": "tiny-english-weights-v1",
		"tokenizer.json": `{"tiny":"english"}`,
	}
	worldwide = map[string]string{
		"onnx/model.bin": "tiny-worldwide-weights-v1",
		"tokenizer.json": `{"tiny":"worldwide"}`,
	}
	return english, worldwide
}

func tinySHAHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func tinyDirHash(files map[string]string) string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte("\n"))
		h.Write([]byte(files[k]))
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func buildTinyPinnedJSON(modelsDir string, english, worldwide map[string]string) []byte {
	fileEntries := func(files map[string]string) []map[string]any {
		keys := make([]string, 0, len(files))
		for k := range files {
			keys = append(keys, k)
		}
		for i := 1; i < len(keys); i++ {
			for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
				keys[j], keys[j-1] = keys[j-1], keys[j]
			}
		}
		var out []map[string]any
		for _, k := range keys {
			out = append(out, map[string]any{
				"path":   k,
				"sha256": tinySHAHex(files[k]),
				"size":   int64(len(files[k])),
			})
		}
		return out
	}
	doc := map[string]any{
		"models": map[string]any{
			"omahab-embed-english": map[string]any{
				"model_id":        "test/english",
				"repo":            "test/english",
				"revision":        "testrev1",
				"artifact_path":   filepath.Join(modelsDir, "test-english"),
				"artifact_sha256": tinyDirHash(english),
				"files":           fileEntries(english),
			},
			"omahab-embed-worldwide": map[string]any{
				"model_id":        "test/worldwide",
				"repo":            "test/worldwide",
				"revision":        "testrev1",
				"artifact_path":   filepath.Join(modelsDir, "test-worldwide"),
				"artifact_sha256": tinyDirHash(worldwide),
				"files":           fileEntries(worldwide),
			},
		},
		"models_base_dir":    modelsDir,
		"allow_test_adapter": false,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return raw
}

// setupTinyEmbeddingFixture redirects the worker config, models dir, pinned
// reference, and download origin into t.TempDir with deterministic tiny
// models served from a local httptest server (no /var/lib, no internet).
// When serve is nil the server 404s every download so missing/corrupt
// artifacts fail fast and deterministically.
func setupTinyEmbeddingFixture(t *testing.T, serve map[string]string, hits *atomic.Int64) *tinyEmbeddingFixture {
	t.Helper()
	english, worldwide := tinyModelContents()
	root := t.TempDir()
	modelsDir := filepath.Join(root, "models")
	configPath := filepath.Join(root, "embedding", "pinned_models.json")
	restoreJSON := knowledge.SetPinnedModelsJSONForTest(buildTinyPinnedJSON(modelsDir, english, worldwide))
	t.Cleanup(restoreJSON)
	origConfigPath := embeddingWorkerConfigPath
	origModelsDir := embeddingModelsBaseDir
	embeddingWorkerConfigPath = configPath
	embeddingModelsBaseDir = modelsDir
	t.Cleanup(func() {
		embeddingWorkerConfigPath = origConfigPath
		embeddingModelsBaseDir = origModelsDir
	})
	if hits == nil {
		hits = &atomic.Int64{}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if serve == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		for prefix, body := range serve {
			if strings.HasSuffix(r.URL.Path, prefix) {
				_, _ = w.Write([]byte(body))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	restoreFetch := knowledge.SetFetchSeamsForTest(srv.URL, srv.Client())
	t.Cleanup(restoreFetch)
	return &tinyEmbeddingFixture{
		modelsDir:      modelsDir,
		configPath:     configPath,
		hits:           hits,
		englishFiles:   english,
		worldwideFiles: worldwide,
	}
}

// setupTinyEmbeddingFixtureWithDownload serves the tiny fixture contents over
// HTTP so KnowledgeSetIndexSetup exercises the real download+verify path.
func setupTinyEmbeddingFixtureWithDownload(t *testing.T, hits *atomic.Int64) *tinyEmbeddingFixture {
	t.Helper()
	english, worldwide := tinyModelContents()
	serve := map[string]string{}
	for p, body := range english {
		serve["/test/english/resolve/testrev1/"+p] = body
	}
	for p, body := range worldwide {
		serve["/test/worldwide/resolve/testrev1/"+p] = body
	}
	return setupTinyEmbeddingFixture(t, serve, hits)
}

func stageTinyFiles(t *testing.T, modelsDir, subdir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(modelsDir, subdir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
}

func assertTinyStaged(t *testing.T, modelsDir, subdir string, files map[string]string) {
	t.Helper()
	for rel, want := range files {
		raw, err := os.ReadFile(filepath.Join(modelsDir, subdir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("staged %s/%s: %v", subdir, rel, err)
		}
		if string(raw) != want {
			t.Fatalf("staged %s/%s = %q want %q", subdir, rel, raw, want)
		}
	}
}

// newEmbeddingWorkerBackend builds a backend with knowledge wired and the
// embedding-worker bundle installed through the scripted runner.
func newEmbeddingWorkerBackend(t *testing.T, runner *scriptedRunner) *Backend {
	t.Helper()
	ctx := context.Background()
	b, _ := newSetupBackend(t, runner)
	wireKnowledgeTestService(t, b)
	digest := "sha256:" + strings.Repeat("a", 64)
	cat, err := apps.NewCatalog(
		testSetupBundle("caddy", digest, domain.ExposurePublic, "", nil),
		testSetupBundle("embedding-worker", digest, domain.ExposurePrivate, "", nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := apps.NewService(b.db, apps.Options{Catalog: cat, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	b.apps = svc
	if err := b.ensureDefaultApp(ctx, testSetupBundle("caddy", digest, domain.ExposurePublic, "", nil), "omahab.com"); err != nil {
		t.Fatalf("install caddy: %v", err)
	}
	if err := b.ensureDefaultApp(ctx, testSetupBundle("embedding-worker", digest, domain.ExposurePrivate, "", nil), "omahab.com"); err != nil {
		t.Fatalf("install embedding-worker: %v", err)
	}
	return b
}

func TestEnsureEmbeddingWorkerConfigWritesAndRedeploys(t *testing.T) {
	ctx := context.Background()
	var hits atomic.Int64
	fx := setupTinyEmbeddingFixtureWithDownload(t, &hits)
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b := newEmbeddingWorkerBackend(t, runner)
	// Plant the choice without side effects so ensure is the first to
	// stage (real tiny download), write config, and restart. The full
	// KnowledgeSetIndexSetup path is covered by TestKnowledgeIndexSetupOKChoices.
	if err := b.knowledge.SetIndexSetupChoice(ctx, "english"); err != nil {
		t.Fatal(err)
	}
	base := runner.starts()
	if err := b.ensureEmbeddingWorkerConfig(ctx); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	assertTinyStaged(t, fx.modelsDir, "test-english", fx.englishFiles)
	if hits.Load() == 0 {
		t.Fatal("expected local download of the tiny english artifact")
	}
	downloaded := hits.Load()
	raw, err := os.ReadFile(embeddingWorkerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "omahab-embed-english") {
		t.Fatalf("worker config should pin the english model, got %s", string(raw))
	}
	if got := runner.starts() - base; got != 1 {
		t.Fatalf("worker restarts = %d, want 1", got)
	}
	// Second run re-verifies idempotently but must not restart the worker
	// or hit the network again.
	if err := b.ensureEmbeddingWorkerConfig(ctx); err != nil {
		t.Fatalf("re-ensure: %v", err)
	}
	if hits.Load() != downloaded {
		t.Fatalf("re-ensure must reuse verified artifacts, hits %d -> %d", downloaded, hits.Load())
	}
	if got := runner.starts() - base; got != 1 {
		t.Fatalf("worker restarts = %d, want no restart for unchanged config", got)
	}
	fx.assertNoVarLib(t)
}

func TestEnsureEmbeddingWorkerConfigNoChoiceNoop(t *testing.T) {
	ctx := context.Background()
	var hits atomic.Int64
	fx := setupTinyEmbeddingFixtureWithDownload(t, &hits)
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b := newEmbeddingWorkerBackend(t, runner)
	base := runner.starts()
	if err := b.ensureEmbeddingWorkerConfig(ctx); err != nil {
		t.Fatalf("ensure without choice: %v", err)
	}
	if _, err := os.Stat(embeddingWorkerConfigPath); !os.IsNotExist(err) {
		t.Fatalf("no choice must not write config, stat err = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("no choice must not stage models, hits = %d", hits.Load())
	}
	if entries, err := os.ReadDir(fx.modelsDir); err == nil && len(entries) != 0 {
		t.Fatalf("no choice must not stage models, staged %d entries", len(entries))
	}
	if runner.starts() != base {
		t.Fatalf("worker restarts = %d, want 0", runner.starts()-base)
	}
	fx.assertNoVarLib(t)
}

func TestEnsureEmbeddingWorkerConfigFullTextKeepsMemoryModel(t *testing.T) {
	ctx := context.Background()
	var hits atomic.Int64
	fx := setupTinyEmbeddingFixtureWithDownload(t, &hits)
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b := newEmbeddingWorkerBackend(t, runner)
	if err := b.knowledge.SetIndexSetupChoice(ctx, "full_text"); err != nil {
		t.Fatal(err)
	}
	base := runner.starts()
	if err := b.ensureEmbeddingWorkerConfig(ctx); err != nil {
		t.Fatalf("ensure full_text: %v", err)
	}
	assertTinyStaged(t, fx.modelsDir, "test-english", fx.englishFiles)
	if _, err := os.Stat(filepath.Join(fx.modelsDir, "test-worldwide")); !os.IsNotExist(err) {
		t.Fatalf("full_text must stage English memory only, worldwide stat err = %v", err)
	}
	if hits.Load() == 0 {
		t.Fatal("expected local download of the tiny memory artifact")
	}
	raw, err := os.ReadFile(embeddingWorkerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "omahab-embed-english") {
		t.Fatalf("full_text must keep the English memory model, got %s", string(raw))
	}
	if strings.Contains(string(raw), "omahab-embed-worldwide") {
		t.Fatalf("full_text must not add document models, got %s", string(raw))
	}
	if got := runner.starts() - base; got != 1 {
		t.Fatalf("worker restarts = %d, want 1 to pick up the opt-out", got)
	}
	fx.assertNoVarLib(t)
}

func TestEnsureEmbeddingWorkerConfigInvalidChoiceFails(t *testing.T) {
	ctx := context.Background()
	var hits atomic.Int64
	fx := setupTinyEmbeddingFixtureWithDownload(t, &hits)
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b := newEmbeddingWorkerBackend(t, runner)
	if _, err := b.db.ExecContext(ctx, `INSERT INTO knowledge_settings (key, value, updated_at) VALUES ('index_setup_choice', 'german', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	base := runner.starts()
	if err := b.ensureEmbeddingWorkerConfig(ctx); err == nil {
		t.Fatal("invalid stored choice must fail, never default silently")
	}
	if runner.starts() != base {
		t.Fatalf("worker restarts = %d, want 0 on failure", runner.starts()-base)
	}
	if hits.Load() != 0 {
		t.Fatalf("invalid choice must be rejected before staging, hits = %d", hits.Load())
	}
	fx.assertNoVarLib(t)
}

func TestEnsureEmbeddingWorkerConfigWorldwidePinsWorldwide(t *testing.T) {
	ctx := context.Background()
	var hits atomic.Int64
	fx := setupTinyEmbeddingFixtureWithDownload(t, &hits)
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b := newEmbeddingWorkerBackend(t, runner)
	if err := b.knowledge.SetIndexSetupChoice(ctx, "worldwide"); err != nil {
		t.Fatal(err)
	}
	base := runner.starts()
	if err := b.ensureEmbeddingWorkerConfig(ctx); err != nil {
		t.Fatalf("ensure worldwide: %v", err)
	}
	assertTinyStaged(t, fx.modelsDir, "test-english", fx.englishFiles)
	assertTinyStaged(t, fx.modelsDir, "test-worldwide", fx.worldwideFiles)
	if hits.Load() == 0 {
		t.Fatal("expected local download of the tiny worldwide artifacts")
	}
	raw, err := os.ReadFile(embeddingWorkerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "omahab-embed-worldwide") {
		t.Fatalf("worker config should pin the worldwide model, got %s", string(raw))
	}
	if !strings.Contains(string(raw), "omahab-embed-english") {
		t.Fatalf("worldwide config must retain the English memory model, got %s", string(raw))
	}
	if got := runner.starts() - base; got != 1 {
		t.Fatalf("worker restarts = %d, want 1", got)
	}
	fx.assertNoVarLib(t)
}

func TestEnsureEmbeddingWorkerConfigMissingArtifactFails(t *testing.T) {
	ctx := context.Background()
	var hits atomic.Int64
	// 404 origin: nothing pre-staged, so staging must attempt the real
	// download path and fail deterministically without the internet.
	fx := setupTinyEmbeddingFixture(t, nil, &hits)
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b := newEmbeddingWorkerBackend(t, runner)
	// Plant the choice without staging so ensure faces a cold cache.
	if err := b.knowledge.SetIndexSetupChoice(ctx, "english"); err != nil {
		t.Fatal(err)
	}
	base := runner.starts()
	if err := b.ensureEmbeddingWorkerConfig(ctx); err == nil {
		t.Fatal("missing artifact must fail, never write config silently")
	} else if !strings.Contains(err.Error(), "stage embedding model") {
		t.Fatalf("missing artifact error = %q, want staging failure", err)
	}
	if hits.Load() == 0 {
		t.Fatal("missing artifact must attempt download before failing")
	}
	if _, err := os.Stat(embeddingWorkerConfigPath); !os.IsNotExist(err) {
		t.Fatalf("failed staging must not write config, stat err = %v", err)
	}
	if runner.starts() != base {
		t.Fatalf("worker restarts = %d, want 0 on staging failure", runner.starts()-base)
	}
	fx.assertNoVarLib(t)
}

func TestEnsureEmbeddingWorkerConfigCorruptArtifactFails(t *testing.T) {
	ctx := context.Background()
	var hits atomic.Int64
	// 404 origin: the corrupt tree cannot self-heal, so verification must
	// fail deterministically without the internet.
	fx := setupTinyEmbeddingFixture(t, nil, &hits)
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b := newEmbeddingWorkerBackend(t, runner)
	if err := b.knowledge.SetIndexSetupChoice(ctx, "english"); err != nil {
		t.Fatal(err)
	}
	stageTinyFiles(t, fx.modelsDir, "test-english", fx.englishFiles)
	corruptPath := filepath.Join(fx.modelsDir, "test-english", "onnx", "model.bin")
	if err := os.WriteFile(corruptPath, []byte("tampered-bytes"), 0o640); err != nil {
		t.Fatal(err)
	}
	base := runner.starts()
	if err := b.ensureEmbeddingWorkerConfig(ctx); err == nil {
		t.Fatal("corrupt artifact must fail, never write config silently")
	} else if !strings.Contains(err.Error(), "stage embedding model") {
		t.Fatalf("corrupt artifact error = %q, want staging failure", err)
	}
	if hits.Load() == 0 {
		t.Fatal("corrupt artifact must attempt re-download before failing")
	}
	if _, err := os.Stat(embeddingWorkerConfigPath); !os.IsNotExist(err) {
		t.Fatalf("failed staging must not write config, stat err = %v", err)
	}
	if runner.starts() != base {
		t.Fatalf("worker restarts = %d, want 0 on staging failure", runner.starts()-base)
	}
	fx.assertNoVarLib(t)
}
