package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Golden dir hash cross-checked against the Python worker's
// config._sha256_of_path for layout {a.txt: "hello", sub/b.txt: "world"}:
// sorted rel paths, rel + "\n" + content + "\n" sequences.
const goldenDirHash = "34355a7fda449286104e90acb2c919219f1681e1ecc18d26687d9005aea26cd1"

func shaOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func testSource(files map[string]string) *pinnedSource {
	var out []pinnedSourceFile
	for p, body := range files {
		out = append(out, pinnedSourceFile{Path: p, SHA256: shaOf(body), Size: int64(len(body))})
	}
	// Deterministic dir hash over sorted rel paths.
	keys := []string{}
	for p := range files {
		keys = append(keys, p)
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
	return &pinnedSource{
		Repo:           "test/repo",
		Revision:       "testrev",
		ArtifactSHA256: hex.EncodeToString(h.Sum(nil)),
		Files:          out,
	}
}

func serveFiles(t *testing.T, files map[string]string, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		// Expect /test/repo/resolve/testrev/<path>.
		p := strings.TrimPrefix(r.URL.Path, "/test/repo/resolve/testrev/")
		body, ok := files[p]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
}

// Setup downloads only missing artifacts, verifies per-file sha256 and the
// directory hash, and stages atomically outside backup sources.
func TestFetchPinnedFilesRoundTrip(t *testing.T) {
	ctx := context.Background()
	files := map[string]string{"onnx/model.onnx": "fake-onnx-bytes", "tokenizer.json": `{"v":1}`}
	src := testSource(files)
	var hits atomic.Int64
	srv := serveFiles(t, files, &hits)
	t.Cleanup(srv.Close)
	dest := filepath.Join(t.TempDir(), "models", "test-model")
	if err := fetchPinnedFiles(ctx, srv.Client(), srv.URL, src, dest); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if hits.Load() != int64(len(files)) {
		t.Fatalf("requests = %d, want %d", hits.Load(), len(files))
	}
	for p, body := range files {
		raw, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(p)))
		if err != nil || string(raw) != body {
			t.Fatalf("staged %s = %q, err=%v", p, raw, err)
		}
	}
	// Idempotent: second fetch reuses the verified destination.
	if err := fetchPinnedFiles(ctx, srv.Client(), srv.URL, src, dest); err != nil {
		t.Fatalf("refetch: %v", err)
	}
}

func TestFetchPinnedFilesRejectsTamperedBytes(t *testing.T) {
	ctx := context.Background()
	want := map[string]string{"onnx/model.onnx": "good-bytes", "tokenizer.json": `{}`}
	src := testSource(want)
	got := map[string]string{"onnx/model.onnx": "evil-bytes", "tokenizer.json": `{}`}
	srv := serveFiles(t, got, nil)
	t.Cleanup(srv.Close)
	dest := filepath.Join(t.TempDir(), "models", "test-model")
	if err := fetchPinnedFiles(ctx, srv.Client(), srv.URL, src, dest); err == nil {
		t.Fatal("tampered bytes must fail verification")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("failed fetch must leave no destination, stat err=%v", err)
	}
}

func TestFetchPinnedFiles404Fails(t *testing.T) {
	ctx := context.Background()
	src := testSource(map[string]string{"onnx/model.onnx": "x"})
	srv := serveFiles(t, map[string]string{}, nil)
	t.Cleanup(srv.Close)
	dest := filepath.Join(t.TempDir(), "models", "test-model")
	if err := fetchPinnedFiles(ctx, srv.Client(), srv.URL, src, dest); err == nil {
		t.Fatal("missing upstream file must fail")
	}
}

// Go setup and the Python worker agree on the directory hash contract.
func TestVerifyDirSHA256Golden(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("world"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := verifyDirSHA256(dir, goldenDirHash); err != nil {
		t.Fatalf("golden dir hash: %v", err)
	}
	if err := verifyDirSHA256(dir, strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong dir hash must fail")
	}
}

// Invalid choices fail before any staging (offline).
func TestPrepareChoiceModelInvalidFails(t *testing.T) {
	for _, choice := range []string{"", "german"} {
		if _, err := PrepareChoiceModel(context.Background(), choice, t.TempDir()); err == nil {
			t.Fatalf("choice %q must fail", choice)
		}
	}
}

// Staging contract (memory-mandatory matrix): english stages English,
// worldwide stages English + worldwide, full_text stages English for memory
// with no document model. Document aliases stay distinct from required sets.
func TestPrepareChoiceModelMatrix(t *testing.T) {
	doc, err := ChoiceModelAlias("worldwide")
	if err != nil || doc != "omahab-embed-worldwide" {
		t.Fatalf("worldwide document alias = %q, err = %v", doc, err)
	}
	all, err := ChoiceModelAliases("worldwide")
	if err != nil || len(all) != 2 {
		t.Fatalf("worldwide required = %v, err = %v", all, err)
	}
	doc, err = ChoiceModelAlias("full_text")
	if err != nil || doc != "" {
		t.Fatalf("full_text document alias = %q, err = %v", doc, err)
	}
	all, err = ChoiceModelAliases("full_text")
	if err != nil || len(all) != 1 || all[0] != "omahab-embed-english" {
		t.Fatalf("full_text required = %v, err = %v", all, err)
	}
}
func TestPinnedSourceValidation(t *testing.T) {
	if _, err := pinnedSourceForAlias("no-such-alias"); err == nil {
		t.Fatal("unknown alias must fail")
	}
	if _, err := pinnedSourceForAlias(""); err == nil {
		t.Fatal("empty alias must fail")
	}
	// Real pins must carry repo/revision/files (no placeholders).
	for _, alias := range []string{"omahab-embed-english", "omahab-embed-worldwide"} {
		src, err := pinnedSourceForAlias(alias)
		if err != nil {
			t.Fatalf("%s: %v", alias, err)
		}
		if strings.TrimSpace(src.Revision) == "" || len(src.Revision) < 40 {
			t.Fatalf("%s revision = %q, want full commit sha", alias, src.Revision)
		}
		if strings.Contains(src.Revision, "abc123") || strings.TrimSpace(src.ArtifactSHA256) == strings.Repeat("0", 64) {
			t.Fatalf("%s still carries placeholder pin", alias)
		}
	}
}

// IntegrationAudit: the installed tree must be traversable/readable by the
// sandboxed worker (root:omahab-embedding 0750 dirs / 0640 files). Modes are
// asserted here (ownership converges to the service user when run as root).
func TestFetchPinnedFilesServiceAccessible(t *testing.T) {
	ctx := context.Background()
	files := map[string]string{"onnx/model.onnx": "bytes", "tokenizer.json": `{}`}
	src := testSource(files)
	srv := serveFiles(t, files, nil)
	t.Cleanup(srv.Close)
	base := t.TempDir()
	dest := filepath.Join(base, "models", "test-model")
	if err := fetchPinnedFiles(ctx, srv.Client(), srv.URL, src, dest); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(base, "models")); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o750 {
		t.Fatalf("models base perms = %v err=%v, want drwxr-x---", fi, err)
	}
	if err := filepath.Walk(dest, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		want := os.FileMode(0o640)
		if info.IsDir() {
			want = 0o750
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s perms = %o, want %o", p, info.Mode().Perm(), want)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
}
