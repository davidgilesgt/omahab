package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/backups"
)

func TestCheckRestoreScopes(t *testing.T) {
	manifest := backups.SnapshotManifest{
		FormatVersion: backups.ManifestFormatVersion,
		Instance:      backups.ManifestInstance{ID: "i"},
		Scopes: []backups.ManifestScope{
			{Name: backups.ScopeServerConfig},
			{Name: backups.AppScope("immich")},
		},
	}
	if err := checkRestoreScopes(manifest, []string{"app:immich"}); err != nil {
		t.Fatalf("present scope: %v", err)
	}
	if err := checkRestoreScopes(manifest, []string{"app:paperless-ngx"}); err == nil {
		t.Fatal("expected missing-scope error")
	}
	if err := checkRestoreScopes(manifest, nil); err != nil {
		t.Fatalf("empty selection means all manifest scopes: %v", err)
	}
}

func restorePlanBackend(t *testing.T) (*Backend, string, string) {
	t.Helper()
	root := t.TempDir()
	b := captureTestBackend(t, root)
	ctx := context.Background()
	if _, err := b.apps.Install(ctx, apps.InstallRequest{BundleID: "immich"}); err != nil {
		t.Fatal(err)
	}
	// Staged tree mirroring the canonical layout plus a dump.
	staged := t.TempDir()
	immichRoot := filepath.Join(root, "apps", "immich")
	src := filepath.Join(staged, immichRoot)
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "photo.jpg"), []byte("img"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "immich.pgdump"), []byte("dump"), 0o644); err != nil {
		t.Fatal(err)
	}
	return b, staged, immichRoot
}

func restoreTestManifest() backups.SnapshotManifest {
	return backups.SnapshotManifest{
		FormatVersion: backups.ManifestFormatVersion,
		Instance:      backups.ManifestInstance{ID: "i", Domain: "omahab.com"},
		System:        backups.ManifestSystem{ReleaseRef: systemReleaseRef()},
		Scopes: []backups.ManifestScope{{
			Name:       backups.AppScope("immich"),
			AppVersion: systemReleaseRef(),
			Volumes:    []backups.ManifestDisk{{Volume: "immich:state"}},
			Databases:  []backups.ManifestDatabase{{Name: "immich", File: "immich.pgdump", SHA256: strings.Repeat("a", 64)}},
		}},
	}
}

func TestPlanLiveRestoreMapsStagedFiles(t *testing.T) {
	b, staged, immichRoot := restorePlanBackend(t)
	plan, err := b.planLiveRestore(context.Background(), restoreTestManifest(), []string{"app:immich"}, staged)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	src, ok := plan.stagedFiles[immichRoot]
	if !ok {
		t.Fatalf("staged files = %v, want %q", plan.stagedFiles, immichRoot)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("staged source missing: %v", err)
	}
	if plan.databases["immich"] == "" {
		t.Fatalf("databases = %v, want immich dump", plan.databases)
	}
	if plan.preview == "" || !strings.Contains(plan.preview, "immich") {
		t.Fatalf("preview = %q, want affected apps", plan.preview)
	}
}

func TestPlanLiveRestoreRejectsServerDataScopes(t *testing.T) {
	b, _, _ := restorePlanBackend(t)
	manifest := restoreTestManifest()
	manifest.Scopes[0].Name = backups.ScopeServerProjects
	if _, err := b.planLiveRestore(context.Background(), manifest, []string{"server-projects"}, t.TempDir()); err == nil {
		t.Fatal("server-projects must restore through server-config recovery, not live")
	}
}

func TestPlanLiveRestoreRefusesDowngrade(t *testing.T) {
	b, staged, _ := restorePlanBackend(t)
	manifest := restoreTestManifest()
	manifest.Scopes[0].AppVersion = "v99-from-the-future"
	plan, err := b.planLiveRestore(context.Background(), manifest, []string{"app:immich"}, staged)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := checkRestoreVersionsAgainst(plan, "v1-installed"); err == nil {
		t.Fatal("newer-than-installed formats must refuse auto-downgrade")
	}
	if err := checkRestoreVersionsAgainst(plan, "v99-from-the-future"); err != nil {
		t.Fatalf("matching versions must pass: %v", err)
	}
}

// TestPlanLiveRestoreRefusesNewerPostgresMajor pins the pg-major refusal:
// a snapshot dumped on a newer PostgreSQL major must never restore into
// the running cluster.
func TestPlanLiveRestoreRefusesNewerPostgresMajor(t *testing.T) {
	manifest := restoreTestManifest()
	manifest.Scopes[0].PostgresMajor = 17
	t.Setenv("OMAHAB_PG_MAJOR", "16")
	if err := checkPostgresMajor(manifest); err == nil ||
		!strings.Contains(err.Error(), "PostgreSQL 17") || !strings.Contains(err.Error(), "runs 16") {
		t.Fatalf("err = %v, want pg-major refusal naming both versions", err)
	}
	manifest.Scopes[0].PostgresMajor = 16
	if err := checkPostgresMajor(manifest); err != nil {
		t.Fatalf("same major must pass: %v", err)
	}
}

func TestRestoreJournalRoundtrip(t *testing.T) {
	dir := t.TempDir()
	op := newOperation("restore", []string{"immich"})
	j := restoreJournal{Operation: op, BackupID: "b1", RepositoryID: "r1", SnapshotID: "s1", Scopes: []string{"app:immich"}}
	if err := writeRestoreJournal(dir, j); err != nil {
		t.Fatal(err)
	}
	got, err := loadRestoreJournal(dir, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SnapshotID != "s1" || len(got.Scopes) != 1 {
		t.Fatalf("journal = %+v", got)
	}
	if _, err := loadRestoreJournal(dir, "missing"); err == nil {
		t.Fatal("expected not-found for missing journal")
	}
}
