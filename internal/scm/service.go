package scm

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// Sentinel errors. Callers must use errors.Is to distinguish validation,
// not-found, and conflict cases.
var (
	ErrNotFound   = errors.New("scm resource not found")
	ErrValidation = errors.New("validation error")
	ErrConflict   = errors.New("conflict")
)

// EventSink receives normalized control-plane events as domain.Event.
// The interface is package-local and small to keep event publication testable.
type EventSink interface {
	Emit(ctx context.Context, event domain.Event) error
}

// NoopSink discards events.
type NoopSink struct{}

func (NoopSink) Emit(_ context.Context, _ domain.Event) error { return nil }

// Service owns Forgejo, Woodpecker, and GitHub mirror coordination for a project.
//
// Invariants:
//   - One repository equals one project: enforced by UNIQUE(project_id) and
//     UNIQUE(owner,name) in SQLite and checked explicitly for ErrConflict.
//   - Forgejo is canonical: only Forgejo creates the canonical repository;
//     GitHub is a read-only push mirror configured via Forgejo. No method
//     creates a GitHub repository directly.
//   - Woodpecker only for CI: Forgejo Actions is disabled on provision;
//     Woodpecker supplies run/log/rerun/cancel.
//   - No host SSH key or Omahab admin token reaches Woodpecker: the
//     WoodpeckerClient interface has no credential-bearing method; its
//     implementation resolves a narrowly-scoped CI credential internally.
//   - Secrets are references: SQLite stores only the secret reference string
//     (scope/name); raw values go only through SecretStore.
type Service struct {
	db         *sql.DB
	forgejo    ForgejoClient
	woodpecker WoodpeckerClient
	secrets    SecretStore
	sink       EventSink
}

// New creates a Service. Nil clients are replaced with no-ops so tests can
// wire only the narrow client under test.
func New(db *sql.DB, forgejo ForgejoClient, woodpecker WoodpeckerClient, secrets SecretStore, sink EventSink) *Service {
	if forgejo == nil {
		forgejo = NoopForgejo{}
	}
	if woodpecker == nil {
		woodpecker = NoopWoodpecker{}
	}
	if secrets == nil {
		secrets = NoopSecretStore{}
	}
	if sink == nil {
		sink = NoopSink{}
	}
	return &Service{db: db, forgejo: forgejo, woodpecker: woodpecker, secrets: secrets, sink: sink}
}

// Repository is the persisted integration desired/observed state for the
// canonical Forgejo repository.
type Repository struct {
	ID              domain.ID `json:"id"`
	ProjectID       domain.ID `json:"project_id"`
	Owner           string    `json:"owner"`
	Name            string    `json:"name"`
	CloneURL        string    `json:"clone_url"`
	DefaultBranch   string    `json:"default_branch"`
	ForgejoRemoteID int64     `json:"forgejo_remote_id"`
	Private         bool      `json:"private"`
	ActionsDisabled bool      `json:"actions_disabled"`
	DesiredState    string    `json:"desired_state"`
	ObservedState   string    `json:"observed_state"`
	ObservedDetail  string    `json:"observed_detail"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// CIRepoState is the persisted desired/observed state for the Woodpecker repo.
type CIRepoState struct {
	ID               domain.ID `json:"id"`
	RepositoryID     domain.ID `json:"repository_id"`
	WoodpeckerRepoID int64     `json:"woodpecker_repo_id"`
	ForgejoRemoteID  int64     `json:"forgejo_remote_id"`
	PipelinePath     string    `json:"pipeline_path"`
	Enabled          bool      `json:"enabled"`
	Trusted          bool      `json:"trusted"`
	DesiredState     string    `json:"desired_state"`
	ObservedState    string    `json:"observed_state"`
	ObservedDetail   string    `json:"observed_detail"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Mirror is the persisted push-mirror configuration.
type Mirror struct {
	ID                  domain.ID  `json:"id"`
	RepositoryID        domain.ID  `json:"repository_id"`
	RemoteURL           string     `json:"remote_url"`
	RemoteName          string     `json:"remote_name"`
	CredentialSecretRef string     `json:"credential_secret_ref"`
	IntervalSeconds     int        `json:"interval_seconds"`
	LFSEnabled          bool       `json:"lfs_enabled"`
	DesiredState        string     `json:"desired_state"`
	ObservedState       string     `json:"observed_state"`
	ObservedDetail      string     `json:"observed_detail"`
	LastSyncedAt        *time.Time `json:"last_synced_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// RunRecord is a persisted CI run metadata row. Log content itself is not
// persisted; LogRefs returns live references from Woodpecker.
type RunRecord struct {
	ID           domain.ID  `json:"id"`
	RepositoryID domain.ID  `json:"repository_id"`
	Number       int        `json:"number"`
	WoodpeckerID int64      `json:"woodpecker_id"`
	Status       string     `json:"status"`
	Branch       string     `json:"branch"`
	CommitSHA    string     `json:"commit_sha"`
	Event        string     `json:"event"`
	Message      string     `json:"message"`
	Author       string     `json:"author"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// Status is the combined desired/observed view for a project's SCM integration.
type Status struct {
	Repository *Repository  `json:"repository"`
	CI         *CIRepoState `json:"ci,omitempty"`
	Mirror     *Mirror      `json:"mirror,omitempty"`
}

// ProvisionInput holds validated inputs for provisioning.
type ProvisionInput struct {
	ProjectID              domain.ID
	Owner                  string
	RepoName               string
	Description            string
	DefaultBranch          string
	RegistryHost           string
	ReleaseCallbackBaseURL string
	Mirror                 *MirrorConfig
}

// MirrorConfig is the caller-supplied GitHub mirror configuration. Token is
// the raw repository-scoped credential; the service stores it via SecretStore
// and persists only the reference.
type MirrorConfig struct {
	RemoteURL string
	Token     string
	LFS       bool
}

// ProvisionResult is returned from Provision.
type ProvisionResult struct {
	Repository       *Repository  `json:"repository"`
	CI               *CIRepoState `json:"ci"`
	Mirror           *Mirror      `json:"mirror,omitempty"`
	PipelineTemplate string       `json:"pipeline_template"`
	Warnings         []string     `json:"warnings,omitempty"`
}

var repoNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func validateRepoName(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("%w: repo name is required", ErrValidation)
	}
	if strings.Contains(s, "\x00") {
		return "", fmt.Errorf("%w: repo name contains NUL", ErrValidation)
	}
	if len(s) > 100 {
		return "", fmt.Errorf("%w: repo name too long", ErrValidation)
	}
	low := strings.ToLower(s)
	if low != s {
		return "", fmt.Errorf("%w: repo name must be lowercase", ErrValidation)
	}
	if !repoNameRe.MatchString(s) {
		return "", fmt.Errorf("%w: repo name must match %s", ErrValidation, repoNameRe.String())
	}
	if strings.HasSuffix(s, ".git") || strings.HasSuffix(s, ".") || strings.HasSuffix(s, "-") {
		return "", fmt.Errorf("%w: repo name has invalid suffix", ErrValidation)
	}
	return s, nil
}

func validateOwner(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("%w: owner is required", ErrValidation)
	}
	if strings.Contains(s, "\x00") || strings.Contains(s, "/") {
		return "", fmt.Errorf("%w: invalid owner", ErrValidation)
	}
	if len(s) > 39 {
		return "", fmt.Errorf("%w: owner too long", ErrValidation)
	}
	return s, nil
}

func validateDefaultBranch(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "master"
	}
	return s
}

func validateMirrorURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("%w: mirror url is required", ErrValidation)
	}
	if strings.Contains(s, "\x00") {
		return "", fmt.Errorf("%w: mirror url contains NUL", ErrValidation)
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("%w: invalid mirror url", ErrValidation)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("%w: mirror url must be https", ErrValidation)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: mirror url must not contain credentials", ErrValidation)
	}
	host := strings.ToLower(u.Host)
	// Only GitHub mirrors are supported; Forgejo remains canonical.
	if host != "github.com" && !strings.HasSuffix(host, ".github.com") {
		// Allow github.com host with optional port stripped already by Host? Use hostname.
		if strings.ToLower(u.Hostname()) != "github.com" {
			return "", fmt.Errorf("%w: mirror must be a github.com url", ErrValidation)
		}
	}
	// Must look like https://github.com/owner/name.git or without .git
	return strings.TrimRight(s, "/"), nil
}

func mirrorWarnings(lfs bool) []string {
	w := []string{
		"push mirroring force-pushes and overwrites GitHub-side changes",
		"issues, pull requests, releases, and CI state are not mirrored",
		"Forgejo remains the canonical repository; GitHub is read-only",
	}
	if !lfs {
		w = append(w, "Git LFS objects are not mirrored unless LFS is explicitly enabled")
	}
	return w
}

func secretRefForProject(projectID domain.ID) (scope, name string) {
	return "scm:project:" + string(projectID), "github-mirror-token"
}

// Provision creates the private canonical Forgejo repository, disables
// Forgejo Actions, configures the Woodpecker repository, emits a pipeline
// template for the OCI digest release callback, and optionally configures a
// read-only GitHub push mirror.
//
// Invariants enforced:
//   - One repo per project: ErrConflict if project already has a repository
//     whose observed state is not "error" (error allows a single retry after
//     a failed prior attempt).
//   - One project per repo: ErrConflict if (owner,name) is already mapped to
//     another project.
//   - Forgejo-canonical: GitHub is only ever a push mirror.
//   - Woodpecker-only CI: Forgejo Actions is disabled.
//   - No host SSH/admin token reaches Woodpecker: Woodpecker is configured
//     only via EnsureRepo with forge IDs and pipeline path.
//   - Mirror credential is stored as a repository-scoped secret reference;
//     raw values never land in SQLite state, logs, or Woodpecker arguments.
func (s *Service) Provision(ctx context.Context, in ProvisionInput) (*ProvisionResult, error) {
	if strings.TrimSpace(string(in.ProjectID)) == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	owner, err := validateOwner(in.Owner)
	if err != nil {
		return nil, err
	}
	repoName, err := validateRepoName(in.RepoName)
	if err != nil {
		return nil, err
	}
	defaultBranch := validateDefaultBranch(in.DefaultBranch)
	if strings.Contains(defaultBranch, "\x00") {
		return nil, fmt.Errorf("%w: default branch contains NUL", ErrValidation)
	}
	if in.Mirror != nil {
		if _, err := validateMirrorURL(in.Mirror.RemoteURL); err != nil {
			return nil, err
		}
		if strings.TrimSpace(in.Mirror.Token) == "" {
			return nil, fmt.Errorf("%w: mirror token is required", ErrValidation)
		}
		if strings.Contains(in.Mirror.Token, "\x00") {
			return nil, fmt.Errorf("%w: mirror token contains NUL", ErrValidation)
		}
	}

	// Enforce one-repo-one-project and one-project-one-repo before touching
	// upstream systems. Check inside a transaction-like sequence.
	existing, err := s.getRepositoryByProjectID(ctx, in.ProjectID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if existing != nil && existing.ObservedState != "error" {
		return nil, fmt.Errorf("%w: project already has a repository", ErrConflict)
	}
	// If retrying after error, clean up the error row so the unique constraints
	// on (owner,name) vs prior error row don't surprise us? Instead reuse row.
	retryingAfterError := existing != nil && existing.ObservedState == "error"

	// Check (owner,name) is not owned by another project.
	ownerRepo, err := s.getRepositoryByOwnerName(ctx, owner, repoName)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if ownerRepo != nil && ownerRepo.ProjectID != in.ProjectID {
		return nil, fmt.Errorf("%w: repository %s/%s already mapped to another project", ErrConflict, owner, repoName)
	}
	if ownerRepo != nil && ownerRepo.ProjectID == in.ProjectID && !retryingAfterError {
		return nil, fmt.Errorf("%w: repository %s/%s already exists for this project", ErrConflict, owner, repoName)
	}

	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)

	var repoID string
	var repoRow *Repository
	if retryingAfterError {
		repoID = string(existing.ID)
		repoRow = existing
		// Normalize owner/name/default_branch on retry.
		_, err = s.db.ExecContext(ctx,
			`UPDATE scm_repositories SET owner=?, name=?, default_branch=?, desired_state=?, observed_state=?, observed_detail=?, updated_at=? WHERE id=?`,
			owner, repoName, defaultBranch, "provisioned", "pending", "", nowStr, repoID,
		)
		if err != nil {
			return nil, fmt.Errorf("update repository on retry: %w", err)
		}
	} else {
		repoID = newID()
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO scm_repositories (id, project_id, owner, name, clone_url, default_branch, forgejo_remote_id, private, actions_disabled, desired_state, observed_state, observed_detail, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			repoID, string(in.ProjectID), owner, repoName, "", defaultBranch, 0, 1, 0, "provisioned", "pending", "", nowStr, nowStr,
		)
		if err != nil {
			if isUniqueViolation(err) {
				return nil, fmt.Errorf("%w: repository mapping conflict", ErrConflict)
			}
			return nil, fmt.Errorf("insert repository: %w", err)
		}
		repoRow = &Repository{
			ID:            domain.ID(repoID),
			ProjectID:     in.ProjectID,
			Owner:         owner,
			Name:          repoName,
			DefaultBranch: defaultBranch,
			Private:       true,
			DesiredState:  "provisioned",
			ObservedState: "pending",
			CreatedAt:     now,
			UpdatedAt:     now,
		}
	}

	// On retry, remove stale CI/mirror rows so we can recreate them cleanly.
	if retryingAfterError {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM scm_mirrors WHERE repository_id=?`, repoID)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM scm_ci_repos WHERE repository_id=?`, repoID)
	}

	markError := func(detail string) {
		_, _ = s.db.ExecContext(ctx,
			`UPDATE scm_repositories SET observed_state='error', observed_detail=?, updated_at=? WHERE id=?`,
			detail, time.Now().UTC().Format(time.RFC3339Nano), repoID,
		)
	}

	// Drive Forgejo: create private repo.
	forgejoRepo, err := s.forgejo.CreateRepo(ctx, CreateRepoInput{
		Owner:         owner,
		Name:          repoName,
		Description:   in.Description,
		Private:       true,
		DefaultBranch: defaultBranch,
	})
	if err != nil {
		// If Forgejo reports conflict and we are retrying, try to adopt existing repo.
		if errors.Is(err, ErrConflict) || strings.Contains(strings.ToLower(err.Error()), "already exists") {
			if got, gerr := s.forgejo.GetRepo(ctx, RepoRef{Owner: owner, Name: repoName}); gerr == nil {
				forgejoRepo = got
			} else {
				markError(fmt.Sprintf("create repo failed: %v", err))
				return nil, fmt.Errorf("create forgejo repo: %w", err)
			}
		} else {
			markError(fmt.Sprintf("create repo failed: %v", err))
			return nil, fmt.Errorf("create forgejo repo: %w", err)
		}
	}

	cloneURL := forgejoRepo.CloneURL
	remoteID := forgejoRepo.RemoteID
	_, err = s.db.ExecContext(ctx,
		`UPDATE scm_repositories SET clone_url=?, forgejo_remote_id=?, updated_at=? WHERE id=?`,
		cloneURL, remoteID, time.Now().UTC().Format(time.RFC3339Nano), repoID,
	)
	if err != nil {
		return nil, fmt.Errorf("update repository clone url: %w", err)
	}
	repoRow.CloneURL = cloneURL
	repoRow.ForgejoRemoteID = remoteID

	// Disable Forgejo Actions — Woodpecker is the only CI system.
	if err := s.forgejo.SetActionsEnabled(ctx, RepoRef{Owner: owner, Name: repoName}, false); err != nil {
		markError(fmt.Sprintf("disable actions failed: %v", err))
		return nil, fmt.Errorf("disable forgejo actions: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE scm_repositories SET actions_disabled=1, updated_at=? WHERE id=?`,
		time.Now().UTC().Format(time.RFC3339Nano), repoID,
	)
	if err != nil {
		return nil, fmt.Errorf("mark actions disabled: %w", err)
	}
	repoRow.ActionsDisabled = true

	// Configure Woodpecker repo. No credential crosses this call.
	ciRepo, err := s.woodpecker.EnsureRepo(ctx, EnsureCIRepoInput{
		ForgejoRemoteID: remoteID,
		Owner:           owner,
		Name:            repoName,
		Trusted:         false,
		PipelinePath:    ".woodpecker.yaml",
	})
	if err != nil {
		markError(fmt.Sprintf("configure woodpecker failed: %v", err))
		return nil, fmt.Errorf("configure woodpecker repo: %w", err)
	}
	ciID := newID()
	ciNow := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO scm_ci_repos (id, repository_id, woodpecker_repo_id, forgejo_remote_id, pipeline_path, enabled, trusted, desired_state, observed_state, observed_detail, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ciID, repoID, ciRepo.ID, remoteID, ciRepo.PipelinePath, 1, boolToInt(ciRepo.Trusted), "enabled", "ready", "", ciNow, ciNow,
	)
	if err != nil {
		markError(fmt.Sprintf("persist ci repo failed: %v", err))
		return nil, fmt.Errorf("insert ci repo: %w", err)
	}
	ciRow := &CIRepoState{
		ID:               domain.ID(ciID),
		RepositoryID:     domain.ID(repoID),
		WoodpeckerRepoID: ciRepo.ID,
		ForgejoRemoteID:  remoteID,
		PipelinePath:     ciRepo.PipelinePath,
		Enabled:          true,
		Trusted:          ciRepo.Trusted,
		DesiredState:     "enabled",
		ObservedState:    "ready",
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}

	// Optional GitHub push mirror.
	var mirrorRow *Mirror
	var warnings []string
	if in.Mirror != nil {
		mirrorURL, _ := validateMirrorURL(in.Mirror.RemoteURL)
		scope, mname := secretRefForProject(in.ProjectID)
		// Store raw token only in the encrypted secret store.
		if err := s.secrets.Put(ctx, scope, mname, in.Mirror.Token); err != nil {
			markError(fmt.Sprintf("store mirror secret failed: %v", err))
			return nil, fmt.Errorf("store mirror secret: %w", err)
		}
		secretRef := scope + "/" + mname
		intervalSeconds := 0
		mInput := MirrorInput{
			RemoteURL:           mirrorURL,
			RemoteName:          "github",
			CredentialSecretRef: secretRef,
			IntervalSeconds:     intervalSeconds,
			LFSEnabled:          in.Mirror.LFS,
		}
		if err := s.forgejo.PutPushMirror(ctx, RepoRef{Owner: owner, Name: repoName}, mInput); err != nil {
			_ = s.secrets.Delete(ctx, scope, mname)
			markError(fmt.Sprintf("configure push mirror failed: %v", err))
			return nil, fmt.Errorf("configure push mirror: %w", err)
		}
		mID := newID()
		mNow := time.Now().UTC().Format(time.RFC3339Nano)
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO scm_mirrors (id, repository_id, remote_url, remote_name, credential_secret_ref, interval_seconds, lfs_enabled, desired_state, observed_state, observed_detail, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			mID, repoID, mirrorURL, "github", secretRef, intervalSeconds, boolToInt(in.Mirror.LFS), "configured", "ready", "", mNow, mNow,
		)
		if err != nil {
			return nil, fmt.Errorf("insert mirror: %w", err)
		}
		mirrorRow = &Mirror{
			ID:                  domain.ID(mID),
			RepositoryID:        domain.ID(repoID),
			RemoteURL:           mirrorURL,
			RemoteName:          "github",
			CredentialSecretRef: secretRef,
			IntervalSeconds:     intervalSeconds,
			LFSEnabled:          in.Mirror.LFS,
			DesiredState:        "configured",
			ObservedState:       "ready",
			CreatedAt:           time.Now().UTC(),
			UpdatedAt:           time.Now().UTC(),
		}
		warnings = mirrorWarnings(in.Mirror.LFS)
	}

	// Mark repository ready.
	readyAt := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.ExecContext(ctx,
		`UPDATE scm_repositories SET observed_state='ready', observed_detail='', updated_at=? WHERE id=?`,
		readyAt, repoID,
	)
	if err != nil {
		return nil, fmt.Errorf("mark repository ready: %w", err)
	}
	repoRow.ObservedState = "ready"
	repoRow.ObservedDetail = ""
	// Refresh for response.
	if refreshed, gerr := s.getRepositoryByID(ctx, domain.ID(repoID)); gerr == nil {
		repoRow = refreshed
	}

	callbackURL := in.ReleaseCallbackBaseURL
	if callbackURL == "" {
		callbackURL = fmt.Sprintf("https://omahabd.local/v1/projects/%s/releases", string(in.ProjectID))
	} else {
		callbackURL = strings.TrimRight(callbackURL, "/") + fmt.Sprintf("/v1/projects/%s/releases", string(in.ProjectID))
	}
	tmpl := PipelineTemplate(PipelineTemplateInput{
		Owner:              owner,
		Name:               repoName,
		DefaultBranch:      defaultBranch,
		RegistryHost:       in.RegistryHost,
		ReleaseCallbackURL: callbackURL,
	})

	// If a mirror was requested without explicit warnings yet (should always have),
	// ensure warnings are present even if mirror was not configured via this path
	// (e.g. existing mirror).
	if mirrorRow != nil && len(warnings) == 0 {
		warnings = mirrorWarnings(mirrorRow.LFSEnabled)
	}

	_ = s.sink.Emit(ctx, domain.Event{
		ID:         domain.ID(newID()),
		Type:       "scm.repository.provisioned",
		Severity:   "info",
		ResourceID: in.ProjectID,
		Message:    fmt.Sprintf("provisioned %s/%s", owner, repoName),
		Data: map[string]any{
			"owner":      owner,
			"repo":       repoName,
			"project_id": string(in.ProjectID),
		},
		CreatedAt: time.Now().UTC(),
	})

	return &ProvisionResult{
		Repository:       repoRow,
		CI:               ciRow,
		Mirror:           mirrorRow,
		PipelineTemplate: tmpl,
		Warnings:         warnings,
	}, nil
}

// Status returns the combined desired/observed SCM state for a project.
func (s *Service) Status(ctx context.Context, projectID domain.ID) (*Status, error) {
	if strings.TrimSpace(string(projectID)) == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	repo, err := s.getRepositoryByProjectID(ctx, projectID)
	if err != nil {
		return nil, err
	}
	ci, err := s.getCIRepoByRepositoryID(ctx, repo.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	mirror, err := s.getMirrorByRepositoryID(ctx, repo.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return &Status{Repository: repo, CI: ci, Mirror: mirror}, nil
}

// ListRuns returns persisted CI run metadata for a project's repository,
// ordered by run number descending. Use SyncRuns to refresh from Woodpecker.
func (s *Service) ListRuns(ctx context.Context, projectID domain.ID, limit int) ([]*RunRecord, error) {
	if strings.TrimSpace(string(projectID)) == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	repo, err := s.getRepositoryByProjectID(ctx, projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, repository_id, run_number, woodpecker_run_id, status, branch, commit_sha, event, message, author, started_at, finished_at, created_at, updated_at
		 FROM scm_ci_runs WHERE repository_id=? ORDER BY run_number DESC`+limitClause(limit),
		string(repo.ID),
	)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	var out []*RunRecord
	for rows.Next() {
		r, err := scanRunRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SyncRuns fetches the latest runs from Woodpecker and upserts them into
// scm_ci_runs, then returns the persisted records. This keeps CI runs
// metadata persisted as required without unbounded log persistence.
func (s *Service) SyncRuns(ctx context.Context, projectID domain.ID) ([]*RunRecord, error) {
	if strings.TrimSpace(string(projectID)) == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	repo, err := s.getRepositoryByProjectID(ctx, projectID)
	if err != nil {
		return nil, err
	}
	ci, err := s.getCIRepoByRepositoryID(ctx, repo.ID)
	if err != nil {
		return nil, err
	}
	if ci.WoodpeckerRepoID == 0 {
		return nil, fmt.Errorf("%w: woodpecker repo not configured", ErrNotFound)
	}
	runs, err := s.woodpecker.ListRuns(ctx, ci.WoodpeckerRepoID, 50)
	if err != nil {
		return nil, fmt.Errorf("list woodpecker runs: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, r := range runs {
		id := newID()
		// Upsert by (repository_id, run_number) with prevStatus tracking for ci.failed emission.
		var existingID, prevStatus string
		err = s.db.QueryRowContext(ctx,
			`SELECT id, status FROM scm_ci_runs WHERE repository_id=? AND run_number=?`,
			string(repo.ID), r.Number,
		).Scan(&existingID, &prevStatus)
		if err == nil {
			_, err = s.db.ExecContext(ctx,
				`UPDATE scm_ci_runs SET woodpecker_run_id=?, status=?, branch=?, commit_sha=?, event=?, message=?, author=?, started_at=?, finished_at=?, updated_at=? WHERE id=?`,
				r.WoodpeckerID, r.Status, r.Branch, r.CommitSHA, r.Event, r.Message, r.Author, nullStr(r.StartedAt), nullStr(r.FinishedAt), now, existingID,
			)
		} else if errors.Is(err, sql.ErrNoRows) {
			prevStatus = ""
			_, err = s.db.ExecContext(ctx,
				`INSERT INTO scm_ci_runs (id, repository_id, run_number, woodpecker_run_id, status, branch, commit_sha, event, message, author, started_at, finished_at, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				id, string(repo.ID), r.Number, r.WoodpeckerID, r.Status, r.Branch, r.CommitSHA, r.Event, r.Message, r.Author, nullStr(r.StartedAt), nullStr(r.FinishedAt), now, now,
			)
		}
		if err != nil {
			return nil, fmt.Errorf("upsert run %d: %w", r.Number, err)
		}
		_ = CheckAndEmitCIFailed(ctx, s.sink, projectID, repo.Owner, repo.Name, repo.ID, prevStatus, r)
	}
	return s.ListRuns(ctx, projectID, 0)
}

// GetRun returns a single persisted run by number.
func (s *Service) GetRun(ctx context.Context, projectID domain.ID, number int) (*RunRecord, error) {
	if strings.TrimSpace(string(projectID)) == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	repo, err := s.getRepositoryByProjectID(ctx, projectID)
	if err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT id, repository_id, run_number, woodpecker_run_id, status, branch, commit_sha, event, message, author, started_at, finished_at, created_at, updated_at
		 FROM scm_ci_runs WHERE repository_id=? AND run_number=?`,
		string(repo.ID), number,
	)
	return scanRunRecord(row)
}

// LogRefs returns live log references for a run from Woodpecker. Logs themselves
// are not persisted; only these references are returned.
func (s *Service) LogRefs(ctx context.Context, projectID domain.ID, runNumber int) ([]*LogRef, error) {
	if strings.TrimSpace(string(projectID)) == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if runNumber <= 0 {
		return nil, fmt.Errorf("%w: run number must be positive", ErrValidation)
	}
	repo, err := s.getRepositoryByProjectID(ctx, projectID)
	if err != nil {
		return nil, err
	}
	ci, err := s.getCIRepoByRepositoryID(ctx, repo.ID)
	if err != nil {
		return nil, err
	}
	if ci.WoodpeckerRepoID == 0 {
		return nil, fmt.Errorf("%w: woodpecker repo not configured", ErrNotFound)
	}
	refs, err := s.woodpecker.LogRefs(ctx, ci.WoodpeckerRepoID, runNumber)
	if err != nil {
		return nil, err
	}
	return refs, nil
}

// Rerun triggers a rerun of a CI run via Woodpecker.
func (s *Service) Rerun(ctx context.Context, projectID domain.ID, runNumber int) error {
	if strings.TrimSpace(string(projectID)) == "" {
		return fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if runNumber <= 0 {
		return fmt.Errorf("%w: run number must be positive", ErrValidation)
	}
	repo, err := s.getRepositoryByProjectID(ctx, projectID)
	if err != nil {
		return err
	}
	ci, err := s.getCIRepoByRepositoryID(ctx, repo.ID)
	if err != nil {
		return err
	}
	if ci.WoodpeckerRepoID == 0 {
		return fmt.Errorf("%w: woodpecker repo not configured", ErrNotFound)
	}
	if err := s.woodpecker.Rerun(ctx, ci.WoodpeckerRepoID, runNumber); err != nil {
		return err
	}
	_ = s.sink.Emit(ctx, domain.Event{
		ID:         domain.ID(newID()),
		Type:       "scm.run.rerun",
		Severity:   "info",
		ResourceID: projectID,
		Message:    fmt.Sprintf("rerun %d for %s/%s", runNumber, repo.Owner, repo.Name),
		Data: map[string]any{
			"project_id": string(projectID),
			"run_number": runNumber,
		},
		CreatedAt: time.Now().UTC(),
	})
	return nil
}

// Cancel cancels a running CI run via Woodpecker.
func (s *Service) Cancel(ctx context.Context, projectID domain.ID, runNumber int) error {
	if strings.TrimSpace(string(projectID)) == "" {
		return fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if runNumber <= 0 {
		return fmt.Errorf("%w: run number must be positive", ErrValidation)
	}
	repo, err := s.getRepositoryByProjectID(ctx, projectID)
	if err != nil {
		return err
	}
	ci, err := s.getCIRepoByRepositoryID(ctx, repo.ID)
	if err != nil {
		return err
	}
	if ci.WoodpeckerRepoID == 0 {
		return fmt.Errorf("%w: woodpecker repo not configured", ErrNotFound)
	}
	if err := s.woodpecker.Cancel(ctx, ci.WoodpeckerRepoID, runNumber); err != nil {
		return err
	}
	_ = s.sink.Emit(ctx, domain.Event{
		ID:         domain.ID(newID()),
		Type:       "scm.run.cancelled",
		Severity:   "info",
		ResourceID: projectID,
		Message:    fmt.Sprintf("cancel %d for %s/%s", runNumber, repo.Owner, repo.Name),
		Data: map[string]any{
			"project_id": string(projectID),
			"run_number": runNumber,
		},
		CreatedAt: time.Now().UTC(),
	})
	return nil
}

// ConfigureMirror creates or updates the optional read-only GitHub push mirror.
// It validates that Forgejo remains canonical, stores the credential as a
// repository-scoped secret reference, and configures the Forgejo push mirror.
// Raw token values never appear in SQLite state or Woodpecker arguments.
func (s *Service) ConfigureMirror(ctx context.Context, projectID domain.ID, cfg MirrorConfig) (*Mirror, []string, error) {
	if strings.TrimSpace(string(projectID)) == "" {
		return nil, nil, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	remoteURL, err := validateMirrorURL(cfg.RemoteURL)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, nil, fmt.Errorf("%w: mirror token is required", ErrValidation)
	}
	if strings.Contains(cfg.Token, "\x00") {
		return nil, nil, fmt.Errorf("%w: mirror token contains NUL", ErrValidation)
	}

	repo, err := s.getRepositoryByProjectID(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}

	scope, mname := secretRefForProject(projectID)
	if err := s.secrets.Put(ctx, scope, mname, cfg.Token); err != nil {
		return nil, nil, fmt.Errorf("store mirror secret: %w", err)
	}
	secretRef := scope + "/" + mname

	mInput := MirrorInput{
		RemoteURL:           remoteURL,
		RemoteName:          "github",
		CredentialSecretRef: secretRef,
		IntervalSeconds:     0,
		LFSEnabled:          cfg.LFS,
	}
	if err := s.forgejo.PutPushMirror(ctx, RepoRef{Owner: repo.Owner, Name: repo.Name}, mInput); err != nil {
		_ = s.secrets.Delete(ctx, scope, mname)
		return nil, nil, fmt.Errorf("configure push mirror: %w", err)
	}

	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	existingMirror, err := s.getMirrorByRepositoryID(ctx, repo.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, nil, err
	}
	var mirror *Mirror
	if existingMirror != nil {
		_, err = s.db.ExecContext(ctx,
			`UPDATE scm_mirrors SET remote_url=?, credential_secret_ref=?, lfs_enabled=?, desired_state='configured', observed_state='ready', observed_detail='', updated_at=? WHERE id=?`,
			remoteURL, secretRef, boolToInt(cfg.LFS), nowStr, string(existingMirror.ID),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("update mirror: %w", err)
		}
		mirror, _ = s.getMirrorByRepositoryID(ctx, repo.ID)
	} else {
		mID := newID()
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO scm_mirrors (id, repository_id, remote_url, remote_name, credential_secret_ref, interval_seconds, lfs_enabled, desired_state, observed_state, observed_detail, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			mID, string(repo.ID), remoteURL, "github", secretRef, 0, boolToInt(cfg.LFS), "configured", "ready", "", nowStr, nowStr,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("insert mirror: %w", err)
		}
		mirror, _ = s.getMirrorByID(ctx, domain.ID(mID))
	}

	warnings := mirrorWarnings(cfg.LFS)
	_ = s.sink.Emit(ctx, domain.Event{
		ID:         domain.ID(newID()),
		Type:       "scm.mirror.configured",
		Severity:   "info",
		ResourceID: projectID,
		Message:    fmt.Sprintf("configured github mirror for %s/%s", repo.Owner, repo.Name),
		Data: map[string]any{
			"project_id": string(projectID),
			"remote_url": remoteURL,
		},
		CreatedAt: time.Now().UTC(),
	})
	return mirror, warnings, nil
}

// RemoveMirror deletes the GitHub push mirror configuration and its secret reference.
func (s *Service) RemoveMirror(ctx context.Context, projectID domain.ID) error {
	if strings.TrimSpace(string(projectID)) == "" {
		return fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	repo, err := s.getRepositoryByProjectID(ctx, projectID)
	if err != nil {
		return err
	}
	mirror, err := s.getMirrorByRepositoryID(ctx, repo.ID)
	if err != nil {
		return err
	}

	_ = s.forgejo.DeletePushMirror(ctx, RepoRef{Owner: repo.Owner, Name: repo.Name}, mirror.RemoteName)

	scope, mname := secretRefForProject(projectID)
	_ = s.secrets.Delete(ctx, scope, mname)

	_, err = s.db.ExecContext(ctx, `DELETE FROM scm_mirrors WHERE id=?`, string(mirror.ID))
	if err != nil {
		return fmt.Errorf("delete mirror: %w", err)
	}

	_ = s.sink.Emit(ctx, domain.Event{
		ID:         domain.ID(newID()),
		Type:       "scm.mirror.removed",
		Severity:   "info",
		ResourceID: projectID,
		Message:    fmt.Sprintf("removed github mirror for %s/%s", repo.Owner, repo.Name),
		Data: map[string]any{
			"project_id": string(projectID),
		},
		CreatedAt: time.Now().UTC(),
	})
	return nil
}

// GetMirror returns the current mirror configuration, or ErrNotFound.
func (s *Service) GetMirror(ctx context.Context, projectID domain.ID) (*Mirror, error) {
	if strings.TrimSpace(string(projectID)) == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	repo, err := s.getRepositoryByProjectID(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return s.getMirrorByRepositoryID(ctx, repo.ID)
}

// TemplateFor returns the pipeline template for an already-provisioned project.
func (s *Service) TemplateFor(ctx context.Context, projectID domain.ID, registryHost, releaseCallbackBaseURL string) (string, error) {
	if strings.TrimSpace(string(projectID)) == "" {
		return "", fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	repo, err := s.getRepositoryByProjectID(ctx, projectID)
	if err != nil {
		return "", err
	}
	callback := releaseCallbackBaseURL
	if callback == "" {
		callback = fmt.Sprintf("https://omahabd.local/v1/projects/%s/releases", string(projectID))
	} else {
		callback = strings.TrimRight(callback, "/") + fmt.Sprintf("/v1/projects/%s/releases", string(projectID))
	}
	return PipelineTemplate(PipelineTemplateInput{
		Owner:              repo.Owner,
		Name:               repo.Name,
		DefaultBranch:      repo.DefaultBranch,
		RegistryHost:       registryHost,
		ReleaseCallbackURL: callback,
	}), nil
}

// --- persistence helpers ---

func (s *Service) getRepositoryByProjectID(ctx context.Context, projectID domain.ID) (*Repository, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, project_id, owner, name, clone_url, default_branch, forgejo_remote_id, private, actions_disabled, desired_state, observed_state, observed_detail, created_at, updated_at
		 FROM scm_repositories WHERE project_id=?`, string(projectID))
	return scanRepository(row)
}

func (s *Service) getRepositoryByOwnerName(ctx context.Context, owner, name string) (*Repository, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, project_id, owner, name, clone_url, default_branch, forgejo_remote_id, private, actions_disabled, desired_state, observed_state, observed_detail, created_at, updated_at
		 FROM scm_repositories WHERE owner=? AND name=?`, owner, name)
	return scanRepository(row)
}

func (s *Service) getRepositoryByID(ctx context.Context, id domain.ID) (*Repository, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, project_id, owner, name, clone_url, default_branch, forgejo_remote_id, private, actions_disabled, desired_state, observed_state, observed_detail, created_at, updated_at
		 FROM scm_repositories WHERE id=?`, string(id))
	return scanRepository(row)
}

func (s *Service) getCIRepoByRepositoryID(ctx context.Context, repositoryID domain.ID) (*CIRepoState, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, repository_id, woodpecker_repo_id, forgejo_remote_id, pipeline_path, enabled, trusted, desired_state, observed_state, observed_detail, created_at, updated_at
		 FROM scm_ci_repos WHERE repository_id=?`, string(repositoryID))
	return scanCIRepo(row)
}

func (s *Service) getCIRepoByWoodpeckerID(ctx context.Context, woodpeckerRepoID int64) (*CIRepoState, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, repository_id, woodpecker_repo_id, forgejo_remote_id, pipeline_path, enabled, trusted, desired_state, observed_state, observed_detail, created_at, updated_at
		 FROM scm_ci_repos WHERE woodpecker_repo_id=?`, woodpeckerRepoID)
	return scanCIRepo(row)
}

func (s *Service) getMirrorByRepositoryID(ctx context.Context, repositoryID domain.ID) (*Mirror, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, repository_id, remote_url, remote_name, credential_secret_ref, interval_seconds, lfs_enabled, desired_state, observed_state, observed_detail, last_synced_at, created_at, updated_at
		 FROM scm_mirrors WHERE repository_id=?`, string(repositoryID))
	return scanMirror(row)
}

func (s *Service) getMirrorByID(ctx context.Context, id domain.ID) (*Mirror, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, repository_id, remote_url, remote_name, credential_secret_ref, interval_seconds, lfs_enabled, desired_state, observed_state, observed_detail, last_synced_at, created_at, updated_at
		 FROM scm_mirrors WHERE id=?`, string(id))
	return scanMirror(row)
}

type rowScanner interface{ Scan(dest ...any) error }

func scanRepository(row rowScanner) (*Repository, error) {
	var id, projectID, owner, name, cloneURL, defaultBranch, desiredState, observedState, observedDetail, createdAt, updatedAt string
	var forgejoRemoteID int64
	var priv, actionsDisabled int
	if err := row.Scan(&id, &projectID, &owner, &name, &cloneURL, &defaultBranch, &forgejoRemoteID, &priv, &actionsDisabled, &desiredState, &observedState, &observedDetail, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: repository not found", ErrNotFound)
		}
		return nil, fmt.Errorf("scan repository: %w", err)
	}
	ca, _ := time.Parse(time.RFC3339Nano, createdAt)
	ua, _ := time.Parse(time.RFC3339Nano, updatedAt)
	return &Repository{
		ID:              domain.ID(id),
		ProjectID:       domain.ID(projectID),
		Owner:           owner,
		Name:            name,
		CloneURL:        cloneURL,
		DefaultBranch:   defaultBranch,
		ForgejoRemoteID: forgejoRemoteID,
		Private:         priv != 0,
		ActionsDisabled: actionsDisabled != 0,
		DesiredState:    desiredState,
		ObservedState:   observedState,
		ObservedDetail:  observedDetail,
		CreatedAt:       ca,
		UpdatedAt:       ua,
	}, nil
}

func scanCIRepo(row rowScanner) (*CIRepoState, error) {
	var id, repositoryID, pipelinePath, desiredState, observedState, observedDetail, createdAt, updatedAt string
	var woodpeckerRepoID, forgejoRemoteID int64
	var enabled, trusted int
	if err := row.Scan(&id, &repositoryID, &woodpeckerRepoID, &forgejoRemoteID, &pipelinePath, &enabled, &trusted, &desiredState, &observedState, &observedDetail, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: ci repo not found", ErrNotFound)
		}
		return nil, fmt.Errorf("scan ci repo: %w", err)
	}
	ca, _ := time.Parse(time.RFC3339Nano, createdAt)
	ua, _ := time.Parse(time.RFC3339Nano, updatedAt)
	return &CIRepoState{
		ID:               domain.ID(id),
		RepositoryID:     domain.ID(repositoryID),
		WoodpeckerRepoID: woodpeckerRepoID,
		ForgejoRemoteID:  forgejoRemoteID,
		PipelinePath:     pipelinePath,
		Enabled:          enabled != 0,
		Trusted:          trusted != 0,
		DesiredState:     desiredState,
		ObservedState:    observedState,
		ObservedDetail:   observedDetail,
		CreatedAt:        ca,
		UpdatedAt:        ua,
	}, nil
}

func scanMirror(row rowScanner) (*Mirror, error) {
	var id, repositoryID, remoteURL, remoteName, credRef, desiredState, observedState, observedDetail, createdAt, updatedAt string
	var intervalSeconds, lfsEnabled int
	var lastSyncedAt sql.NullString
	if err := row.Scan(&id, &repositoryID, &remoteURL, &remoteName, &credRef, &intervalSeconds, &lfsEnabled, &desiredState, &observedState, &observedDetail, &lastSyncedAt, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: mirror not found", ErrNotFound)
		}
		return nil, fmt.Errorf("scan mirror: %w", err)
	}
	ca, _ := time.Parse(time.RFC3339Nano, createdAt)
	ua, _ := time.Parse(time.RFC3339Nano, updatedAt)
	var lsa *time.Time
	if lastSyncedAt.Valid && lastSyncedAt.String != "" {
		if t, err := time.Parse(time.RFC3339Nano, lastSyncedAt.String); err == nil {
			lsa = &t
		}
	}
	return &Mirror{
		ID:                  domain.ID(id),
		RepositoryID:        domain.ID(repositoryID),
		RemoteURL:           remoteURL,
		RemoteName:          remoteName,
		CredentialSecretRef: credRef,
		IntervalSeconds:     intervalSeconds,
		LFSEnabled:          lfsEnabled != 0,
		DesiredState:        desiredState,
		ObservedState:       observedState,
		ObservedDetail:      observedDetail,
		LastSyncedAt:        lsa,
		CreatedAt:           ca,
		UpdatedAt:           ua,
	}, nil
}

func scanRunRecord(row rowScanner) (*RunRecord, error) {
	var id, repositoryID, status, branch, commitSHA, event, message, author, createdAt, updatedAt string
	var runNumber int
	var woodpeckerID int64
	var startedAt, finishedAt sql.NullString
	if err := row.Scan(&id, &repositoryID, &runNumber, &woodpeckerID, &status, &branch, &commitSHA, &event, &message, &author, &startedAt, &finishedAt, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: run not found", ErrNotFound)
		}
		return nil, fmt.Errorf("scan run: %w", err)
	}
	ca, _ := time.Parse(time.RFC3339Nano, createdAt)
	ua, _ := time.Parse(time.RFC3339Nano, updatedAt)
	var sa, fa *time.Time
	if startedAt.Valid && startedAt.String != "" {
		if t, err := time.Parse(time.RFC3339Nano, startedAt.String); err == nil {
			sa = &t
		}
	}
	if finishedAt.Valid && finishedAt.String != "" {
		if t, err := time.Parse(time.RFC3339Nano, finishedAt.String); err == nil {
			fa = &t
		}
	}
	return &RunRecord{
		ID:           domain.ID(id),
		RepositoryID: domain.ID(repositoryID),
		Number:       runNumber,
		WoodpeckerID: woodpeckerID,
		Status:       status,
		Branch:       branch,
		CommitSHA:    commitSHA,
		Event:        event,
		Message:      message,
		Author:       author,
		StartedAt:    sa,
		FinishedAt:   fa,
		CreatedAt:    ca,
		UpdatedAt:    ua,
	}, nil
}

// --- small helpers ---

// SeedWoodpeckerConfig seeds .woodpecker.yaml via the underlying Forgejo client if it supports PutFile.
// It is a thin adapter for controlplane CreateProject to ensure the pipeline file exists after Provision.
func (s *Service) SeedWoodpeckerConfig(ctx context.Context, ref RepoRef, content string) error {
	if s == nil || s.forgejo == nil {
		return nil
	}
	// Try interface with SeedWoodpeckerConfig first (concrete client).
	type seeder interface {
		SeedWoodpeckerConfig(context.Context, RepoRef, string) error
	}
	if sc, ok := s.forgejo.(seeder); ok {
		return sc.SeedWoodpeckerConfig(ctx, ref, content)
	}
	// Fallback to generic PutFile if available.
	type putter interface {
		PutFile(context.Context, RepoRef, string, []byte, string) error
	}
	if sc, ok := s.forgejo.(putter); ok {
		return sc.PutFile(ctx, ref, ".woodpecker.yaml", []byte(content), "Add .woodpecker.yaml (managed by Omahab)")
	}
	return nil
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "UNIQUE constraint")
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func limitClause(limit int) string {
	if limit > 0 {
		return fmt.Sprintf(" LIMIT %d", limit)
	}
	return ""
}
