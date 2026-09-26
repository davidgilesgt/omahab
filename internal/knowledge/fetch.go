package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Supervised setup download of every model a choice requires
// (F-embedding-worker-no-models).
//
// The ISO never bundles model weights (3 GiB cap): omahabd fetches exactly
// the choice's models during setup, pinned by revision with per-file sha256,
// stages atomically outside backup sources (/var/lib/omahab/models lives
// outside /srv/omahab), verifies, then writes the worker config and
// restarts the worker (UpgradeSetup setup hook). English is mandatory for
// Hindsight memory in every mode (even full-text-only, which stages no
// document model). allow_test_adapter is never enabled by this path.

// hfBaseURL is the download origin; tests override via fetchPinnedFiles.
const hfBaseURL = "https://huggingface.co"

// fetchBaseURL and fetchHTTPClient are test seams mirroring hfBaseURL and
// http.DefaultClient. Production leaves them at zero values (nil client means
// http.DefaultClient, empty base means hfBaseURL); tests redirect them at a
// local httptest server through SetFetchSeamsForTest so missing/corrupt
// artifacts exercise the real download+verify path without external network.
var fetchBaseURL string
var fetchHTTPClient *http.Client

// SetFetchSeamsForTest redirects the pinned-model download origin and client
// so staging tests run hermetically against a local server. It returns a
// restore func the test must defer. Production code never calls this.
func SetFetchSeamsForTest(baseURL string, client *http.Client) (restore func()) {
	oldBase, oldClient := fetchBaseURL, fetchHTTPClient
	fetchBaseURL, fetchHTTPClient = baseURL, client
	return func() { fetchBaseURL, fetchHTTPClient = oldBase, oldClient }
}

func fetchOrigin() (string, *http.Client) {
	base := fetchBaseURL
	if strings.TrimSpace(base) == "" {
		base = hfBaseURL
	}
	client := fetchHTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return base, client
}

// maxPinnedFileBytes bounds any single staged file (largest pin is ~613 MiB).
const maxPinnedFileBytes = 3 << 30

// pinnedSourceFile is one verifiable artifact file inside a pin.
type pinnedSourceFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// pinnedSource is the download contract for one alias, parsed from the same
// embedded pinned reference that renders the worker config (single source:
// workers/embedding/pinned_models.json.example, mirrored in models.go).
type pinnedSource struct {
	Repo           string             `json:"repo"`
	Revision       string             `json:"revision"`
	ArtifactPath   string             `json:"artifact_path"`
	ArtifactSHA256 string             `json:"artifact_sha256"`
	Files          []pinnedSourceFile `json:"files"`
}

// pinnedSourceForAlias extracts the download contract for alias from the
// embedded reference. Full-text-only has no alias and no source.
func pinnedSourceForAlias(alias string) (*pinnedSource, error) {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return nil, fmt.Errorf("no model alias for full-text-only choice: nothing to download")
	}
	var doc struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(embeddedPinnedModels, &doc); err != nil {
		return nil, fmt.Errorf("parse pinned reference: %w", err)
	}
	raw, ok := doc.Models[alias]
	if !ok {
		return nil, fmt.Errorf("pinned reference has no model %q", alias)
	}
	var entry struct {
		Repo           string             `json:"repo"`
		Revision       string             `json:"revision"`
		ArtifactPath   string             `json:"artifact_path"`
		ArtifactSHA256 string             `json:"artifact_sha256"`
		Files          []pinnedSourceFile `json:"files"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, fmt.Errorf("parse pin for %q: %w", alias, err)
	}
	if strings.TrimSpace(entry.Repo) == "" || strings.TrimSpace(entry.Revision) == "" {
		return nil, fmt.Errorf("pin for %q lacks repo/revision", alias)
	}
	if len(entry.Files) == 0 {
		return nil, fmt.Errorf("pin for %q lists no verifiable files", alias)
	}
	for _, f := range entry.Files {
		if strings.TrimSpace(f.Path) == "" || len(strings.TrimSpace(f.SHA256)) != 64 || f.Size <= 0 {
			return nil, fmt.Errorf("pin for %q has invalid file entry %+v", alias, f)
		}
	}
	src := &pinnedSource{
		Repo:           strings.TrimSpace(entry.Repo),
		Revision:       strings.TrimSpace(entry.Revision),
		ArtifactPath:   strings.TrimSpace(entry.ArtifactPath),
		ArtifactSHA256: strings.ToLower(strings.TrimSpace(entry.ArtifactSHA256)),
		Files:          entry.Files,
	}
	return src, nil
}

// PrepareChoiceModel ensures every artifact a setup choice requires is
// staged under modelsBaseDir and returns the document-search alias
// ("" for full-text-only, which stages English for memory but no document
// model). English is always staged first (mandatory Hindsight memory model);
// worldwide additionally stages the worldwide document model. All required
// artifacts are staged and verified BEFORE the caller rewrites the live
// worker config — never half-configured. Idempotent per alias: present,
// fully verified artifacts are reused without network. Only the choice's
// models are ever fetched.
func PrepareChoiceModel(ctx context.Context, choice, modelsBaseDir string) (string, error) {
	aliases, err := ChoiceModelAliases(choice)
	if err != nil {
		return "", err
	}
	docAlias, err := ChoiceModelAlias(choice)
	if err != nil {
		return "", err
	}
	for _, alias := range aliases {
		if err := FetchPinnedModel(ctx, alias, modelsBaseDir); err != nil {
			return "", err
		}
	}
	return docAlias, nil
}

// FetchPinnedModel stages the pinned artifact for alias under modelsBaseDir
// (default /var/lib/omahab/models). Skips network when the destination
// already verifies.
func FetchPinnedModel(ctx context.Context, alias, modelsBaseDir string) error {
	src, err := pinnedSourceForAlias(alias)
	if err != nil {
		return err
	}
	base := strings.TrimSpace(modelsBaseDir)
	if base == "" {
		base = EmbeddingModelsBaseDir
	}
	dest := filepath.Join(base, filepath.Base(strings.TrimSpace(src.ArtifactPath)))
	if dest == "" {
		return fmt.Errorf("pin for %q has empty artifact path", alias)
	}
	if verifyStagedArtifact(dest, src) == nil {
		return nil
	}
	baseURL, client := fetchOrigin()
	return fetchPinnedFiles(ctx, client, baseURL, src, dest)
}

// verifyStagedArtifact checks every pinned file plus the directory hash. Nil
// means the destination is usable as-is.
func verifyStagedArtifact(dest string, src *pinnedSource) error {
	for _, f := range src.Files {
		p := filepath.Join(dest, filepath.FromSlash(f.Path))
		if err := verifyFileSHA256(p, f.SHA256, f.Size); err != nil {
			return err
		}
	}
	if err := verifyDirSHA256(dest, src.ArtifactSHA256); err != nil {
		return err
	}
	return nil
}

func verifyFileSHA256(path, wantHex string, wantSize int64) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Size() != wantSize {
		return fmt.Errorf("%s size %d != pinned %d", path, fi.Size(), wantSize)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, wantHex) {
		return fmt.Errorf("%s sha256 mismatch", path)
	}
	return nil
}

// verifyDirSHA256 mirrors workers/embedding/config._sha256_of_path:
// sorted relative paths + contents. The worker gate enforces the same value
// at startup, so setup and worker agree on one hash.
func verifyDirSHA256(dir, wantHex string) error {
	var files []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("%s: no staged files", dir)
	}
	// Sort for determinism (Walk is lexical but be explicit).
	for i := 1; i < len(files); i++ {
		for j := i; j > 0 && files[j] < files[j-1]; j-- {
			files[j], files[j-1] = files[j-1], files[j]
		}
	}
	h := sha256.New()
	for _, rel := range files {
		h.Write([]byte(rel))
		h.Write([]byte("\n"))
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		if _, err := io.Copy(h, f); err != nil {
			_ = f.Close()
			return err
		}
		_ = f.Close()
		h.Write([]byte("\n"))
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, wantHex) {
		return fmt.Errorf("%s directory sha256 mismatch", dir)
	}
	return nil
}

// fetchPinnedFiles downloads every pinned file into a temp staging dir next
// to dest (same filesystem for atomic rename), verifies per-file sha256 and
// the directory hash, then renames into place. Staging lives outside backup
// sources (/var/lib, never /srv/omahab).
func fetchPinnedFiles(ctx context.Context, client *http.Client, baseURL string, src *pinnedSource, dest string) error {
	if client == nil {
		client = http.DefaultClient
	}
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return fmt.Errorf("create models dir: %w", err)
	}
	// The worker traverses/reads through ReadOnlyPaths as omahab-embedding:
	// the base dir must be root:omahab-embedding 0750 (nix init converges
	// the same). Fatal for root; modes still converge below for tests.
	if err := ownServiceTree(parent, false); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".stage-*")
	if err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()
	for _, f := range src.Files {
		if err := downloadPinnedFile(ctx, client, baseURL, src, f, stage); err != nil {
			return err
		}
	}
	if err := verifyDirSHA256(stage, src.ArtifactSHA256); err != nil {
		return fmt.Errorf("staged artifact failed verification: %w", err)
	}
	// Own the verified tree BEFORE the atomic rename: dirs 0750,
	// files 0640, root:omahab-embedding throughout, so the worker can
	// traverse and read the installed artifact. Root failures are fatal;
	// non-root callers (tests) converge modes and skip chown.
	if err := ownServiceTree(stage, true); err != nil {
		return err
	}
	if err := os.RemoveAll(dest); err != nil {
		return fmt.Errorf("clear destination: %w", err)
	}
	if err := os.Rename(stage, dest); err != nil {
		return fmt.Errorf("install staged artifact: %w", err)
	}
	return nil
}

// ownServiceTree converges dirs to 0750 and files to 0640 under root,
// chowned root:EmbeddingWorkerUser. With recurse=false only root itself is
// converged (models base dir). Ownership applies when running as root
// (production omahabd) and is fatal on failure; non-root callers converge
// modes only so tests can assert service-user accessibility by mode bits.
func ownServiceTree(root string, recurse bool) error {
	asRoot := os.Geteuid() == 0
	var uid, gid int
	if asRoot {
		u, err := user.Lookup(EmbeddingWorkerUser)
		if err != nil {
			return fmt.Errorf("lookup %s: %w", EmbeddingWorkerUser, err)
		}
		if uid, err = strconv.Atoi(u.Uid); err != nil {
			return fmt.Errorf("parse uid for %s: %w", EmbeddingWorkerUser, err)
		}
		if gid, err = strconv.Atoi(u.Gid); err != nil {
			return fmt.Errorf("parse gid for %s: %w", EmbeddingWorkerUser, err)
		}
	}
	ownOne := func(p string, dir bool) error {
		mode := os.FileMode(0o640)
		if dir {
			mode = 0o750
		}
		if err := os.Chmod(p, mode); err != nil {
			return fmt.Errorf("chmod %s: %w", p, err)
		}
		if asRoot {
			if err := os.Chown(p, uid, gid); err != nil {
				return fmt.Errorf("chown %s: %w", p, err)
			}
		}
		return nil
	}
	if err := ownOne(root, true); err != nil {
		return err
	}
	if !recurse {
		return nil
	}
	return filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		return ownOne(p, info.IsDir())
	})
}

func downloadPinnedFile(ctx context.Context, client *http.Client, baseURL string, src *pinnedSource, f pinnedSourceFile, stage string) error {
	if f.Size > maxPinnedFileBytes {
		return fmt.Errorf("%s pinned size %d exceeds limit", f.Path, f.Size)
	}
	url := strings.TrimRight(baseURL, "/") + "/" + src.Repo + "/resolve/" + src.Revision + "/" + f.Path
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", f.Path, err)
	}
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	req = req.WithContext(ctx2)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", f.Path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download %s: status %d", f.Path, resp.StatusCode)
	}
	outPath := filepath.Join(stage, filepath.FromSlash(f.Path))
	if err := os.MkdirAll(filepath.Dir(outPath), 0o750); err != nil {
		return fmt.Errorf("create staging subdir: %w", err)
	}
	out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("create staged file: %w", err)
	}
	h := sha256.New()
	written, err := io.CopyN(io.MultiWriter(out, h), io.LimitReader(resp.Body, maxPinnedFileBytes+1), maxPinnedFileBytes+1)
	cerr := out.Close()
	if err != nil && err != io.EOF {
		return fmt.Errorf("stream %s: %w", f.Path, err)
	}
	if cerr != nil {
		return fmt.Errorf("close staged file: %w", cerr)
	}
	if written != f.Size {
		return fmt.Errorf("%s downloaded %d bytes, pinned %d", f.Path, written, f.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, f.SHA256) {
		return fmt.Errorf("%s sha256 mismatch after download", f.Path)
	}
	return nil
}
