package apitypes

import (
	"context"
	"time"

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

// EnrollCompanionResponse is the one-time response for POST /api/v1/companion/enroll.
// It returns the device token and, when machine backups are enabled, per-device restic REST credentials.
type EnrollCompanionResponse struct {
	Token          string `json:"token"`
	TokenPrefix    string `json:"token_prefix"`
	ResticRepo     string `json:"restic_repo,omitempty"`
	ResticPassword string `json:"restic_password,omitempty"`
	RestUser       string `json:"rest_user,omitempty"`
	RestPassword   string `json:"rest_password,omitempty"`
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

// RecoveryKeyMaterial is the one-time response of recovery phrase generation.
// The phrase is shown once and never persisted; fingerprint identifies the
// recovery kit (first 8 hex of SHA-256(seed)).
type RecoveryKeyMaterial struct {
	Phrase      []string `json:"phrase"`
	Fingerprint string   `json:"fingerprint"`
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
// Two forms are accepted:
//   - Hetzner Storage Box (recommended): {kind:"hetzner_storagebox", username, host, sub_account_password}
//   - Generic/Advanced: {location, password}
type CreateBackupRepositoryRequest struct {
	Kind               string            `json:"kind,omitempty"`
	Username           string            `json:"username,omitempty"`
	Host               string            `json:"host,omitempty"`
	SubAccountPassword string            `json:"sub_account_password,omitempty"`
	Label              string            `json:"label"`
	Location           string            `json:"location"`
	Password           string            `json:"password"`
	Env                map[string]string `json:"env,omitempty"`
	// Phrase allows deriving the restic password from the recovery phrase
	// when configuring Hetzner without a stored seed; optional.
	Phrase string `json:"phrase,omitempty"`
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
	ProjectID          domain.ID `json:"project_id"`
	Title              string    `json:"title"`
	Instructions       string    `json:"instructions,omitempty"`
	Agent              string    `json:"agent,omitempty"`
	DevcontainerSource string    `json:"devcontainer_source,omitempty"`
	// Branch is deprecated: use Title. Kept for internal SkipBranchCreate tests.
	Branch string `json:"branch,omitempty"`
}

type SendWorkspaceRequest struct {
	Message string `json:"message"`
}

type CompanionCreateWorkspaceRequest struct {
	ProjectSlug  string `json:"project_slug"`
	Title        string `json:"title"`
	Instructions string `json:"instructions,omitempty"`
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

// BootstrapGate is the control-plane side of first-boot bootstrap.
type BootstrapGate interface {
	// Claim validates the one-time code; success consumes it.
	Claim(code, sourceIP string) error
	// SSHKeys installs keys for the admin user.
	SSHKeys(githubUser string, pastedKeys []string) (int, error)
	// TailscaleUp starts enrollment, returning the auth URL.
	TailscaleUp() (string, error)
	// TailscaleStatus polls enrollment state.
	TailscaleStatus() (running bool, ip string, state string, err error)
	// Complete writes bootstrap-done and closes the listener.
	Complete() error
	// Active reports whether bootstrap is still pending.
	Active() bool
	// RestoreConnect verifies Hetzner/generic repo + phrase, uploads SSH key,
	// and lists snapshots. Returns up to 10 latest.
	RestoreConnect(ctx context.Context, req BootstrapRestoreConnectRequest) ([]BootstrapRestoreSnapshot, error)
	// RestoreRun starts the restore of snapshotID in background.
	RestoreRun(ctx context.Context, snapshotID string) error
	// RestoreEvents streams progress events for the running restore.
	RestoreEvents(ctx context.Context) <-chan BootstrapRestoreEvent
}

// BootstrapRestoreConnectRequest is the body for POST /api/bootstrap/restore/connect.
type BootstrapRestoreConnectRequest struct {
	Kind               string `json:"kind,omitempty"`
	Username           string `json:"username,omitempty"`
	Host               string `json:"host,omitempty"`
	SubAccountPassword string `json:"sub_account_password,omitempty"`
	Location           string `json:"location,omitempty"`
	Phrase             string `json:"phrase,omitempty"`
	PhraseWords        []string `json:"phrase_words,omitempty"`
}

// BootstrapRestoreSnapshot is one snapshot returned by restore/connect.
type BootstrapRestoreSnapshot struct {
	ID       string `json:"id"`
	Time     string `json:"time"`
	Hostname string `json:"hostname"`
}

// BootstrapRestoreRunRequest is the body for POST /api/bootstrap/restore/run.
type BootstrapRestoreRunRequest struct {
	SnapshotID string `json:"snapshot_id"`
}

// BootstrapRestoreEvent is one SSE event during restore.
type BootstrapRestoreEvent struct {
	Stage   string `json:"stage"`
	Message string `json:"message"`
	Done    bool   `json:"done"`
	Error   string `json:"error,omitempty"`
}

// HealthReport aliases for convenience.
type HealthReport = health.Report

// Identity types aliases to avoid direct imports in handlers.
type EnrollmentState = identity.EnrollmentState
type AppAccess = identity.AppAccess
type Group = identity.Group

// Knowledge aliases
type Citation = knowledge.Citation
type PaperlessMetadata = knowledge.PaperlessMetadata
type Source = knowledge.Source
type IndexSetupOption = knowledge.IndexSetupOption
type ModelInfo = knowledge.ModelInfo

// SCM aliases
type PullRequestEvent = scm.PullRequestEvent
type PushEvent = scm.PushEvent
