package api

import (
	"context"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// Pagination controls list endpoints.
type Pagination struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

// EventFilter filters event listings.
type EventFilter struct {
	Type     string
	Severity string
	Unread   *bool
}

// ExposureState represents the inspectable exposure record.
type ExposureState struct {
	ResourceType string          `json:"resource_type"`
	ResourceID   domain.ID       `json:"resource_id"`
	Hostname     string          `json:"hostname"`
	Exposure     domain.Exposure `json:"exposure"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// ProviderCredential represents a model provider credential (metadata only).
type ProviderCredential struct {
	ID          domain.ID  `json:"id"`
	Provider    string     `json:"provider"`
	Name        string     `json:"name"`
	Kind        string     `json:"kind"`
	Status      string     `json:"status"`
	Configured  bool       `json:"configured"`
	Entitlement *string    `json:"entitlement,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// RecoverySession is returned from identity recovery endpoints.
type RecoverySession struct {
	ExpiresAt time.Time `json:"expires_at"`
	LoginURL  *string   `json:"login_url,omitempty"`
	Code      *string   `json:"code,omitempty"`
}

// Create / Update request payloads. They deliberately reject unknown fields via DisallowUnknownFields.

type CreateProjectRequest struct {
	Slug          string          `json:"slug"`
	Name          string          `json:"name"`
	RepositoryURL string          `json:"repository_url"`
	Exposure      domain.Exposure `json:"exposure,omitempty"`
	Hostname      string          `json:"hostname,omitempty"`
}

type UpdateProjectRequest struct {
	Name     *string          `json:"name,omitempty"`
	Exposure *domain.Exposure `json:"exposure,omitempty"`
	Hostname *string          `json:"hostname,omitempty"`
}

type CreateReleaseRequest struct {
	Commit string `json:"commit"`
	Digest string `json:"digest"`
}

type UpdateApplicationRequest struct {
	Exposure     *domain.Exposure `json:"exposure,omitempty"`
	DesiredState *string          `json:"desired_state,omitempty"`
}

type ApplicationActionRequest struct {
	Action string `json:"action"`
}

// InstallApplicationRequest installs a curated bundle. The name defaults to
// the bundle id; non-private exposure requires a hostname.
type InstallApplicationRequest struct {
	BundleID string          `json:"bundle_id"`
	Name     string          `json:"name,omitempty"`
	Hostname string          `json:"hostname,omitempty"`
	Exposure domain.Exposure `json:"exposure,omitempty"`
}

// CatalogBundle is the installable view of one curated bundle. Compose
// templates and digest pinning stay server-side.
type CatalogBundle struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Image           string          `json:"image"`
	Architectures   []string        `json:"architectures"`
	DefaultExposure domain.Exposure `json:"default_exposure"`
	MaxExposure     domain.Exposure `json:"max_exposure"`
	MemoryMB        int             `json:"memory_mb,omitempty"`
	Installed       bool            `json:"installed"`
}

type CreateSecretRequest struct {
	Scope string `json:"scope"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

type UpdateSecretRequest struct {
	Value string `json:"value"`
}

type CreateBackupRequest struct {
	Repository string `json:"repository"`
}

type UpdateExposureRequest struct {
	Exposure domain.Exposure `json:"exposure"`
	// Confirmation is required when exposure=public: the caller must type
	// the hostname being made public. Enforced against the stored hostname.
	Confirmation string `json:"confirmation,omitempty"`
}

type CreateSyncFolderRequest struct {
	Name        string `json:"name"`
	ServerPath  string `json:"server_path"`
	ShareWithAI bool   `json:"share_with_ai"`
}

type UpdateSyncFolderRequest struct {
	Name        *string `json:"name,omitempty"`
	ShareWithAI *bool   `json:"share_with_ai,omitempty"`
}

type CreateWorkspaceRequest struct {
	ProjectID domain.ID `json:"project_id"`
	Branch    string    `json:"branch"`
	Agent     string    `json:"agent"`
}

type CreateUserRequest struct {
	Email  string   `json:"email"`
	Name   string   `json:"name"`
	Groups []string `json:"groups,omitempty"`
}

type UpdateUserRequest struct {
	Name     *string   `json:"name,omitempty"`
	Groups   *[]string `json:"groups,omitempty"`
	Disabled *bool     `json:"disabled,omitempty"`
}

type CreateProviderCredentialRequest struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Value    string `json:"value"`
}

type EmailIngestRequest struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Timestamp string `json:"timestamp"`
	Nonce     string `json:"nonce"`
	Raw       []byte `json:"-"`
	RawSize   int    `json:"rawSize"`
	Signature string `json:"-"`
}

// Backend is the explicit control-plane interface consumed by the HTTP layer.
// Every dashboard operation has a corresponding method; implementations remain
// free to compose separate controllers behind this surface.
type Backend interface {
	// Status / instance
	GetStatus(ctx context.Context) (domain.Status, error)
	GetInstance(ctx context.Context) (domain.Instance, error)

	// Applications
	ListApplications(ctx context.Context, p Pagination) ([]domain.Application, error)
	InstallApplication(ctx context.Context, req InstallApplicationRequest) (domain.Application, error)
	ListCatalog(ctx context.Context) ([]CatalogBundle, error)
	GetApplication(ctx context.Context, id domain.ID) (domain.Application, error)
	UpdateApplication(ctx context.Context, id domain.ID, req UpdateApplicationRequest) (domain.Application, error)
	DoApplicationAction(ctx context.Context, id domain.ID, action string) (domain.Application, error)

	// Exposure (inspectable / reversible)
	GetExposure(ctx context.Context, resourceType string, id domain.ID) (ExposureState, error)
	ListExposure(ctx context.Context) ([]ExposureState, error)
	UpdateExposure(ctx context.Context, resourceType string, id domain.ID, exposure domain.Exposure) (ExposureState, error)

	// Projects
	ListProjects(ctx context.Context, p Pagination) ([]domain.Project, error)
	GetProject(ctx context.Context, id domain.ID) (domain.Project, error)
	CreateProject(ctx context.Context, req CreateProjectRequest) (domain.Project, error)
	UpdateProject(ctx context.Context, id domain.ID, req UpdateProjectRequest) (domain.Project, error)
	DeleteProject(ctx context.Context, id domain.ID) error

	// Releases
	ListReleases(ctx context.Context, projectID domain.ID, p Pagination) ([]domain.Release, error)
	GetRelease(ctx context.Context, projectID domain.ID, releaseID domain.ID) (domain.Release, error)
	CreateRelease(ctx context.Context, projectID domain.ID, req CreateReleaseRequest) (domain.Release, error)
	RollbackRelease(ctx context.Context, projectID domain.ID, releaseID domain.ID) (domain.Release, error)

	// Secrets (metadata only on reads)
	ListSecrets(ctx context.Context, scope string, p Pagination) ([]domain.Secret, error)
	GetSecret(ctx context.Context, id domain.ID) (domain.Secret, error)
	CreateSecret(ctx context.Context, req CreateSecretRequest) (domain.Secret, error)
	UpdateSecret(ctx context.Context, id domain.ID, req UpdateSecretRequest) (domain.Secret, error)
	DeleteSecret(ctx context.Context, id domain.ID) error

	// Backups
	ListBackups(ctx context.Context, p Pagination) ([]domain.Backup, error)
	GetBackup(ctx context.Context, id domain.ID) (domain.Backup, error)
	CreateBackup(ctx context.Context, req CreateBackupRequest) (domain.Backup, error)
	RestoreBackup(ctx context.Context, id domain.ID) (domain.Backup, error)
	VerifyBackup(ctx context.Context, id domain.ID) (domain.Backup, error)

	// Events
	ListEvents(ctx context.Context, p Pagination, filter EventFilter) ([]domain.Event, error)
	GetEvent(ctx context.Context, id domain.ID) (domain.Event, error)
	MarkEventRead(ctx context.Context, id domain.ID) (domain.Event, error)
	MarkAllEventsRead(ctx context.Context) error
	StreamEvents(ctx context.Context, since domain.ID, out chan<- domain.Event) error

	// Sync folders
	ListSyncFolders(ctx context.Context, p Pagination) ([]domain.SyncFolder, error)
	GetSyncFolder(ctx context.Context, id domain.ID) (domain.SyncFolder, error)
	CreateSyncFolder(ctx context.Context, req CreateSyncFolderRequest) (domain.SyncFolder, error)
	UpdateSyncFolder(ctx context.Context, id domain.ID, req UpdateSyncFolderRequest) (domain.SyncFolder, error)
	DeleteSyncFolder(ctx context.Context, id domain.ID) error

	// Workspaces
	ListWorkspaces(ctx context.Context, p Pagination) ([]domain.Workspace, error)
	GetWorkspace(ctx context.Context, id domain.ID) (domain.Workspace, error)
	CreateWorkspace(ctx context.Context, req CreateWorkspaceRequest) (domain.Workspace, error)
	StopWorkspace(ctx context.Context, id domain.ID) (domain.Workspace, error)
	DeleteWorkspace(ctx context.Context, id domain.ID) error

	// Users / identity
	ListUsers(ctx context.Context, p Pagination) ([]domain.User, error)
	GetUser(ctx context.Context, id domain.ID) (domain.User, error)
	CreateUser(ctx context.Context, req CreateUserRequest) (domain.User, error)
	UpdateUser(ctx context.Context, id domain.ID, req UpdateUserRequest) (domain.User, error)
	DeleteUser(ctx context.Context, id domain.ID) error
	CreateRecoverySession(ctx context.Context, email string) (RecoverySession, error)
	CreateUserRecoverySession(ctx context.Context, userID domain.ID) (RecoverySession, error)

	// Provider credentials (metadata only)
	ListProviderCredentials(ctx context.Context, p Pagination) ([]ProviderCredential, error)
	GetProviderCredential(ctx context.Context, id domain.ID) (ProviderCredential, error)
	CreateProviderCredential(ctx context.Context, req CreateProviderCredentialRequest) (ProviderCredential, error)
	DeleteProviderCredential(ctx context.Context, id domain.ID) error

	// Email ingestion
	IngestEmail(ctx context.Context, req EmailIngestRequest) (domain.EmailMessage, error)
	ListEmailMessages(ctx context.Context, p Pagination) ([]domain.EmailMessage, error)
	GetEmailMessage(ctx context.Context, id domain.ID) (domain.EmailMessage, error)
}
