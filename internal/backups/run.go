package backups

import (
	"context"
	"fmt"
	"strings"
)

// RunBackup starts a backup run: it resolves repository credentials, runs
// every registered pre-backup hook, and only then snapshots the configured
// paths with restic. Any hook failure aborts the run before restic starts,
// because copying live database files is not a valid backup.
//
// The returned run is the persisted record; when the run fails, the record
// carries the failed stage and redacted error and the error is non-nil.
func (s *Service) RunBackup(ctx context.Context, req RunRequest) (Run, error) {
	repo, err := s.resolveRepository(ctx, req.RepositoryID)
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

	creds, err := s.secrets.Resolve(ctx, repo.SecretRef)
	if err != nil {
		werr := fmt.Errorf("resolve credentials for secret %s version %d: %w", repo.SecretRef.ID, repo.SecretRef.Version, err)
		s.failRun(ctx, run, StageCredentials, werr)
		return *run, fmt.Errorf("backups: backup run %s failed: %w", run.ID, err)
	}

	if herr := s.executeHooks(ctx, run, HookPreBackup, creds); herr != nil {
		s.failRun(ctx, run, StageHooks, herr, creds.Password, creds.AccessKey)
		return *run, fmt.Errorf("backups: backup run %s failed: %w", run.ID, herr)
	}

	if err := s.advanceRun(ctx, run, StageSnapshot, creds); err != nil {
		return *run, fmt.Errorf("backups: backup run %s failed: %w", run.ID, err)
	}
	snap, err := s.runner.Backup(ctx, s.scopedRepository(ctx, repo), creds, BackupRequest{
		Paths: s.cfg.Paths,
		Host:  s.cfg.Host,
		Tags:  backupTags,
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

// executeHooks runs every hook of the given kind sequentially and records
// each outcome. Hooks after the first failure are recorded as skipped, so
// partial failures persist accurately. It returns the first failure, or
// nil when every hook succeeded.
func (s *Service) executeHooks(ctx context.Context, run *Run, kind HookKind, creds Credentials) error {
	if s.hooks == nil {
		return nil
	}
	hooks, err := s.hooks.Hooks(ctx, kind)
	if err != nil {
		return fmt.Errorf("list %s hooks: %w", kind, err)
	}
	redactions := []string{creds.Password, creds.AccessKey}
	var firstErr error
	for _, h := range hooks {
		if firstErr != nil {
			if serr := s.recordHook(ctx, run.ID, h, kind, HookSkipped, HookOutcome{}, redactions); serr != nil {
				return serr
			}
			continue
		}
		if verr := h.validate(); verr != nil {
			_ = s.recordHook(ctx, run.ID, h, kind, HookFailed, HookOutcome{Error: fmt.Errorf("invalid hook: %w", verr)}, redactions)
			firstErr = fmt.Errorf("%s hook %q is invalid: %w", kind, h.Application, verr)
			continue
		}
		out := s.runHook(ctx, h)
		status := HookOK
		if out.Error != nil || out.ExitCode == nil || *out.ExitCode != 0 {
			status = HookFailed
		}
		if serr := s.recordHook(ctx, run.ID, h, kind, status, out, redactions); serr != nil {
			return serr
		}
		if status == HookFailed {
			firstErr = describeHookFailure(kind, h, out, redactions)
		}
	}
	return firstErr
}

// runHook executes one hook under its configured timeout.
func (s *Service) runHook(ctx context.Context, h Hook) HookOutcome {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = defaultHookTimeout
	}
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return s.hookRunner.RunHook(hctx, h)
}

// recordHook persists one hook outcome with redacted, truncated output.
func (s *Service) recordHook(ctx context.Context, runID string, h Hook, kind HookKind, status HookStatus, out HookOutcome, redactions []string) error {
	row := HookResult{
		ID:          newID(),
		RunID:       runID,
		Application: h.Application,
		Hook:        kind,
		Status:      status,
	}
	if status == HookSkipped {
		row.StartedAt = s.nowUTC()
		return s.insertHookResult(ctx, row)
	}
	start := out.StartedAt.UTC()
	if start.IsZero() {
		start = s.nowUTC()
	}
	finished := out.FinishedAt.UTC()
	if finished.IsZero() || finished.Before(start) {
		finished = start
	}
	row.ExitCode = out.ExitCode
	row.StartedAt = start
	row.FinishedAt = &finished
	row.Output = truncate(redact(out.Output, redactions...), maxHookOutput)
	if out.Error != nil {
		row.Error = truncate(redact(out.Error.Error(), redactions...), maxErrorLength)
	}
	return s.insertHookResult(ctx, row)
}

func describeHookFailure(kind HookKind, h Hook, out HookOutcome, redactions []string) error {
	var msg strings.Builder
	fmt.Fprintf(&msg, "%s hook %q failed", kind, h.Application)
	if out.ExitCode != nil {
		fmt.Fprintf(&msg, ": exit code %d", *out.ExitCode)
	}
	if out.Error != nil {
		fmt.Fprintf(&msg, ": %s", redact(out.Error.Error(), redactions...))
	}
	if out.Output != "" {
		fmt.Fprintf(&msg, ": %s", truncate(redact(out.Output, redactions...), 200))
	}
	return fmt.Errorf("%s", msg.String())
}
