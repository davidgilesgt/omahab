package backups

import (
	"context"
	"fmt"
	"strings"
)

// RunBackup starts a backup run: it resolves repository credentials,
// guarantees the scoped restic backend is initialized, and snapshots the
// explicitly requested paths with restic. Paths and scopes are required:
// scoped capture (Backend.CreateBackupScoped) computes both from the
// selected scopes; there is no implicit default path set.
//
// The returned run is the persisted record; when the run fails, the record
// carries the failed stage and redacted error and the error is non-nil.
// Preflight failures (unknown repository, unreachable backend) return
// before any run row is inserted, so callers can fail fast before stopping
// applications.
func (s *Service) RunBackup(ctx context.Context, req RunRequest) (Run, error) {
	repo, creds, err := s.PreflightRepository(ctx, req.RepositoryID)
	if err != nil {
		return Run{}, err
	}

	run := &Run{
		ID:           newID(),
		Kind:         KindBackup,
		RepositoryID: repo.ID,
		Status:       StatusRunning,
		Trigger:      nonEmpty(req.Trigger, TriggerManual),
		Stage:        StageCredentials,
		StartedAt:    s.nowUTC(),
	}
	if err := s.insertRun(ctx, run); err != nil {
		return Run{}, err
	}

	if err := s.advanceRun(ctx, run, StageSnapshot, creds); err != nil {
		return *run, fmt.Errorf("backups: backup run %s failed: %w", run.ID, err)
	}
	if len(req.Paths) == 0 {
		s.failRun(ctx, run, StageSnapshot, fmt.Errorf("%w: backup run %s: no capture paths", ErrInvalid, run.ID), creds.Password, creds.AccessKey)
		return *run, fmt.Errorf("backups: backup run %s failed: %w", run.ID, ErrInvalid)
	}
	if len(req.Scopes) == 0 {
		s.failRun(ctx, run, StageSnapshot, fmt.Errorf("%w: backup run %s: no capture scopes", ErrInvalid, run.ID), creds.Password, creds.AccessKey)
		return *run, fmt.Errorf("backups: backup run %s failed: %w", run.ID, ErrInvalid)
	}
	snap, err := s.runner.Backup(ctx, repo, creds, BackupRequest{
		Paths:    req.Paths,
		Host:     s.cfg.Host,
		Tags:     snapshotTags(req.Scopes),
		Excludes: req.Excludes,
	})
	if err != nil {
		s.failRun(ctx, run, StageSnapshot, err, creds.Password, creds.AccessKey)
		return *run, fmt.Errorf("backups: backup run %s failed: %w", run.ID, err)
	}

	now := s.nowUTC()
	record := Snapshot{
		ID:           snap.ID,
		RepositoryID: repo.ID,
		RunID:        run.ID,
		CreatedAt:    now,
		Paths:        snap.Paths,
		SizeBytes:    snap.SizeBytes,
		FileCount:    snap.FileCount,
	}
	if err := s.upsertSnapshot(ctx, record); err != nil {
		s.failRun(ctx, run, StageSnapshot, err, creds.Password, creds.AccessKey)
		return *run, fmt.Errorf("backups: backup run %s failed: %w", run.ID, err)
	}

	stats := RunStats{Files: snap.FileCount, Bytes: snap.SizeBytes}
	run.Status = StatusCompleted
	run.Stage = StageCompleted
	run.SnapshotID = snap.ID
	run.Stats = &stats
	run.FinishedAt = &now
	if err := s.updateRun(ctx, run); err != nil {
		return *run, fmt.Errorf("backups: persist completed backup run %s: %w", run.ID, err)
	}

	s.emit(ctx, EventBackupCompleted, severityInfo, run.ID,
		fmt.Sprintf("backup run %s created snapshot %s", run.ID, snap.ID),
		map[string]any{
			"run_id":        run.ID,
			"repository_id": repo.ID,
			"snapshot_id":   snap.ID,
			"files":         stats.Files,
			"bytes":         stats.Bytes,
		})
	return *run, nil
}

// advanceRun persists a stage transition; the run fails if even this
// bookkeeping cannot be recorded.
func (s *Service) advanceRun(ctx context.Context, run *Run, stage string, creds Credentials) error {
	run.Stage = stage
	if err := s.updateRun(ctx, run); err != nil {
		s.failRun(ctx, run, stage, err, creds.Password, creds.AccessKey)
		return err
	}
	return nil
}

// failRun persists a failed terminal state and emits the normalized failure
// event. Secret values in the error are redacted before storage.
func (s *Service) failRun(ctx context.Context, run *Run, stage string, err error, redactions ...string) {
	run.Status = StatusFailed
	run.Stage = stage
	run.Error = truncate(redact(err.Error(), redactions...), maxErrorLength)
	now := s.nowUTC()
	run.FinishedAt = &now
	if uerr := s.updateRun(ctx, run); uerr != nil {
		s.emit(ctx, EventBackupFailed, severityError, run.ID,
			fmt.Sprintf("additionally, persisting failed run %s did not succeed: %s", run.ID, uerr.Error()), nil)
	}
	s.emit(ctx, EventBackupFailed, severityError, run.ID,
		fmt.Sprintf("%s run %s failed at stage %q: %s", run.Kind, run.ID, stage, run.Error),
		map[string]any{
			"run_id":        run.ID,
			"kind":          string(run.Kind),
			"repository_id": run.RepositoryID,
			"stage":         stage,
			"error":         run.Error,
		})
}

// snapshotTags returns the tags stamped on every snapshot: the omahab
// marker plus one scope:<name> tag per captured scope. Retention groups
// by tags, so scope tags are what keeps server-config history
// independent from chatty app scopes. Scopes are required: every
// snapshot carries its scope set.
func snapshotTags(scopes []string) []string {
	tags := append([]string{}, backupTags...)
	seen := map[string]bool{"omahab": true}
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		tags = append(tags, "scope:"+s)
		seen[s] = true
	}
	return tags
}
