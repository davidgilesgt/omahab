package scm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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

// New creates a Service. Returns error if required dependencies are nil.
// Tests must provide fakes; the only production constructor is controlplane Backend.bindSCM.
func New(db *sql.DB, forgejo ForgejoClient, woodpecker WoodpeckerClient, secrets SecretStore, sink EventSink) (*Service, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: db is required", ErrValidation)
	}
	if forgejo == nil {
		return nil, fmt.Errorf("%w: forgejo client is required", ErrValidation)
	}
	if woodpecker == nil {
		return nil, fmt.Errorf("%w: woodpecker client is required", ErrValidation)
	}
	if secrets == nil {
		return nil, fmt.Errorf("%w: secret store is required", ErrValidation)
	}
	if sink == nil {
		sink = NoopSink{}
	}
	return &Service{db: db, forgejo: forgejo, woodpecker: woodpecker, secrets: secrets, sink: sink}, nil
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
	ProjectID          domain.ID
	Owner              string
	RepoName           string
	Description        string
	DefaultBranch      string
	RegistryHost       string
	ReleaseCallbackURL string
	BuilderImage       string
	ReleaseToken       string
	WebhookURL         string
	WebhookSecret      string
	Mirror             *MirrorConfig
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

func registrySecretRef(projectID domain.ID) (scope, name string) {
	return "scm:project:" + string(projectID), "registry-token"
}

func ciUsernameForProject(projectID domain.ID) string {
	h := sha256.Sum256([]byte(string(projectID)))
	hexStr := hex.EncodeToString(h[:])
	if len(hexStr) > 12 {
		hexStr = hexStr[:12]
	}
	return "ci-" + hexStr
}

func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrConflict) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "already_exist")
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
	// upstream systems.
	existing, err := s.getRepositoryByProjectID(ctx, in.ProjectID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if existing != nil && existing.ObservedState != "error" {
		return nil, fmt.Errorf("%w: project already has a repository", ErrConflict)
	}
	retryingAfterError := existing != nil && existing.ObservedState == "error"

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

	if retryingAfterError {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM scm_mirrors WHERE repository_id=?`, repoID)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM scm_ci_repos WHERE repository_id=?`, repoID)
	}

	markError := func(detail string) {
		// Never include raw secret values in detail
		_, _ = s.db.ExecContext(ctx,
			`UPDATE scm_repositories SET observed_state='error', observed_detail=?, updated_at=? WHERE id=?`,
			detail, time.Now().UTC().Format(time.RFC3339Nano), repoID,
		)
	}

	// Track resources created in this attempt for compensation.
	var (
		createdForgejoRepo     bool
		createdWoodpeckerRepo  bool
		woodpeckerRepoID       int64
		ciUsername             string
		ciUserCreated          bool
		registryTokenCreated   bool
		registryTokenScope     string
		registryTokenName      = "registry-token"
		woodpeckerSecretsCreated bool
	)

	compensate := func() {
		// Best-effort compensation: remove resources created in this attempt.
		if woodpeckerSecretsCreated && woodpeckerRepoID != 0 {
			_ = s.woodpecker.DeleteRepoSecret(ctx, woodpeckerRepoID, "omahab_registry_user")
			_ = s.woodpecker.DeleteRepoSecret(ctx, woodpeckerRepoID, "omahab_registry_password")
			_ = s.woodpecker.DeleteRepoSecret(ctx, woodpeckerRepoID, "omahab_release_token")
		}
		if createdWoodpeckerRepo && woodpeckerRepoID != 0 {
			_ = s.woodpecker.DeactivateRepo(ctx, woodpeckerRepoID)
			_, _ = s.db.ExecContext(ctx, `DELETE FROM scm_ci_repos WHERE repository_id=?`, repoID)
		}
		if ciUserCreated && ciUsername != "" {
			_ = s.forgejo.DeleteUser(ctx, ciUsername)
		}
		if registryTokenCreated && registryTokenScope != "" {
			_ = s.secrets.Delete(ctx, registryTokenScope, registryTokenName)
		}
		if createdForgejoRepo {
			_ = s.forgejo.DeleteRepo(ctx, RepoRef{Owner: owner, Name: repoName})
		}
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
		if errors.Is(err, ErrConflict) || strings.Contains(strings.ToLower(err.Error()), "already exists") {
			if got, gerr := s.forgejo.GetRepo(ctx, RepoRef{Owner: owner, Name: repoName}); gerr == nil {
				forgejoRepo = got
			} else {
				markError("create repo failed")
				compensate()
				return nil, fmt.Errorf("create forgejo repo: %w", err)
			}
		} else {
			markError("create repo failed")
			compensate()
			return nil, fmt.Errorf("create forgejo repo: %w", err)
		}
	} else {
		createdForgejoRepo = true
	}

	cloneURL := forgejoRepo.CloneURL
	remoteID := forgejoRepo.RemoteID
	_, err = s.db.ExecContext(ctx,
		`UPDATE scm_repositories SET clone_url=?, forgejo_remote_id=?, updated_at=? WHERE id=?`,
		cloneURL, remoteID, time.Now().UTC().Format(time.RFC3339Nano), repoID,
	)
	if err != nil {
		markError("update clone url failed")
		compensate()
		return nil, fmt.Errorf("update repository clone url: %w", err)
	}
	repoRow.CloneURL = cloneURL
	repoRow.ForgejoRemoteID = remoteID

	// Ensure initial main commit exists. If repo is empty, create README.
	if got, gerr := s.forgejo.GetRepo(ctx, RepoRef{Owner: owner, Name: repoName}); gerr == nil {
		// Check if empty via file list? For fake, we check if repo has any files; for real, check if GetRepo indicates empty? We use attempt to GetRepo and if no files, create README.
		// If the repo is newly created, ensure a commit exists by putting README.
		if createdForgejoRepo {
			// Try to create initial commit via PutFile. If file already exists, ignore conflict.
			_ = s.forgejo.PutFile(ctx, RepoRef{Owner: owner, Name: repoName}, "README.md", []byte("# "+repoName+"\n"), "Initial commit")
		} else if got != nil {
			// Adopted repo: ensure it has at least one commit; if Get shows empty, the PutFile above already handled.
			_ = got
		}
	} else {
		// If GetRepo fails, we still try to create initial commit for safety.
		_ = s.forgejo.PutFile(ctx, RepoRef{Owner: owner, Name: repoName}, "README.md", []byte("# "+repoName+"\n"), "Initial commit")
	}

	// Disable Forgejo Actions — Woodpecker is the only CI system.
	if err := s.forgejo.SetActionsEnabled(ctx, RepoRef{Owner: owner, Name: repoName}, false); err != nil {
		markError("disable actions failed")
		compensate()
		return nil, fmt.Errorf("disable forgejo actions: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE scm_repositories SET actions_disabled=1, updated_at=? WHERE id=?`,
		time.Now().UTC().Format(time.RFC3339Nano), repoID,
	)
	if err != nil {
		markError("mark actions disabled failed")
		compensate()
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
		markError("configure woodpecker failed")
		compensate()
		return nil, fmt.Errorf("configure woodpecker repo: %w", err)
	}
	woodpeckerRepoID = ciRepo.ID
	createdWoodpeckerRepo = true
	ciID := newID()
	ciNow := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO scm_ci_repos (id, repository_id, woodpecker_repo_id, forgejo_remote_id, pipeline_path, enabled, trusted, desired_state, observed_state, observed_detail, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ciID, repoID, ciRepo.ID, remoteID, ciRepo.PipelinePath, 1, boolToInt(ciRepo.Trusted), "enabled", "ready", "", ciNow, ciNow,
	)
	if err != nil {
		markError("persist ci repo failed")
		compensate()
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

	// Create deterministic restricted Forgejo CI user and PAT.
	ciUsername = ciUsernameForProject(in.ProjectID)
	ciEmail := ciUsername + "@users.noreply.example.com"
	// Derive domain for email if registry host present
	if in.RegistryHost != "" {
		if idx := strings.Index(in.RegistryHost, "."); idx > 0 {
			domainPart := in.RegistryHost[idx+1:]
			if domainPart != "" {
				ciEmail = ciUsername + "@users.noreply." + domainPart
			}
		}
	}
	if err := s.forgejo.CreateUser(ctx, ciUsername, ciEmail); err != nil {
		if !errors.Is(err, ErrConflict) && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
			markError("create ci user failed")
			compensate()
			return nil, fmt.Errorf("create ci user: %w", err)
		}
	} else {
		ciUserCreated = true
	}
	if err := s.forgejo.AddCollaborator(ctx, RepoRef{Owner: owner, Name: repoName}, ciUsername, "write"); err != nil {
		markError("grant ci user access failed")
		compensate()
		return nil, fmt.Errorf("add ci collaborator: %w", err)
	}
	// Mint/reuse PAT scoped write:repository,write:package
	registryTokenScope = "scm:project:" + string(in.ProjectID)
	// Try to reuse existing token from secret store first
	var registryPassword string
	if existingTok, gerr := s.secrets.Get(ctx, registryTokenScope, registryTokenName); gerr == nil && strings.TrimSpace(existingTok) != "" {
		registryPassword = strings.TrimSpace(existingTok)
	} else {
		tok, terr := s.forgejo.CreateToken(ctx, ciUsername, registryTokenName, []string{"write:repository", "write:package"})
		if terr != nil {
			markError("create registry token failed")
			compensate()
			return nil, fmt.Errorf("create registry token: %w", terr)
		}
		registryPassword = tok
		if err := s.secrets.Put(ctx, registryTokenScope, registryTokenName, registryPassword); err != nil {
			// Clean up token if secret store fails
			_ = s.forgejo.DeleteToken(ctx, ciUsername, registryTokenName)
			markError("store registry token failed")
			compensate()
			return nil, fmt.Errorf("store registry token: %w", err)
		}
		registryTokenCreated = true
	}

	// Upsert Woodpecker repo secrets. Raw values only in memory/secret store/Woodpecker.
	releaseToken := strings.TrimSpace(in.ReleaseToken)
	if releaseToken == "" {
		markError("missing release token")
		compensate()
		return nil, fmt.Errorf("%w: release token is required", ErrValidation)
	}
	if err := s.woodpecker.UpsertRepoSecret(ctx, woodpeckerRepoID, "omahab_registry_user", ciUsername); err != nil {
		markError("upsert registry user secret failed")
		compensate()
		return nil, fmt.Errorf("upsert registry user secret: %w", err)
	}
	if err := s.woodpecker.UpsertRepoSecret(ctx, woodpeckerRepoID, "omahab_registry_password", registryPassword); err != nil {
		markError("upsert registry password secret failed")
		compensate()
		return nil, fmt.Errorf("upsert registry password secret: %w", err)
	}
	if err := s.woodpecker.UpsertRepoSecret(ctx, woodpeckerRepoID, "omahab_release_token", releaseToken); err != nil {
		markError("upsert release token secret failed")
		compensate()
		return nil, fmt.Errorf("upsert release token secret: %w", err)
	}
	woodpeckerSecretsCreated = true

	// Optional GitHub push mirror.
	var mirrorRow *Mirror
	var warnings []string
	if in.Mirror != nil {
		mirrorURL, _ := validateMirrorURL(in.Mirror.RemoteURL)
		scope, mname := secretRefForProject(in.ProjectID)
		if err := s.secrets.Put(ctx, scope, mname, in.Mirror.Token); err != nil {
			markError("store mirror secret failed")
			compensate()
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
			markError("configure push mirror failed")
			compensate()
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
			markError("insert mirror failed")
			compensate()
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

	// Generate pipeline template with exact callback URL and pinned builder image.
	callbackURL := strings.TrimSpace(in.ReleaseCallbackURL)
	if callbackURL == "" {
		callbackURL = fmt.Sprintf("https://omahab.example.com/api/v1/projects/%s/releases/with-token", string(in.ProjectID))
	}
	builderImage := strings.TrimSpace(in.BuilderImage)
	tmpl := PipelineTemplate(PipelineTemplateInput{
		Owner:              owner,
		Name:               repoName,
		DefaultBranch:      defaultBranch,
		RegistryHost:       in.RegistryHost,
		ReleaseCallbackURL: callbackURL,
		BuilderImage:       builderImage,
		ProjectID:          string(in.ProjectID),
	})

	// Seed .woodpecker.yaml via Forgejo only after initial commit exists.
	if _, err := s.forgejo.GetRepo(ctx, RepoRef{Owner: owner, Name: repoName}); err != nil {
		markError("verify repo for seeding failed")
		compensate()
		return nil, fmt.Errorf("verify repo for seeding: %w", err)
	}
	if err := s.forgejo.PutFile(ctx, RepoRef{Owner: owner, Name: repoName}, ".woodpecker.yaml", []byte(tmpl), "Add .woodpecker.yaml (managed by Omahab)"); err != nil {
		markError("seed pipeline failed")
		compensate()
		return nil, fmt.Errorf("seed woodpecker config: %w", err)
	}

	// Ensure Forgejo webhook for pull_request and push events (HMAC-verified ingress).
	if strings.TrimSpace(in.WebhookURL) != "" && strings.TrimSpace(in.WebhookSecret) != "" {
		if err := s.forgejo.EnsureWebhook(ctx, RepoRef{Owner: owner, Name: repoName}, strings.TrimSpace(in.WebhookURL), strings.TrimSpace(in.WebhookSecret), []string{"pull_request", "push"}); err != nil {
			markError("ensure webhook failed")
			compensate()
			return nil, fmt.Errorf("ensure webhook: %w", err)
		}
	}

	// Mark repository ready.
	readyAt := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.ExecContext(ctx,
		`UPDATE scm_repositories SET observed_state='ready', observed_detail='', updated_at=? WHERE id=?`,
		readyAt, repoID,
	)
	if err != nil {
		markError("mark ready failed")
		compensate()
		return nil, fmt.Errorf("mark repository ready: %w", err)
	}
	repoRow.ObservedState = "ready"
	repoRow.ObservedDetail = ""
	if refreshed, gerr := s.getRepositoryByID(ctx, domain.ID(repoID)); gerr == nil {
		repoRow = refreshed
	}

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
// Deprovision removes all external resources for a project: Woodpecker repo and secrets, Forgejo repo and CI user/token, encrypted secrets, and SCM rows. 404 is treated as already removed.
func (s *Service) Deprovision(ctx context.Context, projectID domain.ID) error {
	if strings.TrimSpace(string(projectID)) == "" {
		return fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	repo, err := s.getRepositoryByProjectID(ctx, projectID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	var repoID domain.ID
	var forgejoOwner, forgejoName string
	var woodpeckerRepoID int64
	var ciRepoID domain.ID
	if repo != nil {
		repoID = repo.ID
		forgejoOwner = repo.Owner
		forgejoName = repo.Name
		if ci, cerr := s.getCIRepoByRepositoryID(ctx, repo.ID); cerr == nil && ci != nil {
			woodpeckerRepoID = ci.WoodpeckerRepoID
			ciRepoID = ci.ID
		} else if cerr != nil && !errors.Is(cerr, ErrNotFound) {
			return cerr
		}
	}
	ciUsername := ciUsernameForProject(projectID)
	registryScope, registryName := registrySecretRef(projectID)
	if woodpeckerRepoID != 0 {
		_ = s.woodpecker.DeleteRepoSecret(ctx, woodpeckerRepoID, "omahab_registry_user")
		_ = s.woodpecker.DeleteRepoSecret(ctx, woodpeckerRepoID, "omahab_registry_password")
		_ = s.woodpecker.DeleteRepoSecret(ctx, woodpeckerRepoID, "omahab_release_token")
		if err := s.woodpecker.DeactivateRepo(ctx, woodpeckerRepoID); err != nil && !isNotFoundErr(err) {
			return fmt.Errorf("deactivate woodpecker repo: %w", err)
		}
		if ciRepoID != "" {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM scm_ci_repos WHERE id=?`, string(ciRepoID))
		} else if repoID != "" {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM scm_ci_repos WHERE repository_id=?`, string(repoID))
		}
	}
	if forgejoOwner != "" && forgejoName != "" {
		if err := s.forgejo.DeleteRepo(ctx, RepoRef{Owner: forgejoOwner, Name: forgejoName}); err != nil && !isNotFoundErr(err) {
			return fmt.Errorf("delete forgejo repo: %w", err)
		}
	}
	_ = s.forgejo.DeleteToken(ctx, ciUsername, registryName)
	if err := s.forgejo.DeleteUser(ctx, ciUsername); err != nil && !isNotFoundErr(err) {
		return fmt.Errorf("delete ci user: %w", err)
	}
	_ = s.secrets.Delete(ctx, registryScope, registryName)
	if scope, name := secretRefForProject(projectID); scope != registryScope || name != registryName {
		_ = s.secrets.Delete(ctx, scope, name)
	}
	if repoID != "" {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM scm_mirrors WHERE repository_id=?`, string(repoID))
		_, _ = s.db.ExecContext(ctx, `DELETE FROM scm_repositories WHERE id=?`, string(repoID))
	}
	_ = s.sink.Emit(ctx, domain.Event{
		ID:         domain.ID(newID()),
		Type:       "scm.repository.deprovisioned",
		Severity:   "info",
		ResourceID: projectID,
		Message:    fmt.Sprintf("deprovisioned %s", string(projectID)),
		Data: map[string]any{
			"project_id": string(projectID),
		},
		CreatedAt: time.Now().UTC(),
	})
	return nil
}

// ReconcileLegacySentinels repairs rows created by the old no-op clients.
// It identifies only the sentinel clone prefix https://git.example.invalid/,
// deletes its synthetic CI row, marks the repository integration error,
// rewrites forgejo.local/registry.local defaults to git.<domain>, and lets
// normal provisioning repair it after the PAT handoff. Non-sentinel rows are untouched.
func (s *Service) ReconcileLegacySentinels(ctx context.Context, domainName string) error {
	if strings.TrimSpace(domainName) == "" {
		return nil
	}
	domainName = strings.TrimSpace(strings.ToLower(domainName))
	gitHost := "git." + domainName
	rows, err := s.db.QueryContext(ctx, `SELECT id, project_id, clone_url FROM scm_repositories WHERE clone_url LIKE 'https://git.example.invalid/%'`)
	if err != nil {
		return fmt.Errorf("query sentinel repos: %w", err)
	}
	defer rows.Close()
	type sentinel struct {
		id        string
		projectID string
		cloneURL  string
	}
	var sentinels []sentinel
	for rows.Next() {
		var id, pid, curl string
		if err := rows.Scan(&id, &pid, &curl); err != nil {
			return err
		}
		sentinels = append(sentinels, sentinel{id: id, projectID: pid, cloneURL: curl})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, ss := range sentinels {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM scm_ci_repos WHERE repository_id=?`, ss.id)
		_, _ = s.db.ExecContext(ctx, `UPDATE scm_repositories SET observed_state='error', observed_detail='legacy sentinel requires reprovisioning', updated_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), ss.id)
		_, _ = s.db.ExecContext(ctx, `UPDATE projects SET repository_url = REPLACE(repository_url, 'https://forgejo.local', 'https://`+gitHost+`') WHERE id=? AND repository_url LIKE 'https://forgejo.local%'`, ss.projectID)
		_, _ = s.db.ExecContext(ctx, `UPDATE projects SET repository_url = REPLACE(repository_url, 'https://git.example.invalid', 'https://`+gitHost+`') WHERE id=? AND repository_url LIKE 'https://git.example.invalid%'`, ss.projectID)
		_, _ = s.db.ExecContext(ctx, `UPDATE projects SET image_base = REPLACE(image_base, 'registry.local', '`+gitHost+`') WHERE id=? AND image_base LIKE 'registry.local%'`, ss.projectID)
		_, _ = s.db.ExecContext(ctx, `UPDATE projects SET image_base = REPLACE(image_base, 'forgejo.local', '`+gitHost+`') WHERE id=? AND image_base LIKE '%forgejo.local%'`, ss.projectID)
		newClone := strings.Replace(ss.cloneURL, "https://git.example.invalid", "https://"+gitHost, 1)
		if newClone != ss.cloneURL {
			_, _ = s.db.ExecContext(ctx, `UPDATE scm_repositories SET clone_url=?, updated_at=? WHERE id=?`, newClone, time.Now().UTC().Format(time.RFC3339Nano), ss.id)
		}
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE projects SET repository_url = REPLACE(repository_url, 'https://forgejo.local', 'https://`+gitHost+`') WHERE repository_url LIKE 'https://forgejo.local%'`)
	_, _ = s.db.ExecContext(ctx, `UPDATE projects SET repository_url = REPLACE(repository_url, 'https://git.example.invalid', 'https://`+gitHost+`') WHERE repository_url LIKE 'https://git.example.invalid%'`)
	_, _ = s.db.ExecContext(ctx, `UPDATE projects SET image_base = REPLACE(image_base, 'registry.local', '`+gitHost+`') WHERE image_base LIKE 'registry.local%'`)
	_, _ = s.db.ExecContext(ctx, `UPDATE projects SET image_base = REPLACE(image_base, 'forgejo.local', '`+gitHost+`') WHERE image_base LIKE '%forgejo.local%'`)
	return nil
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
	callback := strings.TrimSpace(releaseCallbackBaseURL)
	if callback == "" {
		callback = fmt.Sprintf("https://omahab.example.com/api/v1/projects/%s/releases/with-token", string(projectID))
	} else if !strings.Contains(callback, "/releases/with-token") {
		base := strings.TrimRight(callback, "/")
		if strings.Contains(base, "/api/v1/projects") {
			callback = base
		} else {
			callback = base + fmt.Sprintf("/api/v1/projects/%s/releases/with-token", string(projectID))
		}
	}
	return PipelineTemplate(PipelineTemplateInput{
		Owner:              repo.Owner,
		Name:               repo.Name,
		DefaultBranch:      repo.DefaultBranch,
		RegistryHost:       registryHost,
		ReleaseCallbackURL: callback,
		ProjectID:          string(projectID),
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
// It checks that the repository has an initial commit via GetRepo before attempting PutFile.
func (s *Service) SeedWoodpeckerConfig(ctx context.Context, ref RepoRef, content string) error {
	if s == nil || s.forgejo == nil {
		return nil
	}
	if _, err := s.forgejo.GetRepo(ctx, ref); err != nil {
		return err
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
