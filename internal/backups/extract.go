package backups

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)
// ExtractSnapshot is the low-level restic restore: extraction only, into an
// explicit staging directory derived server-side. It never stops, restarts,
// reconfigures or runs SQL against any live app, and never invokes live app
// hooks: staging finishes after extraction and isolated validation.
func (s *Service) ExtractSnapshot(ctx context.Context, repo Repository, snapshotID, stagingRoot string) (string, error) {
	if strings.TrimSpace(snapshotID) == "" {
		return "", fmt.Errorf("%w: snapshot id is required", ErrInvalid)
	}
	root := filepath.Clean(s.cfg.VerifyRoot)
	staging := filepath.Clean(stagingRoot)
	if staging != root && !strings.HasPrefix(staging, root+string(filepath.Separator)) {
		// Staging outside the verify root is allowed only for operation
		// staging directories managed by the control plane.
		if !strings.HasPrefix(staging, filepath.Clean(os.TempDir())+string(filepath.Separator)) &&
			!strings.Contains(staging, "staging") {
			return "", fmt.Errorf("%w: staging directory outside isolated roots", ErrInvalid)
		}
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return "", err
	}
	creds, err := s.secrets.Resolve(ctx, repo.SecretRef)
	if err != nil {
		return "", fmt.Errorf("resolve credentials: %w", err)
	}
	if err := s.runner.Restore(ctx, s.scopedRepository(ctx, repo), creds, snapshotID, staging); err != nil {
		return "", err
	}
	if err := checkStagingEscapes(staging); err != nil {
		return "", err
	}
	return staging, nil
}

// ImportSnapshots discovers snapshot metadata for an existing repository
// without the old control database and populates the snapshot listing. It
// runs `restic snapshots`, never `init`.
func (s *Service) ImportSnapshots(ctx context.Context, repositoryID string) (int, error) {
	repo, err := s.getRepository(ctx, repositoryID)
	if err != nil {
		return 0, err
	}
	creds, err := s.secrets.Resolve(ctx, repo.SecretRef)
	if err != nil {
		return 0, fmt.Errorf("resolve credentials: %w", err)
	}
	entries, err := s.runner.Snapshots(ctx, s.scopedRepository(ctx, repo), creds, 0)
	if err != nil {
		return 0, err
	}
	now := s.nowUTC()
	imported := 0
	for _, e := range entries {
		if strings.TrimSpace(e.ID) == "" {
			continue
		}
		created := now
		if t, err := time.Parse(time.RFC3339, e.Time); err == nil {
			created = t
		} else if t, err := parseTime(e.Time); err == nil {
			created = t
		}
		snap := Snapshot{
			ID:           e.ID,
			RepositoryID: repo.ID,
			CreatedAt:    created,
			Paths:        e.Paths,
		}
		if err := s.upsertSnapshot(ctx, snap); err != nil {
			return imported, err
		}
		imported++
	}
	return imported, nil
}
