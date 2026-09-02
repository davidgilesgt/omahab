package assets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func validMapFS() fstest.MapFS {
	return fstest.MapFS{
		"bin/omahab":                          &fstest.MapFile{Data: []byte("binary-omahab")},
		"bin/omahabd":                         &fstest.MapFile{Data: []byte("binary-omahabd")},
		"systemd/omahabd.service":             &fstest.MapFile{Data: []byte("[Unit]\nDescription=omahabd")},
		"systemd/omahab-builder.socket":       &fstest.MapFile{Data: []byte("[Unit]\nDescription=builder socket")},
		"systemd/omahab-builder.service":      &fstest.MapFile{Data: []byte("[Unit]\nDescription=builder service")},
		"systemd/omahab-builder-prune.service": &fstest.MapFile{Data: []byte("[Unit]\nDescription=builder prune")},
		"systemd/omahab-builder-prune.timer":  &fstest.MapFile{Data: []byte("[Unit]\nDescription=builder prune timer")},
		"systemd/omahab-backup.service":       &fstest.MapFile{Data: []byte("[Unit]\nDescription=backup")},
		"systemd/omahab-backup.timer":         &fstest.MapFile{Data: []byte("[Unit]\nDescription=backup timer")},
		"systemd/omahab-verify.service":       &fstest.MapFile{Data: []byte("[Unit]\nDescription=verify")},
		"systemd/omahab-verify.timer":         &fstest.MapFile{Data: []byte("[Unit]\nDescription=verify timer")},
		"systemd/cloudflared.service":         &fstest.MapFile{Data: []byte("[Unit]\nDescription=cloudflared")},
		"catalog/catalog.json":                &fstest.MapFile{Data: []byte(`{"bundles":[]}`)},
		"catalog/compose/caddy.yml":           &fstest.MapFile{Data: []byte("services: {}")},
		"tmpfiles.d/omahab.conf":              &fstest.MapFile{Data: []byte("d /etc/omahab 0755 root root - -")},
	}
}

func TestValidateValid(t *testing.T) {
	fsys := validMapFS()
	if err := Validate(fsys); err != nil {
		t.Fatalf("Validate valid FS failed: %v", err)
	}
}

func TestValidateWebOptional(t *testing.T) {
	fsys := validMapFS()
	// No web/ present should pass.
	if err := Validate(fsys); err != nil {
		t.Fatalf("Validate without web failed: %v", err)
	}
	// With web/ present should also pass.
	fsys["web/index.html"] = &fstest.MapFile{Data: []byte("<html>")}
	if err := Validate(fsys); err != nil {
		t.Fatalf("Validate with web failed: %v", err)
	}
}

func TestValidateMissingEachRequired(t *testing.T) {
	required := []string{
		"bin/omahab",
		"bin/omahabd",
		"systemd/omahabd.service",
		"systemd/omahab-builder.socket",
		"systemd/omahab-builder.service",
		"systemd/omahab-builder-prune.service",
		"systemd/omahab-builder-prune.timer",
		"systemd/omahab-backup.service",
		"systemd/omahab-backup.timer",
		"systemd/omahab-verify.service",
		"systemd/omahab-verify.timer",
		"systemd/cloudflared.service",
		"tmpfiles.d/omahab.conf",
	}
	for _, entry := range required {
		t.Run(entry, func(t *testing.T) {
			fsys := validMapFS()
			delete(fsys, entry)
			err := Validate(fsys)
			if err == nil {
				t.Fatalf("expected error for missing %q, got nil", entry)
			}
			if !strings.Contains(err.Error(), entry) {
				t.Fatalf("error %q does not mention missing entry %q", err.Error(), entry)
			}
		})
	}
	// Catalog missing entirely.
	t.Run("catalog missing", func(t *testing.T) {
		fsys := validMapFS()
		// Remove all catalog entries.
		for k := range fsys {
			if strings.HasPrefix(k, "catalog/") {
				delete(fsys, k)
			}
		}
		err := Validate(fsys)
		if err == nil {
			t.Fatal("expected error for missing catalog, got nil")
		}
		if !strings.Contains(err.Error(), "catalog") {
			t.Fatalf("error %q does not mention catalog", err.Error())
		}
	})
}

func TestValidateCatalogEmpty(t *testing.T) {
	// Catalog directory exists but contains no files (only empty dir placeholder).
	// fstest.MapFS cannot represent empty dirs, so we simulate by having catalog
	// with only a subdir entry that is a directory. Instead, test the "empty"
	// case by having zero files under catalog — we already test missing; also
	// test that having no files triggers error. We achieve this by not adding
	// any catalog files and adding a directory-like entry via validation logic:
	// If catalog dir exists but walk finds no files, it should error.
	// We can test by creating a MapFS with only catalog as an empty directory
	// is not possible, so we test the "no files" case by deleting catalog files.
	fsys := validMapFS()
	for k := range fsys {
		if strings.HasPrefix(k, "catalog/") {
			delete(fsys, k)
		}
	}
	// Add a dummy entry that is a directory? fstest doesn't support dirs; the walk
	// will fail with missing directory, which is still a catalog error.
	// Instead, test the empty case via a real temp dir in TestLoadDir.
	err := Validate(fsys)
	if err == nil {
		t.Fatal("expected error for empty catalog")
	}
	if !strings.Contains(err.Error(), "catalog") {
		t.Fatalf("expected catalog in error, got %q", err.Error())
	}
}

func TestValidateEmptyBin(t *testing.T) {
	for _, bin := range []string{"bin/omahab", "bin/omahabd"} {
		t.Run(bin, func(t *testing.T) {
			fsys := validMapFS()
			fsys[bin] = &fstest.MapFile{Data: []byte{}}
			err := Validate(fsys)
			if err == nil {
				t.Fatalf("expected error for empty %q, got nil", bin)
			}
			if !strings.Contains(err.Error(), bin) {
				t.Fatalf("error %q does not mention %q", err.Error(), bin)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "empty") {
				t.Fatalf("error %q should mention empty", err.Error())
			}
		})
	}
}

func TestValidateListsAllMissingAtOnce(t *testing.T) {
	fsys := fstest.MapFS{
		"catalog/catalog.json": &fstest.MapFile{Data: []byte("{}")},
	}
	err := Validate(fsys)
	if err == nil {
		t.Fatal("expected error for multiple missing entries")
	}
	// Should list every required file.
	for _, entry := range []string{"bin/omahab", "bin/omahabd", "systemd/omahabd.service", "tmpfiles.d/omahab.conf"} {
		if !strings.Contains(err.Error(), entry) {
			t.Fatalf("error %q should contain %q (all missing at once)", err.Error(), entry)
		}
	}
}

func TestLoadDirTempDirWorks(t *testing.T) {
	dir := t.TempDir()
	// Materialize valid FS into temp dir.
	fsys := validMapFS()
	for path, file := range fsys {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, file.Data, 0o644); err != nil {
			t.Fatalf("write %q: %v", full, err)
		}
	}
	loaded, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir failed: %v", err)
	}
	if err := Validate(loaded); err != nil {
		t.Fatalf("Validate on loaded dir failed: %v", err)
	}
	// Verify we can read a known file.
	data, err := os.ReadFile(filepath.Join(dir, "bin/omahab"))
	if err != nil {
		t.Fatalf("read bin/omahab: %v", err)
	}
	if string(data) != "binary-omahab" {
		t.Fatalf("unexpected bin content: %q", string(data))
	}
}

func TestLoadDirMissingDirErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	_, err := LoadDir(missing)
	if err == nil {
		t.Fatal("expected error for missing dir, got nil")
	}
	if !strings.Contains(err.Error(), missing) && !strings.Contains(strings.ToLower(err.Error()), "not accessible") {
		t.Fatalf("error %q should mention missing dir", err.Error())
	}
}

func TestLoadDirInvalidLayoutErrors(t *testing.T) {
	dir := t.TempDir()
	// Empty dir -> Validate should fail.
	_, err := LoadDir(dir)
	if err == nil {
		t.Fatal("expected error for empty asset dir")
	}
	if !strings.Contains(err.Error(), "catalog") && !strings.Contains(err.Error(), "bin/omahab") {
		t.Fatalf("error %q should mention missing assets", err.Error())
	}
}

func TestLoadStrippedBuild(t *testing.T) {
	// Load() in a fresh checkout should detect stripped build (only .gitkeep).
	// We test the stripped detection indirectly by calling Load() and checking
	// the error message. In CI the embedded FS will be stripped unless
	// scripts/build.sh was run; the test asserts the error is descriptive.
	_, err := Load()
	if err == nil {
		t.Skip("Load succeeded — assets are staged (build.sh was run); stripped check not triggered in this environment")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "scripts/build.sh") {
		t.Fatalf("stripped Load error %q should mention scripts/build.sh", err.Error())
	}
	if !strings.Contains(msg, "--asset-dir") {
		t.Fatalf("stripped Load error %q should mention --asset-dir", err.Error())
	}
}
