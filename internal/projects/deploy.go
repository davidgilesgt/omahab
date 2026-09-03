package projects

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// DeployParams carry theWoodpecker-observed release facts for a project
// deployment. Digest must be a sha256 OCI reference (design §6.2/6.4).
type DeployParams struct {
	ProjectID domain.ID
	Commit    string
	Digest    string // sha256:<hex>
}

// ReleaseParams are the Woodpecker-initiated narrow release request. The
// token is scoped to this project's release action (design §6.4): Woodpecker
// holds no SSH or administrator credential, so every call verifies through
// the ReleaseTokenVerifier.
type ReleaseParams struct {
	Slug   string
	Commit string
	Digest string
	Token  string
}

// RollbackParams select the project to roll back.
type RollbackParams struct {
	ProjectID domain.ID
}

const (
	ReleaseDeploying = "deploying"
	ReleaseSucceeded = "succeeded"
	ReleaseFailed    = "failed"
)

// Deploy is the control-plane entrypoint for a project deployment. It
// validates the commit and digest, persists a release row, invokes ONCE with
// loopback proxy, external TLS, and secrets-file inputs, polls the /up health
// endpoint, and activates only the resulting healthy digest (design §6.2).
// The previous release, if any, is retained with active=0 so rollback can
// reactivate it.
//
// Errors:
//   - ErrNotFound         404 (unknown project)
//   - ErrValidation       400 (bad commit or digest)
//   - ErrReleaseMismatch  409 (digest re-used with a different commit)
//   - ErrDeployInProgress 409 (another deploy holds the per-project lock)
//   - ErrDeployFailed     502-ish (runner error or health never became ready)
func (s *Service) Deploy(ctx context.Context, p DeployParams) (*domain.Release, error) {
	if strings.TrimSpace(string(p.ProjectID)) == "" {
		return nil, invalidf("project_id", "must not be empty")
	}
	proj, err := s.fetchProject(ctx, "id = ?", string(p.ProjectID))
	if err != nil {
		return nil, err
	}
	var out *domain.Release
	var derr error
	lerr := s.withDeployLock(ctx, proj.ID, func() error {
		// Re-read under lock so the caller cannot deploy against a project
		// that vanished between the initial fetch and lock acquisition.
		active, err := s.fetchProject(ctx, "id = ?", string(proj.ID))
		if err != nil {
			return err
		}
		proj = active
		rel, err := s.deployProject(ctx, proj, p.Commit, p.Digest, false)
		out = rel
		derr = err
		switch {
		case err != nil && rel != nil:
			// Surface the failing release alongside the error so callers can
			// record the release ID even on failure.
			return err
		case err != nil:
			return err
		default:
			return nil
		}
	})
	if lerr != nil && errors.Is(lerr, ErrDeployFailed) {
		return out, lerr
	}
	if lerr != nil {
		return nil, lerr
	}
	_ = derr
	return out, derr
}

// Release is the narrow Woodpecker release endpoint. It verifies the
// per-project release token via the ReleaseTokenVerifier, then performs the
// same deployment flow as Deploy. The verifier is required for Release; when
// unconfigured every call is rejected with ErrUnauthorized.
//
// The token error text never appears in returned errors — the verifier's
// reason is deliberately dropped so the token cannot leak through logs or
// JSON responses. Verifier implementations must likewise keep the presented
// token out of their error messages.
func (s *Service) Release(ctx context.Context, p ReleaseParams) (*domain.Release, error) {
	slugNorm := strings.TrimSpace(p.Slug)
	if slugNorm == "" {
		return nil, invalidf("slug", "must not be empty")
	}
	slug, err := validateSlug(slugNorm)
	if err != nil {
		return nil, err
	}
	proj, err := s.fetchProject(ctx, "slug = ?", slug)
	if err != nil {
		return nil, err
	}
	if s.tokens == nil {
		return nil, fmt.Errorf("%w: no release token verifier configured", ErrUnauthorized)
	}
	if strings.TrimSpace(p.Token) == "" {
		return nil, fmt.Errorf("%w: missing release token", ErrUnauthorized)
	}
	if err := s.tokens.VerifyReleaseToken(ctx, proj.ID, p.Token); err != nil {
		return nil, fmt.Errorf("%w: release token rejected", ErrUnauthorized)
	}
	// Delegate to the same flow as Deploy (0001 used the server-side project
	// ID, but the deployment state machine is identical).
	var out *domain.Release
	var derr error
	lerr := s.withDeployLock(ctx, proj.ID, func() error {
		active, err := s.fetchProject(ctx, "id = ?", string(proj.ID))
		if err != nil {
			return err
		}
		proj = active
		rel, err := s.deployProject(ctx, proj, p.Commit, p.Digest, false)
		out = rel
		derr = err
		if err != nil {
			return err
		}
		return nil
	})
	if lerr != nil && errors.Is(lerr, ErrDeployFailed) {
		return out, lerr
	}
	if lerr != nil {
		return nil, lerr
	}
	_ = derr
	return out, derr
}

// Rollback reactivates the most recent previous release whose status is
// succeeded. It rejects concurrent deploys, marks the previously active
// release as inactive, runs health checks against the target digest, and emits
// a rolled-back event. Only healthy digests are ever made active.
func (s *Service) Rollback(ctx context.Context, p RollbackParams) (*domain.Release, error) {
	if strings.TrimSpace(string(p.ProjectID)) == "" {
		return nil, invalidf("project_id", "must not be empty")
	}
	proj, err := s.fetchProject(ctx, "id = ?", string(p.ProjectID))
	if err != nil {
		return nil, err
	}
	var out *domain.Release
	var derr error
	lerr := s.withDeployLock(ctx, proj.ID, func() error {
		active, err := s.fetchProject(ctx, "id = ?", string(proj.ID))
		if err != nil {
			return err
		}
		proj = active
		current, err := s.activeRelease(ctx, proj.ID)
		if err != nil {
			return err
		}
		if current == nil {
			return ErrNoRollbackTarget
		}
		target, err := s.previousSucceededRelease(ctx, proj.ID, current.ID)
		if err != nil {
			return err
		}
		if target == nil {
			return ErrNoRollbackTarget
		}
		rel, err := s.deployProject(ctx, proj, target.Commit, target.Digest, true)
		out = rel
		derr = err
		if err != nil {
			return err
		}
		return nil
	})
	if lerr != nil && errors.Is(lerr, ErrDeployFailed) {
		return out, lerr
	}
	if lerr != nil {
		return nil, lerr
	}
	_ = derr
	return out, derr
}

// Releases lists releases for a project ordered newest first. The active
// release, if any, carries Active=true.
func (s *Service) Releases(ctx context.Context, projectID domain.ID) ([]domain.Release, error) {
	if strings.TrimSpace(string(projectID)) == "" {
		return nil, invalidf("project_id", "must not be empty")
	}
	if _, err := s.fetchProject(ctx, "id = ?", string(projectID)); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, project_id, commit_sha, digest, status, active, created_at, updated_at
FROM releases WHERE project_id = ? ORDER BY created_at DESC, rowid DESC`, string(projectID))
	if err != nil {
		return nil, fmt.Errorf("list releases: %w", err)
	}
	defer rows.Close()
	var out []domain.Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *Service) deployProject(ctx context.Context, proj *Project, rawCommit, rawDigest string, isRollback bool) (*domain.Release, error) {
	commit, err := normalizeAndValidateCommit(rawCommit)
	if err != nil {
		return nil, err
	}
	digest, err := normalizeAndValidateDigest(rawDigest)
	if err != nil {
		return nil, err
	}
	// Host storage the runner mounts at contract StoragePath ("/storage").
	storagePath := s.storageHostPath(proj.Slug)
	if err := os.MkdirAll(storagePath, 0o750); err != nil {
		return nil, fmt.Errorf("create project storage %s: %w", storagePath, err)
	}
	if err := s.writeProjectSecretsFile(ctx, proj); err != nil {
		return nil, fmt.Errorf("write project secrets file: %w", err)
	}
	rel, err := s.beginRelease(ctx, proj.ID, commit, digest)
	in := DeployInput{
		App:         proj.Slug,
		Hostname:    s.routeHostname(proj),
		Image:       proj.Image + "@" + digest,
		Port:        proj.Contract.Port,
		HealthPath:  proj.Contract.HealthPath,
		StoragePath: storagePath,
		ProxyBind:   s.cfg.ProxyBind,
		TLS:         TLSModeExternal,
		SecretsFile: s.secretsFilePath(proj.Slug),
	}
	deployCtx := ctx
	// Deploy with no secrets in logs — the input only carries paths.
	res, derr := s.runner.Deploy(deployCtx, in)
	if derr == nil {
		ok, detail := s.awaitHealth(deployCtx, proj)
		if !ok {
			derr = fmt.Errorf("health check %s did not become healthy: %s", proj.Contract.HealthPath, detail)
		}
	}
	if derr != nil {
		_ = s.failRelease(ctx, rel, proj, commit, digest, derr)
		return rel, fmt.Errorf("%w: %w", ErrDeployFailed, derr)
	}
	if err := s.activateRelease(ctx, proj.ID, rel.ID); err != nil {
		return rel, fmt.Errorf("%w: %w", ErrDeployFailed, err)
	}
	rel.Status = ReleaseSucceeded
	rel.Active = true
	now := umtimeNow()
	rel.UpdatedAt = now
	if rel.CreatedAt.IsZero() {
		rel.CreatedAt = now
	}
	if isRollback {
		s.emit(context.WithoutCancel(ctx), "deployment.rolled_back", severityInfo, proj.ID,
			fmt.Sprintf("project %q rolled back to %s", proj.Slug, digest),
			map[string]any{"slug": proj.Slug, "digest": digest, "commit": commit})
	} else {
		data := map[string]any{"slug": proj.Slug, "digest": digest, "commit": commit}
		if v := strings.TrimSpace(res.Version); v != "" {
			data["version"] = v
		}
		s.emit(context.WithoutCancel(ctx), "deployment.completed", severityInfo, proj.ID,
			fmt.Sprintf("project %q deployed %s", proj.Slug, digest), data)
	}
	return rel, nil
}

func (s *Service) awaitHealth(ctx context.Context, proj *Project) (bool, string) {
	in := HealthInput{
		App:       proj.Slug,
		ProxyBind: s.cfg.ProxyBind,
		Hostname:  s.routeHostname(proj),
		Port:      proj.Contract.Port,
		Path:      proj.Contract.HealthPath,
	}
	deadline := time.Now().Add(s.cfg.HealthTimeout)
	detail := "no probe completed"
	for {
		if err := ctx.Err(); err != nil {
			return false, "context canceled: " + err.Error()
		}
		res, err := s.runner.Health(ctx, in)
		switch {
		case err == nil && res.Healthy:
			return true, res.Detail
		case err != nil:
			detail = "probe error: " + err.Error()
		default:
			if strings.TrimSpace(res.Detail) != "" {
				detail = res.Detail
			} else {
				detail = "reported unhealthy"
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, detail
		}
		wait := s.cfg.HealthInterval
		if wait > remaining {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return false, "context canceled: " + ctx.Err().Error()
		case <-time.After(wait):
		}
	}
}

func (s *Service) beginRelease(ctx context.Context, projectID domain.ID, commit, digest string) (*domain.Release, error) {
	existing, err := s.releaseByProjectDigest(ctx, projectID, digest)
	switch {
	case err == nil:
		if existing.Commit != commit {
			return nil, fmt.Errorf("%w: digest %s was released from commit %s", ErrReleaseMismatch, digest, existing.Commit)
		}
		nowS := formatTime(time.Now().UTC())
		if _, err := s.db.ExecContext(ctx, `UPDATE releases SET status = ?, updated_at = ? WHERE id = ?`, ReleaseDeploying, nowS, string(existing.ID)); err != nil {
			return nil, fmt.Errorf("mark release deploying: %w", err)
		}
		if t, err := parseTime(nowS); err == nil {
			existing.UpdatedAt = t
		}
		existing.Status = ReleaseDeploying
		return existing, nil
	case errors.Is(err, ErrNotFound):
		now := umtimeNow()
		nowS := formatTime(now)
		rel := &domain.Release{
			ID:        newID(),
			ProjectID: projectID,
			Commit:    commit,
			Digest:    digest,
			Status:    ReleaseDeploying,
			Active:    false,
			CreatedAt: now,
			UpdatedAt: now,
		}
		_, ierr := s.db.ExecContext(ctx, `
INSERT INTO releases (id, project_id, commit_sha, digest, status, active, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 0, ?, ?)`,
			string(rel.ID), string(rel.ProjectID), rel.Commit, rel.Digest, rel.Status, nowS, nowS)
		if ierr != nil {
			if isUniqueViolation(ierr) {
				// Concurrent creation of the same digest — retry the update path.
				return s.beginRelease(ctx, projectID, commit, digest)
			}
			return nil, fmt.Errorf("record release: %w", ierr)
		}
		return rel, nil
	default:
		return nil, err
	}
}

func (s *Service) failRelease(ctx context.Context, rel *domain.Release, proj *Project, commit, digest string, cause error) error {
	nowS := formatTime(time.Now().UTC())
	if _, err := s.db.ExecContext(context.WithoutCancel(ctx), `UPDATE releases SET status = ?, updated_at = ? WHERE id = ?`, ReleaseFailed, nowS, string(rel.ID)); err != nil {
		return err
	}
	if t, err := parseTime(nowS); err == nil {
		rel.UpdatedAt = t
	}
	rel.Status = ReleaseFailed
	rel.Active = false
	s.emit(context.WithoutCancel(ctx), "deployment.failed", severityError, proj.ID,
		fmt.Sprintf("project %q deployment failed: %s", proj.Slug, cause.Error()),
		map[string]any{"slug": proj.Slug, "digest": digest, "commit": commit, "reason": cause.Error()})
	return nil
}

func (s *Service) activateRelease(ctx context.Context, projectID, releaseID domain.ID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin activation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	nowS := formatTime(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `UPDATE releases SET active = 0, updated_at = ? WHERE project_id = ? AND active = 1 AND id <> ?`, nowS, string(projectID), string(releaseID)); err != nil {
		return fmt.Errorf("deactivate previous release: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE releases SET status = ?, active = 1, updated_at = ? WHERE id = ?`, ReleaseSucceeded, nowS, string(releaseID)); err != nil {
		return fmt.Errorf("activate release: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET updated_at = ? WHERE id = ?`, nowS, string(projectID)); err != nil {
		return fmt.Errorf("touch project: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit activation: %w", err)
	}
	return nil
}

func (s *Service) activeRelease(ctx context.Context, projectID domain.ID) (*domain.Release, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, project_id, commit_sha, digest, status, active, created_at, updated_at
FROM releases WHERE project_id = ? AND active = 1 LIMIT 1`, string(projectID))
	r, err := scanRelease(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("active release: %w", err)
	}
	return r, nil
}

func (s *Service) previousSucceededRelease(ctx context.Context, projectID, currentID domain.ID) (*domain.Release, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, project_id, commit_sha, digest, status, active, created_at, updated_at
FROM releases
WHERE project_id = ? AND id <> ? AND status = ?
ORDER BY created_at DESC, rowid DESC LIMIT 1`, string(projectID), string(currentID), ReleaseSucceeded)
	r, err := scanRelease(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("previous release: %w", err)
	}
	return r, nil
}

func (s *Service) releaseByProjectDigest(ctx context.Context, projectID domain.ID, digest string) (*domain.Release, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, project_id, commit_sha, digest, status, active, created_at, updated_at
FROM releases WHERE project_id = ? AND digest = ? LIMIT 1`, string(projectID), digest)
	r, err := scanRelease(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("release lookup: %w", err)
	}
	return r, nil
}

func scanRelease(sc interface{ Scan(dest ...any) error }) (*domain.Release, error) {
	var idStr, projectIDStr, commitSHA, digest, status string
	var active int
	var createdAtS, updatedAtS string
	if err := sc.Scan(&idStr, &projectIDStr, &commitSHA, &digest, &status, &active, &createdAtS, &updatedAtS); err != nil {
		return nil, err
	}
	createdAt, err := parseTime(createdAtS)
	if err != nil {
		return nil, fmt.Errorf("parse release created_at: %w", err)
	}
	updatedAt, err := parseTime(updatedAtS)
	if err != nil {
		return nil, fmt.Errorf("parse release updated_at: %w", err)
	}
	return &domain.Release{
		ID:        domain.ID(idStr),
		ProjectID: domain.ID(projectIDStr),
		Commit:    commitSHA,
		Digest:    digest,
		Status:    status,
		Active:    active != 0,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}, nil
}

// per-project lock plus durable SQLite flag so concurrent deploys across
// processes are rejected. deploy_started_ns allows stale locks to self-heal
// after StaleDeployAge without requiring a startup recovery sweep.

func (s *Service) withDeployLock(ctx context.Context, id domain.ID, fn func() error) error {
	if err := s.beginDeployFlag(ctx, id); err != nil {
		return err
	}
	defer func() { s.endDeployFlag(context.WithoutCancel(ctx), id) }()
	return fn()
}

func (s *Service) beginDeployFlag(ctx context.Context, id domain.ID) error {
	nowNS := time.Now().UTC().UnixNano()
	staleNS := time.Now().UTC().Add(-s.cfg.StaleDeployAge).UnixNano()
	res, err := s.db.ExecContext(ctx, `
UPDATE projects SET deploying = 1, deploy_started_ns = ?
WHERE id = ? AND (deploying = 0 OR (deploy_started_ns <> 0 AND deploy_started_ns < ?))`,
		nowNS, string(id), staleNS)
	if err != nil {
		return fmt.Errorf("acquire deploy lock: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		return nil
	}
	// Zero rows: the project may not exist, or a non-stale deploy is in progress.
	if _, err := s.fetchProject(ctx, "id = ?", string(id)); err != nil {
		return err
	}
	return ErrDeployInProgress
}

func (s *Service) endDeployFlag(ctx context.Context, id domain.ID) {
	_, _ = s.db.ExecContext(ctx, `UPDATE projects SET deploying = 0, deploy_started_ns = 0 WHERE id = ?`, string(id))
}

func umtimeNow() time.Time { return time.Now().UTC() }
