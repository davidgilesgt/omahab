package providers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// Sentinel errors preserving validation / not-found / conflict distinctions.
var (
	ErrNotFound                  = errors.New("provider: not found")
	ErrAlreadyExists             = errors.New("provider: already exists")
	ErrValidation                = errors.New("provider: validation failed")
	ErrUnsupportedCredentialType = errors.New("provider: unsupported credential type")
	ErrEntitlement               = errors.New("provider: entitlement failure")
	ErrTokenInvalid              = errors.New("provider: token invalid or corrupted")
	ErrVirtualKeyInvalid         = errors.New("provider: virtual key invalid")
	ErrVirtualKeyExpired         = errors.New("provider: virtual key expired")
	ErrVirtualKeyRevoked         = errors.New("provider: virtual key revoked")
)

// Provider constants — only these are sanctioned.
const (
	ProviderOpenAI     = "openai"
	ProviderAnthropic  = "anthropic"
	ProviderOpenRouter = "openrouter"
	ProviderChatGPT    = "chatgpt"
	ProviderXAI        = "xai"
)

// Credential types — only sanctioned types are accepted.
const (
	CredentialTypeAPIKey = "api_key"
	CredentialTypeOAuth  = "oauth"
)

// Entitlement states (persisted in entitlement column, distinct from health/expiry).
const (
	EntitlementEntitled    = "entitled"
	EntitlementNotEntitled = "not_entitled"
	EntitlementUnknown     = "unknown"
)




// Alias names routed to applications via the gateway.
const (
	AliasFast      = "omahab/fast"
	AliasBalanced  = "omahab/balanced"
	AliasReasoning = "omahab/reasoning"
	AliasEmbedding = "omahab/embedding"
)


// ManagedBy values for provider_credentials.managed_by.
const (
	ManagedByOmahab  = "omahab"
	ManagedByLiteLLM = "litellm"
)

// Alias for compatibility with differing capitalization expectations.
const ManagedByLitellm = ManagedByLiteLLM


// ExternalRef values for provider_credentials.external_ref (litellm-managed).
const (
	ExternalRefChatGPT = "chatgpt"
	ExternalRefXAI     = "xai_oauth"
)

// OwnerKind values for provider_virtual_keys.owner_kind.
const (
	OwnerKindHermes  = "hermes"
	OwnerKindDevice  = "device"
	OwnerKindHarness = "harness"
)

var allowedProviders = map[string]bool{
	ProviderOpenAI:     true,
	ProviderAnthropic:  true,
	ProviderOpenRouter: true,
	ProviderChatGPT:    true,
	ProviderXAI:        true,
}

var allowedCredentialTypes = map[string]bool{
	CredentialTypeAPIKey: true,
	CredentialTypeOAuth:  true,
}

// allowedProviderCredentialType enforces which credential type each provider may use.
var allowedProviderCredentialType = map[string]map[string]bool{
	ProviderOpenAI:     {CredentialTypeAPIKey: true},
	ProviderAnthropic:  {CredentialTypeAPIKey: true},
	ProviderOpenRouter: {CredentialTypeAPIKey: true},
	ProviderChatGPT:    {CredentialTypeOAuth: true},
	ProviderXAI:        {CredentialTypeOAuth: true},
}

var allowedAliases = map[string]bool{
	AliasFast:      true,
	AliasBalanced:  true,
	AliasReasoning: true,
	AliasEmbedding: true,
}

// Rejected substrings for cookie/session exfiltration.
var rejectedCredentialSubstrings = []string{
	"cookie",
	"session",
	"browser",
}

var allowedEntitlements = map[string]bool{
	EntitlementEntitled:    true,
	EntitlementNotEntitled: true,
	EntitlementUnknown:     true,
}

var allowedManagedBy = map[string]bool{
	ManagedByOmahab:  true,
	ManagedByLiteLLM: true,
}

var allowedExternalRef = map[string]bool{
	ExternalRefChatGPT: true,
	ExternalRefXAI:     true,
}

var allowedOwnerKind = map[string]bool{
	OwnerKindHermes:  true,
	OwnerKindDevice:  true,
	OwnerKindHarness: true,
}

// SupportedProviders returns the sanctioned provider list.
func SupportedProviders() []string {
	return []string{ProviderOpenAI, ProviderAnthropic, ProviderOpenRouter, ProviderChatGPT, ProviderXAI}
}

// SupportedAliases returns the routed alias list.
func SupportedAliases() []string {
	return []string{AliasFast, AliasBalanced, AliasReasoning, AliasEmbedding}
}

// IsEntitlementError reports whether err is an entitlement (403) failure distinct from token corruption.
func IsEntitlementError(err error) bool { return errors.Is(err, ErrEntitlement) }

// IsTokenInvalidError reports whether err is a token-corruption / auth failure.
func IsTokenInvalidError(err error) bool { return errors.Is(err, ErrTokenInvalid) }

// ClassifyHTTPStatus maps an upstream HTTP status to the distinct entitlement vs token errors.
// 403 => ErrEntitlement (subscription tier / quota), 401 => ErrTokenInvalid.
func ClassifyHTTPStatus(statusCode int) error {
	switch statusCode {
	case 403:
		return ErrEntitlement
	case 401:
		return ErrTokenInvalid
	default:
		return nil
	}
}

// EventSink receives normalized control-plane events.
type EventSink interface {
	Emit(ctx context.Context, event domain.Event) error
}

type noopSink struct{}

func (noopSink) Emit(_ context.Context, _ domain.Event) error { return nil }

// DeviceAuthorization is the OAuth device-flow challenge returned to the dashboard.
type DeviceAuthorization struct {
	Provider                string        `json:"provider"`
	DeviceCode              string        `json:"-"` // never serialized to clients; only verification data is shown
	UserCode                string        `json:"user_code"`
	VerificationURI         string        `json:"verification_uri"`
	VerificationURIComplete string        `json:"verification_uri_complete,omitempty"`
	ExpiresAt               time.Time     `json:"expires_at"`
	Interval                time.Duration `json:"interval"`
}

// OAuthToken is the result of a successful device-flow poll or refresh.
// No token value is persisted in the providers tables; it is projected via the secrets broker.
type OAuthToken struct {
	AccessToken  string    `json:"-"` // never logged or persisted in provider tables
	RefreshToken string    `json:"-"` // never logged or persisted in provider tables
	ExpiresAt    time.Time `json:"expires_at"`
	TokenType    string    `json:"token_type"`
}

// OAuthClient abstracts sanctioned OAuth device-flow and token lifecycle for
// ChatGPT and xAI. Implementations must not use browser-cookie extraction.
type OAuthClient interface {
	StartDeviceFlow(ctx context.Context, provider string) (*DeviceAuthorization, error)
	PollDeviceFlow(ctx context.Context, provider, deviceCode string) (*OAuthToken, error)
	RefreshToken(ctx context.Context, provider string, credentialID domain.ID) (*OAuthToken, error)
	RevokeToken(ctx context.Context, provider string, credentialID domain.ID) error
}

// NoopOAuthClient is a no-op OAuthClient for testing / when OAuth is not configured.
type NoopOAuthClient struct{}

func (NoopOAuthClient) StartDeviceFlow(_ context.Context, provider string) (*DeviceAuthorization, error) {
	return &DeviceAuthorization{
		Provider:        provider,
		DeviceCode:      "noop-device-code",
		UserCode:        "NOOP-CODE",
		VerificationURI: "https://example.com/activate",
		ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
		Interval:        5 * time.Second,
	}, nil
}
func (NoopOAuthClient) PollDeviceFlow(_ context.Context, _ string, _ string) (*OAuthToken, error) {
	return nil, fmt.Errorf("%w: device flow not configured", ErrValidation)
}
func (NoopOAuthClient) RefreshToken(_ context.Context, _ string, _ domain.ID) (*OAuthToken, error) {
	return nil, fmt.Errorf("%w: refresh not configured", ErrValidation)
}
func (NoopOAuthClient) RevokeToken(_ context.Context, _ string, _ domain.ID) error { return nil }

// Credential is the persisted metadata for a provider credential.
// Secret material is never stored here; SecretID references the secrets broker
// for omahab-managed rows. For litellm-managed subscription rows, SecretID is
// empty and ExternalRef identifies the upstream OAuth linkage.
type Credential struct {
	ID                 domain.ID     `json:"id"`
	Provider           string        `json:"provider"`
	CredentialType     string        `json:"credential_type"`
	DisplayName        string        `json:"display_name,omitempty"`
	SecretID           domain.ID     `json:"secret_id,omitempty"`
	ManagedBy          string        `json:"managed_by"`
	ExternalRef        *string       `json:"external_ref,omitempty"`
	Entitlement        string        `json:"entitlement"`
	EntitlementMessage string        `json:"entitlement_message,omitempty"`
	ExpiresAt          *time.Time    `json:"expires_at,omitempty"`
	Health             domain.Health `json:"health"`
	LastCheckedAt      *time.Time    `json:"last_checked_at,omitempty"`
	CreatedAt          time.Time     `json:"created_at"`
	UpdatedAt          time.Time     `json:"updated_at"`
}

// Alias maps an omahab/* alias to a concrete credential and upstream model.
type Alias struct {
	Name         string    `json:"name"`
	CredentialID domain.ID `json:"credential_id"`
	Model        string    `json:"model"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// VirtualKey is the persisted metadata for a scoped virtual key.
// The plaintext token is never persisted; only its hash is stored.
// Deprecated: ValidateVirtualKey is audit-only; LiteLLM is authoritative for request authentication.
// This struct retains hash/prefix for audit correlation, plus gateway linkage and ownership metadata.
type VirtualKey struct {
	ID               domain.ID  `json:"id"`
	Name             string     `json:"name"`
	KeyPrefix        string     `json:"key_prefix"`
	Scopes           []string   `json:"scopes"`
	GatewayKeyID     *string    `json:"gateway_key_id,omitempty"`
	OwnerKind        *string    `json:"owner_kind,omitempty"`
	OwnerID          *string    `json:"owner_id,omitempty"`
	RPMLimit         *int       `json:"rpm_limit,omitempty"`
	TPMLimit         *int       `json:"tpm_limit,omitempty"`
	ConcurrencyLimit *int       `json:"concurrency_limit,omitempty"`
	BudgetAmount     *float64   `json:"budget_amount,omitempty"`
	BudgetDuration   *string    `json:"budget_duration,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	RevokedAt        *time.Time `json:"revoked_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// VirtualKeyWithToken is returned once when a virtual key is issued.
// Token is the only time the plaintext is available.
type VirtualKeyWithToken struct {
	VirtualKey *VirtualKey `json:"virtual_key"`
	Token      string      `json:"token"`
}

// CreateCredentialInput holds fields for credential creation.
type CreateCredentialInput struct {
	ID             domain.ID `json:"id,omitempty"`
	Provider       string
	CredentialType string
	DisplayName    string
	SecretID       domain.ID
	ManagedBy      string
	ExternalRef    *string
	ExpiresAt      *time.Time
}

// UpdateCredentialInput holds mutable fields.
type UpdateCredentialInput struct {
	DisplayName        *string
	SecretID           *domain.ID
	Entitlement        *string
	EntitlementMessage *string
	ExpiresAt          *time.Time
	Health             *domain.Health
}

// SetAliasInput holds fields for alias upsert.
type SetAliasInput struct {
	Name         string
	CredentialID domain.ID
	Model        string
}

// IssueVirtualKeyInput holds fields for virtual key issuance.
type IssueVirtualKeyInput struct {
	Name             string
	Scopes           []string
	ExpiresAt        *time.Time
	OwnerKind        *string
	OwnerID          *string
	RPMLimit         *int
	TPMLimit         *int
	ConcurrencyLimit *int
	BudgetAmount     *float64
	BudgetDuration   *string
}

// virtualKeyGateway is the narrow gateway contract needed by Service.IssueVirtualKey
// to store the LiteLLM-side key ID. The full GatewayAdmin (Health, ReconcileModels,
// IssueVirtualKey, RevokeVirtualKey, StartOAuth, PollOAuth, ForwardOAuthCallback,
// ProbeModel) is defined in litellm.go; litellmGateway implements this narrow
// interface as well so Service can call it without a package-level duplicate.
type virtualKeyGateway interface {
	IssueVirtualKey(ctx context.Context, vk VirtualKey) (string, error)
	RevokeVirtualKey(ctx context.Context, gatewayKeyID string) error
}

// Service brokers provider credential metadata, aliases, and scoped virtual
// keys. Secret material never transits this service: SecretID references the
// secrets broker, and virtual-key tokens are persisted only as hashes.
type Service struct {
	db        *sql.DB
	oauth     OAuthClient
	sink      EventSink
	now       func() time.Time
	vkGateway virtualKeyGateway
}

// New creates a Service. oauth and sink may be nil.
func New(db *sql.DB, oauth OAuthClient) *Service {
	if oauth == nil {
		oauth = NoopOAuthClient{}
	}
	return &Service{db: db, oauth: oauth, sink: noopSink{}, now: func() time.Time { return time.Now().UTC() }}
}

// NewWithSink creates a Service with an explicit EventSink.
func NewWithSink(db *sql.DB, oauth OAuthClient, sink EventSink) *Service {
	if oauth == nil {
		oauth = NoopOAuthClient{}
	}
	if sink == nil {
		sink = noopSink{}
	}
	return &Service{db: db, oauth: oauth, sink: sink, now: func() time.Time { return time.Now().UTC() }}
}

// SetVirtualKeyGateway injects the gateway used to issue/revoke virtual keys
// in LiteLLM. The full GatewayAdmin in litellm.go satisfies this narrow interface.
func (s *Service) SetVirtualKeyGateway(gw virtualKeyGateway) {
	s.vkGateway = gw
}

// SupportedProvidersForService returns the providers this service instance supports.
func (s *Service) SupportedProviders() []string { return SupportedProviders() }

// SupportedAliasesForService returns the aliases this service instance supports.
func (s *Service) SupportedAliases() []string { return SupportedAliases() }

// nowUTC returns current UTC time via injected clock.
func (s *Service) nowUTC() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

// --- Credential methods ---

// CreateCredential validates inputs and persists credential metadata.
// Provider tokens are never stored or returned; only SecretID is persisted for
// omahab-managed rows. For litellm-managed subscription rows, ExternalRef must
// be set and SecretID must be empty.
func (s *Service) CreateCredential(ctx context.Context, in CreateCredentialInput) (*Credential, error) {
	provider := strings.TrimSpace(strings.ToLower(in.Provider))
	credType := strings.TrimSpace(strings.ToLower(in.CredentialType))
	displayName := strings.TrimSpace(in.DisplayName)
	secretID := domain.ID(strings.TrimSpace(string(in.SecretID)))
	managedBy := strings.TrimSpace(strings.ToLower(in.ManagedBy))
	if managedBy == "" {
		managedBy = ManagedByOmahab
	}
	var externalRef *string
	if in.ExternalRef != nil {
		v := strings.TrimSpace(*in.ExternalRef)
		if v != "" {
			externalRef = &v
		}
	}

	if provider == "" {
		return nil, fmt.Errorf("%w: provider is required", ErrValidation)
	}
	if !allowedProviders[provider] {
		return nil, fmt.Errorf("%w: unsupported provider %q", ErrValidation, provider)
	}
	if credType == "" {
		return nil, fmt.Errorf("%w: credential_type is required", ErrValidation)
	}
	if err := validateCredentialType(credType); err != nil {
		return nil, err
	}
	// Enforce allowed provider+type combinations.
	if m, ok := allowedProviderCredentialType[provider]; !ok || !m[credType] {
		return nil, fmt.Errorf("%w: credential type %q not allowed for provider %q", ErrValidation, credType, provider)
	}
	if !allowedManagedBy[managedBy] {
		return nil, fmt.Errorf("%w: unsupported managed_by %q", ErrValidation, managedBy)
	}
	// Enforce credential_type <-> managed_by mapping: api_key is omahab, oauth is litellm.
	if credType == CredentialTypeAPIKey && managedBy != ManagedByOmahab {
		return nil, fmt.Errorf("%w: credential type %q must be managed_by=%q", ErrValidation, credType, ManagedByOmahab)
	}
	if credType == CredentialTypeOAuth && managedBy != ManagedByLiteLLM {
		return nil, fmt.Errorf("%w: credential type %q must be managed_by=%q", ErrValidation, credType, ManagedByLiteLLM)
	}
	// Enforce managed_by / secret_id / external_ref correlation.
	switch managedBy {
	case ManagedByOmahab:
		if strings.TrimSpace(string(secretID)) == "" {
			return nil, fmt.Errorf("%w: secret_id is required for managed_by=omahab", ErrValidation)
		}
		if strings.Contains(string(secretID), "\x00") {
			return nil, fmt.Errorf("%w: secret_id contains NUL", ErrValidation)
		}
		if externalRef != nil {
			return nil, fmt.Errorf("%w: external_ref must be null for managed_by=omahab", ErrValidation)
		}
	case ManagedByLiteLLM:
		if strings.TrimSpace(string(secretID)) != "" {
			return nil, fmt.Errorf("%w: secret_id must be empty for managed_by=litellm", ErrValidation)
		}
		if externalRef == nil || *externalRef == "" {
			return nil, fmt.Errorf("%w: external_ref is required for managed_by=litellm", ErrValidation)
		}
		er := strings.TrimSpace(strings.ToLower(*externalRef))
		if !allowedExternalRef[er] {
			return nil, fmt.Errorf("%w: unsupported external_ref %q", ErrValidation, *externalRef)
		}
		// Normalize and enforce provider <-> external_ref mapping.
		switch provider {
		case ProviderChatGPT:
			if er != ExternalRefChatGPT {
				return nil, fmt.Errorf("%w: provider %q requires external_ref %q", ErrValidation, provider, ExternalRefChatGPT)
			}
		case ProviderXAI:
			if er != ExternalRefXAI {
				return nil, fmt.Errorf("%w: provider %q requires external_ref %q", ErrValidation, provider, ExternalRefXAI)
			}
		default:
			// For non-subscription providers, litellm-managed is not expected; already enforced via credType mapping,
			// but if they reach here, ensure external_ref is still valid.
		}
		*externalRef = er
	default:
		return nil, fmt.Errorf("%w: unsupported managed_by %q", ErrValidation, managedBy)
	}
	id := domain.ID(strings.TrimSpace(string(in.ID)))
	if id == "" {
		id = domain.ID(newID())
	}
	if strings.Contains(string(id), "\x00") {
		return nil, fmt.Errorf("%w: id contains NUL", ErrValidation)
	}
	now := s.nowUTC()
	cred := &Credential{
		ID:             id,
		Provider:       provider,
		CredentialType: credType,
		DisplayName:    displayName,
		SecretID:       secretID,
		ManagedBy:      managedBy,
		ExternalRef:    externalRef,
		Entitlement:    EntitlementUnknown,
		Health:         domain.HealthUnknown,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if in.ExpiresAt != nil {
		t := in.ExpiresAt.UTC()
		cred.ExpiresAt = &t
	}
	var secretIDVal any
	if managedBy == ManagedByOmahab {
		secretIDVal = string(cred.SecretID)
	} else {
		secretIDVal = nil
	}
	var externalRefVal any
	if externalRef != nil {
		externalRefVal = *externalRef
	} else {
		externalRefVal = nil
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO provider_credentials (id, provider, credential_type, display_name, secret_id, managed_by, external_ref, entitlement, entitlement_message, expires_at, health, last_checked_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(cred.ID), cred.Provider, cred.CredentialType, cred.DisplayName, secretIDVal, cred.ManagedBy, externalRefVal,
		cred.Entitlement, cred.EntitlementMessage, timePtrToString(cred.ExpiresAt), string(cred.Health),
		nil, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrAlreadyExists
		}
		return nil, fmt.Errorf("insert credential: %w", err)
	}

	_ = s.sink.Emit(ctx, domain.Event{
		ID:         domain.ID(newID()),
		Type:       "provider.credential.created",
		Severity:   "info",
		ResourceID: cred.ID,
		Message:    fmt.Sprintf("provider credential created: %s/%s", provider, credType),
		Data:       map[string]any{"provider": provider, "credential_type": credType, "managed_by": managedBy},
		CreatedAt:  now,
	})

	return cred, nil
}

// GetCredential returns credential metadata by ID.
func (s *Service) GetCredential(ctx context.Context, id domain.ID) (*Credential, error) {
	if strings.TrimSpace(string(id)) == "" {
		return nil, fmt.Errorf("%w: id is required", ErrValidation)
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT id, provider, credential_type, display_name, secret_id, managed_by, external_ref, entitlement, entitlement_message, expires_at, health, last_checked_at, created_at, updated_at
		 FROM provider_credentials WHERE id = ?`, string(id))
	return scanCredential(row)
}

// ListCredentials returns all credentials ordered by provider and creation time.
func (s *Service) ListCredentials(ctx context.Context) ([]*Credential, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, provider, credential_type, display_name, secret_id, managed_by, external_ref, entitlement, entitlement_message, expires_at, health, last_checked_at, created_at, updated_at
		 FROM provider_credentials ORDER BY provider ASC, created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	defer rows.Close()
	var out []*Credential
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListCredentialsByProvider returns credentials for a provider.
func (s *Service) ListCredentialsByProvider(ctx context.Context, provider string) ([]*Credential, error) {
	provider = strings.TrimSpace(strings.ToLower(provider))
	if !allowedProviders[provider] {
		return nil, fmt.Errorf("%w: unsupported provider %q", ErrValidation, provider)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, provider, credential_type, display_name, secret_id, managed_by, external_ref, entitlement, entitlement_message, expires_at, health, last_checked_at, created_at, updated_at
		 FROM provider_credentials WHERE provider = ? ORDER BY created_at ASC`, provider)
	if err != nil {
		return nil, fmt.Errorf("list credentials by provider: %w", err)
	}
	defer rows.Close()
	var out []*Credential
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateCredential mutates mutable metadata. SecretID rotation is atomic.
// ManagedBy and ExternalRef are immutable after creation.
func (s *Service) UpdateCredential(ctx context.Context, id domain.ID, in UpdateCredentialInput) (*Credential, error) {
	existing, err := s.GetCredential(ctx, id)
	if err != nil {
		return nil, err
	}

	if in.DisplayName != nil {
		existing.DisplayName = strings.TrimSpace(*in.DisplayName)
	}
	if in.SecretID != nil {
		if existing.ManagedBy == ManagedByLiteLLM {
			return nil, fmt.Errorf("%w: secret_id cannot be set for litellm-managed credential", ErrValidation)
		}
		sid := domain.ID(strings.TrimSpace(string(*in.SecretID)))
		if strings.TrimSpace(string(sid)) == "" {
			return nil, fmt.Errorf("%w: secret_id is required", ErrValidation)
		}
		if strings.Contains(string(sid), "\x00") {
			return nil, fmt.Errorf("%w: secret_id contains NUL", ErrValidation)
		}
		existing.SecretID = sid
	}
	if in.Entitlement != nil {
		ent := strings.TrimSpace(strings.ToLower(*in.Entitlement))
		if !allowedEntitlements[ent] {
			return nil, fmt.Errorf("%w: invalid entitlement %q", ErrValidation, ent)
		}
		existing.Entitlement = ent
	}
	if in.EntitlementMessage != nil {
		existing.EntitlementMessage = strings.TrimSpace(*in.EntitlementMessage)
	}
	if in.ExpiresAt != nil {
		t := in.ExpiresAt.UTC()
		existing.ExpiresAt = &t
	}
	if in.Health != nil {
		switch *in.Health {
		case domain.HealthUnknown, domain.HealthHealthy, domain.HealthDegraded, domain.HealthUnhealthy:
			existing.Health = *in.Health
		default:
			return nil, fmt.Errorf("%w: invalid health %q", ErrValidation, *in.Health)
		}
	}

	now := s.nowUTC()
	existing.UpdatedAt = now

	var secretIDVal any
	if existing.ManagedBy == ManagedByOmahab {
		secretIDVal = string(existing.SecretID)
	} else {
		secretIDVal = nil
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE provider_credentials SET display_name = ?, secret_id = ?, entitlement = ?, entitlement_message = ?, expires_at = ?, health = ?, updated_at = ? WHERE id = ?`,
		existing.DisplayName, secretIDVal, existing.Entitlement, existing.EntitlementMessage,
		timePtrToString(existing.ExpiresAt), string(existing.Health), now.Format(time.RFC3339Nano), string(id),
	)
	if err != nil {
		return nil, fmt.Errorf("update credential: %w", err)
	}
	return existing, nil
}

// DeleteCredential removes credential metadata. Underlying secret must be revoked via secrets broker separately.
func (s *Service) DeleteCredential(ctx context.Context, id domain.ID) error {
	if strings.TrimSpace(string(id)) == "" {
		return fmt.Errorf("%w: id is required", ErrValidation)
	}
	// Ensure exists for not-found distinction.
	if _, err := s.GetCredential(ctx, id); err != nil {
		return err
	}
	// Check alias references to prevent orphaned routing.
	var aliasCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM provider_aliases WHERE credential_id = ?`, string(id)).Scan(&aliasCount); err != nil {
		return fmt.Errorf("check alias references: %w", err)
	}
	if aliasCount > 0 {
		return fmt.Errorf("%w: credential is referenced by %d alias(es)", ErrValidation, aliasCount)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM provider_credentials WHERE id = ?`, string(id))
	if err != nil {
		return fmt.Errorf("delete credential: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	_ = s.sink.Emit(ctx, domain.Event{
		ID:         domain.ID(newID()),
		Type:       "provider.credential.deleted",
		Severity:   "info",
		ResourceID: id,
		Message:    "provider credential deleted",
		CreatedAt:  s.nowUTC(),
	})
	return nil
}

// RefreshCredential refreshes an OAuth credential via the OAuthClient.
// It updates expiry and health on success, and classifies entitlement vs token errors distinctly.
func (s *Service) RefreshCredential(ctx context.Context, id domain.ID) error {
	cred, err := s.GetCredential(ctx, id)
	if err != nil {
		return err
	}
	if cred.CredentialType != CredentialTypeOAuth {
		return fmt.Errorf("%w: refresh only valid for oauth credentials", ErrValidation)
	}
	token, err := s.oauth.RefreshToken(ctx, cred.Provider, cred.ID)
	if err != nil {
		// Do not store token material; only map error classification to health/entitlement.
		if errors.Is(err, ErrEntitlement) {
			_ = s.UpdateHealth(ctx, cred.ID, domain.HealthDegraded, EntitlementNotEntitled, err.Error())
			return ErrEntitlement
		}
		if errors.Is(err, ErrTokenInvalid) {
			_ = s.UpdateHealth(ctx, cred.ID, domain.HealthUnhealthy, EntitlementUnknown, "token invalid")
			return ErrTokenInvalid
		}
		// Generic failure -> degraded health, entitlement unknown (not entitlement).
		_ = s.UpdateHealth(ctx, cred.ID, domain.HealthDegraded, EntitlementUnknown, err.Error())
		return fmt.Errorf("refresh credential: %w", err)
	}
	// Success: update expiry and health; never persist token values.
	now := s.nowUTC()
	cred.ExpiresAt = &token.ExpiresAt
	cred.Health = domain.HealthHealthy
	cred.Entitlement = EntitlementEntitled
	_, err = s.db.ExecContext(ctx,
		`UPDATE provider_credentials SET expires_at = ?, health = ?, entitlement = ?, entitlement_message = ?, last_checked_at = ?, updated_at = ? WHERE id = ?`,
		token.ExpiresAt.Format(time.RFC3339Nano), string(domain.HealthHealthy), EntitlementEntitled, "", now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), string(id),
	)
	if err != nil {
		return fmt.Errorf("update credential after refresh: %w", err)
	}
	_ = s.sink.Emit(ctx, domain.Event{
		ID:         domain.ID(newID()),
		Type:       "provider.credential.refreshed",
		Severity:   "info",
		ResourceID: id,
		Message:    fmt.Sprintf("provider credential refreshed: %s", cred.Provider),
		Data:       map[string]any{"provider": cred.Provider},
		CreatedAt:  now,
	})
	return nil
}

// RevokeCredential revokes an OAuth credential via the OAuthClient and deletes metadata.
func (s *Service) RevokeCredential(ctx context.Context, id domain.ID) error {
	cred, err := s.GetCredential(ctx, id)
	if err != nil {
		return err
	}
	if err := s.oauth.RevokeToken(ctx, cred.Provider, cred.ID); err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	// Remove alias references that would dangle (or require caller to delete first).
	// We enforce DeleteCredential's reference check, so revoke via Delete.
	// For OAuth revoke we first remove aliases that point to this credential.
	_, _ = s.db.ExecContext(ctx, `DELETE FROM provider_aliases WHERE credential_id = ?`, string(id))
	if err := s.DeleteCredential(ctx, id); err != nil {
		// If Delete fails due to not found, treat as success (already revoked).
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	_ = s.sink.Emit(ctx, domain.Event{
		ID:         domain.ID(newID()),
		Type:       "provider.credential.revoked",
		Severity:   "info",
		ResourceID: id,
		Message:    fmt.Sprintf("provider credential revoked: %s", cred.Provider),
		Data:       map[string]any{"provider": cred.Provider},
		CreatedAt:  s.nowUTC(),
	})
	return nil
}

// UpdateHealth sets health and entitlement separately. Entitlement failures (403) are not conflated with token corruption.
func (s *Service) UpdateHealth(ctx context.Context, id domain.ID, health domain.Health, entitlement, message string) error {
	if strings.TrimSpace(string(id)) == "" {
		return fmt.Errorf("%w: id is required", ErrValidation)
	}
	if _, err := s.GetCredential(ctx, id); err != nil {
		return err
	}
	switch health {
	case domain.HealthUnknown, domain.HealthHealthy, domain.HealthDegraded, domain.HealthUnhealthy:
	default:
		return fmt.Errorf("%w: invalid health %q", ErrValidation, health)
	}
	entitlement = strings.TrimSpace(strings.ToLower(entitlement))
	if entitlement == "" {
		entitlement = EntitlementUnknown
	}
	if !allowedEntitlements[entitlement] {
		return fmt.Errorf("%w: invalid entitlement %q", ErrValidation, entitlement)
	}
	now := s.nowUTC()
	_, err := s.db.ExecContext(ctx,
		`UPDATE provider_credentials SET health = ?, entitlement = ?, entitlement_message = ?, last_checked_at = ?, updated_at = ? WHERE id = ?`,
		string(health), entitlement, strings.TrimSpace(message), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), string(id),
	)
	if err != nil {
		return fmt.Errorf("update health: %w", err)
	}
	return nil
}

// ReportEntitlementFailure marks a credential as not_entitled (403) without marking token corrupted.
func (s *Service) ReportEntitlementFailure(ctx context.Context, id domain.ID, message string) error {
	return s.UpdateHealth(ctx, id, domain.HealthDegraded, EntitlementNotEntitled, message)
}

// ReportTokenInvalid marks a credential as token-invalid (401), distinct from entitlement.
func (s *Service) ReportTokenInvalid(ctx context.Context, id domain.ID, message string) error {
	return s.UpdateHealth(ctx, id, domain.HealthUnhealthy, EntitlementUnknown, message)
}

// --- OAuth device flow ---

// StartDeviceFlow initiates a sanctioned OAuth device authorization for ChatGPT or xAI.
// Browser-cookie flows are rejected.
func (s *Service) StartDeviceFlow(ctx context.Context, provider string) (*DeviceAuthorization, error) {
	provider = strings.TrimSpace(strings.ToLower(provider))
	if !allowedProviders[provider] {
		return nil, fmt.Errorf("%w: unsupported provider %q", ErrValidation, provider)
	}
	if provider != ProviderChatGPT && provider != ProviderXAI {
		return nil, fmt.Errorf("%w: device flow only supported for chatgpt and xai providers: %q", ErrValidation, provider)
	}
	return s.oauth.StartDeviceFlow(ctx, provider)
}

// PollDeviceFlow polls for device-flow completion. On success the caller should persist the
// credential via CreateCredential with a secrets-broker secret_id. No token is stored here.
func (s *Service) PollDeviceFlow(ctx context.Context, provider, deviceCode string) (*OAuthToken, error) {
	provider = strings.TrimSpace(strings.ToLower(provider))
	if !allowedProviders[provider] {
		return nil, fmt.Errorf("%w: unsupported provider %q", ErrValidation, provider)
	}
	if strings.TrimSpace(deviceCode) == "" {
		return nil, fmt.Errorf("%w: device_code is required", ErrValidation)
	}
	token, err := s.oauth.PollDeviceFlow(ctx, provider, deviceCode)
	if err != nil {
		if errors.Is(err, ErrEntitlement) || errors.Is(err, ErrTokenInvalid) {
			return nil, err
		}
		return nil, fmt.Errorf("poll device flow: %w", err)
	}
	return token, nil
}

// ClassifyProviderError is the Service variant that classifies an HTTP status for callers that
// need to distinguish entitlement (403) from token corruption (401).
func (s *Service) ClassifyProviderError(_ context.Context, statusCode int) error {
	return ClassifyHTTPStatus(statusCode)
}

// --- Alias methods ---

// SetAlias creates or updates an alias routing. Only omahab/* aliases are allowed.
func (s *Service) SetAlias(ctx context.Context, in SetAliasInput) (*Alias, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: alias name is required", ErrValidation)
	}
	if !allowedAliases[name] {
		return nil, fmt.Errorf("%w: unsupported alias %q", ErrValidation, name)
	}
	if strings.TrimSpace(string(in.CredentialID)) == "" {
		return nil, fmt.Errorf("%w: credential_id is required", ErrValidation)
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		return nil, fmt.Errorf("%w: model is required", ErrValidation)
	}
	if strings.Contains(model, "\x00") || strings.Contains(name, "\x00") {
		return nil, fmt.Errorf("%w: NUL byte not allowed", ErrValidation)
	}
	// Ensure credential exists.
	if _, err := s.GetCredential(ctx, in.CredentialID); err != nil {
		return nil, err
	}

	now := s.nowUTC()
	nowStr := now.Format(time.RFC3339Nano)

	// Upsert.
	var exists bool
	var existingModel string
	err := s.db.QueryRowContext(ctx, `SELECT model FROM provider_aliases WHERE name = ?`, name).Scan(&existingModel)
	if err == nil {
		exists = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check alias: %w", err)
	}

	if exists {
		_, err = s.db.ExecContext(ctx,
			`UPDATE provider_aliases SET credential_id = ?, model = ?, updated_at = ? WHERE name = ?`,
			string(in.CredentialID), model, nowStr, name,
		)
		if err != nil {
			return nil, fmt.Errorf("update alias: %w", err)
		}
	} else {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO provider_aliases (name, credential_id, model, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
			name, string(in.CredentialID), model, nowStr, nowStr,
		)
		if err != nil {
			return nil, fmt.Errorf("insert alias: %w", err)
		}
	}

	_ = s.sink.Emit(ctx, domain.Event{
		ID:        domain.ID(newID()),
		Type:      "provider.alias.set",
		Severity:  "info",
		Message:   fmt.Sprintf("provider alias %s -> %s", name, model),
		Data:      map[string]any{"alias": name, "model": model, "credential_id": string(in.CredentialID)},
		CreatedAt: now,
	})

	// Fetch created/updated row.
	return s.GetAlias(ctx, name)
}

// GetAlias returns an alias by name.
func (s *Service) GetAlias(ctx context.Context, name string) (*Alias, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: alias name is required", ErrValidation)
	}
	var (
		aliasName, credentialID, model string
		createdAt, updatedAt           string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT name, credential_id, model, created_at, updated_at FROM provider_aliases WHERE name = ?`, name).
		Scan(&aliasName, &credentialID, &model, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get alias: %w", err)
	}
	ca, _ := time.Parse(time.RFC3339Nano, createdAt)
	ua, _ := time.Parse(time.RFC3339Nano, updatedAt)
	return &Alias{
		Name:         aliasName,
		CredentialID: domain.ID(credentialID),
		Model:        model,
		CreatedAt:    ca,
		UpdatedAt:    ua,
	}, nil
}

// ListAliases returns all aliases ordered by name.
func (s *Service) ListAliases(ctx context.Context) ([]*Alias, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, credential_id, model, created_at, updated_at FROM provider_aliases ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list aliases: %w", err)
	}
	defer rows.Close()
	var out []*Alias
	for rows.Next() {
		var (
			name, credentialID, model string
			createdAt, updatedAt      string
		)
		if err := rows.Scan(&name, &credentialID, &model, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan alias: %w", err)
		}
		ca, _ := time.Parse(time.RFC3339Nano, createdAt)
		ua, _ := time.Parse(time.RFC3339Nano, updatedAt)
		out = append(out, &Alias{
			Name:         name,
			CredentialID: domain.ID(credentialID),
			Model:        model,
			CreatedAt:    ca,
			UpdatedAt:    ua,
		})
	}
	return out, rows.Err()
}

// DeleteAlias removes an alias.
func (s *Service) DeleteAlias(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: alias name is required", ErrValidation)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM provider_aliases WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete alias: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	_ = s.sink.Emit(ctx, domain.Event{
		ID:        domain.ID(newID()),
		Type:      "provider.alias.deleted",
		Severity:  "info",
		Message:   fmt.Sprintf("provider alias deleted: %s", name),
		Data:      map[string]any{"alias": name},
		CreatedAt: s.nowUTC(),
	})
	return nil
}

// ResolveAlias resolves an alias to its credential metadata and model. No secrets are returned.
func (s *Service) ResolveAlias(ctx context.Context, name string) (*Credential, string, error) {
	alias, err := s.GetAlias(ctx, name)
	if err != nil {
		return nil, "", err
	}
	cred, err := s.GetCredential(ctx, alias.CredentialID)
	if err != nil {
		return nil, "", err
	}
	return cred, alias.Model, nil
}

// --- Virtual key methods ---

// IssueVirtualKey generates a scoped virtual key. The plaintext token is returned once;
// only its SHA256 hash is persisted. Scopes must be known aliases when non-empty.
// Owner and limit fields are optional but validated when present.
// If a virtualKeyGateway is configured, the key is first issued in the gateway (LiteLLM)
// and the returned gateway_key_id is persisted; if the gateway fails, no row is persisted.
func (s *Service) IssueVirtualKey(ctx context.Context, in IssueVirtualKeyInput) (*VirtualKeyWithToken, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrValidation)
	}
	if len(name) > 128 {
		return nil, fmt.Errorf("%w: name too long", ErrValidation)
	}
	if strings.Contains(name, "\x00") {
		return nil, fmt.Errorf("%w: name contains NUL", ErrValidation)
	}
	scopes := normalizeScopes(in.Scopes)
	for _, sc := range scopes {
		if !allowedAliases[sc] {
			return nil, fmt.Errorf("%w: unsupported scope %q", ErrValidation, sc)
		}
	}
	if in.ExpiresAt != nil {
		if in.ExpiresAt.UTC().Before(s.nowUTC()) {
			return nil, fmt.Errorf("%w: expires_at is in the past", ErrValidation)
		}
	}
	// Validate owner fields.
	var ownerKind *string
	if in.OwnerKind != nil {
		raw := strings.TrimSpace(strings.ToLower(*in.OwnerKind))
		if raw != "" {
			if !allowedOwnerKind[raw] {
				return nil, fmt.Errorf("%w: unsupported owner_kind %q", ErrValidation, *in.OwnerKind)
			}
			ownerKind = &raw
		}
	}
	var ownerID *string
	if in.OwnerID != nil {
		raw := strings.TrimSpace(*in.OwnerID)
		if raw != "" {
			if strings.Contains(raw, "\x00") {
				return nil, fmt.Errorf("%w: owner_id contains NUL", ErrValidation)
			}
			if len(raw) > 128 {
				return nil, fmt.Errorf("%w: owner_id too long", ErrValidation)
			}
			ownerID = &raw
		}
	}
	if ownerKind != nil && ownerID == nil {
		return nil, fmt.Errorf("%w: owner_id is required when owner_kind is set", ErrValidation)
	}
	if ownerID != nil && ownerKind == nil {
		return nil, fmt.Errorf("%w: owner_kind is required when owner_id is set", ErrValidation)
	}
	if in.RPMLimit != nil {
		if *in.RPMLimit <= 0 {
			return nil, fmt.Errorf("%w: rpm_limit must be > 0", ErrValidation)
		}
	}
	if in.TPMLimit != nil {
		if *in.TPMLimit <= 0 {
			return nil, fmt.Errorf("%w: tpm_limit must be > 0", ErrValidation)
		}
	}
	if in.ConcurrencyLimit != nil {
		if *in.ConcurrencyLimit <= 0 {
			return nil, fmt.Errorf("%w: concurrency_limit must be > 0", ErrValidation)
		}
	}
	if in.BudgetAmount != nil {
		if *in.BudgetAmount <= 0 {
			return nil, fmt.Errorf("%w: budget_amount must be > 0", ErrValidation)
		}
	}
	if in.BudgetDuration != nil {
		bd := strings.TrimSpace(*in.BudgetDuration)
		if bd == "" {
			return nil, fmt.Errorf("%w: budget_duration cannot be empty", ErrValidation)
		}
		if strings.Contains(bd, "\x00") || strings.Contains(bd, "\n") || strings.Contains(bd, "\r") {
			return nil, fmt.Errorf("%w: budget_duration contains invalid character", ErrValidation)
		}
		in.BudgetDuration = &bd
	}
	if in.BudgetAmount != nil && in.BudgetDuration == nil {
		return nil, fmt.Errorf("%w: budget_duration is required when budget_amount is set", ErrValidation)
	}
	if in.BudgetDuration != nil && in.BudgetAmount == nil {
		return nil, fmt.Errorf("%w: budget_amount is required when budget_duration is set", ErrValidation)
	}

	// Generate 32 random bytes, hex-encoded, prefixed for identification.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate virtual key: %w", err)
	}
	// Token format: omk_<64 hex chars>  (no prompt content, no provider token)
	token := "omk_" + hex.EncodeToString(raw)
	hash := hashToken(token)
	prefix := token[:12] // "omk_" + 8 hex for prefix identification
	id := domain.ID(newID())
	now := s.nowUTC()
	scopesStr := strings.Join(scopes, ",")

	var expiresAtStr *string
	if in.ExpiresAt != nil {
		v := in.ExpiresAt.UTC().Format(time.RFC3339Nano)
		expiresAtStr = &v
	}

	// If gateway is configured, issue there first to obtain gateway_key_id.
	var gatewayKeyID *string
	if s.vkGateway != nil {
		gwVK := VirtualKey{
			Name:             name,
			Scopes:           scopes,
			OwnerKind:        ownerKind,
			OwnerID:          ownerID,
			RPMLimit:         in.RPMLimit,
			TPMLimit:         in.TPMLimit,
			ConcurrencyLimit: in.ConcurrencyLimit,
			BudgetAmount:     in.BudgetAmount,
			BudgetDuration:   in.BudgetDuration,
		}
		if in.ExpiresAt != nil {
			t := in.ExpiresAt.UTC()
			gwVK.ExpiresAt = &t
		}
		gid, err := s.vkGateway.IssueVirtualKey(ctx, gwVK)
		if err != nil {
			return nil, fmt.Errorf("issue virtual key via gateway: %w", err)
		}
		gidTrim := strings.TrimSpace(gid)
		if gidTrim != "" {
			gatewayKeyID = &gidTrim
		}
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO provider_virtual_keys (id, name, key_hash, key_prefix, scopes, gateway_key_id, owner_kind, owner_id, rpm_limit, tpm_limit, concurrency_limit, budget_amount, budget_duration, expires_at, revoked_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(id), name, hash, prefix, scopesStr,
		nullableString(gatewayKeyID),
		nullableString(ownerKind),
		nullableString(ownerID),
		nullableInt(in.RPMLimit),
		nullableInt(in.TPMLimit),
		nullableInt(in.ConcurrencyLimit),
		nullableFloat(in.BudgetAmount),
		nullableString(in.BudgetDuration),
		nullableString(expiresAtStr), nil, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: virtual key hash collision", ErrAlreadyExists)
		}
		// If we had issued in gateway, attempt to revoke to avoid orphan.
		if gatewayKeyID != nil && s.vkGateway != nil {
			_ = s.vkGateway.RevokeVirtualKey(ctx, *gatewayKeyID)
		}
		return nil, fmt.Errorf("insert virtual key: %w", err)
	}

	vk := &VirtualKey{
		ID:               id,
		Name:             name,
		KeyPrefix:        prefix,
		Scopes:           scopes,
		GatewayKeyID:     gatewayKeyID,
		OwnerKind:        ownerKind,
		OwnerID:          ownerID,
		RPMLimit:         in.RPMLimit,
		TPMLimit:         in.TPMLimit,
		ConcurrencyLimit: in.ConcurrencyLimit,
		BudgetAmount:     in.BudgetAmount,
		BudgetDuration:   in.BudgetDuration,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if in.ExpiresAt != nil {
		t := in.ExpiresAt.UTC()
		vk.ExpiresAt = &t
	}

	_ = s.sink.Emit(ctx, domain.Event{
		ID:         domain.ID(newID()),
		Type:       "provider.virtual_key.issued",
		Severity:   "info",
		ResourceID: id,
		Message:    fmt.Sprintf("virtual key issued: %s (%s)", name, prefix),
		Data:       map[string]any{"name": name, "key_prefix": prefix, "scopes": scopes},
		CreatedAt:  now,
	})

	return &VirtualKeyWithToken{VirtualKey: vk, Token: token}, nil
}

// ListVirtualKeys returns virtual-key metadata without hash/plaintext. Ordered by creation time.
func (s *Service) ListVirtualKeys(ctx context.Context) ([]*VirtualKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, key_prefix, scopes, gateway_key_id, owner_kind, owner_id, rpm_limit, tpm_limit, concurrency_limit, budget_amount, budget_duration, expires_at, revoked_at, created_at, updated_at FROM provider_virtual_keys ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list virtual keys: %w", err)
	}
	defer rows.Close()
	var out []*VirtualKey
	for rows.Next() {
		vk, err := scanVirtualKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, vk)
	}
	return out, rows.Err()
}

// GetVirtualKey returns a virtual key by ID (metadata only).
func (s *Service) GetVirtualKey(ctx context.Context, id domain.ID) (*VirtualKey, error) {
	if strings.TrimSpace(string(id)) == "" {
		return nil, fmt.Errorf("%w: id is required", ErrValidation)
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, key_prefix, scopes, gateway_key_id, owner_kind, owner_id, rpm_limit, tpm_limit, concurrency_limit, budget_amount, budget_duration, expires_at, revoked_at, created_at, updated_at FROM provider_virtual_keys WHERE id = ?`, string(id))
	vk, err := scanVirtualKey(row)
	if err != nil {
		return nil, err
	}
	return vk, nil
}

// RevokeVirtualKey marks a virtual key as revoked. Idempotent.
// If the key has a gateway_key_id and a gateway is configured, it also revokes in the gateway.
func (s *Service) RevokeVirtualKey(ctx context.Context, id domain.ID) error {
	if strings.TrimSpace(string(id)) == "" {
		return fmt.Errorf("%w: id is required", ErrValidation)
	}
	vk, err := s.GetVirtualKey(ctx, id)
	if err != nil {
		return err
	}
	now := s.nowUTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE provider_virtual_keys SET revoked_at = ?, updated_at = ? WHERE id = ? AND revoked_at IS NULL`,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), string(id))
	if err != nil {
		return fmt.Errorf("revoke virtual key: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Check existence for not-found distinction.
		var exists string
		err := s.db.QueryRowContext(ctx, `SELECT id FROM provider_virtual_keys WHERE id = ?`, string(id)).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("check virtual key: %w", err)
		}
		// Already revoked — still attempt gateway revoke if needed, but treat as success.
		if vk.GatewayKeyID != nil && s.vkGateway != nil {
			_ = s.vkGateway.RevokeVirtualKey(ctx, *vk.GatewayKeyID)
		}
		return nil
	}
	if vk.GatewayKeyID != nil && s.vkGateway != nil {
		if err := s.vkGateway.RevokeVirtualKey(ctx, *vk.GatewayKeyID); err != nil {
			// Gateway revoke failure should not mask local revoke success, but surface as warning.
			// We still consider the key revoked locally; caller can retry gateway revocation if needed.
			_ = s.sink.Emit(ctx, domain.Event{
				ID:         domain.ID(newID()),
				Type:       "provider.virtual_key.revoke_gateway_failed",
				Severity:   "warn",
				ResourceID: id,
				Message:    fmt.Sprintf("virtual key gateway revoke failed: %v", err),
				CreatedAt:  now,
			})
		}
	}
	_ = s.sink.Emit(ctx, domain.Event{
		ID:         domain.ID(newID()),
		Type:       "provider.virtual_key.revoked",
		Severity:   "info",
		ResourceID: id,
		Message:    "virtual key revoked",
		CreatedAt:  now,
	})
	return nil
}

// ValidateVirtualKey validates a presented virtual-key token (hash check, expiry, revocation).
// It does not mark the key as consumed; virtual keys are reusable until revoked/expired.
//
// Deprecated: This method is retained for audit correlation and tests only.
// LiteLLM is authoritative for request authentication; do NOT use this as the
// request auth path in production. Caller should authenticate via the gateway.
func (s *Service) ValidateVirtualKey(ctx context.Context, token string) (*VirtualKey, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrVirtualKeyInvalid
	}
	hash := hashToken(token)
	if hash == "" {
		return nil, ErrVirtualKeyInvalid
	}
	var (
		id, name, keyPrefix, scopesStr string
		gatewayKeyID, ownerKind, ownerID sql.NullString
		rpmLimit, tpmLimit, concurrencyLimit sql.NullInt64
		budgetAmount sql.NullFloat64
		budgetDuration sql.NullString
		expiresAt, revokedAt           sql.NullString
		createdAt, updatedAt           string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, key_prefix, scopes, gateway_key_id, owner_kind, owner_id, rpm_limit, tpm_limit, concurrency_limit, budget_amount, budget_duration, expires_at, revoked_at, created_at, updated_at FROM provider_virtual_keys WHERE key_hash = ?`, hash).
		Scan(&id, &name, &keyPrefix, &scopesStr, &gatewayKeyID, &ownerKind, &ownerID, &rpmLimit, &tpmLimit, &concurrencyLimit, &budgetAmount, &budgetDuration, &expiresAt, &revokedAt, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrVirtualKeyInvalid
		}
		return nil, fmt.Errorf("lookup virtual key: %w", err)
	}
	if revokedAt.Valid {
		return nil, ErrVirtualKeyRevoked
	}
	if expiresAt.Valid {
		exp, _ := time.Parse(time.RFC3339Nano, expiresAt.String)
		if s.nowUTC().After(exp) {
			return nil, ErrVirtualKeyExpired
		}
	}
	// Re-parse for VirtualKey struct.
	vk, err := scanVirtualKeyFromValuesExtended(id, name, keyPrefix, scopesStr, gatewayKeyID, ownerKind, ownerID, rpmLimit, tpmLimit, concurrencyLimit, budgetAmount, budgetDuration, expiresAt, revokedAt, createdAt, updatedAt)
	if err != nil {
		return nil, err
	}
	return vk, nil
}

// Allows checks whether a virtual key is allowed to use a given alias.
func (vk *VirtualKey) Allows(alias string) bool {
	if len(vk.Scopes) == 0 {
		return true // empty scopes means all aliases
	}
	for _, s := range vk.Scopes {
		if s == alias {
			return true
		}
	}
	return false
}

// --- helpers ---

func validateCredentialType(credType string) error {
	credType = strings.TrimSpace(strings.ToLower(credType))
	if allowedCredentialTypes[credType] {
		return nil
	}
	// Explicitly reject cookie/session variants with a distinct message; still ErrValidation.
	for _, sub := range rejectedCredentialSubstrings {
		if strings.Contains(credType, sub) {
			return fmt.Errorf("%w: credential type %q is not sanctioned (cookie/session extraction not allowed)", ErrValidation, credType)
		}
	}
	return fmt.Errorf("%w: unsupported credential type %q", ErrValidation, credType)
}

func normalizeScopes(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

type credScanner interface{ Scan(dest ...any) error }

func scanCredential(row credScanner) (*Credential, error) {
	var (
		id, provider, credType, displayName string
		secretID sql.NullString
		managedBy sql.NullString
		externalRef sql.NullString
		entitlement, entitlementMessage               string
		expiresAt, lastCheckedAt                      sql.NullString
		health, createdAt, updatedAt                  string
	)
	if err := row.Scan(&id, &provider, &credType, &displayName, &secretID, &managedBy, &externalRef, &entitlement, &entitlementMessage, &expiresAt, &health, &lastCheckedAt, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan credential: %w", err)
	}
	ca, _ := time.Parse(time.RFC3339Nano, createdAt)
	ua, _ := time.Parse(time.RFC3339Nano, updatedAt)
	c := &Credential{
		ID:                 domain.ID(id),
		Provider:           provider,
		CredentialType:     credType,
		DisplayName:        displayName,
		Entitlement:        entitlement,
		EntitlementMessage: entitlementMessage,
		Health:             domain.Health(health),
		CreatedAt:          ca,
		UpdatedAt:          ua,
	}
	if secretID.Valid {
		c.SecretID = domain.ID(secretID.String)
	}
	if managedBy.Valid {
		c.ManagedBy = managedBy.String
	} else {
		c.ManagedBy = ManagedByOmahab
	}
	if externalRef.Valid {
		er := externalRef.String
		c.ExternalRef = &er
	}
	if expiresAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, expiresAt.String)
		c.ExpiresAt = &t
	}
	if lastCheckedAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, lastCheckedAt.String)
		c.LastCheckedAt = &t
	}
	return c, nil
}

type vkScanner interface{ Scan(dest ...any) error }

func scanVirtualKey(row vkScanner) (*VirtualKey, error) {
	var (
		id, name, keyPrefix, scopesStr string
		gatewayKeyID, ownerKind, ownerID sql.NullString
		rpmLimit, tpmLimit, concurrencyLimit sql.NullInt64
		budgetAmount sql.NullFloat64
		budgetDuration sql.NullString
		expiresAt, revokedAt           sql.NullString
		createdAt, updatedAt           string
	)
	if err := row.Scan(&id, &name, &keyPrefix, &scopesStr, &gatewayKeyID, &ownerKind, &ownerID, &rpmLimit, &tpmLimit, &concurrencyLimit, &budgetAmount, &budgetDuration, &expiresAt, &revokedAt, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan virtual key: %w", err)
	}
	return scanVirtualKeyFromValuesExtended(id, name, keyPrefix, scopesStr, gatewayKeyID, ownerKind, ownerID, rpmLimit, tpmLimit, concurrencyLimit, budgetAmount, budgetDuration, expiresAt, revokedAt, createdAt, updatedAt)
}

func scanVirtualKeyFromValues(id, name, keyPrefix, scopesStr string, expiresAt, revokedAt sql.NullString, createdAt, updatedAt string) (*VirtualKey, error) {
	return scanVirtualKeyFromValuesExtended(id, name, keyPrefix, scopesStr, sql.NullString{}, sql.NullString{}, sql.NullString{}, sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{}, sql.NullFloat64{}, sql.NullString{}, expiresAt, revokedAt, createdAt, updatedAt)
}

func scanVirtualKeyFromValuesExtended(id, name, keyPrefix, scopesStr string, gatewayKeyID, ownerKind, ownerID sql.NullString, rpmLimit, tpmLimit, concurrencyLimit sql.NullInt64, budgetAmount sql.NullFloat64, budgetDuration sql.NullString, expiresAt, revokedAt sql.NullString, createdAt, updatedAt string) (*VirtualKey, error) {
	ca, _ := time.Parse(time.RFC3339Nano, createdAt)
	ua, _ := time.Parse(time.RFC3339Nano, updatedAt)
	vk := &VirtualKey{
		ID:        domain.ID(id),
		Name:      name,
		KeyPrefix: keyPrefix,
		CreatedAt: ca,
		UpdatedAt: ua,
	}
	if scopesStr != "" {
		vk.Scopes = strings.Split(scopesStr, ",")
	}
	if gatewayKeyID.Valid {
		v := gatewayKeyID.String
		vk.GatewayKeyID = &v
	}
	if ownerKind.Valid {
		v := ownerKind.String
		vk.OwnerKind = &v
	}
	if ownerID.Valid {
		v := ownerID.String
		vk.OwnerID = &v
	}
	if rpmLimit.Valid {
		v := int(rpmLimit.Int64)
		vk.RPMLimit = &v
	}
	if tpmLimit.Valid {
		v := int(tpmLimit.Int64)
		vk.TPMLimit = &v
	}
	if concurrencyLimit.Valid {
		v := int(concurrencyLimit.Int64)
		vk.ConcurrencyLimit = &v
	}
	if budgetAmount.Valid {
		v := budgetAmount.Float64
		vk.BudgetAmount = &v
	}
	if budgetDuration.Valid {
		v := budgetDuration.String
		vk.BudgetDuration = &v
	}
	if expiresAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, expiresAt.String)
		vk.ExpiresAt = &t
	}
	if revokedAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, revokedAt.String)
		vk.RevokedAt = &t
	}
	return vk, nil
}

func hashToken(token string) string {
	if token == "" {
		return ""
	}
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func timePtrToString(t *time.Time) *string {
	if t == nil {
		return nil
	}
	v := t.UTC().Format(time.RFC3339Nano)
	return &v
}

func nullableString(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
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
