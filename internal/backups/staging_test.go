package backups

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
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

func TestValidateStagingPasses(t *testing.T) {
	dir := t.TempDir()
	writeSQLite(t, filepath.Join(dir, "app.db"))
	if err := os.WriteFile(filepath.Join(dir, "photo.jpg"), []byte("img"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := ValidateStaging(context.Background(), dir)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rep.SQLiteChecked != 1 || rep.SQLiteOK != 1 {
		t.Fatalf("report = %+v, want one sqlite proof", rep)
	}
	if !rep.Legacy || rep.ManifestFound {
		t.Fatalf("report = %+v, want legacy without manifest", rep)
	}
}

func TestValidateStagingRejectsCorruptDB(t *testing.T) {
	dir := t.TempDir()
	writeSQLite(t, filepath.Join(dir, "app.db"))
	// Truncate the database file: integrity must fail before any promotion.
	f, err := os.OpenFile(filepath.Join(dir, "app.db"), os.O_WRONLY, 0o644)
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
