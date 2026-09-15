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
	rep := backups.StagingReport{ManifestFound: true, Manifests: []backups.SnapshotManifest{{
		FormatVersion: 1, InstanceID: "i", Scopes: []string{"server-config", "app:immich"},
	}}}
	if err := checkRestoreScopes(rep, []string{"app:immich"}); err != nil {
		t.Fatalf("present scope: %v", err)
	}
	if err := checkRestoreScopes(rep, []string{"app:paperless-ngx"}); err == nil {
		t.Fatal("expected missing-scope error")
	}
	legacy := backups.StagingReport{Legacy: true}
	if err := checkRestoreScopes(legacy, []string{"app:immich"}); err == nil {
		t.Fatal("legacy snapshots must refuse scope selection, not guess")
	}
	if err := checkRestoreScopes(legacy, nil); err != nil {
		t.Fatalf("empty scopes on legacy: %v", err)
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
		FormatVersion: 1, InstanceID: "i", Domain: "omahab.com", ReleaseRef: systemReleaseRef(),
		Scopes: []string{"app:immich"},
		Applications: []backups.ManifestApp{{
			BundleID: "immich", Version: systemReleaseRef(),
			Volumes:   []backups.ManifestDisk{{Volume: "immich:state"}},
			Databases: []string{"immich"},
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
	manifest.Scopes = []string{"server-projects"}
	if _, err := b.planLiveRestore(context.Background(), manifest, []string{"server-projects"}, t.TempDir()); err == nil {
		t.Fatal("server-projects must restore through server-config recovery, not live")
	}
}

func TestPlanLiveRestoreRefusesDowngrade(t *testing.T) {
	b, staged, _ := restorePlanBackend(t)
	manifest := restoreTestManifest()
	manifest.Applications[0].Version = "v99-from-the-future"
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
