package workspaces

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// Sentinel errors.
var (
	ErrNotFound           = errors.New("workspace not found")
	ErrAlreadyExists      = errors.New("workspace already exists")
	ErrValidation         = errors.New("validation error")
	ErrCapabilityInvalid  = errors.New("invalid capability")
	ErrCapabilityExpired  = errors.New("capability expired")
	ErrCapabilityConsumed = errors.New("capability already consumed")
)

// Workspace statuses.
const (
	StatusPending = "pending"
	StatusRunning = "running"
	StatusStopped = "stopped"
	StatusExpired = "expired"
)

// Default idle timeout and capability TTL.
const (
	DefaultIdleTimeout   = 30 * time.Minute
	DefaultCapabilityTTL = 5 * time.Minute
)

// Allowed agents for a workspace.
var allowedAgents = map[string]bool{
	"omp":   true,
	"codex": true,
	"":      true, // default / none
}

// Runner is the DevPod-style backend that actually creates and stops workspace
// containers. External commands must go through this interface so behavior is
// testable. The Runner must never expose the Docker socket or production secrets
// to the workspace container.
type Runner interface {
	Up(ctx context.Context, workspaceID string, projectID domain.ID, branch, agent string, opts RunnerOpts) error
	Stop(ctx context.Context, workspaceID string) error
	Delete(ctx context.Context, workspaceID string) error
	Attach(ctx context.Context, workspaceID string) error
	IsRunning(ctx context.Context, workspaceID string) (bool, error)
}

// RunnerOpts holds non-secret, non-privileged options for workspace creation.
// It intentionally has no DockerSocket or Secret fields.
type RunnerOpts struct {
	// DevcontainerSource describes which devcontainer configuration to use:
	// either "devcontainer" (from .devcontainer/devcontainer.json) or "default".
	DevcontainerSource string
}

// NoopRunner is a no-op Runner for testing. It satisfies Runner without
// touching the host. Production code should use NewDevPodRunner.
type NoopRunner struct{}

func (NoopRunner) Up(_ context.Context, _ string, _ domain.ID, _, _ string, _ RunnerOpts) error {
	return nil
}
func (NoopRunner) Stop(_ context.Context, _ string) error              { return nil }
func (NoopRunner) Delete(_ context.Context, _ string) error            { return nil }
func (NoopRunner) Attach(_ context.Context, _ string) error            { return nil }
func (NoopRunner) IsRunning(_ context.Context, _ string) (bool, error) { return true, nil }

// Service owns workspace lifecycle.
type Service struct {
	db     *sql.DB
	runner Runner
}

// New creates a Service. runner may be nil, in which case NoopRunner is used.
// The integrator wires a real runner via NewDevPodRunner:
//
//	runner := workspaces.NewDevPodRunner(workspaces.DevPodRunnerConfig{
//	    WorkspacesDir: cfg.DataDir + "/workspaces",
//	    RepoResolver: func(ctx context.Context, id domain.ID) (string, error) {
//	        p, _ := projectsSvc.Get(ctx, id)
//	        return p.RepositoryURL, nil
//	    },
//	})
//	svc := workspaces.New(db, runner)
//	svc.StartIdleExpirer(ctx, time.Minute)
func New(db *sql.DB, runner Runner) *Service {
	if runner == nil {
		runner = NoopRunner{}
	}
	return &Service{db: db, runner: runner}
}

// branchRe validates a git branch name (simplified but rejects path traversal
// and shell metacharacters).
var branchRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/\-]*$`)

// CreateInput holds fields for workspace creation.
type CreateInput struct {
	ProjectID domain.ID
	Branch    string
	Agent     string
	// DevcontainerSource is "devcontainer" or "default". Empty defaults to "default".
	DevcontainerSource string
}

// Capability represents a short-lived, one-time attach token.
type Capability struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Create validates inputs, persists a workspace row, and delegates to the Runner.
func (s *Service) Create(ctx context.Context, in CreateInput) (*domain.Workspace, error) {
	if strings.TrimSpace(string(in.ProjectID)) == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	branch := strings.TrimSpace(in.Branch)
	if branch == "" {
		return nil, fmt.Errorf("%w: branch is required", ErrValidation)
	}
	if branch == "." || branch == ".." || strings.Contains(branch, "..") {
		return nil, fmt.Errorf("%w: branch contains path traversal", ErrValidation)
	}
	if !branchRe.MatchString(branch) {
		return nil, fmt.Errorf("%w: invalid branch name", ErrValidation)
	}
	agent := strings.TrimSpace(in.Agent)
	if !allowedAgents[agent] {
		return nil, fmt.Errorf("%w: unsupported agent %q", ErrValidation, agent)
	}
	devcontainerSource := strings.TrimSpace(in.DevcontainerSource)
	if devcontainerSource == "" {
		devcontainerSource = "default"
	}
	if devcontainerSource != "default" && devcontainerSource != "devcontainer" {
		return nil, fmt.Errorf("%w: devcontainer source must be 'default' or 'devcontainer'", ErrValidation)
	}

	// Check for existing workspace for same project+branch that is not stopped/expired
	var existingID string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM workspaces WHERE project_id = ? AND branch = ? AND status IN (?, ?)`,
		string(in.ProjectID), branch, StatusPending, StatusRunning).Scan(&existingID)
	if err == nil {
		return nil, ErrAlreadyExists
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check existing workspace: %w", err)
	}

	id := newID()
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO workspaces (id, project_id, branch, agent, devcontainer_source, status, last_active_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, string(in.ProjectID), branch, agent, devcontainerSource, StatusPending,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrAlreadyExists
		}
		return nil, fmt.Errorf("insert workspace: %w", err)
	}

	ws := &domain.Workspace{
		ID:           domain.ID(id),
		ProjectID:    in.ProjectID,
		Branch:       branch,
		Agent:        agent,
		Status:       StatusPending,
		LastActiveAt: now,
		CreatedAt:    now,
	}

	// Delegate to Runner. If Runner fails, mark workspace accordingly but still return it.
	if err := s.runner.Up(ctx, id, in.ProjectID, branch, agent, RunnerOpts{DevcontainerSource: devcontainerSource}); err != nil {
		// Surface runner error; caller can inspect workspace status
		return ws, fmt.Errorf("runner up: %w", err)
	}

	// Mark running on success
	_, _ = s.db.ExecContext(ctx, `UPDATE workspaces SET status = ?, updated_at = ? WHERE id = ?`,
		StatusRunning, now.Format(time.RFC3339Nano), id)
	ws.Status = StatusRunning

	return ws, nil
}

// Get returns a workspace by ID.
func (s *Service) Get(ctx context.Context, id string) (*domain.Workspace, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, project_id, branch, agent, status, last_active_at, expires_at, created_at, updated_at FROM workspaces WHERE id = ?`, id)
	return scanWorkspace(row)
}

// List returns all workspaces ordered by creation time.
func (s *Service) List(ctx context.Context) ([]*domain.Workspace, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, project_id, branch, agent, status, last_active_at, expires_at, created_at, updated_at FROM workspaces ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer rows.Close()
	var out []*domain.Workspace
	for rows.Next() {
		ws, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

// ListByProject returns workspaces for a given project.
func (s *Service) ListByProject(ctx context.Context, projectID domain.ID) ([]*domain.Workspace, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, project_id, branch, agent, status, last_active_at, expires_at, created_at, updated_at FROM workspaces WHERE project_id = ? ORDER BY created_at ASC`,
		string(projectID))
	if err != nil {
		return nil, fmt.Errorf("list workspaces by project: %w", err)
	}
	defer rows.Close()
	var out []*domain.Workspace
	for rows.Next() {
		ws, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

// Stop stops a workspace via the Runner and marks it stopped.
func (s *Service) Stop(ctx context.Context, id string) error {
	ws, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if ws.Status == StatusStopped || ws.Status == StatusExpired {
		return nil
	}
	if err := s.runner.Stop(ctx, id); err != nil {
		return fmt.Errorf("runner stop: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.ExecContext(ctx, `UPDATE workspaces SET status = ?, updated_at = ? WHERE id = ?`, StatusStopped, now, id)
	return err
}

// Delete deletes a workspace via the Runner and removes its row. It also
// removes associated capabilities. The runner is called best-effort before
// the row is deleted.
func (s *Service) Delete(ctx context.Context, id string) error {
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	// Best-effort runner delete; surface error if runner fails.
	if err := s.runner.Delete(ctx, id); err != nil {
		return fmt.Errorf("runner delete: %w", err)
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM workspace_capabilities WHERE workspace_id = ?`, id)
	res, err := s.db.ExecContext(ctx, `DELETE FROM workspaces WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete workspace: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Attach creates-or-attaches a resumable tmux session for the workspace via
// the Runner. On success it touches last_active_at to extend idle expiry.
func (s *Service) Attach(ctx context.Context, id string) error {
	ws, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if ws.Status == StatusStopped || ws.Status == StatusExpired {
		return fmt.Errorf("%w: workspace is %s", ErrValidation, ws.Status)
	}
	if err := s.runner.Attach(ctx, id); err != nil {
		return fmt.Errorf("runner attach: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = s.db.ExecContext(ctx, `UPDATE workspaces SET last_active_at = ?, updated_at = ? WHERE id = ?`, now, now, id)
	return nil
}

// Touch updates last_active_at to now, extending idle expiry.
func (s *Service) Touch(ctx context.Context, id string) error {
	ws, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if ws.Status == StatusStopped || ws.Status == StatusExpired {
		return fmt.Errorf("%w: workspace is %s", ErrValidation, ws.Status)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.ExecContext(ctx, `UPDATE workspaces SET last_active_at = ?, updated_at = ? WHERE id = ?`, now, now, id)
	return err
}

// ExpireIdle stops workspaces whose last_active_at is older than idleTimeout.
// It returns the number of workspaces expired.
func (s *Service) ExpireIdle(ctx context.Context, idleTimeout time.Duration) (int, error) {
	if idleTimeout <= 0 {
		idleTimeout = DefaultIdleTimeout
	}
	cutoff := time.Now().UTC().Add(-idleTimeout).Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM workspaces WHERE status IN (?, ?) AND last_active_at < ?`,
		StatusPending, StatusRunning, cutoff)
	if err != nil {
		return 0, fmt.Errorf("query idle workspaces: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	count := 0
	for _, id := range ids {
		// Best-effort stop via runner
		_ = s.runner.Stop(ctx, id)
		now := time.Now().UTC().Format(time.RFC3339Nano)
		_, err := s.db.ExecContext(ctx,
			`UPDATE workspaces SET status = ?, updated_at = ? WHERE id = ?`, StatusExpired, now, id)
		if err == nil {
			count++
		}
	}
	return count, nil
}

// StartIdleExpirer launches a background goroutine that calls ExpireIdle
// every `every` interval using DefaultIdleTimeout. It stops when ctx is
// canceled. If every <= 0 it defaults to one minute. This is the hook the
// integrator starts from omahabd:
//
//	svc.StartIdleExpirer(ctx, time.Minute)
func (s *Service) StartIdleExpirer(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = s.ExpireIdle(ctx, DefaultIdleTimeout)
			}
		}
	}()
}

// IssueCapability generates a short-lived, one-time attach token for a workspace.
// The plaintext token is returned once; only its SHA256 hash is stored.
func (s *Service) IssueCapability(ctx context.Context, workspaceID string, ttl time.Duration) (*Capability, error) {
	if _, err := s.Get(ctx, workspaceID); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		ttl = DefaultCapabilityTTL
	}
	// Cap TTL to avoid indefinite tokens (max 1 hour)
	if ttl > time.Hour {
		ttl = time.Hour
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate capability: %w", err)
	}
	token := hex.EncodeToString(raw)
	hash := hashToken(token)
	expiresAt := time.Now().UTC().Add(ttl)
	id := newID()
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workspace_capabilities (id, workspace_id, token_hash, expires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, workspaceID, hash, expiresAt.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, fmt.Errorf("insert capability: %w", err)
	}
	return &Capability{Token: token, ExpiresAt: expiresAt}, nil
}

// ValidateCapability checks a presented token for the given workspace. It enforces
// one-time use (consumed flag) and expiry. On success the capability is marked
// consumed and cannot be reused.
func (s *Service) ValidateCapability(ctx context.Context, workspaceID, token string) error {
	hash := hashToken(strings.TrimSpace(token))
	if hash == "" || strings.TrimSpace(token) == "" {
		return ErrCapabilityInvalid
	}
	// Find matching capability by hash
	var (
		id         string
		expiresAt  string
		consumedAt sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, expires_at, consumed_at FROM workspace_capabilities WHERE workspace_id = ? AND token_hash = ?`,
		workspaceID, hash).Scan(&id, &expiresAt, &consumedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCapabilityInvalid
		}
		return fmt.Errorf("lookup capability: %w", err)
	}
	if consumedAt.Valid {
		return ErrCapabilityConsumed
	}
	exp, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return ErrCapabilityInvalid
	}
	if time.Now().UTC().After(exp) {
		return ErrCapabilityExpired
	}
	// Constant-time comparison of hash (defense in depth, though DB lookup already matched)
	var storedHash string
	_ = s.db.QueryRowContext(ctx, `SELECT token_hash FROM workspace_capabilities WHERE id = ?`, id).Scan(&storedHash)
	if subtle.ConstantTimeCompare([]byte(hash), []byte(storedHash)) != 1 {
		return ErrCapabilityInvalid
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx,
		`UPDATE workspace_capabilities SET consumed_at = ? WHERE id = ? AND consumed_at IS NULL`, now, id)
	if err != nil {
		return fmt.Errorf("consume capability: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCapabilityConsumed
	}
	// Touch workspace activity
	_, _ = s.db.ExecContext(ctx,
		`UPDATE workspaces SET last_active_at = ?, updated_at = ? WHERE id = ?`, now, now, workspaceID)
	return nil
}

func hashToken(token string) string {
	if token == "" {
		return ""
	}
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

type wsScanner interface {
	Scan(dest ...any) error
}

func scanWorkspace(row wsScanner) (*domain.Workspace, error) {
	var (
		id, projectID, branch, agent, status string
		lastActiveAt, createdAt, updatedAt   string
		expiresAt                            sql.NullString
	)
	if err := row.Scan(&id, &projectID, &branch, &agent, &status, &lastActiveAt, &expiresAt, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan workspace: %w", err)
	}
	la, _ := time.Parse(time.RFC3339Nano, lastActiveAt)
	ca, _ := time.Parse(time.RFC3339Nano, createdAt)
	ws := &domain.Workspace{
		ID:           domain.ID(id),
		ProjectID:    domain.ID(projectID),
		Branch:       branch,
		Agent:        agent,
		Status:       status,
		LastActiveAt: la,
		CreatedAt:    ca,
	}
	if expiresAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, expiresAt.String)
		ws.ExpiresAt = &t
	}
	// UpdatedAt is not in domain.Workspace; parsed but not stored externally.
	_ = updatedAt
	return ws, nil
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "unique constraint")
}
