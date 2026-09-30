package backups

import (
	"context"
	"database/sql"
	"encoding/json"
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

// TestValidateStagingRejectsDatabaseSHA256Mismatch pins the v2 integrity
// contract for database dumps: scope.Databases hashes are verified against
// the staged content exactly like artifacts.
func TestValidateStagingRejectsDatabaseSHA256Mismatch(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, backupStagingDir)
	stagedServerConfigTree(t, dir)
	dump := filepath.Join(staging, "immich.pgdump")
	if err := os.WriteFile(dump, []byte("pgdump-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Rebuild the manifest with a database record vouching for different
	// content than what was staged.
	raw, err := os.ReadFile(filepath.Join(staging, manifestFileName))
	if err != nil {
		t.Fatal(err)
	}
	var m SnapshotManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m.Scopes[0].Databases = []ManifestDatabase{{
		Name: "immich", File: "immich.pgdump", SHA256: strings.Repeat("c", 64),
	}}
	if err := WriteManifest(staging, m); err != nil {
		t.Fatal(err)
	}
	_, err = ValidateStaging(context.Background(), dir)
	if err == nil {
		t.Fatal("expected database sha256 mismatch rejection")
	}
	for _, want := range []string{"database", "immich.pgdump", "mismatch"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("mismatch must name %s: %v", want, err)
		}
	}
	// The matching hash must pass (dump validation itself is skipped
	// when pg binaries are absent, but the hash check runs).
	sum, err := SHA256File(dump)
	if err != nil {
		t.Fatal(err)
	}
	m.Scopes[0].Databases[0].SHA256 = sum
	if err := WriteManifest(staging, m); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateStaging(context.Background(), dir); err != nil {
		t.Fatalf("matching database hash must validate: %v", err)
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
