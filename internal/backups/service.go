package backups

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// Configure creates or updates a backup repository. The repository's
// credentials are referenced by secret id and version; the secret value is
// never accepted here. On creation (and on update when the location
// changed) it initializes the scoped restic repository (restic init) using
// the resolved credentials; init failures are ignored if the repo is
// already initialized. Scoped means the installation folder suffix is
// included: the initialized path is exactly the path backups use, so the
// first backup never fails with "repository does not exist".
func (s *Service) Configure(ctx context.Context, req ConfigureRequest) (Repository, error) {
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
	req.Label = nonEmpty(req.Label, "primary")

	now := s.nowUTC()
	if req.ID != "" {
		existing, err := s.getRepository(ctx, req.ID)
		if err != nil {
			return Repository{}, err
		}
		locationChanged := existing.Location != req.Location
		// Grandfathered repositories keep working when only the label or
		// secret reference changes; a new or changed location must pass
		// current sandbox validation.
		if locationChanged {
			if err := rejectSandboxedRepoPath(req.Location); err != nil {
				return Repository{}, err
			}
		}
		existing.Label = req.Label
		existing.Location = req.Location
		existing.SecretRef = req.SecretRef
		existing.UpdatedAt = now
		if err := s.updateRepository(ctx, existing); err != nil {
			return Repository{}, err
		}
		if locationChanged {
			s.ensureInitialized(ctx, existing)
		}
		return existing, nil
	}
	if err := rejectSandboxedRepoPath(req.Location); err != nil {
		return Repository{}, err
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
	// Initialize the scoped restic repository (idempotent; an existing
	// repo reports "already initialized" which is treated as success).
	// Any other outcome leaves the repository configured but
	// initialization pending: report through the event sink so the
	// observable status never claims a verified backup merely because
	// configuration was saved.
	s.ensureInitialized(ctx, repo)
	return repo, nil
}

// ensureInitialized runs restic init against the scoped repository path
// actually used by backups. Already-initialized repositories are silent
// success; any other outcome emits EventBackupInitPending so the pending
// state stays observable instead of surfacing as a first-backup failure.
// Attached repositories are never initialized: they adopt an existing
// source, and init would silently replace a disappeared source with an
// empty repository hiding its historical snapshots.
func (s *Service) ensureInitialized(ctx context.Context, repo Repository) {
	if s.runner == nil || s.secrets == nil || repo.Attached {
		return
	}
	creds, err := s.secrets.Resolve(ctx, repo.SecretRef)
	if err != nil {
		s.emit(ctx, EventBackupInitPending, severityWarning, repo.ID,
			fmt.Sprintf("backup %q configured but initialization is pending: credential resolution failed", repo.Label),
			map[string]any{"repository_id": repo.ID})
		return
	}
	scoped := s.scopedRepository(ctx, repo)
	if err := s.runner.Init(ctx, scoped, creds); err != nil {
		// If repo already initialized, ignore. Detect by error string.
		if !strings.Contains(strings.ToLower(err.Error()), "already initialized") && !strings.Contains(err.Error(), "already exists") {
			s.emit(ctx, EventBackupInitPending, severityWarning, repo.ID,
				fmt.Sprintf("backup %q configured but initialization is pending: %v", repo.Label, redact(err.Error(), creds.Password, creds.AccessKey)),
				map[string]any{"repository_id": repo.ID})
		}
	}
}

// PreflightRepository resolves the repository and guarantees its scoped
// restic backend is initialized before any caller stops applications. It
// returns the scoped repository and resolved credentials so the caller can
// reuse them without resolving twice. A missing Omahab-created backend is
// initialized idempotently; attached repositories are never initialized
// (a missing attached source fails loudly so a disappeared mount can
// never be masked by a fresh empty repository). Any other probe failure
// is returned as an actionable error and the caller must not proceed to
// quiesce.
func (s *Service) PreflightRepository(ctx context.Context, id string) (Repository, Credentials, error) {
	repo, err := s.resolveRepository(ctx, id)
	if err != nil {
		return Repository{}, Credentials{}, err
	}
	creds, err := s.secrets.Resolve(ctx, repo.SecretRef)
	if err != nil {
		return Repository{}, Credentials{}, fmt.Errorf("backups: backup repository %q credential resolution failed: %w", repo.Label, err)
	}
	scoped := s.scopedRepository(ctx, repo)
	if _, err := s.runner.Snapshots(ctx, scoped, creds, 1); err == nil {
		return scoped, creds, nil
	} else if !isMissingRepository(err) {
		return Repository{}, Credentials{}, fmt.Errorf("backups: backup repository %q at %q unreachable: %w", repo.Label, scoped.Location, err)
	}
	if repo.Attached {
		return Repository{}, Credentials{}, fmt.Errorf("backups: attached backup repository %q at %q is missing (source disappeared or wrong mount); refusing to initialize over it — re-attach the correct source instead of creating an empty repository", repo.Label, scoped.Location)
	}
	if err := s.runner.Init(ctx, scoped, creds); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "already initialized") && !strings.Contains(err.Error(), "already exists") {
			return Repository{}, Credentials{}, fmt.Errorf("backups: backup repository %q at %q is not initialized and automatic init failed: %v; reconfigure the repository or run `restic init` there", repo.Label, scoped.Location, redact(err.Error(), creds.Password, creds.AccessKey))
		}
	}
	if _, err := s.runner.Snapshots(ctx, scoped, creds, 1); err != nil {
		return Repository{}, Credentials{}, fmt.Errorf("backups: backup repository %q at %q unreachable after init: %w", repo.Label, scoped.Location, err)
	}
	return scoped, creds, nil
}

// isMissingRepository reports whether a restic probe error signals an
// uninitialized backend rather than a credential, network, or permission
// failure. Only this signal may trigger automatic initialization.
func isMissingRepository(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, hint := range []string{
		"repository does not exist",
		"unable to open config file",
		"no such file or directory",
		"repository not open",
		"config file cannot be loaded",
	} {
		if strings.Contains(msg, hint) {
			return true
		}
	}
	return false
}

// validateLocation rejects anything that is not a restic backend location:
// an sftp/rclone/rest/s3/b2/azure/gs backend reference or an absolute local
// path. Bare words, relative paths, and unknown schemes are rejected with
// ErrInvalid so misconfiguration surfaces at configure time instead of at
// the first backup run.
func validateLocation(loc string) error {
	if filepath.IsAbs(loc) {
		return nil
	}
	u, err := url.Parse(loc)
	if err != nil || u.Scheme == "" {
		return fmt.Errorf("%w: unsupported location %q (use sftp://, s3:, rest:, rclone:, b2:, azure:, gs:, or an absolute local path)", ErrInvalid, loc)
	}
	switch strings.ToLower(u.Scheme) {
	case "sftp":
		// URL form (sftp://[user@]host[:port]/path) needs a host; restic's
		// native form (sftp:user@host:path) carries the target opaquely.
		if strings.HasPrefix(strings.ToLower(loc), "sftp://") {
			if u.Host == "" {
				return fmt.Errorf("%w: invalid sftp location %q: host is required", ErrInvalid, loc)
			}
			return nil
		}
		if strings.TrimSpace(u.Opaque) == "" {
			return fmt.Errorf("%w: invalid sftp location %q", ErrInvalid, loc)
		}
		return nil
	case "s3", "rest", "rclone", "b2", "azure", "gs":
		if strings.TrimSpace(loc[len(u.Scheme)+1:]) == "" {
			return fmt.Errorf("%w: invalid location %q: repository path is required", ErrInvalid, loc)
		}
		return nil
	default:
		return fmt.Errorf("%w: unsupported location %q (use sftp://, s3:, rest:, rclone:, b2:, azure:, gs:, or an absolute local path)", ErrInvalid, loc)
	}
}

// rejectSandboxedRepoPath rejects absolute local repository paths that the
// backup runner cannot see. omahabd runs with PrivateTmp=yes, so the host
// /tmp and /var/tmp are replaced by empty private namespaces: a repository
// initialized by hand under those directories looks populated on the host
// yet every product restic invocation reports "repository does not exist".
// Remote backends are unaffected. Existing repositories configured before
// this validation keep working; only new or changed locations are checked.
func rejectSandboxedRepoPath(loc string) error {
	if !filepath.IsAbs(loc) {
		return nil
	}
	clean := filepath.Clean(loc)
	for _, hidden := range []string{"/tmp", "/var/tmp"} {
		if clean == hidden || strings.HasPrefix(clean, hidden+"/") {
			return fmt.Errorf("%w: local repository path %q is invisible to the backup service (PrivateTmp hides /tmp and /var/tmp); use a persistent location such as /srv/omahab/backups/repos", ErrInvalid, loc)
		}
	}
	return nil
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
