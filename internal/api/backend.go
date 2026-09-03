package api

import (
	"context"
	"time"

	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/health"
	"github.com/omahab/omahab/internal/identity"
	"github.com/omahab/omahab/internal/knowledge"
	"github.com/omahab/omahab/internal/scm"
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
// Value and secret_id are never returned; managed_by distinguishes API-key vs subscription.
type ProviderCredential struct {
	ID          domain.ID  `json:"id"`
	Provider    string     `json:"provider"`
	Name        string     `json:"name"`
	Kind        string     `json:"kind"`
	Status      string     `json:"status"`
	Configured  bool       `json:"configured"`
	ManagedBy   string     `json:"managed_by"`
	ExternalRef *string    `json:"external_ref,omitempty"`
	Entitlement *string    `json:"entitlement,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// ModelAlias is a stable omahab/* alias routed via LiteLLM.
type ModelAlias struct {
	Name          string    `json:"name"`
	CredentialID  domain.ID `json:"credential_id"`
	Model         string    `json:"model"`
	FallbackOrder []string  `json:"fallback_order,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// SetModelAliasRequest is the body for PUT /api/v1/model-aliases/{name}.
type SetModelAliasRequest struct {
	CredentialID  string   `json:"credential_id"`
	Model         string   `json:"model"`
	FallbackOrder []string `json:"fallback_order,omitempty"`
}

// ModelKey is metadata for a scoped LiteLLM virtual key (plaintext never returned except once on create).
type ModelKey struct {
	ID          domain.ID  `json:"id"`
	Name        string     `json:"name"`
	KeyPrefix   string     `json:"key_prefix"`
	OwnerKind   string     `json:"owner_kind"`
	OwnerID     string     `json:"owner_id"`
	Scopes      []string   `json:"scopes"`
	RPM         *int       `json:"rpm,omitempty"`
	TPM         *int       `json:"tpm,omitempty"`
	Concurrency *int       `json:"concurrency,omitempty"`
	Budget      *float64   `json:"budget,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

// CreateModelKeyRequest is the body for POST /api/v1/model-keys.
type CreateModelKeyRequest struct {
	Name        string     `json:"name"`
	OwnerKind   string     `json:"owner_kind"`
	OwnerID     string     `json:"owner_id"`
	Scopes      []string   `json:"scopes,omitempty"`
	RPM         *int       `json:"rpm,omitempty"`
	TPM         *int       `json:"tpm,omitempty"`
	Concurrency *int       `json:"concurrency,omitempty"`
	Budget      *float64   `json:"budget,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

// OAuthSession is the safe OAuth session returned to clients; never includes device codes, tokens, or master key.
type OAuthSession struct {
	ID              string    `json:"id"`
	Provider        string    `json:"provider"`
	Flow            string    `json:"flow"`
	VerificationURL string    `json:"verification_url"`
	UserCode        *string   `json:"user_code,omitempty"`
	CallbackPort    *int      `json:"callback_port,omitempty"`
	ExpiresAt       time.Time `json:"expires_at"`
	Status          string    `json:"status"`
}

// StartProviderOAuthRequest is the body for POST /api/v1/provider-oauth/{provider}/start.
type StartProviderOAuthRequest struct {
	Flow string `json:"flow"`
}

// ForwardProviderOAuthCallbackRequest is the body for POST /api/v1/provider-oauth/{provider}/callback/{session_id}.
type ForwardProviderOAuthCallbackRequest struct {
	CallbackPath string `json:"callback_path"`
}

// CompanionDevice is a placeholder for Phase 6 enrollment (device record).
// TODO Phase 6: implement full companion device lifecycle.
type CompanionDevice struct {
	ID                 domain.ID `json:"id"`
	Name               string    `json:"name"`
	AllowProviderOAuth bool      `json:"allow_provider_oauth"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// CompanionEnrollment is a placeholder for Phase 6 single-use enrollment codes.
// TODO Phase 6: implement hashing, expiry, and consumption.
type CompanionEnrollment struct {
	ID        domain.ID `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

// ToolEnvEntry is metadata for a tool-environment variable (no value).
type ToolEnvEntry struct {
	Name      string    `json:"name"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PutToolEnvRequest is the body for PUT /api/v1/tool-environment/{NAME}
type PutToolEnvRequest struct {
	Value string `json:"value"`
}

// ToolEnvListResponse is the response for GET /api/v1/tool-environment
type ToolEnvListResponse struct {
	Items []ToolEnvEntry `json:"items"`
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

// VerifyCloudflareTokenResult reports a live token check.
type VerifyCloudflareTokenResult struct {
	OK     bool   `json:"ok"`
	Status string `json:"status,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// RecoveryKeyMaterial is the one-time response of recovery key generation.
// PrivateKey is never persisted server-side.
type RecoveryKeyMaterial struct {
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key"`
	Kit        string `json:"kit"`
}

// Disk is one candidate storage filesystem.
type Disk struct {
	Name       string `json:"name"`
	Size       string `json:"size"`
	Type       string `json:"type"`
	FSType     string `json:"fstype"`
	UUID       string `json:"uuid"`
	Mountpoint string `json:"mountpoint,omitempty"`
}

// ConfigureStorageRequest assigns a filesystem to a volume role.
type ConfigureStorageRequest struct {
	Volume string `json:"volume"` // "media" | "data"
	FSUUID string `json:"fs_uuid"`
}

// CreateBackupRepositoryRequest carries repository credentials to the
// secrets store (value material bridges to SecretRef server-side).
type CreateBackupRepositoryRequest struct {
	Label    string            `json:"label"`
	Location string            `json:"location"`
	Password string            `json:"password"`
	Env      map[string]string `json:"env,omitempty"`
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
	// Runtime is "systemd" (native NixOS service; auto-installed and
	// versioned by the system image) or "compose".
	Runtime string `json:"runtime,omitempty"`
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

// UserEnrollmentFields documents the additional fields returned on user creation when Pocket ID is configured.
// The domain.User payload now carries enrollment_url, enrollment_expires_at and pocket_user_id.
type UserEnrollmentFields struct {
	EnrollmentURL       *string    `json:"enrollment_url,omitempty"`
	EnrollmentExpiresAt *time.Time `json:"enrollment_expires_at,omitempty"`
	PocketUserID        string     `json:"pocket_user_id,omitempty"`
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
type Backend interface {
	// Status / instance / doctor
	GetStatus(ctx context.Context) (domain.Status, error)
	GetInstance(ctx context.Context) (domain.Instance, error)
	UpdateInstance(ctx context.Context, domain string, assistantName string) (domain.Instance, error)
	GetDoctor(ctx context.Context) (*health.Report, error)
	// Applications
	ListApplications(ctx context.Context, p Pagination) ([]domain.Application, error)
	InstallApplication(ctx context.Context, req InstallApplicationRequest) (domain.Application, error)
	ListCatalog(ctx context.Context) ([]CatalogBundle, error)

	// First-run wizard surfaces.
	VerifyCloudflareToken(ctx context.Context, token string) (VerifyCloudflareTokenResult, error)
	GenerateRecoveryKey(ctx context.Context) (RecoveryKeyMaterial, error)
	ConfirmRecoveryKey(ctx context.Context, publicKey string) error
	ListDisks(ctx context.Context) ([]Disk, error)
	ConfigureStorage(ctx context.Context, req ConfigureStorageRequest) error
	ListBackupRepositories(ctx context.Context) ([]backups.Repository, error)
	CreateBackupRepository(ctx context.Context, req CreateBackupRepositoryRequest) (backups.Repository, error)
	DeleteBackupRepository(ctx context.Context, id string) error
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
	IssueUserEnrollment(ctx context.Context, id domain.ID) (domain.User, error)

	// Provider credentials (metadata only)
	ListProviderCredentials(ctx context.Context, p Pagination) ([]ProviderCredential, error)
	GetProviderCredential(ctx context.Context, id domain.ID) (ProviderCredential, error)
	CreateProviderCredential(ctx context.Context, req CreateProviderCredentialRequest) (ProviderCredential, error)
	DeleteProviderCredential(ctx context.Context, id domain.ID) error

	// Model gateway (LiteLLM) — provider/alias/virtual-key wiring
	ListModelAliases(ctx context.Context) ([]ModelAlias, error)
	SetModelAlias(ctx context.Context, name string, req SetModelAliasRequest) (ModelAlias, error)
	ListModelKeys(ctx context.Context, p Pagination) ([]ModelKey, error)
	// CreateModelKey returns the created key metadata plus plaintext token once.
	// The plaintext token is returned in the `key` field of the response envelope and never persisted.
	CreateModelKey(ctx context.Context, req CreateModelKeyRequest) (ModelKey, string, error)
	DeleteModelKey(ctx context.Context, id domain.ID) error

	// Provider OAuth (subscription) — safe session, no secrets
	StartProviderOAuth(ctx context.Context, provider string, req StartProviderOAuthRequest) (OAuthSession, error)
	PollProviderOAuth(ctx context.Context, provider, sessionID string) (OAuthSession, error)
	// ForwardProviderOAuthCallback forwards only the /callback?<query> path to LiteLLM's fixed loopback 127.0.0.1:56121.
	// Only device-authenticated companions with allow_provider_oauth=true may call; admin bearer is rejected.
	ForwardProviderOAuthCallback(ctx context.Context, provider, sessionID string, req ForwardProviderOAuthCallbackRequest) (OAuthSession, error)

	// Companion / enrollment (Phase 6 stubs) — TODO: implement full lifecycle
	ListCompanionDevices(ctx context.Context, p Pagination) ([]CompanionDevice, error)
	CreateCompanionEnrollment(ctx context.Context) (CompanionEnrollment, string, error)
	EnrollCompanion(ctx context.Context, code string) (string, error)
	RevokeCompanionDevice(ctx context.Context, id domain.ID) error
	SetDeviceAllowOAuth(ctx context.Context, id domain.ID, allow bool) (CompanionDevice, error)
	// GetCompanionEnvironment is device-authenticated and returns raw values with ETag; admin bearer rejected.
	GetCompanionEnvironment(ctx context.Context, deviceToken string) (map[string]string, string, error)
	// Tool environment (server authoritative singleton agent-tools)
	ListToolEnvironments(ctx context.Context) ([]ToolEnvEntry, error)
	PutToolEnvironment(ctx context.Context, name, value string) (ToolEnvEntry, error)
	DeleteToolEnvironment(ctx context.Context, name string) error
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

	// Setup aggregates first-run provisioning state.
	GetSetupStatus(ctx context.Context) (SetupStatus, error)
	TriggerSetupReconcile(ctx context.Context) (bool, error)
	SetupWoodpecker(ctx context.Context, req SetupWoodpeckerRequest) error

	// Email routing gated on verification
	EnsureEmailRoute(ctx context.Context, recipient string) error

	// SCM webhooks (Forgejo pull_request/push, HMAC-verified)
	OnPullRequest(ctx context.Context, ev scm.PullRequestEvent) error
	OnPush(ctx context.Context, ev scm.PushEvent) error
	ForgejoWebhookSecret(ctx context.Context) (string, error)
}

// SetupWoodpeckerRequest is the body for PUT /api/v1/setup/woodpecker.
// Token is never returned and must not be logged.
type SetupWoodpeckerRequest struct {
	Username string `json:"username"`
	Token    string `json:"token"`
}

// SetupStatus is the aggregated first-run setup checklist state.
// State is one of waiting_for_cloudflare, reconciling, attention, complete.
type SetupStatus struct {
	State  string       `json:"state"`
	Checks []SetupCheck `json:"checks"`
}

// SetupCheck is one checklist entry. Status is ok|pending|failed|skipped.
type SetupCheck struct {
	ID           string           `json:"id"`
	Label        string           `json:"label"`
	Owner        string           `json:"owner"`
	Status       string           `json:"status"`
	Detail       string           `json:"detail,omitempty"`
	Action       string           `json:"action,omitempty"`
	Apps         []SetupAppStatus `json:"apps,omitempty"`
	PasskeyCount *int             `json:"passkey_count,omitempty"`
	Target       *int             `json:"target,omitempty"`
}

// SetupAppStatus tracks one default bundle in the core_apps check.
type SetupAppStatus struct {
	BundleID string `json:"bundle_id"`
	Status   string `json:"status"`
	Detail   string `json:"detail,omitempty"`
}
