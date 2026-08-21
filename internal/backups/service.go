package backups

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Configure creates or updates a backup repository. The repository's
// credentials are referenced by secret id and version; the secret value is
// never accepted here.
func (s *Service) Configure(ctx context.Context, req ConfigureRequest) (Repository, error) {
	req.Location = strings.TrimSpace(req.Location)
	if req.Location == "" {
		return Repository{}, fmt.Errorf("%w: location is required", ErrInvalid)
	}
	if req.SecretRef.ID == "" {
		return Repository{}, fmt.Errorf("%w: secret_ref.id is required", ErrInvalid)
	}
	if req.SecretRef.Version < 1 {
		return Repository{}, fmt.Errorf("%w: secret_ref.version must be at least 1", ErrInvalid)
	}
	req.Label = nonEmpty(req.Label, "primary")

	now := s.nowUTC()
	if req.ID != "" {
		existing, err := s.getRepository(ctx, req.ID)
		if err != nil {
			return Repository{}, err
		}
		existing.Label = req.Label
		existing.Location = req.Location
		existing.SecretRef = req.SecretRef
		existing.UpdatedAt = now
		if err := s.updateRepository(ctx, existing); err != nil {
			return Repository{}, err
		}
		return existing, nil
	}
	repo := Repository{
		ID:        newID(),
		Label:     req.Label,
		Location:  req.Location,
		SecretRef: req.SecretRef,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.insertRepository(ctx, repo); err != nil {
		return Repository{}, err
	}
	return repo, nil
}

// Repositories lists configured backup repositories.
func (s *Service) Repositories(ctx context.Context) ([]Repository, error) {
	return s.listRepositories(ctx)
}

// Repository returns one configured repository.
func (s *Service) Repository(ctx context.Context, id string) (Repository, error) {
	return s.getRepository(ctx, id)
}

// DeleteRepository removes a repository configuration. Repositories with
// recorded runs are kept for audit and must not be deleted.
func (s *Service) DeleteRepository(ctx context.Context, id string) error {
	if _, err := s.getRepository(ctx, id); err != nil {
		return err
	}
	n, err := s.countRunsForRepository(ctx, id)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: repository %q has %d recorded runs; backup history is retained", ErrConflict, id, n)
	}
	_, err = s.st.DB().ExecContext(ctx, "DELETE FROM backup_repositories WHERE id = ?", id)
	return err
}

// resolveRepository selects the destination repository. An empty id is only
// valid while exactly one repository is configured.
func (s *Service) resolveRepository(ctx context.Context, id string) (Repository, error) {
	if id != "" {
		return s.getRepository(ctx, id)
	}
	repos, err := s.listRepositories(ctx)
	if err != nil {
		return Repository{}, err
	}
	switch len(repos) {
	case 0:
		return Repository{}, ErrNoRepository
	case 1:
		return repos[0], nil
	default:
		return Repository{}, fmt.Errorf("%w: repository_id is required when multiple repositories are configured", ErrInvalid)
	}
}

// ListRuns returns runs, newest first.
func (s *Service) ListRuns(ctx context.Context, f ListFilter) ([]Run, error) {
	return s.listRuns(ctx, f)
}

// GetRun returns a run with its hook outcomes, snapshot, and verification
// record.
func (s *Service) GetRun(ctx context.Context, id string) (RunDetail, error) {
	run, err := s.getRun(ctx, id)
	if err != nil {
		return RunDetail{}, err
	}
	d := RunDetail{Run: run}
	if d.Hooks, err = s.listHookResults(ctx, id); err != nil {
		return d, err
	}
	if run.SnapshotID != "" {
		snap, err := s.findSnapshot(ctx, run.SnapshotID)
		switch {
		case err == nil:
			d.Snapshot = &snap
		case !errors.Is(err, ErrNotFound):
			return d, err
		}
	}
	if v, err := s.getVerificationByRun(ctx, id); err == nil {
		d.Verification = &v
	} else if !errors.Is(err, ErrNotFound) {
		return d, err
	}
	return d, nil
}

// ListSnapshots returns snapshots, newest first. An empty repository id
// lists snapshots across repositories.
func (s *Service) ListSnapshots(ctx context.Context, repositoryID string) ([]Snapshot, error) {
	return s.listSnapshots(ctx, repositoryID)
}

// ListVerifications returns restore-verification records, newest first.
func (s *Service) ListVerifications(ctx context.Context, limit int) ([]Verification, error) {
	return s.listVerifications(ctx, limit)
}

// ReconcileInterrupted closes runs and verifications left running by a
// daemon restart, marking them failed and cleaning up stale verification
// targets. Call once at daemon startup, before scheduling new operations.
func (s *Service) ReconcileInterrupted(ctx context.Context) (int, error) {
	runs, err := s.listRuns(ctx, ListFilter{Status: StatusRunning, Limit: maxListingLimit})
	if err != nil {
		return 0, err
	}
	closed := 0
	for i := range runs {
		run := &runs[i]
		stage := run.Stage
		if stage == "" {
			stage = StagePrepare
		}
		interrupted := errors.New("operation interrupted before completion")
		s.failRun(ctx, run, stage, interrupted)
		closed++
	}
	vers, err := s.listVerificationsByStatus(ctx, VerificationRunning)
	if err != nil {
		return closed, err
	}
	for i := range vers {
		ver := &vers[i]
		now := s.nowUTC()
		ver.Status = VerificationFailed
		ver.Error = "operation interrupted before completion"
		ver.FinishedAt = &now
		if err := s.updateVerification(ctx, ver); err != nil {
			return closed, err
		}
		s.cleanupVerification(ctx, ver)
	}
	return closed, nil
}
