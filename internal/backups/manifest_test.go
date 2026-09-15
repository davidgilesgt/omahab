package backups

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func testManifest() SnapshotManifest {
	return SnapshotManifest{
		FormatVersion: 1,
		InstanceID:    "inst-1",
		Domain:        "omahab.com",
		ReleaseRef:    "v1",
		Scopes:        []string{ScopeServerConfig, AppScope("immich")},
		Applications: []ManifestApp{{
			BundleID:  "immich",
			Version:   "v1",
			Volumes:   []ManifestDisk{{Volume: "immich:state", Path: "/srv/omahab/apps/immich"}},
			Databases: []string{"immich"},
			Secrets:   []string{"immich_db_password"},
		}},
	}
}

func TestManifestRoundtrip(t *testing.T) {
	dir := t.TempDir()
	m := testManifest()
	if err := WriteManifest(dir, m); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.InstanceID != "inst-1" || len(got.Applications) != 1 || got.Applications[0].BundleID != "immich" {
		t.Fatalf("manifest = %+v", got)
	}
	if got.CreatedAt == "" {
		t.Fatal("created_at must be stamped")
	}
}

func TestManifestRejectsUnknownVersion(t *testing.T) {
	dir := t.TempDir()
	m := testManifest()
	m.FormatVersion = 99
	if err := WriteManifest(dir, m); err == nil {
		t.Fatal("expected version rejection")
	}
}

func TestManifestRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	raw := `{"format_version":1,"instance_id":"i","domain":"d","release_ref":"r","created_at":"t","scopes":["server-config"],"applications":[],"evil":"x"}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(dir); err == nil {
		t.Fatal("expected unknown-field rejection; manifests must never smuggle commands")
	}
}

func TestScopeRoundtrip(t *testing.T) {
	if AppScope("immich") != "app:immich" {
		t.Fatal("app scope format")
	}
	id, ok := ScopeBundleID("app:immich")
	if !ok || id != "immich" {
		t.Fatal("scope split")
	}
	if _, ok := ScopeBundleID("server-config"); ok {
		t.Fatal("server scope must not split as app")
	}
}

func TestScopedPathsAndExcludesReachRestic(t *testing.T) {
	svc, _, fr := newTestService(t, nil)
	repo := mustConfigure(t, svc)
	_, err := svc.RunBackup(context.Background(), RunRequest{
		RepositoryID: repo.ID,
		Scopes:       []string{ScopeServerConfig},
		Paths:        []string{"/srv/omahab/apps/immich"},
		Excludes:     []string{"*.sock"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(fr.backupCalls) != 1 {
		t.Fatalf("restic calls = %d", len(fr.backupCalls))
	}
	call := fr.backupCalls[0]
	if len(call.Paths) != 1 || call.Paths[0] != "/srv/omahab/apps/immich" {
		t.Fatalf("paths = %v", call.Paths)
	}
	if len(call.Excludes) != 1 || call.Excludes[0] != "*.sock" {
		t.Fatalf("excludes = %v", call.Excludes)
	}
}
