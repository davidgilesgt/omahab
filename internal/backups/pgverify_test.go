package backups

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildStagedTree builds a staging tree with the manifest and artifacts
// rooted at manifestDir (already relative to dir) and returns the manifest
// written into it.
func buildStagedTree(t *testing.T, dir string, manifestRel string, files map[string]string) SnapshotManifest {
	t.Helper()
	staging := filepath.Join(dir, filepath.FromSlash(manifestRel))
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	var artifacts []ManifestArtifact
	for name, content := range files {
		p := filepath.Join(staging, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		sum, err := SHA256File(p)
		if err != nil {
			t.Fatal(err)
		}
		kind := ArtifactFile
		if strings.HasSuffix(name, ".pgdump") {
			kind = ArtifactPGDump
		}
		artifacts = append(artifacts, ManifestArtifact{File: name, SHA256: sum, Kind: kind})
	}
	m := SnapshotManifest{
		FormatVersion: ManifestFormatVersion,
		Instance:      ManifestInstance{ID: "inst-1", Domain: "omahab.com", Placement: "vps"},
		System:        ManifestSystem{ReleaseRef: "v1"},
		Scopes:        []ManifestScope{{Name: ScopeServerConfig, Artifacts: artifacts}},
	}
	if err := WriteManifest(staging, m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestStagedManifestDirResolvesExtractedAbsoluteCapture pins the real
// restic layout: captures snapshot the staging tree at its absolute system
// path, so an extracted snapshot puts the manifest at
// <target>/var/lib/omahab/backup-staging — not at the restore root.
func TestStagedManifestDirResolvesExtractedAbsoluteCapture(t *testing.T) {
	dir := t.TempDir()
	buildStagedTree(t, dir, "var/lib/omahab/backup-staging", map[string]string{"storage.json": `{"version":2}`})
	got, err := StagedManifestDir(dir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := filepath.Join(dir, "var", "lib", "omahab", "backup-staging")
	if got != want {
		t.Fatalf("dir = %q, want %q", got, want)
	}
	if _, err := ReadStagedManifest(dir); err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	// Full validation of the extracted layout must succeed end to end.
	if _, err := ValidateStaging(context.Background(), dir); err != nil {
		t.Fatalf("validate extracted tree: %v", err)
	}
}

// TestStagedManifestDirForCustomStateDir pins the parameterized variant
// used by the offline recovery executor with relocated state dirs.
func TestStagedManifestDirForCustomStateDir(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(t.TempDir(), "state")
	buildStagedTree(t, dir, filepath.Join(stateDir, "backup-staging"), map[string]string{"storage.json": `{"version":2}`})
	got, err := StagedManifestDirFor(dir, stateDir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != filepath.Join(dir, stateDir, "backup-staging") {
		t.Fatalf("dir = %q", got)
	}
	// The default resolver must not find a foreign-stateDir manifest.
	if _, err := StagedManifestDir(dir); err == nil {
		t.Fatal("default resolver must reject a manifest under a non-production state dir")
	}
}

// TestStagedManifestDirInPlaceStaging pins the in-place shapes: target is
// the restore root with backup-staging/ below it, or the staging dir
// itself.
func TestStagedManifestDirInPlaceStaging(t *testing.T) {
	root := t.TempDir()
	buildStagedTree(t, root, "backup-staging", map[string]string{"a.txt": "a"})
	got, err := StagedManifestDir(root)
	if err != nil {
		t.Fatalf("restore-root shape: %v", err)
	}
	if got != filepath.Join(root, backupStagingDir) {
		t.Fatalf("dir = %q", got)
	}
	// Target IS the staging directory.
	// Target IS the staging directory: the manifest sits directly in it.
	staging := filepath.Join(t.TempDir(), backupStagingDir)
	buildStagedTree(t, staging, ".", map[string]string{"a.txt": "a"})
	got, err = StagedManifestDir(staging)
	if err != nil {
		t.Fatalf("staging-dir shape: %v", err)
	}
	if got != staging {
		t.Fatalf("dir = %q", got)
	}
}

// TestStagedManifestDirRejectsAmbiguity: two manifests in one tree must
// never be guessed between.
func TestStagedManifestDirRejectsAmbiguity(t *testing.T) {
	dir := t.TempDir()
	buildStagedTree(t, dir, "backup-staging", map[string]string{"a.txt": "a"})
	buildStagedTree(t, dir, "var/lib/omahab/backup-staging", map[string]string{"a.txt": "a"})
	if _, err := StagedManifestDir(dir); err == nil || !strings.Contains(err.Error(), "more than one manifest") {
		t.Fatalf("err = %v, want ambiguity rejection", err)
	}
}

// TestStagedManifestDirRejectsMissingAndRelative: no manifest, or a
// relative target, are hard errors.
func TestStagedManifestDirRejectsMissingAndRelative(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := StagedManifestDir(dir); err == nil || !strings.Contains(err.Error(), "no v2 manifest") {
		t.Fatalf("err = %v, want missing-manifest rejection", err)
	}
	if _, err := StagedManifestDir("relative/path"); err == nil {
		t.Fatal("relative target must be rejected")
	}
}

// TestValidateStagingVerifiesExtractedLayoutArtifacts pins that artifact
// hashes verify against the manifest directory StagedManifestDir resolved
// — a mismatch in the extracted absolute capture layout fails closed.
func TestValidateStagingVerifiesExtractedLayoutArtifacts(t *testing.T) {
	dir := t.TempDir()
	buildStagedTree(t, dir, "var/lib/omahab/backup-staging", map[string]string{"storage.json": `{"version":2}`})
	if err := os.WriteFile(filepath.Join(dir, "var/lib/omahab/backup-staging/storage.json"),
		[]byte(`{"version":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ValidateStaging(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("err = %v, want sha256 mismatch rejection in extracted layout", err)
	}
}

// TestPgBinariesAbsentFailsClosed pins the validator contract: pg dumps in
// the tree without the tooling to prove them fail validation instead of
// reporting a silent skip.
func TestPgBinariesAbsentFailsClosed(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no initdb/pg_ctl/pg_restore on PATH
	dir := t.TempDir()
	buildStagedTree(t, dir, "var/lib/omahab/backup-staging", map[string]string{"app.pgdump": "PGDMP"})
	report, err := ValidateStaging(context.Background(), dir)
	if !errors.Is(err, ErrInvalid) || report.PGOK != 0 {
		t.Fatalf("missing tools produced database proof: report=%+v err=%v", report, err)
	}
}
