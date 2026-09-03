package integrations

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Sentinel errors.
var (
	ErrNotFound   = errors.New("integration not found")
	ErrValidation = errors.New("validation error")
	ErrHAInvalid  = errors.New("home assistant validation failed")
)

// Default scope for Home Assistant credentials. Only the default Hermes profile
// may receive HASS_SERVER and HASS_TOKEN. projects must never receive them.
const DefaultAssistantScope = "hermes:default"

// hassEnvNames are the only credential names projected for Home Assistant.
const (
	EnvHassServer = "HASS_SERVER"
	EnvHassToken  = "HASS_TOKEN"
)

// HomeAssistantConfig holds the non-secret HA configuration persisted in SQLite.
// Secrets are stored exclusively via SecretStore.
type HomeAssistantConfig struct {
	ID              string     `json:"id"`
	ServerURL       string     `json:"server_url"`
	Status          string     `json:"status"`
	LastValidatedAt *time.Time `json:"last_validated_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// SecretStore abstracts encrypted secret persistence. Implementations must keep
// values out of logs and JSON responses.
type SecretStore interface {
	Put(ctx context.Context, scope, name, value string) error
	Delete(ctx context.Context, scope, name string) error
	Get(ctx context.Context, scope, name string) (string, error)
}

// HassRunner abstracts direct hass-cli invocation for validation and skill
// installation. Hermes invokes hass-cli directly; Omahab does not proxy commands.
type HassRunner interface {
	// Validate performs a read operation against the Home Assistant server
	// (e.g. `hass-cli state list` or `hass-cli service list`) to confirm the
	// server URL and token are valid.
	Validate(ctx context.Context, serverURL, token string) error
	// InstallSkill installs the concise Hermes skill describing hass-cli usage
	// into the default assistant's skill set.
	InstallSkill(ctx context.Context) error
}

// NoopHassRunner is a no-op HassRunner for testing.
type NoopHassRunner struct{}

func (NoopHassRunner) Validate(_ context.Context, _, _ string) error { return nil }
func (NoopHassRunner) InstallSkill(_ context.Context) error          { return nil }

// MemorySecretStore is an in-memory SecretStore for testing.
type MemorySecretStore struct {
	data map[string]string
}

func NewMemorySecretStore() *MemorySecretStore {
	return &MemorySecretStore{data: make(map[string]string)}
}

func (m *MemorySecretStore) Put(_ context.Context, scope, name, value string) error {
	m.data[scope+":"+name] = value
	return nil
}
func (m *MemorySecretStore) Delete(_ context.Context, scope, name string) error {
	delete(m.data, scope+":"+name)
	return nil
}
func (m *MemorySecretStore) Get(_ context.Context, scope, name string) (string, error) {
	v, ok := m.data[scope+":"+name]
	if !ok {
		return "", fmt.Errorf("secret not found: %s/%s", scope, name)
	}
	return v, nil
}

// Service owns external integration configuration, currently Home Assistant
// via direct hass-cli.
type Service struct {
	db      *sql.DB
	secrets SecretStore
	runner  HassRunner
}

// New creates a Service. secrets and runner may be nil for testing.
func New(db *sql.DB, secrets SecretStore, runner HassRunner) *Service {
	if secrets == nil {
		secrets = NewMemorySecretStore()
	}
	if runner == nil {
		runner = NoopHassRunner{}
	}
	return &Service{db: db, secrets: secrets, runner: runner}
}

// Configure validates, persists, and projects Home Assistant credentials for the
// default assistant only. It performs a live read validation via HassRunner.
// The token is never stored in SQLite; it is projected only through SecretStore
// under DefaultAssistantScope.
func (s *Service) Configure(ctx context.Context, serverURL, token string) (*HomeAssistantConfig, error) {
	serverURL = strings.TrimSpace(serverURL)
	token = strings.TrimSpace(token)

	if serverURL == "" {
		return nil, fmt.Errorf("%w: server url is required", ErrValidation)
	}
	if token == "" {
		return nil, fmt.Errorf("%w: access token is required", ErrValidation)
	}
	if strings.Contains(token, "\x00") || strings.Contains(serverURL, "\x00") {
		return nil, fmt.Errorf("%w: NUL byte not allowed", ErrValidation)
	}
	u, err := url.Parse(serverURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("%w: invalid server url", ErrValidation)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: server url must be http or https", ErrValidation)
	}
	// Normalize: no trailing slash, no userinfo, no fragment
	u.Fragment = ""
	u.User = nil
	serverURL = strings.TrimRight(u.String(), "/")

	// Live validation via hass-cli
	if err := s.runner.Validate(ctx, serverURL, token); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHAInvalid, err)
	}

	// Project credentials exclusively to default assistant scope.
	if err := s.secrets.Put(ctx, DefaultAssistantScope, EnvHassServer, serverURL); err != nil {
		return nil, fmt.Errorf("store %s: %w", EnvHassServer, err)
	}
	if err := s.secrets.Put(ctx, DefaultAssistantScope, EnvHassToken, token); err != nil {
		_ = s.secrets.Delete(ctx, DefaultAssistantScope, EnvHassServer)
		return nil, fmt.Errorf("store %s: %w", EnvHassToken, err)
	}

	// Install Hermes skill (best-effort)
	_ = s.runner.InstallSkill(ctx)

	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)

	// Upsert: there is at most one HA integration row (id = 'ha')
	var exists bool
	var existingID string
	err = s.db.QueryRowContext(ctx, `SELECT id FROM ha_integrations LIMIT 1`).Scan(&existingID)
	if err == nil {
		exists = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check existing ha integration: %w", err)
	}

	if exists {
		_, err = s.db.ExecContext(ctx,
			`UPDATE ha_integrations SET server_url = ?, status = ?, last_validated_at = ?, updated_at = ? WHERE id = ?`,
			serverURL, "connected", nowStr, nowStr, existingID,
		)
		if err != nil {
			return nil, fmt.Errorf("update ha integration: %w", err)
		}
		return &HomeAssistantConfig{
			ID:              existingID,
			ServerURL:       serverURL,
			Status:          "connected",
			LastValidatedAt: &now,
			UpdatedAt:       now,
		}, nil
	}

	id := newID()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO ha_integrations (id, server_url, status, last_validated_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, serverURL, "connected", nowStr, nowStr, nowStr,
	)
	if err != nil {
		return nil, fmt.Errorf("insert ha integration: %w", err)
	}
	return &HomeAssistantConfig{
		ID:              id,
		ServerURL:       serverURL,
		Status:          "connected",
		LastValidatedAt: &now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

// Get returns the non-secret Home Assistant configuration, or ErrNotFound.
func (s *Service) Get(ctx context.Context) (*HomeAssistantConfig, error) {
	var (
		id, serverURL, status string
		lastValidatedAt       sql.NullString
		createdAt, updatedAt  string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, server_url, status, last_validated_at, created_at, updated_at FROM ha_integrations LIMIT 1`).
		Scan(&id, &serverURL, &status, &lastValidatedAt, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get ha integration: %w", err)
	}
	ca, _ := time.Parse(time.RFC3339Nano, createdAt)
	ua, _ := time.Parse(time.RFC3339Nano, updatedAt)
	cfg := &HomeAssistantConfig{
		ID:        id,
		ServerURL: serverURL,
		Status:    status,
		CreatedAt: ca,
		UpdatedAt: ua,
	}
	if lastValidatedAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, lastValidatedAt.String)
		cfg.LastValidatedAt = &t
	}
	return cfg, nil
}

// Remove deletes the HA integration and removes projected secrets from the
// default assistant scope. It never touches project scopes.
func (s *Service) Remove(ctx context.Context) error {
	_, err := s.Get(ctx)
	if err != nil {
		return err
	}
	// Remove secrets from default scope only
	_ = s.secrets.Delete(ctx, DefaultAssistantScope, EnvHassServer)
	_ = s.secrets.Delete(ctx, DefaultAssistantScope, EnvHassToken)
	_, err = s.db.ExecContext(ctx, `DELETE FROM ha_integrations`)
	return err
}

// Validate re-validates the current configuration with a live hass-cli read.
func (s *Service) Validate(ctx context.Context) error {
	cfg, err := s.Get(ctx)
	if err != nil {
		return err
	}
	token, err := s.secrets.Get(ctx, DefaultAssistantScope, EnvHassToken)
	if err != nil {
		return fmt.Errorf("%w: token not found", ErrHAInvalid)
	}
	if err := s.runner.Validate(ctx, cfg.ServerURL, token); err != nil {
		return fmt.Errorf("%w: %v", ErrHAInvalid, err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = s.db.ExecContext(ctx,
		`UPDATE ha_integrations SET status = ?, last_validated_at = ?, updated_at = ?`, "connected", now, now)
	return nil
}

// ProjectHasAccess reports whether a project should receive Home Assistant
// credentials. It always returns false — projects must never receive HA
// credentials per DESIGN §17.
func (s *Service) ProjectHasAccess(_ string) bool { return false }

// DefaultAssistantScopeName returns the only scope that may hold HA credentials.
func (s *Service) DefaultAssistantScopeName() string { return DefaultAssistantScope }

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
