package backups

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSQLite(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT); INSERT INTO t (v) VALUES ('a'),('b')"); err != nil {
		t.Fatal(err)
	}
}

// stagedServerConfigTree builds a v2-style staged tree: control.db under
// the fixed staging path plus a v2 manifest describing it.
func stagedServerConfigTree(t *testing.T, dir string) {
	t.Helper()
	staging := filepath.Join(dir, backupStagingDir)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSQLite(t, filepath.Join(staging, "control.db"))
	if err := os.WriteFile(filepath.Join(staging, "storage.json"), []byte(`{"version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	controlSHA, err := SHA256File(filepath.Join(staging, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	storageSHA, err := SHA256File(filepath.Join(staging, "storage.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := SnapshotManifest{
		FormatVersion: ManifestFormatVersion,
		Instance:      ManifestInstance{ID: "inst-1", Domain: "omahab.com", Placement: "vps"},
		System:        ManifestSystem{ReleaseRef: "v1"},
		Scopes: []ManifestScope{{
			Name: ScopeServerConfig,
			Artifacts: []ManifestArtifact{
				{File: "control.db", SHA256: controlSHA, Kind: ArtifactSQLiteDump},
				{File: "storage.json", SHA256: storageSHA, Kind: ArtifactFile},
			},
		}},
	}
	if err := WriteManifest(staging, m); err != nil {
		t.Fatal(err)
	}
}

func TestValidateStagingPasses(t *testing.T) {
	dir := t.TempDir()
	stagedServerConfigTree(t, dir)
	rep, err := ValidateStaging(context.Background(), dir)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rep.SQLiteChecked != 1 || rep.SQLiteOK != 1 {
		t.Fatalf("report = %+v, want one sqlite proof", rep)
	}
	if rep.Manifest == nil || len(rep.Manifest.Scopes) != 1 || rep.Manifest.Scopes[0].Name != ScopeServerConfig {
		t.Fatalf("report = %+v, want the v2 server-config manifest", rep)
	}
}

func TestValidateStagingRejectsArtifactSHA256Mismatch(t *testing.T) {
	dir := t.TempDir()
	stagedServerConfigTree(t, dir)
	// Tamper with one staged artifact after the manifest recorded its
	// hash: validation must refuse the tree before any promotion.
	if err := os.WriteFile(filepath.Join(dir, backupStagingDir, "storage.json"), []byte(`{"version":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ValidateStaging(context.Background(), dir)
	if err == nil {
		t.Fatal("expected sha256 mismatch rejection")
	}
	for _, want := range []string{"server-config", "storage.json", "mismatch"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("mismatch must name %s: %v", want, err)
		}
	}
}

func TestValidateStagingRejectsMissingManifest(t *testing.T) {
	dir := t.TempDir()
	writeSQLite(t, filepath.Join(dir, "app.db"))
	if err := os.WriteFile(filepath.Join(dir, "photo.jpg"), []byte("img"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ValidateStaging(context.Background(), dir)
	if err == nil {
		t.Fatal("expected refusal: pre-v2 snapshots are not restoreable")
	}
}

func TestValidateStagingRejectsCorruptDB(t *testing.T) {
	dir := t.TempDir()
	stagedServerConfigTree(t, dir)
	// Truncate the database file: integrity must fail before any promotion.
	f, err := os.OpenFile(filepath.Join(dir, backupStagingDir, "control.db"), os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(512); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := ValidateStaging(context.Background(), dir); err == nil {
		t.Fatal("expected integrity failure for truncated dump")
	}
}

func TestValidateStagingRejectsEscapes(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "evil")); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateStaging(context.Background(), dir); err == nil {
		t.Fatal("expected escape refusal for symlink leaving staging")
	}
}

func TestValidateStagingRejectsEmpty(t *testing.T) {
	if _, err := ValidateStaging(context.Background(), t.TempDir()); err == nil {
		t.Fatal("expected failure for empty tree")
	}
}
