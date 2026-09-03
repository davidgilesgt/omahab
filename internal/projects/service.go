package projects

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

// Project is the control-plane project record: the deployment-relevant
// extension of domain.Project. One repository equals one project.
//
// The Contract and Image fields are deployment metadata validated against the
// default ONCE contract (port 80, health path /up, storage /storage); the
// embedded domain.Project carries the user-visible identity and timestamps.
type Project struct {
	domain.Project
	Image     string   `json:"image"`     // base OCI image reference without digest
	Contract  Contract `json:"contract"`  // validated ONCE contract metadata
	Deploying bool     `json:"deploying"` // true while a deployment holds the per-project lock
}

// CreateParams describe a new project.
type CreateParams struct {
	Slug          string
	Name          string
	RepositoryURL string
	Image         string
	Exposure      domain.Exposure
	Hostname      string
	Contract      Contract // zero value means DefaultContract
}

// Service implements one-repo-one-project CRUD and ONCE deployment
// orchestration for the Omahab control plane.
type Service struct {
	db     *sql.DB
	runner ONCERunner
	cfg    Config
	events EventRecorder
	tokens ReleaseTokenVerifier
	secrets secretsSource
}

// secretsSource is the minimal interface for project secrets file projection.
type secretsSource interface {
	List(ctx context.Context) ([]domain.Secret, error)
	Reveal(ctx context.Context, id domain.ID) (string, error)
}

// Deps wires the service. DB and Runner are required; Events and Tokens are
// optional (nil disables event emission and CI releases respectively).
type Deps struct {
	DB     *sql.DB
	Runner ONCERunner
	Config Config
	Events EventRecorder
	Tokens ReleaseTokenVerifier
}

// NewService creates a projects service from the supplied dependencies.
func NewService(deps Deps) (*Service, error) {
	if deps.DB == nil {
		return nil, errors.New("projects: DB is required")
	}
	if deps.Runner == nil {
		return nil, errors.New("projects: ONCERunner is required")
	}
	_ = store.Migration{} // verify store contract at compile time
	cfg := deps.Config.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("projects: invalid config: %w", err)
	}
	return &Service{
		db:     deps.DB,
		runner: deps.Runner,
		cfg:    cfg,
		events: deps.Events,
		tokens: deps.Tokens,
	}, nil
}

// SetReleaseTokenVerifier sets the token verifier after construction.
// This allows wiring the service as its own verifier (Deps.Tokens = svc).
func (s *Service) SetReleaseTokenVerifier(v ReleaseTokenVerifier) {
	s.tokens = v
}

// SetSecrets sets the secrets source for per-project secrets file projection.
func (s *Service) SetSecrets(src secretsSource) {
	s.secrets = src
}
// Ensure the package honors the store.Migrate contract.
var _ = func() {
	var _ = Migrations
}

// Create registers a new project. Slug uniqueness is enforced at the
// database layer and surfaced as ErrSlugTaken (409).
func (s *Service) Create(ctx context.Context, p CreateParams) (*Project, error) {
	slug, err := validateSlug(p.Slug)
	if err != nil {
		return nil, err
	}
	name, err := validateName(p.Name)
	if err != nil {
		return nil, err
	}
	repoURL, err := validateRepositoryURL(p.RepositoryURL)
	if err != nil {
		return nil, err
	}
	image, err := validateImageBase(p.Image)
	if err != nil {
		return nil, err
	}
	hostname, err := validateHostname(p.Hostname)
	if err != nil {
		return nil, err
	}
	exposure := p.Exposure
	if exposure == "" {
		exposure = domain.ExposurePrivate
	}
	if !exposure.Valid() {
		return nil, invalidf("exposure", "must be %q, %q, or %q", domain.ExposurePrivate, domain.ExposureShared, domain.ExposurePublic)
	}
	contract := p.Contract.withDefaults()
	if err := contract.validate(); err != nil {
		return nil, err
	}

	now := fmtTimeNow()
	proj := &Project{
		Project: domain.Project{
			ID:            newID(),
			Slug:          slug,
			Name:          name,
			RepositoryURL: repoURL,
			Exposure:      exposure,
			Hostname:      hostname,
			CreatedAt:     now,
			UpdatedAt:     now,
		},
		Image:     image,
		Contract:  contract,
		Deploying: false,
	}
	nowS := formatTime(now)
	_, err = s.db.ExecContext(ctx, `
INSERT INTO projects
  (id, slug, name, repository_url, image_base, exposure, hostname,
   contract_port, contract_health_path, contract_storage_path,
   deploying, deploy_started_ns, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)`, //nolint:execContext
		string(proj.ID), proj.Slug, proj.Name, proj.RepositoryURL, proj.Image,
		string(proj.Exposure), proj.Hostname,
		proj.Contract.Port, proj.Contract.HealthPath, proj.Contract.StoragePath,
		nowS, nowS)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: slug %q", ErrSlugTaken, slug)
		}
		return nil, fmt.Errorf("create project: %w", err)
	}
	s.emit(ctx, "project.created", severityInfo, proj.ID,
		fmt.Sprintf("project %q created", proj.Slug),
		map[string]any{"slug": proj.Slug, "exposure": string(proj.Exposure)})
	return proj, nil
}

// List returns all projects ordered by slug.
func (s *Service) List(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, slug, name, repository_url, image_base, exposure, hostname,
       contract_port, contract_health_path, contract_storage_path,
       deploying, created_at, updated_at
FROM projects ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// Get retrieves a project by ID.
func (s *Service) Get(ctx context.Context, id domain.ID) (*Project, error) {
	if strings.TrimSpace(string(id)) == "" {
		return nil, invalidf("id", "must not be empty")
	}
	p, err := s.fetchProject(ctx, "id = ?", string(id))
	if err != nil {
		return nil, err
	}
	return p, nil
}

// GetBySlug retrieves a project by its slug.
func (s *Service) GetBySlug(ctx context.Context, slug string) (*Project, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, invalidf("slug", "must not be empty")
	}
	slug, err := validateSlug(slug)
	if err != nil {
		return nil, err
	}
	return s.fetchProject(ctx, "slug = ?", slug)
}

// Delete removes a project and its release history. When releases exist the
// runtime deployment is torn down via the ONCERunner (undeploy) before the
// records are removed; an undeploy failure aborts the delete so the host is
// not left with an untracked container. Project storage under
// DataDir/projects/<slug>/storage is retained by default — destroying data
// is a separate operation.
func (s *Service) Delete(ctx context.Context, id domain.ID) error {
	if strings.TrimSpace(string(id)) == "" {
		return invalidf("id", "must not be empty")
	}
	// Resolve to lock the canonical ID, then delegate under the deploy lock
	// so a concurrent deploy cannot race the delete.
	p, err := s.fetchProject(ctx, "id = ?", string(id))
	if err != nil {
		return err
	}
	return s.withDeployLock(ctx, p.ID, func() error {
		// Re-check inside the lock in case the row disappeared between fetch
		// and lock acquisition.
		active, err := s.hasReleases(ctx, p.ID)
		if err != nil {
			return err
		}
		if active {
			if err := s.runner.Undeploy(ctx, UndeployInput{
				App:      p.Slug,
				Hostname: s.routeHostname(p),
			}); err != nil {
				return fmt.Errorf("%w: %w", ErrUndeployFailed, err)
			}
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("delete project transaction: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `DELETE FROM releases WHERE project_id = ?`, string(p.ID)); err != nil {
			return fmt.Errorf("delete releases: %w", err)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, string(p.ID))
		if err != nil {
			return fmt.Errorf("delete project: %w", err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrNotFound
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit delete: %w", err)
		}
		s.emit(ctx, "project.deleted", severityInfo, p.ID,
			fmt.Sprintf("project %q deleted", p.Slug),
			map[string]any{"slug": p.Slug})
		return nil
	})
}

// hasReleases reports whether any release row exists for projectID.
func (s *Service) hasReleases(ctx context.Context, projectID domain.ID) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM releases WHERE project_id = ?`, string(projectID)).Scan(&count); err != nil {
		return false, fmt.Errorf("count releases: %w", err)
	}
	return count > 0, nil
}

// HasReleases is the exported check for whether a project has releases.
func (s *Service) HasReleases(ctx context.Context, projectID domain.ID) (bool, error) {
	return s.hasReleases(ctx, projectID)
}

// UndeployProject tears down the runtime deployment for a project if it has releases.
// It is used by controlplane DeleteProject to undeploy before SCM/exposure teardown.
func (s *Service) UndeployProject(ctx context.Context, id domain.ID) error {
	p, err := s.fetchProject(ctx, "id = ?", string(id))
	if err != nil {
		return err
	}
	return s.withDeployLock(ctx, p.ID, func() error {
		active, err := s.hasReleases(ctx, p.ID)
		if err != nil {
			return err
		}
		if active {
			if err := s.runner.Undeploy(ctx, UndeployInput{
				App:      p.Slug,
				Hostname: s.routeHostname(p),
			}); err != nil {
				return fmt.Errorf("%w: %w", ErrUndeployFailed, err)
			}
		}
		return nil
	})
}

// DeleteProjectRecord deletes the project and its releases without undeploying.
// It is used after external teardown has succeeded.
func (s *Service) DeleteProjectRecord(ctx context.Context, id domain.ID) error {
	if strings.TrimSpace(string(id)) == "" {
		return invalidf("id", "must not be empty")
	}
	p, err := s.fetchProject(ctx, "id = ?", string(id))
	if err != nil {
		return err
	}
	return s.withDeployLock(ctx, p.ID, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("delete project transaction: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `DELETE FROM releases WHERE project_id = ?`, string(p.ID)); err != nil {
			return fmt.Errorf("delete releases: %w", err)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, string(p.ID))
		if err != nil {
			return fmt.Errorf("delete project: %w", err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrNotFound
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit delete: %w", err)
		}
		s.emit(ctx, "project.deleted", severityInfo, p.ID,
			fmt.Sprintf("project %q deleted", p.Slug),
			map[string]any{"slug": p.Slug})
		return nil
	})
}

func (s *Service) fetchProject(ctx context.Context, where, arg string) (*Project, error) {
	row := s.db.QueryRowContext(ctx, fmt.Sprintf(`
SELECT id, slug, name, repository_url, image_base, exposure, hostname,
       contract_port, contract_health_path, contract_storage_path,
       deploying, created_at, updated_at
FROM projects WHERE %s`, where), arg) //nolint:gosec
	p, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func scanProject(sc interface{ Scan(dest ...any) error }) (*Project, error) {
	var p Project
	var idStr, slug, name, repoURL, imageBase, exposureStr, hostname string
	var contractPort int
	var contractHealthPath, contractStoragePath string
	var deploying int
	var createdAtS, updatedAtS string
	if err := sc.Scan(&idStr, &slug, &name, &repoURL, &imageBase, &exposureStr, &hostname,
		&contractPort, &contractHealthPath, &contractStoragePath,
		&deploying, &createdAtS, &updatedAtS); err != nil {
		return nil, err
	}
	createdAt, err := parseTime(createdAtS)
	if err != nil {
		return nil, fmt.Errorf("parse project created_at: %w", err)
	}
	updatedAt, err := parseTime(updatedAtS)
	if err != nil {
		return nil, fmt.Errorf("parse project updated_at: %w", err)
	}
	p.ID = domain.ID(idStr)
	p.Slug = slug
	p.Name = name
	p.RepositoryURL = repoURL
	p.Image = imageBase
	p.Exposure = domain.Exposure(exposureStr)
	p.Hostname = hostname
	p.Contract = Contract{Port: contractPort, HealthPath: contractHealthPath, StoragePath: contractStoragePath}
	p.Deploying = deploying != 0
	p.CreatedAt = createdAt
	p.UpdatedAt = updatedAt
	return &p, nil
}

func (s *Service) routeHostname(p *Project) string {
	if strings.TrimSpace(p.Hostname) != "" {
		return p.Hostname
	}
	return p.Slug
}

func (s *Service) storageHostPath(slug string) string {
	return filepath.Join(s.cfg.DataDir, "projects", slug, "storage")
}

func (s *Service) secretsFilePath(slug string) string {
	return filepath.Join(s.cfg.SecretsDir, slug+".env")
}

func (s *Service) writeProjectSecretsFile(ctx context.Context, proj *Project) error {
	path := s.secretsFilePath(proj.Slug)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create secrets dir %s: %w", dir, err)
	}
	if s.secrets == nil {
		if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
			return fmt.Errorf("write empty secrets file %s: %w", path, err)
		}
		return nil
	}
	scope := "project:" + string(proj.ID)
	list, err := s.secrets.List(ctx)
	if err != nil {
		return fmt.Errorf("list project secrets: %w", err)
	}
	var lines []string
	for _, sec := range list {
		if sec.Scope != scope {
			continue
		}
		val, err := s.secrets.Reveal(ctx, sec.ID)
		if err != nil {
			continue
		}
		// Validate name already validated on Put; write as KEY=VAL
		lines = append(lines, sec.Name+"="+val)
	}
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write secrets file %s: %w", path, err)
	}
	return nil
}

func fmtTimeNow() domainTime {
	return time.Now().UTC()
}

// domainTime is an alias so fmtTimeNow reads clearly without importing time everywhere.
type domainTime = time.Time
