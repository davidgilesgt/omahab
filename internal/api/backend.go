package api

import (
	"context"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/identity"
	"github.com/omahab/omahab/internal/knowledge"
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

// Release token response for admin-only endpoints.
type ReleaseTokenResponse struct {
	Token       string `json:"token"`
	TokenPrefix string `json:"token_prefix"`
}

// MirrorConfigRequest for push-mirror configuration (repo-scoped credential).
type ConfigureMirrorRequest struct {
	RemoteURL string `json:"remote_url"`
	Token     string `json:"token"`
	LFS       bool   `json:"lfs,omitempty"`
}

type MirrorResponse struct {
	RemoteURL string   `json:"remote_url"`
	SecretRef string   `json:"secret_ref,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
	LFS       bool     `json:"lfs"`
}

type WorkspaceCapabilityResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type KnowledgeSearchRequest struct {
	Query     string `json:"query"`
	Principal string `json:"principal,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type KnowledgeUploadRequest struct {
	Filename  string   `json:"filename"`
	Content   []byte   `json:"content"`
	Tags      []string `json:"tags,omitempty"`
	Principal string   `json:"principal,omitempty"`
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

	// Release tokens (admin only; never exposed to Forgejo)
	IssueReleaseToken(ctx context.Context, projectID domain.ID) (ReleaseTokenResponse, error)
	RotateReleaseToken(ctx context.Context, projectID domain.ID) (ReleaseTokenResponse, error)
	ReleaseWithToken(ctx context.Context, projectID domain.ID, token, commit, digest string) (domain.Release, error)

	// Push mirror (repo-scoped credential, force-push warning, LFS)
	GetPushMirror(ctx context.Context, projectID domain.ID) (MirrorResponse, error)
	ConfigurePushMirror(ctx context.Context, projectID domain.ID, req ConfigureMirrorRequest) (MirrorResponse, error)
	RemovePushMirror(ctx context.Context, projectID domain.ID) error

	// Workspace capabilities (short-lived one-time token)
	IssueWorkspaceCapability(ctx context.Context, workspaceID string) (WorkspaceCapabilityResponse, error)
	ValidateWorkspaceCapability(ctx context.Context, workspaceID, token string) error

	// Knowledge assistant tools (with Paperless permission checks)
	KnowledgeSearch(ctx context.Context, principal, query string, limit int) ([]knowledge.Citation, error)
	KnowledgeGetMetadata(ctx context.Context, principal, docID string) (*knowledge.PaperlessMetadata, error)
	KnowledgeGetText(ctx context.Context, principal, docID string) (string, error)
	KnowledgeListCorrespondents(ctx context.Context, principal string) ([]string, error)
	KnowledgeListDocumentTypes(ctx context.Context, principal string) ([]string, error)
	KnowledgeListTags(ctx context.Context, principal string) ([]string, error)
	KnowledgeUpload(ctx context.Context, principal, filename string, content []byte, tags []string) (string, error)
	KnowledgeAddTag(ctx context.Context, principal, docID, tag string) error
	KnowledgeListSources(ctx context.Context) ([]*knowledge.Source, error)
	KnowledgeIndexSetupOptions(ctx context.Context) ([]knowledge.IndexSetupOption, error)
	KnowledgePinnedModels(ctx context.Context) ([]knowledge.ModelInfo, error)
	KnowledgeGetSummarizationConsent(ctx context.Context, principal, provider string) (bool, error)
	KnowledgeSetSummarizationConsent(ctx context.Context, principal, provider string, granted bool) error

	// Identity extended (enrollment, app access, groups)
	GetEnrollmentState(ctx context.Context, userID string) (identity.EnrollmentState, error)
	ListApplicationAccess(ctx context.Context, userID string) ([]identity.AppAccess, error)
	GetUserGroups(ctx context.Context, userID string) ([]identity.Group, error)
	SetUserGroups(ctx context.Context, userID string, groupIDs []string) error

	// Email routing gated on verification
	EnsureEmailRoute(ctx context.Context, recipient string) error
}
