package backups

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Verify demonstrates restorability: it restores a snapshot into a fresh
// single-use directory under VerifyRoot, checks that the restored tree is
// non-empty, records the outcome, and removes the target. Verification
// never restores over live data. A snapshot only counts as verified after
// this succeeds.
func (s *Service) Verify(ctx context.Context, req VerifyRequest) (Run, Verification, error) {
	snap, err := s.selectSnapshot(ctx, req)
	if err != nil {
		return Run{}, Verification{}, err
	}
	repo, err := s.getRepository(ctx, snap.RepositoryID)
	if err != nil {
		return Run{}, Verification{}, err
	}

	run := &Run{
		ID:           newID(),
		Kind:         KindVerify,
		RepositoryID: repo.ID,
		Status:       StatusRunning,
		Trigger:      nonEmpty(req.Trigger, TriggerManual),
		Stage:        StageCredentials,
		StartedAt:    s.nowUTC(),
	}
	if err := s.insertRun(ctx, run); err != nil {
		return Run{}, Verification{}, err
	}

	creds, err := s.secrets.Resolve(ctx, repo.SecretRef)
	if err != nil {
		werr := fmt.Errorf("resolve credentials for secret %s version %d: %w", repo.SecretRef.ID, repo.SecretRef.Version, err)
		s.failRun(ctx, run, StageCredentials, werr)
		return *run, Verification{}, fmt.Errorf("backups: verification run %s failed: %w", run.ID, err)
	}

	if err := s.advanceRun(ctx, run, StageRestore, creds); err != nil {
		return *run, Verification{}, fmt.Errorf("backups: verification run %s failed: %w", run.ID, err)
	}

	// Isolated, single-use verification target. The run id is unique, so
	// the target cannot collide with live data or earlier targets.
	if err := os.MkdirAll(s.cfg.VerifyRoot, 0o700); err != nil {
		s.failRun(ctx, run, StagePrepare, err, creds.Password, creds.AccessKey)
		return *run, Verification{}, fmt.Errorf("backups: verification run %s failed: %w", run.ID, err)
	}
	target := filepath.Join(s.cfg.VerifyRoot, run.ID)
	if err := os.Mkdir(target, 0o700); err != nil {
		s.failRun(ctx, run, StagePrepare, err, creds.Password, creds.AccessKey)
		return *run, Verification{}, fmt.Errorf("backups: verification run %s failed: %w", run.ID, err)
	}

	ver := &Verification{
		ID:           newID(),
		RunID:        run.ID,
		RepositoryID: repo.ID,
		SnapshotID:   snap.ID,
		Status:       VerificationRunning,
		Target:       target,
		StartedAt:    s.nowUTC(),
	}
	if err := s.insertVerification(ctx, ver); err != nil {
		s.failRun(ctx, run, StagePrepare, err, creds.Password, creds.AccessKey)
		return *run, Verification{}, fmt.Errorf("backups: verification run %s failed: %w", run.ID, err)
	}

	// failVer persists a failed verification, cleans up the target, and
	// closes the run.
	failVer := func(stage string, err error) (Run, Verification, error) {
		now := s.nowUTC()
		ver.Status = VerificationFailed
		ver.Error = truncate(redact(err.Error(), creds.Password, creds.AccessKey), maxErrorLength)
		ver.FinishedAt = &now
		if uerr := s.updateVerification(ctx, ver); uerr != nil {
			err = fmt.Errorf("%w (additionally, persisting the failed verification failed: %v)", err, uerr)
		}
		s.cleanupVerification(ctx, ver)
		s.failRun(ctx, run, stage, err, creds.Password, creds.AccessKey)
		s.emit(ctx, EventBackupVerificationFailed, severityError, ver.ID,
			fmt.Sprintf("restore verification of snapshot %s failed: %s", snap.ID, ver.Error),
			map[string]any{
				"verification_id": ver.ID,
				"run_id":          run.ID,
				"snapshot_id":     snap.ID,
				"repository_id":   repo.ID,
				"error":           ver.Error,
			})
		return *run, *ver, fmt.Errorf("backups: verification run %s failed: %w", run.ID, err)
	}

	if err := s.runner.Restore(ctx, s.scopedRepository(ctx, repo), creds, snap.ID, target); err != nil {
		return failVer(StageRestore, err)
	}
	if err := s.advanceRun(ctx, run, StageVerify, creds); err != nil {
		return failVer(StageVerify, err)
	}

	files, bytes, err := walkStats(target)
	if err != nil {
		return failVer(StageVerify, err)
	}
	if files == 0 {
		return failVer(StageVerify, errors.New("verified restore produced zero files"))
	}

	now := s.nowUTC()
	ver.Status = VerificationPassed
	ver.FilesRestored = files
	ver.BytesRestored = bytes
	ver.FinishedAt = &now
	if err := s.updateVerification(ctx, ver); err != nil {
		return failVer(StageVerify, err)
	}
	// The backup only becomes restorable in the eyes of the product now.
	if err := s.markSnapshotVerified(ctx, repo.ID, snap.ID, now); err != nil {
		return failVer(StageVerify, err)
	}

	run.SnapshotID = snap.ID
	// Cleanup of the isolated target is part of the verification contract.
	s.cleanupVerification(ctx, ver)

	run.Status = StatusCompleted
	run.Stage = StageCompleted
	finished := s.nowUTC()
	run.FinishedAt = &finished
	if err := s.updateRun(ctx, run); err != nil {
		return *run, *ver, fmt.Errorf("backups: persist completed verification run %s: %w", run.ID, err)
	}

	s.emit(ctx, EventBackupVerified, severityInfo, run.ID,
		fmt.Sprintf("restore verification of snapshot %s passed", snap.ID),
		map[string]any{
			"run_id":          run.ID,
			"verification_id": ver.ID,
			"snapshot_id":     snap.ID,
			"repository_id":   repo.ID,
			"files_restored":  files,
			"bytes_restored":  bytes,
		})
	return *run, *ver, nil
}

// Restore is the disaster-recovery path: it restores a snapshot into an
// existing explicit target directory, then runs post-restore application
// hooks. A successful restore also demonstrates restorability and marks the
// snapshot verified.
func (s *Service) Restore(ctx context.Context, req RestoreRequest) (Run, error) {
	if req.SnapshotID == "" {
		return Run{}, fmt.Errorf("%w: snapshot_id is required", ErrInvalid)
	}
	target := filepath.Clean(req.TargetDir)
	if !filepath.IsAbs(target) || target == string(filepath.Separator) {
		return Run{}, fmt.Errorf("%w: target_dir must be an absolute directory path other than %q", ErrInvalid, "/")
	}
	snap, err := s.findSnapshot(ctx, req.SnapshotID)
	if err != nil {
		return Run{}, err
	}
	if req.RepositoryID != "" && req.RepositoryID != snap.RepositoryID {
		return Run{}, fmt.Errorf("%w: snapshot %s belongs to repository %s, not %s", ErrInvalid, snap.ID, snap.RepositoryID, req.RepositoryID)
	}
	repo, err := s.getRepository(ctx, snap.RepositoryID)
	if err != nil {
		return Run{}, err
	}
	fi, err := os.Stat(target)
	if err != nil {
		return Run{}, fmt.Errorf("%w: target_dir %s: %v", ErrInvalid, target, err)
	}
	if !fi.IsDir() {
		return Run{}, fmt.Errorf("%w: target_dir %s is not a directory", ErrInvalid, target)
	}

	run := &Run{
		ID:           newID(),
		Kind:         KindRestore,
		RepositoryID: repo.ID,
		Status:       StatusRunning,
		Trigger:      nonEmpty(req.Trigger, TriggerManual),
		Stage:        StageCredentials,
		StartedAt:    s.nowUTC(),
	}
	if err := s.insertRun(ctx, run); err != nil {
		return Run{}, err
	}

	creds, err := s.secrets.Resolve(ctx, repo.SecretRef)
	if err != nil {
		werr := fmt.Errorf("resolve credentials for secret %s version %d: %w", repo.SecretRef.ID, repo.SecretRef.Version, err)
		s.failRun(ctx, run, StageCredentials, werr)
		return *run, fmt.Errorf("backups: restore run %s failed: %w", run.ID, err)
	}

	if err := s.advanceRun(ctx, run, StageRestore, creds); err != nil {
		return *run, fmt.Errorf("backups: restore run %s failed: %w", run.ID, err)
	}
	if err := s.runner.Restore(ctx, s.scopedRepository(ctx, repo), creds, snap.ID, target); err != nil {
		s.failRun(ctx, run, StageRestore, err, creds.Password, creds.AccessKey)
		return *run, fmt.Errorf("backups: restore run %s failed: %w", run.ID, err)
	}

	// Application bundles bring data back to a consistent state.
	if herr := s.executeHooks(ctx, run, HookPostRestore, creds); herr != nil {
		s.failRun(ctx, run, StageHooks, herr, creds.Password, creds.AccessKey)
		return *run, fmt.Errorf("backups: restore run %s failed: %w", run.ID, herr)
	}

	now := s.nowUTC()
	if err := s.markSnapshotVerified(ctx, repo.ID, snap.ID, now); err != nil {
		s.failRun(ctx, run, StageVerify, err, creds.Password, creds.AccessKey)
		return *run, fmt.Errorf("backups: restore run %s failed: %w", run.ID, err)
	}

	run.Status = StatusCompleted
	run.Stage = StageCompleted
	run.SnapshotID = snap.ID
	run.FinishedAt = &now
	if err := s.updateRun(ctx, run); err != nil {
		return *run, fmt.Errorf("backups: persist completed restore run %s: %w", run.ID, err)
	}

	s.emit(ctx, EventBackupRestored, severityInfo, run.ID,
		fmt.Sprintf("snapshot %s restored to %s", snap.ID, target),
		map[string]any{
			"run_id":        run.ID,
			"snapshot_id":   snap.ID,
			"repository_id": repo.ID,
			"target":        target,
		})
	return *run, nil
}

// selectSnapshot resolves the snapshot a verification targets: an explicit
// snapshot id, otherwise the latest snapshot of the selected repository.
func (s *Service) selectSnapshot(ctx context.Context, req VerifyRequest) (Snapshot, error) {
	if req.SnapshotID != "" {
		snap, err := s.findSnapshot(ctx, req.SnapshotID)
		if err != nil {
			return Snapshot{}, err
		}
		if req.RepositoryID != "" && req.RepositoryID != snap.RepositoryID {
			return Snapshot{}, fmt.Errorf("%w: snapshot %s belongs to repository %s, not %s", ErrInvalid, snap.ID, snap.RepositoryID, req.RepositoryID)
		}
		return snap, nil
	}
	repo, err := s.resolveRepository(ctx, req.RepositoryID)
	if err != nil {
		return Snapshot{}, err
	}
	return s.latestSnapshot(ctx, repo.ID)
}

// cleanupVerification removes the isolated verification target and records
// the outcome. It refuses to delete anything outside VerifyRoot.
func (s *Service) cleanupVerification(ctx context.Context, ver *Verification) {
	if ver.Target == "" {
		return
	}
	root := filepath.Clean(s.cfg.VerifyRoot)
	target := filepath.Clean(ver.Target)
	if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
		ver.CleanupError = "verification target is outside the verification root; not removed automatically"
		_ = s.updateVerification(ctx, ver)
		return
	}
	if err := os.RemoveAll(target); err != nil {
		ver.CleanupError = truncate(err.Error(), maxErrorLength)
	} else {
		now := s.nowUTC()
		ver.CleanedAt = &now
	}
	// Best-effort persistence: the verification outcome itself was already
	// recorded, and a failing cleanup bookkeeping write must not mask it.
	_ = s.updateVerification(ctx, ver)
}

// walkStats counts the files and bytes under dir.
func walkStats(dir string) (files, bytes int64, err error) {
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		files++
		bytes += fi.Size()
		return nil
	})
	return files, bytes, err
}
