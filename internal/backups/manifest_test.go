package backups

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func testManifest() SnapshotManifest {
	return SnapshotManifest{
		FormatVersion: ManifestFormatVersion,
		Instance:      ManifestInstance{ID: "inst-1", Domain: "omahab.com", Placement: "vps"},
		System: ManifestSystem{
			ReleaseRef:     "v1",
			NixpkgsRev:     "34ab99075ac4f7e40cf037eef32cb1c360bb85e9",
			BundlesRepoRev: "abcdef123456",
		},
		Scopes: []ManifestScope{
			{
				Name:     ScopeServerConfig,
				Requires: nil,
				Artifacts: []ManifestArtifact{
					{File: "control.db", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Kind: ArtifactSQLiteDump},
					{File: "storage.json", SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Kind: ArtifactFile},
				},
			},
			{
				Name:     AppScope("immich"),
				Requires: []string{ScopeServerConfig},
				Bundle: &ManifestBundle{
					ID:             "immich",
					Source:         "catalog",
					DefinitionPath: "",
					DefinitionRev:  "",
				},
				AppVersion:  "1.0.0",
				Volumes:     []ManifestDisk{{Volume: "immich:state", Path: "/srv/omahab/apps/immich"}},
				Databases:   []ManifestDatabase{{Name: "immich", File: "immich.pgdump", SHA256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}},
				SecretNames: []string{"immich_db_password"},
			},
		},
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
	if got.Instance.ID != "inst-1" || len(got.Scopes) != 2 {
		t.Fatalf("manifest = %+v", got)
	}
	app, ok := got.Scope(AppScope("immich"))
	if !ok || app.Bundle == nil || app.Bundle.ID != "immich" {
		t.Fatalf("app scope missing bundle: %+v", got.Scopes)
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
	raw := `{"format_version":2,"instance":{"id":"i","domain":"d","placement":"vps"},"system":{},"created_at":"t","scopes":[],"evil":"x"}`
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
