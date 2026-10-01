package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/store"
)

// buildExtractedStagedTree builds the real restic extraction layout under
// staged: app data at its captured absolute path and the manifest with any
// extra staging files at var/lib/omahab/backup-staging. Returns the
// manifest with database hashes stamped for dumpFile (optional).
func buildExtractedStagedTree(t *testing.T, b *Backend, staged string, dumpBytes []byte, bundle *backups.ManifestBundle) backups.SnapshotManifest {
	t.Helper()
	immichRoot := filepath.Join(b.cfg.DataDir, "apps", "immich")
	src := filepath.Join(staged, immichRoot)
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "photo.jpg"), []byte("img"), 0o644); err != nil {
		t.Fatal(err)
	}
	stagingDir := filepath.Join(staged, "var", "lib", "omahab", "backup-staging")
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	scope := backups.ManifestScope{
		Name:       backups.AppScope("immich"),
		AppVersion: systemReleaseRef(),
		Volumes:    []backups.ManifestDisk{{Volume: "immich:state"}},
	}
	if bundle != nil {
		scope.Bundle = bundle
		defDir := filepath.Join(stagingDir, "bundles", bundle.ID)
		if err := os.MkdirAll(defDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(defDir, "bundle.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if dumpBytes != nil {
		dump := filepath.Join(stagingDir, "immich.pgdump")
		if err := os.WriteFile(dump, dumpBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		sum, err := backups.SHA256File(dump)
		if err != nil {
			t.Fatal(err)
		}
		scope.Databases = []backups.ManifestDatabase{{Name: "immich", File: "immich.pgdump", SHA256: sum}}
	}
	m := backups.SnapshotManifest{
		FormatVersion: backups.ManifestFormatVersion,
		Instance:      backups.ManifestInstance{ID: "i", Domain: "omahab.com"},
		System:        backups.ManifestSystem{ReleaseRef: systemReleaseRef()},
		Scopes:        []backups.ManifestScope{scope},
	}
	if err := backups.WriteManifest(stagingDir, m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestPlanLiveRestoreResolvesExtractedLayout pins the real capture shape:
// the manifest and dumps live at var/lib/omahab/backup-staging under the
// extraction root, and the plan must resolve them there — not at the
// restore root where the old code looked.
func TestPlanLiveRestoreResolvesExtractedLayout(t *testing.T) {
	b, _, _ := restorePlanBackend(t)
	staged := t.TempDir()
	manifest := buildExtractedStagedTree(t, b, staged, []byte("dump"), nil)
	plan, err := b.planLiveRestore(context.Background(), manifest, []string{"app:immich"}, staged)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if got := plan.databases["immich"]; !strings.HasSuffix(got, filepath.Join("var", "lib", "omahab", "backup-staging", "immich.pgdump")) {
		t.Fatalf("databases = %v, want the dump under the extracted backup-staging dir", plan.databases)
	}
}

// A snapshot with custom definitions requires a configured bundles service.
func TestRestoreCustomBundlesRejectsMissingDefinitionSources(t *testing.T) {
	b, _, _ := restorePlanBackend(t)
	staged := t.TempDir()
	manifest := buildExtractedStagedTree(t, b, staged, nil,
		&backups.ManifestBundle{ID: "custom-app", Source: "custom", DefinitionPath: "bundles/custom-app"})
	plan, err := b.planLiveRestore(context.Background(), manifest, []string{"app:immich"}, staged)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.customBundles) != 1 || plan.customBundles[0].ID != "custom-app" {
		t.Fatalf("customBundles = %v", plan.customBundles)
	}
	// No custom bundles service configured: the restore must fail closed
	// naming the missing definitions, not guess a source path.
	err = b.restoreCustomBundles(context.Background(), plan, staged, "op-x")
	if !errors.Is(err, store.ErrValidation) {
		t.Fatalf("err = %v, want not-configured failure", err)
	}
}

// TestResumableLiveStageClassification pins which journal stages resume
// the live flow against the kept tree instead of re-extracting.
func TestResumableLiveStageClassification(t *testing.T) {
	resumable := []string{RestoreBundlesWritten, RestoreFilesPromoted, RestoreDBsRestored, RestoreValidating}
	for _, s := range resumable {
		if !resumableLiveStage(s) {
			t.Fatalf("stage %q must be resumable", s)
		}
	}
	for _, s := range []string{RestorePreflight, RestoreStaged, RestoreSafetySaved, RestoreWritersStop, "", "completed"} {
		if resumableLiveStage(s) {
			t.Fatalf("stage %q must not resume the live flow", s)
		}
	}
}

// A failed apply dispatch must leave no operation attached to the restore.
func TestRestoreApplyStageFailureDoesNotRecordOperation(t *testing.T) {
	b, _, _ := restorePlanBackend(t)
	staged := t.TempDir()
	manifest := buildExtractedStagedTree(t, b, staged, []byte("dump"),
		&backups.ManifestBundle{ID: "custom-app", Source: "custom", DefinitionPath: "bundles/custom-app"})
	plan, err := b.planLiveRestore(context.Background(), manifest, []string{"app:immich"}, staged)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	op := newOperation("restore", []string{"immich"})
	op.Status = OpRunning
	j := restoreJournal{Operation: op, Scopes: []string{"app:immich"}, StagingDir: staged}
	if err := writeRestoreJournal(b.cfg.StateDir, j); err != nil {
		t.Fatal(err)
	}
	// No custom bundles repo configured: staging the apply must fail
	// before any journal mutation, and the restore journal must still
	// have no apply ID.
	if err := b.waitRestoreBundleApply(context.Background(), plan, op.ID); err == nil {
		t.Fatal("expected staging failure without custom bundles configured")
	}
	got, err := loadRestoreJournal(b.cfg.StateDir, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BundleApplyID != "" {
		t.Fatalf("bundle apply id = %q, want none recorded on failed stage", got.BundleApplyID)
	}
}

// TestCheckRestoredHealthRunsFreshAfterResume pins that the fresh health
// proof after promotion is not skipped for restore-paused bundles: once
// maintenance is cleared, CheckHealth must actually probe.
func TestCheckRestoredHealthFreshAfterMaintenanceCleared(t *testing.T) {
	b, staged, _ := restorePlanBackend(t)
	ctx := context.Background()
	manifest := restoreTestManifestWithDumpSHA(stagedDumpSHA(t, staged))
	plan, err := b.planLiveRestore(ctx, manifest, []string{"app:immich"}, staged)
	if err != nil {
		t.Fatal(err)
	}
	appsList, err := b.appsByBundle(ctx, "immich")
	if err != nil || len(appsList) == 0 {
		t.Fatalf("appsByBundle: %v (%d)", err, len(appsList))
	}
	// Paused for restore: health is refused — the skip must never be
	// interpreted as healthy during activation.
	if _, err := b.apps.PauseForMaintenance(ctx, "immich", apps.MaintenanceRestore, "op-h"); err != nil {
		t.Fatal(err)
	}
	if err := checkRestoredHealth(ctx, b, plan); err == nil {
		t.Fatal("health proof must not pass while the restore pause is still in place")
	}
	// After the restore clears maintenance (the success path), a fresh
	// probe runs and passes for the healthy fixture.
	if _, err := b.db.ExecContext(ctx, `DELETE FROM app_maintenance WHERE bundle_id = ? AND reason = 'restore'`, "immich"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.apps.Start(ctx, appsList[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := checkRestoredHealth(ctx, b, plan); err != nil {
		t.Fatalf("fresh health after promotion must pass: %v", err)
	}
}
