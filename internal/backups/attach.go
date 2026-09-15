package backups

import (
	"context"
	"fmt"
	"strings"
)

// AttachRepository adopts an existing repository without initializing it.
// It inserts the repository with its exact source URL (no new machine-ID
// suffix; Attached repos keep the exact location at run time) and verifies
// readability with `restic snapshots`, never `init`. An unreadable or empty
// source fails the attach and removes the row so no phantom repository
// remains configured.
func (s *Service) AttachRepository(ctx context.Context, req ConfigureRequest) (Repository, error) {
	req.Location = strings.TrimSpace(req.Location)
	if req.Location == "" {
		return Repository{}, fmt.Errorf("%w: location is required", ErrInvalid)
	}
	if err := validateLocation(req.Location); err != nil {
		return Repository{}, err
	}
	if req.SecretRef.ID == "" {
		return Repository{}, fmt.Errorf("%w: secret_ref.id is required", ErrInvalid)
	}
	if req.SecretRef.Version < 1 {
		return Repository{}, fmt.Errorf("%w: secret_ref.version must be at least 1", ErrInvalid)
	}
	req.Label = nonEmpty(req.Label, "attached")

	now := s.nowUTC()
	repo := Repository{
		ID:        newID(),
		Label:     req.Label,
		Location:  req.Location,
		SecretRef: req.SecretRef,
		Attached:  true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.insertRepository(ctx, repo); err != nil {
		return Repository{}, err
	}
	if s.runner == nil || s.secrets == nil {
		s.emit(ctx, EventBackupInitPending, severityWarning, repo.ID,
			fmt.Sprintf("backup %q attached but verification is pending: runner unavailable", repo.Label),
			map[string]any{"repository_id": repo.ID})
		return repo, nil
	}
	creds, err := s.secrets.Resolve(ctx, repo.SecretRef)
	if err != nil {
		_ = s.deleteRepository(ctx, repo.ID)
		return Repository{}, fmt.Errorf("attach %q: credential resolution failed", repo.Label)
	}
	entries, err := s.runner.Snapshots(ctx, repo, creds, 1)
	if err != nil {
		_ = s.deleteRepository(ctx, repo.ID)
		s.emit(ctx, EventBackupInitPending, severityWarning, repo.ID,
			fmt.Sprintf("backup %q attach failed: source unreadable", repo.Label),
			map[string]any{"repository_id": repo.ID})
		return Repository{}, fmt.Errorf("attach %q: source repository unreadable: %v", repo.Label, err)
	}
	if len(entries) == 0 {
		_ = s.deleteRepository(ctx, repo.ID)
		return Repository{}, fmt.Errorf("%w: attach %q: source repository has no snapshots", ErrInvalid, repo.Label)
	}
	return repo, nil
}

func (s *Service) deleteRepository(ctx context.Context, id string) error {
	_, err := s.st.DB().ExecContext(ctx, "DELETE FROM backup_repositories WHERE id = ?", id)
	return err
}
