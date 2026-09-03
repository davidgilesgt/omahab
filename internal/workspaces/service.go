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
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/providers"
	"github.com/omahab/omahab/internal/scm"
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
// Only omp is supported.
var allowedAgents = map[string]bool{
	"omp": true,
	"":    true, // empty defaults to omp
}

// BranchCreator is the Forgejo branch creation surface needed by Service.
type BranchCreator interface {
	CreateBranch(ctx context.Context, ref scm.RepoRef, newBranch, fromRef string) error
}

// ForgejoTokenIssuer creates per-workspace repository-scoped tokens.
type ForgejoTokenIssuer interface {
	CreateAccessToken(ctx context.Context, username, tokenName string, scopes []string, repos []scm.RepoRef) (string, error)
	DeleteAccessToken(ctx context.Context, username, tokenName string) error
	GetFile(ctx context.Context, ref scm.RepoRef, path, refStr string) ([]byte, error)
	GetRepo(ctx context.Context, ref scm.RepoRef) (*scm.Repo, error)
}

// VirtualKeyIssuer issues scoped LiteLLM virtual keys for workspaces.
type VirtualKeyIssuer interface {
	IssueVirtualKey(ctx context.Context, in providers.IssueVirtualKeyInput) (*providers.VirtualKeyWithToken, error)
	RevokeVirtualKey(ctx context.Context, id domain.ID) error
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
	Send(ctx context.Context, workspaceID string, message string) error
	RunPrint(ctx context.Context, workspaceID string, prompt string) ([]byte, error)
}

// RunnerOpts holds non-secret, non-privileged options for workspace creation.
// It intentionally has no DockerSocket or Secret fields.
type RunnerOpts struct {
	// DevcontainerSource describes which devcontainer configuration to use:
	// either "devcontainer" (from .devcontainer/devcontainer.json) or "default".
	DevcontainerSource string
	// WorkspaceEnv holds --workspace-env values to pass to devpod up.
	WorkspaceEnv map[string]string
	// Source is the devpod source (repo URL + @branch).
	Source string
	// ForgejoHost is the forge host for git credential helper (e.g. git.example.com)
	ForgejoHost string
	// ForgejoToken is the per-workspace repository token for credential helper.
	ForgejoToken string
	// ForgejoOwner/Name for credential URL.
	ForgejoOwner string
	ForgejoName  string
	// Instructions is the task content to materialize as TASK.md.
	Instructions string
	// Name is the devcontainer name suffix (ws-slug-xxxx)
	Name string
	// DevcontainerContent is the raw devcontainer.json when DevcontainerSource=="devcontainer" and fetched.
	DevcontainerContent []byte
}

// NoopRunner is a no-op Runner for testing. It satisfies Runner without
// touching the host. Production code should use NewDevPodRunner.
type NoopRunner struct{}

func (NoopRunner) Up(_ context.Context, _ string, _ domain.ID, _, _ string, _ RunnerOpts) error {
	return nil
}
func (NoopRunner) Stop(_ context.Context, _ string) error                              { return nil }
func (NoopRunner) Delete(_ context.Context, _ string) error                            { return nil }
func (NoopRunner) Attach(_ context.Context, _ string) error                            { return nil }
func (NoopRunner) IsRunning(_ context.Context, _ string) (bool, error)                 { return true, nil }
func (NoopRunner) Send(_ context.Context, _, _ string) error                           { return nil }
func (NoopRunner) RunPrint(_ context.Context, _, _ string) ([]byte, error)             { return []byte("{}"), nil }

// Service owns workspace lifecycle.
type Service struct {
	db     *sql.DB
	runner Runner

	branchCreator BranchCreator
	forgejo       ForgejoTokenIssuer
	providers     VirtualKeyIssuer
	// projectResolver maps project ID to domain.Project (with RepositoryURL)
	projectResolver func(context.Context, domain.ID) (*domain.Project, error)
	// domainResolver returns the control-plane domain (e.g. example.com) for models.<domain>
	domainResolver func(context.Context) (string, error)
}

// New creates a Service. runner may be nil, in which case NoopRunner is used.
func New(db *sql.DB, runner Runner) *Service {
	if runner == nil {
		runner = NoopRunner{}
	}
	return &Service{db: db, runner: runner}
}

// SetBranchCreator injects the Forgejo branch creator.
func (s *Service) SetBranchCreator(bc BranchCreator) { s.branchCreator = bc }

// SetForgejo injects the Forgejo client for tokens and file fetching.
func (s *Service) SetForgejo(f ForgejoTokenIssuer) { s.forgejo = f }

// SetProviders injects the virtual key issuer.
func (s *Service) SetProviders(p VirtualKeyIssuer) { s.providers = p }

// SetProjectResolver injects the project lookup.
func (s *Service) SetProjectResolver(fn func(context.Context, domain.ID) (*domain.Project, error)) {
	s.projectResolver = fn
}

// SetDomainResolver injects the domain resolver for model base URLs.
func (s *Service) SetDomainResolver(fn func(context.Context) (string, error)) {
	s.domainResolver = fn
}

// branchRe validates a git branch name (simplified but rejects path traversal
// and shell metacharacters).
var branchRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/\-]*$`)

// CreateInput holds fields for workspace creation.
type CreateInput struct {
	ProjectID domain.ID
	Title     string
	Instructions string
	Agent     string
	// DevcontainerSource is "devcontainer" or "default". Empty defaults to "default".
	DevcontainerSource string
	// SkipBranchCreate indicates the branch already exists (Step6 PR review).
	SkipBranchCreate bool
	// Branch is used when SkipBranchCreate is true to specify the existing branch.
	// When empty and SkipBranchCreate is false, branch is derived from Title.
	Branch string
}

// Capability represents a short-lived, one-time attach token.
type Capability struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// slugify lowercases Title, replaces non-alnum with '-', collapses, trims, caps at 40.
func slugify(title string) string {
	s := strings.ToLower(strings.TrimSpace(title))
	if s == "" {
		return ""
	}
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else {
			if !prevDash {
				b.WriteRune('-')
				prevDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	// collapse multiple dashes already handled via prevDash
	if len(slug) > 40 {
		slug = slug[:40]
		slug = strings.TrimRight(slug, "-")
	}
	return slug
}

func newShortID() string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// parseRepoRef extracts owner/name from a clone URL like https://git.example.com/owner/repo.git
func parseRepoRef(cloneURL string) (scm.RepoRef, string, error) {
	u, err := url.Parse(strings.TrimSpace(cloneURL))
	if err != nil {
		return scm.RepoRef{}, "", fmt.Errorf("parse clone url: %w", err)
	}
	host := u.Host
	// host may include port
	if strings.Contains(host, ":") {
		h, _, _ := strings.Cut(host, ":")
		host = h
	}
	path := strings.Trim(u.Path, "/")
	// remove .git suffix
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return scm.RepoRef{}, "", fmt.Errorf("invalid clone url path %q", u.Path)
	}
	owner := parts[len(parts)-2]
	name := parts[len(parts)-1]
	if owner == "" || name == "" {
		return scm.RepoRef{}, "", fmt.Errorf("invalid clone url path %q", u.Path)
	}
	return scm.RepoRef{Owner: owner, Name: name}, host, nil
}

// Create validates inputs, persists a workspace row, and delegates to the Runner.
func (s *Service) Create(ctx context.Context, in CreateInput) (*domain.Workspace, error) {
	if strings.TrimSpace(string(in.ProjectID)) == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	title := strings.TrimSpace(in.Title)
	instructions := in.Instructions // preserve as-is, but trim leading/trailing whitespace for empty check?
	// For backward compat, if Title empty but Branch provided (old tests), use Branch as title-derived slug fallback
	if title == "" {
		if strings.TrimSpace(in.Branch) != "" && !in.SkipBranchCreate {
			// Old API using Branch directly: treat Branch as the desired branch and bypass slug generation.
			// Validate branch directly.
			branch := strings.TrimSpace(in.Branch)
			if branch == "." || branch == ".." || strings.Contains(branch, "..") {
				return nil, fmt.Errorf("%w: branch contains path traversal", ErrValidation)
			}
			if !branchRe.MatchString(branch) {
				return nil, fmt.Errorf("%w: invalid branch name", ErrValidation)
			}
			agent := strings.TrimSpace(in.Agent)
			if agent == "" {
				agent = "omp"
			}
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
			// Check existing
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
			// Insert with title empty, instructions
			_, err = s.db.ExecContext(ctx,
				`INSERT INTO workspaces (id, project_id, branch, title, instructions, agent, devcontainer_source, status, last_active_at, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				id, string(in.ProjectID), branch, title, instructions, agent, devcontainerSource, StatusPending,
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
				Title:        title,
				Instructions: instructions,
				Agent:        agent,
				Status:       StatusPending,
				LastActiveAt: now,
				CreatedAt:    now,
			}
			if err := s.runner.Up(ctx, id, in.ProjectID, branch, agent, RunnerOpts{DevcontainerSource: devcontainerSource}); err != nil {
				return ws, fmt.Errorf("runner up: %w", err)
			}
			_, _ = s.db.ExecContext(ctx, `UPDATE workspaces SET status = ?, updated_at = ? WHERE id = ?`,
				StatusRunning, now.Format(time.RFC3339Nano), id)
			ws.Status = StatusRunning
			return ws, nil
		}
		return nil, fmt.Errorf("%w: title is required", ErrValidation)
	}
	// Slugify title
	slug := slugify(title)
	if slug == "" {
		return nil, fmt.Errorf("%w: title must contain alphanumeric characters", ErrValidation)
	}
	agent := strings.TrimSpace(in.Agent)
	if agent == "" {
		agent = "omp"
	}
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

	// Generate branch and name
	short := newShortID()
	branch := "ws/" + slug + "-" + short
	name := strings.ReplaceAll(branch, "/", "-")
	// If SkipBranchCreate and explicit Branch provided, override
	if in.SkipBranchCreate && strings.TrimSpace(in.Branch) != "" {
		branch = strings.TrimSpace(in.Branch)
		name = strings.ReplaceAll(branch, "/", "-")
	}
	if branch == "." || branch == ".." || strings.Contains(branch, "..") {
		return nil, fmt.Errorf("%w: branch contains path traversal", ErrValidation)
	}
	if !branchRe.MatchString(branch) {
		return nil, fmt.Errorf("%w: invalid branch name", ErrValidation)
	}

	// Check existing workspace unique per project+branch
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
		`INSERT INTO workspaces (id, project_id, branch, title, instructions, agent, devcontainer_source, status, last_active_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, string(in.ProjectID), branch, title, instructions, agent, devcontainerSource, StatusPending,
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
		Title:        title,
		Instructions: instructions,
		Agent:        agent,
		Status:       StatusPending,
		LastActiveAt: now,
		CreatedAt:    now,
	}

	// Resolve project and repo info for branch creation and token
	var repoRef scm.RepoRef
	var repoHost string
	var cloneURL string
	var defaultBranch string = "main"
	if s.projectResolver != nil {
		if proj, err := s.projectResolver(ctx, in.ProjectID); err == nil && proj != nil {
			cloneURL = proj.RepositoryURL
			if cloneURL != "" {
				if ref, host, err := parseRepoRef(cloneURL); err == nil {
					repoRef = ref
					repoHost = host
				}
			}
		}
	}
	// Try to get default branch via forgejo GetRepo if available
	if s.forgejo != nil && repoRef.Owner != "" {
		if repo, err := s.forgejo.GetRepo(ctx, repoRef); err == nil && repo != nil && repo.DefaultBranch != "" {
			defaultBranch = repo.DefaultBranch
		}
	}

	// 1. CreateBranch unless skipped
	if !in.SkipBranchCreate && s.branchCreator != nil && repoRef.Owner != "" {
		err = s.branchCreator.CreateBranch(ctx, repoRef, branch, defaultBranch)
		if err != nil {
			// Retry once on already exists with new short id
			if errors.Is(err, scm.ErrConflict) || strings.Contains(strings.ToLower(err.Error()), "already exists") || errors.Is(err, ErrAlreadyExists) {
				short2 := newShortID()
				branch2 := "ws/" + slug + "-" + short2
				// If original branch was custom via SkipBranchCreate? Already handled skip, so only for generated
				name2 := strings.ReplaceAll(branch2, "/", "-")
				// Update DB branch
				_, _ = s.db.ExecContext(ctx, `UPDATE workspaces SET branch = ?, updated_at = ? WHERE id = ?`, branch2, now.Format(time.RFC3339Nano), id)
				branch = branch2
				name = name2
				ws.Branch = branch
				err2 := s.branchCreator.CreateBranch(ctx, repoRef, branch, defaultBranch)
				if err2 != nil {
					return ws, fmt.Errorf("create branch: %w", err2)
				}
			} else {
				return ws, fmt.Errorf("create branch: %w", err)
			}
		}
	}

	// 2. Issue virtual key
	var virtualKeyToken string
	var gatewayKeyID string
	if s.providers != nil {
		kind := "harness"
		ownerID := id
		inVK := providers.IssueVirtualKeyInput{
			Name:      "workspace-" + id,
			Scopes:    []string{"omahab/fast", "omahab/balanced", "omahab/reasoning", "omahab/embedding"},
			OwnerKind: &kind,
			OwnerID:   &ownerID,
		}
		vk, err := s.providers.IssueVirtualKey(ctx, inVK)
		if err == nil && vk != nil && vk.VirtualKey != nil {
			virtualKeyToken = vk.Token
			gatewayKeyID = string(vk.VirtualKey.ID)
			// Store gateway key id
			_, _ = s.db.ExecContext(ctx, `UPDATE workspaces SET gateway_key_id = ?, updated_at = ? WHERE id = ?`, gatewayKeyID, now.Format(time.RFC3339Nano), id)
			gid := domain.ID(gatewayKeyID)
			ws.GatewayKeyID = &gid
		}
	}

	// 3. Create per-workspace Forgejo token
	var forgejoToken string
	if s.forgejo != nil && repoRef.Owner != "" {
		tokenName := "ws-" + id
		scopes := []string{"read:repository", "write:repository"}
		repos := []scm.RepoRef{repoRef}
		tok, err := s.forgejo.CreateAccessToken(ctx, "omahab", tokenName, scopes, repos)
		if err == nil {
			forgejoToken = tok
			_, _ = s.db.ExecContext(ctx, `UPDATE workspaces SET forgejo_token_name = ?, updated_at = ? WHERE id = ?`, tokenName, now.Format(time.RFC3339Nano), id)
		}
	}

	// 4. Resolve domain for model URLs
	domainStr := ""
	if s.domainResolver != nil {
		if d, err := s.domainResolver(ctx); err == nil {
			domainStr = strings.TrimSpace(d)
		}
	}

	// 5. Prepare workspace envs
	envMap := map[string]string{}
	if domainStr != "" {
		envMap["OPENAI_BASE_URL"] = "https://models." + domainStr + "/v1"
		envMap["ANTHROPIC_BASE_URL"] = "https://models." + domainStr
	} else {
		envMap["OPENAI_BASE_URL"] = "https://models.example.com/v1"
		envMap["ANTHROPIC_BASE_URL"] = "https://models.example.com"
	}
	if virtualKeyToken != "" {
		envMap["OPENAI_API_KEY"] = virtualKeyToken
		envMap["ANTHROPIC_API_KEY"] = virtualKeyToken
	}
	envMap["OMAHAB_WORKSPACE_ID"] = id
	envMap["GIT_AUTHOR_NAME"] = "omahab"
	envMap["GIT_AUTHOR_EMAIL"] = "omahab@localhost"
	envMap["GIT_COMMITTER_NAME"] = "omahab"
	envMap["GIT_COMMITTER_EMAIL"] = "omahab@localhost"

	// 6. Determine source URL for devpod
	source := cloneURL
	if source == "" {
		// fallback synthetic
		source = "https://forgejo.local/" + string(in.ProjectID) + ".git"
	}
	// Ensure source includes @branch (devpod convention repo@branch)
	if branch != "" && !strings.Contains(source, "@") {
		source = source + "@" + branch
	}

	// 7. Devcontainer content if needed
	var devContent []byte
	if devcontainerSource == "devcontainer" && s.forgejo != nil && repoRef.Owner != "" {
		// Try to fetch .devcontainer/devcontainer.json at branch
		if data, err := s.forgejo.GetFile(ctx, repoRef, ".devcontainer/devcontainer.json", branch); err == nil && len(data) > 0 {
			devContent = data
		}
	}

	opts := RunnerOpts{
		DevcontainerSource:  devcontainerSource,
		WorkspaceEnv:        envMap,
		Source:              source,
		ForgejoHost:         repoHost,
		ForgejoToken:        forgejoToken,
		ForgejoOwner:        repoRef.Owner,
		ForgejoName:         repoRef.Name,
		Instructions:        instructions,
		Name:                name,
		DevcontainerContent: devContent,
	}

	// Delegate to Runner. If Runner fails, mark workspace accordingly but still return it.
	if err := s.runner.Up(ctx, id, in.ProjectID, branch, agent, opts); err != nil {
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
		`SELECT id, project_id, branch, title, instructions, agent, status, last_active_at, expires_at, created_at, updated_at, gateway_key_id FROM workspaces WHERE id = ?`, id)
	return scanWorkspace(row)
}

// List returns all workspaces ordered by creation time.
func (s *Service) List(ctx context.Context) ([]*domain.Workspace, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, project_id, branch, title, instructions, agent, status, last_active_at, expires_at, created_at, updated_at, gateway_key_id FROM workspaces ORDER BY created_at ASC`)
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
		`SELECT id, project_id, branch, title, instructions, agent, status, last_active_at, expires_at, created_at, updated_at, gateway_key_id FROM workspaces WHERE project_id = ? ORDER BY created_at ASC`,
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
// removes associated capabilities, revokes virtual keys and forgejo tokens. The runner is called best-effort before
// the row is deleted.
func (s *Service) Delete(ctx context.Context, id string) error {
	ws, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	// Revoke gateway key if present
	if ws.GatewayKeyID != nil && s.providers != nil {
		_ = s.providers.RevokeVirtualKey(ctx, *ws.GatewayKeyID)
	}
	// Revoke forgejo token
	if s.forgejo != nil {
		// forgejo_token_name stored; derive if missing
		var tokenName string
		_ = s.db.QueryRowContext(ctx, `SELECT forgejo_token_name FROM workspaces WHERE id = ?`, id).Scan(&tokenName)
		if tokenName == "" {
			tokenName = "ws-" + id
		}
		_ = s.forgejo.DeleteAccessToken(ctx, "omahab", tokenName)
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

// Send sends a message to the workspace tmux session via the Runner.
func (s *Service) Send(ctx context.Context, id string, message string) error {
	ws, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if ws.Status == StatusStopped || ws.Status == StatusExpired {
		return fmt.Errorf("%w: workspace is %s", ErrValidation, ws.Status)
	}
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("%w: message is required", ErrValidation)
	}
	if err := s.runner.Send(ctx, id, message); err != nil {
		return fmt.Errorf("runner send: %w", err)
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
		`SELECT id, gateway_key_id, forgejo_token_name FROM workspaces WHERE status IN (?, ?) AND last_active_at < ?`,
		StatusPending, StatusRunning, cutoff)
	if err != nil {
		return 0, fmt.Errorf("query idle workspaces: %w", err)
	}
	defer rows.Close()
	type idleInfo struct {
		id           string
		gatewayKeyID sql.NullString
		forgejoName  sql.NullString
	}
	var ids []idleInfo
	for rows.Next() {
		var info idleInfo
		if err := rows.Scan(&info.id, &info.gatewayKeyID, &info.forgejoName); err != nil {
			return 0, err
		}
		ids = append(ids, info)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	count := 0
	for _, info := range ids {
		id := info.id
		// Best-effort stop via runner
		_ = s.runner.Stop(ctx, id)
		// Revoke keys
		if info.gatewayKeyID.Valid && s.providers != nil {
			_ = s.providers.RevokeVirtualKey(ctx, domain.ID(info.gatewayKeyID.String))
		}
		if s.forgejo != nil {
			tokenName := info.forgejoName.String
			if tokenName == "" {
				tokenName = "ws-" + id
			}
			_ = s.forgejo.DeleteAccessToken(ctx, "omahab", tokenName)
		}
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
		id, projectID, branch, title, instructions, agent, status string
		lastActiveAt, createdAt, updatedAt                         string
		expiresAt                                                  sql.NullString
		gatewayKeyID                                               sql.NullString
	)
	err := row.Scan(&id, &projectID, &branch, &title, &instructions, &agent, &status, &lastActiveAt, &expiresAt, &createdAt, &updatedAt, &gatewayKeyID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan workspace: %w", err)
	}
	la, _ := time.Parse(time.RFC3339Nano, lastActiveAt)
	ca, _ := time.Parse(time.RFC3339Nano, createdAt)
	ws := &domain.Workspace{
		ID:           domain.ID(id),
		ProjectID:    domain.ID(projectID),
		Branch:       branch,
		Title:        title,
		Instructions: instructions,
		Agent:        agent,
		Status:       status,
		LastActiveAt: la,
		CreatedAt:    ca,
	}
	if expiresAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, expiresAt.String)
		ws.ExpiresAt = &t
	}
	if gatewayKeyID.Valid && gatewayKeyID.String != "" {
		gid := domain.ID(gatewayKeyID.String)
		ws.GatewayKeyID = &gid
	}
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
