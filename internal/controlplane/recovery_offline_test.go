package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/backups"
)

// offlineRestoreTestEnv points the shared maintenance lock at a temp
// path: the offline executor holds it for the whole run, and /run is
// not writable (or shareable) in tests.
func offlineRestoreTestEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("OMAHAB_MAINTENANCE_LOCK", filepath.Join(dir, "maintenance.lock"))
	return dir
}

func TestOfflineFailurePreservesResumeStageAndLiveData(t *testing.T) {
	offlineRestoreTestEnv(t)
	words, seed, err := generateTestPhrase(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{recoveryStageStaged, recoveryStageFilesPromoted, recoveryStageDBsRestored} {
		t.Run(stage, func(t *testing.T) {
			stateDir := t.TempDir()
			staging := filepath.Join(stateDir, "recovery-restore-staging", "fixture")
			if err := os.MkdirAll(staging, 0o700); err != nil {
				t.Fatal(err)
			}
			live := filepath.Join(stateDir, "master.key")
			original := []byte("live-key-must-not-change")
			if err := os.WriteFile(live, original, 0o600); err != nil {
				t.Fatal(err)
			}
			stagedFile := filepath.Join(staging, "keep-for-resume")
			if err := os.WriteFile(stagedFile, []byte("staging"), 0o600); err != nil {
				t.Fatal(err)
			}
			op := newOperation(recoveryRestoreJournalKind, nil)
			op.Stage = stage
			op.Status = OpRunning
			j := recoveryRestoreJournal{Operation: op, StagingDir: staging, Source: recoveryJournalSource{Location: "/srv/omahab/backups/repos/fixture", SnapshotID: "snapshot", PhraseFP: recoverySeedFingerprint(seed)}}
			if err := writeRecoveryRestoreJournal(stateDir, j); err != nil {
				t.Fatal(err)
			}
			result, err := RunOfflineRecoveryRestore(context.Background(), OfflineRecoveryOptions{StateDir: stateDir, OpID: op.ID, Phrase: strings.Join(words, " ")})
			if err == nil || result.Status != OpFailed || result.Stage != stage || !result.CanResume {
				t.Fatalf("failure lost its resume position: result=%+v err=%v", result, err)
			}
			persisted, err := loadRecoveryRestoreJournal(stateDir, op.ID)
			if err != nil || persisted.Operation.Stage != stage || persisted.Operation.Status != OpFailed {
				t.Fatalf("failure journal mismatch: %+v %v", persisted, err)
			}
			setup, err := loadRestoreSetupState(stateDir)
			if err != nil || setup.State != "failed" || setup.Operation != op.ID || setup.Detail == "" {
				t.Fatalf("setup did not expose the failed operation: %+v %v", setup, err)
			}
			got, err := os.ReadFile(live)
			if err != nil || string(got) != string(original) {
				t.Fatalf("failed validation changed live data: %q %v", got, err)
			}
			got, err = os.ReadFile(stagedFile)
			if err != nil || string(got) != "staging" {
				t.Fatalf("failure removed resumable staging: %q %v", got, err)
			}
		})
	}
}

func TestOfflineWrongSnapshotPhraseCannotSwapLiveState(t *testing.T) {
	offlineRestoreTestEnv(t)
	words, seed, err := generateTestPhrase(t)
	if err != nil {
		t.Fatal(err)
	}
	_, otherSeed, err := generateTestPhrase(t)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	staging := filepath.Join(stateDir, "recovery-restore-staging", "fixture")
	manifestDir := filepath.Join(staging, "var/lib/omahab/backup-staging")
	if err := os.MkdirAll(manifestDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := backups.SnapshotManifest{FormatVersion: backups.ManifestFormatVersion, Instance: backups.ManifestInstance{ID: "source"}, RecoveryFingerprint: recoverySeedFingerprint(otherSeed), Scopes: []backups.ManifestScope{{Name: backups.ScopeServerConfig}}}
	if err := backups.WriteManifest(manifestDir, manifest); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(stateDir, "control.db")
	if err := os.WriteFile(live, []byte("original-state"), 0o600); err != nil {
		t.Fatal(err)
	}
	op := newOperation(recoveryRestoreJournalKind, nil)
	op.Stage = recoveryStageStaged
	op.Status = OpCompleted
	j := recoveryRestoreJournal{Operation: op, StagingDir: staging, Source: recoveryJournalSource{Location: "/srv/omahab/backups/repos/source", SnapshotID: "snapshot", PhraseFP: recoverySeedFingerprint(seed)}}
	if err := writeRecoveryRestoreJournal(stateDir, j); err != nil {
		t.Fatal(err)
	}
	result, err := RunOfflineRecoveryRestore(context.Background(), OfflineRecoveryOptions{StateDir: stateDir, OpID: op.ID, Phrase: strings.Join(words, " ")})
	if err == nil || result.Status != OpFailed {
		t.Fatalf("wrong snapshot phrase was accepted: %+v %v", result, err)
	}
	got, err := os.ReadFile(live)
	if err != nil || string(got) != "original-state" {
		t.Fatalf("phrase mismatch changed live state: %q %v", got, err)
	}
}

// writeOfflineJournal persists a recovery restore journal for the
// Find/staging regression tests.
func writeOfflineJournal(t *testing.T, stateDir string, stage, status, snapshot string, canResume bool, fp string) apitypes.Operation {
	t.Helper()
	op := newOperation(recoveryRestoreJournalKind, nil)
	op.Stage = stage
	op.Status = status
	op.CanResume = canResume
	j := recoveryRestoreJournal{
		Operation:  op,
		StagingDir: filepath.Join(stateDir, "recovery-restore-staging", op.ID),
		Source: recoveryJournalSource{
			Location:   "/srv/omahab/backups/repos/source",
			SnapshotID: snapshot,
			PhraseFP:   fp,
		},
	}
	if err := writeRecoveryRestoreJournal(stateDir, j); err != nil {
		t.Fatal(err)
	}
	return op
}

func TestFindOfflineRecoveryOperationSelection(t *testing.T) {
	_, seed, err := generateTestPhrase(t)
	if err != nil {
		t.Fatal(err)
	}
	fp := recoverySeedFingerprint(seed)
	stateDir := t.TempDir()

	// No journals at all: zero operation, no error.
	op, snap, err := FindOfflineRecoveryOperation(stateDir, "/srv/omahab/backups/repos/source", "", fp, nil, false)
	if err != nil || op.ID != "" || snap != "" {
		t.Fatalf("empty state must yield no match: %+v %q %v", op, snap, err)
	}

	// A finished restore is never reused.
	finished := writeOfflineJournal(t, stateDir, recoveryStageCompleted, OpCompleted, "snap-done", false, fp)
	op, _, err = FindOfflineRecoveryOperation(stateDir, "/srv/omahab/backups/repos/source", "", fp, nil, false)
	if err != nil || op.ID != "" {
		t.Fatalf("finished operation %s must not match: %+v %v", finished.ID, op, err)
	}

	// A staged operation (status completed, stage staged) counts as
	// unfinished and is returned with its recorded snapshot.
	staged := writeOfflineJournal(t, stateDir, recoveryStageStaged, OpCompleted, "snap-staged", false, fp)
	op, snap, err = FindOfflineRecoveryOperation(stateDir, "/srv/omahab/backups/repos/source", "", fp, nil, false)
	if err != nil || op.ID != staged.ID || snap != "snap-staged" {
		t.Fatalf("staged operation must match with its snapshot: got %+v %q %v", op, snap, err)
	}

	// The latest unfinished operation wins over an older staged one:
	// the newer journal is written after, so its CreatedAt sorts first.
	newer := writeOfflineJournal(t, stateDir, recoveryStageSwapping, OpFailed, "snap-newer", true, fp)
	op, snap, err = FindOfflineRecoveryOperation(stateDir, "/srv/omahab/backups/repos/source", "", fp, nil, false)
	if err != nil || op.ID != newer.ID || snap != "snap-newer" {
		t.Fatalf("latest unfinished must win: got %+v %q %v", op, snap, err)
	}

	// Mismatched location or fingerprint never match.
	op, _, err = FindOfflineRecoveryOperation(stateDir, "/srv/omahab/backups/repos/other", "", fp, nil, false)
	if err != nil || op.ID != "" {
		t.Fatalf("different repository must not match: %+v %v", op, err)
	}
	op, _, err = FindOfflineRecoveryOperation(stateDir, "/srv/omahab/backups/repos/source", "", "deadbeef", nil, false)
	if err != nil || op.ID != "" {
		t.Fatalf("different phrase fingerprint must not match: %+v %v", op, err)
	}
}

func TestFindOfflineRecoveryOperationSnapshotConflict(t *testing.T) {
	_, seed, err := generateTestPhrase(t)
	if err != nil {
		t.Fatal(err)
	}
	fp := recoverySeedFingerprint(seed)
	stateDir := t.TempDir()
	writeOfflineJournal(t, stateDir, recoveryStageStaged, OpCompleted, "snap-recorded", false, fp)

	// A pin that matches the recorded snapshot matches.
	op, snap, err := FindOfflineRecoveryOperation(stateDir, "/srv/omahab/backups/repos/source", "snap-recorded", fp, nil, false)
	if err != nil || op.ID == "" || snap != "snap-recorded" {
		t.Fatalf("matching pin must match: %+v %q %v", op, snap, err)
	}
	// A pin that differs is a conflict, never a silent no-match.
	op, snap, err = FindOfflineRecoveryOperation(stateDir, "/srv/omahab/backups/repos/source", "snap-other", fp, nil, false)
	if err == nil || op.ID != "" || snap != "" {
		t.Fatalf("mismatched pin must conflict: %+v %q %v", op, snap, err)
	}
	if !strings.Contains(err.Error(), "snap-recorded") || !strings.Contains(err.Error(), "snap-other") {
		t.Fatalf("conflict must name both snapshots: %v", err)
	}
	// An empty recorded snapshot (interrupted during listing) accepts
	// any pin.
	stateDir2 := t.TempDir()
	writeOfflineJournal(t, stateDir2, recoveryStageListing, OpRunning, "", true, fp)
	op, snap, err = FindOfflineRecoveryOperation(stateDir2, "/srv/omahab/backups/repos/source", "snap-any", fp, nil, false)
	if err != nil || op.ID == "" || snap != "" {
		t.Fatalf("unpicked journal must accept any pin: %+v %q %v", op, snap, err)
	}
}

func TestStageOfflineRecoverySnapshotHonorsExplicitOpID(t *testing.T) {
	// Explicit opID must load that exact recorded operation, not create
	// a fresh one: an interrupted restore resumes its original snapshot
	// and selection, and an operation that already reached the swap is
	// returned as-is (never re-extracted).
	_, seed, err := generateTestPhrase(t)
	if err != nil {
		t.Fatal(err)
	}
	fp := recoverySeedFingerprint(seed)
	stateDir := t.TempDir()
	recordedOp := writeOfflineJournal(t, stateDir, recoveryStageStaged, OpCompleted, "snap-original", false, fp)
	recorded, err := loadRecoveryRestoreJournal(stateDir, recordedOp.ID)
	if err != nil {
		t.Fatal(err)
	}
	recorded.Source = recoveryJournalSource{Location: "/srv/omahab/backups/repos/source", SnapshotID: "snap-original", PhraseFP: fp, Scopes: []string{"server-sync"}, IncludeApps: false}
	if err := writeRecoveryRestoreJournal(stateDir, recorded); err != nil {
		t.Fatal(err)
	}

	// Re-staging with the explicit opID and DIFFERENT caller flags keeps
	// the recorded snapshot and selection: the extraction that follows
	// targets the recorded snapshot, not the caller's newer pick (the
	// fake source has no repository, so the run fails at extraction —
	// after the journal already records the pick it is about to
	// extract).
	op, err := StageOfflineRecoverySnapshot(context.Background(), stateDir, stateDir, "", "", "snap-newer", filepath.Join(stateDir, "recovery-restore-staging", recorded.Operation.ID), fp, "/srv/omahab/backups/repos/source", nil, true, backups.RecoverySource{Location: "/srv/omahab/backups/repos/source", Password: "pw"}, recorded.Operation.ID)
	if err == nil {
		t.Fatal("extraction must fail against the passwordless fake source")
	}
	if op.ID != "" && op.ID != recorded.Operation.ID {
		t.Fatalf("explicit staging created a new operation %q, want the recorded %q", op.ID, recorded.Operation.ID)
	}
	got, err := loadRecoveryRestoreJournal(stateDir, recorded.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source.SnapshotID != "snap-original" {
		t.Fatalf("recorded snapshot must win over the caller's pick: %q", got.Source.SnapshotID)
	}

	// An operation past the swap is never re-extracted.
	swappingOp := writeOfflineJournal(t, stateDir, recoveryStageSwapping, OpRunning, "snap-mid", true, fp)
	op, err = StageOfflineRecoverySnapshot(context.Background(), stateDir, stateDir, "", "", "snap-mid", filepath.Join(stateDir, "recovery-restore-staging", swappingOp.ID), fp, "/srv/omahab/backups/repos/source", nil, false, backups.RecoverySource{Location: "/srv/omahab/backups/repos/source", Password: "pw"}, swappingOp.ID)
	if err != nil {
		t.Fatalf("mid-swap staging: %v", err)
	}
	if op.ID != swappingOp.ID {
		t.Fatalf("mid-swap staging must reuse the recorded operation, got %q", op.ID)
	}
	got, err = loadRecoveryRestoreJournal(stateDir, swappingOp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Operation.Stage != recoveryStageSwapping {
		t.Fatalf("mid-swap operation must not be reset to listing: %q", got.Operation.Stage)
	}

	// A completed operation is returned untouched.
	doneOp := writeOfflineJournal(t, stateDir, recoveryStageCompleted, OpCompleted, "snap-done", false, fp)
	op, err = StageOfflineRecoverySnapshot(context.Background(), stateDir, stateDir, "", "", "snap-done", filepath.Join(stateDir, "recovery-restore-staging", doneOp.ID), fp, "/srv/omahab/backups/repos/source", nil, false, backups.RecoverySource{Location: "/srv/omahab/backups/repos/source", Password: "pw"}, doneOp.ID)
	if err != nil || op.ID != doneOp.ID || op.Stage != recoveryStageCompleted || op.Status != OpCompleted {
		t.Fatalf("completed operation must be returned as-is: %+v %v", op, err)
	}
}
