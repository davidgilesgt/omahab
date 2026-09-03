package hermes

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

const (
	DefaultProfileID = "default"
	KindDefault      = "default"
	KindProject      = "project"
)

var (
	ErrNotFound   = errors.New("hermes: not found")
	ErrValidation = errors.New("hermes: validation failed")
	ErrConflict   = errors.New("hermes: conflict")
	ErrForbidden  = errors.New("hermes: forbidden")
)

// EventSink is a package-local normalized event sink.
type EventSink interface {
	Emit(ctx context.Context, event domain.Event) error
}

// NopEventSink discards events.
type NopEventSink struct{}

func (NopEventSink) Emit(_ context.Context, _ domain.Event) error { return nil }

// RemoteConnection holds web/desktop connection metadata. It intentionally
// contains no cookies, no safeStorage payloads, and no browser session
// material. Official Hermes Desktop performs its own OAuth/token flow.
type RemoteConnection struct {
	ID           string    `json:"id"`
	ProfileID    string    `json:"profile_id"`
	ServerURL    string    `json:"server_url"`
	HermesURL    string    `json:"hermes_url"`
	InstanceID   string    `json:"instance_id"`
	OIDCIssuer   string    `json:"oidc_issuer,omitempty"`
	DisplayAlias string    `json:"display_alias,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	GeneratedAt  time.Time `json:"generated_at"`
}

// Service owns Hermes remote connection metadata (post Step 3, projects removed).
type Service struct {
	db         *sql.DB
	sink       EventSink
	serverURL  string
	instanceID string
	oidcIssuer string
	domain     string
}

// Config holds optional Service construction options.
type Config struct {
	ServerURL  string
	InstanceID string
	OIDCIssuer string
	Domain     string
}

// New creates a Service.
func New(db *sql.DB, sink EventSink) *Service {
	return NewWithConfig(db, sink, Config{})
}

// NewWithConfig creates a Service with connection defaults.
func NewWithConfig(db *sql.DB, sink EventSink, cfg Config) *Service {
	if sink == nil {
		sink = NopEventSink{}
	}
	return &Service{db: db, sink: sink, serverURL: strings.TrimSpace(cfg.ServerURL), instanceID: strings.TrimSpace(cfg.InstanceID), oidcIssuer: strings.TrimSpace(cfg.OIDCIssuer), domain: strings.TrimSpace(cfg.Domain)}
}

// SetConnectionDefaults updates server/instance metadata used for RemoteConnection.
func (s *Service) SetConnectionDefaults(serverURL, instanceID, oidcIssuer, domain string) {
	s.serverURL = strings.TrimSpace(serverURL)
	s.instanceID = strings.TrimSpace(instanceID)
	s.oidcIssuer = strings.TrimSpace(oidcIssuer)
	s.domain = strings.TrimSpace(domain)
}

// RemoteConnectionInfo returns the remote web/desktop connection metadata for a profile.
// It contains only server URLs, instance identity, display alias, and OIDC issuer.
// If a hermes_remote_connections row exists it is returned; otherwise metadata is synthesized.
func (s *Service) RemoteConnectionInfo(ctx context.Context, profileID string) (*RemoteConnection, error) {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" {
		return nil, fmt.Errorf("%w: profile id required", ErrValidation)
	}
	// Try persisted row by id (new schema) or profile_id (legacy, for migration grace period).
	var rc RemoteConnection
	var createdAt, updatedAt string
	// Try new schema (id)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, server_url, hermes_url, instance_id, oidc_issuer, created_at, updated_at FROM hermes_remote_connections WHERE id = ?`, profileID).
		Scan(&rc.ID, &rc.ServerURL, &rc.HermesURL, &rc.InstanceID, &rc.OIDCIssuer, &createdAt, &updatedAt)
	if err == nil {
		ca, _ := time.Parse(time.RFC3339Nano, createdAt)
		ua, _ := time.Parse(time.RFC3339Nano, updatedAt)
		rc.CreatedAt = ca
		rc.UpdatedAt = ua
		rc.GeneratedAt = ua
		rc.ProfileID = profileID
		return &rc, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		// If column mismatch (legacy schema), fallback to profile_id query
		if strings.Contains(err.Error(), "no such column") || strings.Contains(err.Error(), "has no column") {
			err2 := s.db.QueryRowContext(ctx,
				`SELECT id, profile_id, server_url, hermes_url, instance_id, oidc_issuer, created_at, updated_at FROM hermes_remote_connections WHERE profile_id = ?`, profileID).
				Scan(&rc.ID, &rc.ProfileID, &rc.ServerURL, &rc.HermesURL, &rc.InstanceID, &rc.OIDCIssuer, &createdAt, &updatedAt)
			if err2 == nil {
				ca, _ := time.Parse(time.RFC3339Nano, createdAt)
				ua, _ := time.Parse(time.RFC3339Nano, updatedAt)
				rc.CreatedAt = ca
				rc.UpdatedAt = ua
				rc.GeneratedAt = ua
				return &rc, nil
			}
			if !errors.Is(err2, sql.ErrNoRows) {
				return nil, fmt.Errorf("get remote connection: %w", err2)
			}
		} else {
			return nil, fmt.Errorf("get remote connection: %w", err)
		}
	}
	// Synthesize from defaults without cookies or safeStorage.
	serverURL := s.serverURL
	hermesURL := ""
	if serverURL != "" {
		hermesURL = strings.TrimRight(serverURL, "/") + "/api/v1/hermes/ws"
	}
	now := time.Now().UTC()
	rc = RemoteConnection{
		ID:          newID(),
		ProfileID:   profileID,
		ServerURL:   serverURL,
		HermesURL:   hermesURL,
		InstanceID:  s.instanceID,
		OIDCIssuer:  s.oidcIssuer,
		GeneratedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if rc.HermesURL == "" && s.domain != "" {
		rc.HermesURL = "https://ai." + s.domain
	}
	return &rc, nil
}

// GetRemoteConnection is an alias for RemoteConnectionInfo.
func (s *Service) GetRemoteConnection(ctx context.Context, profileID string) (*RemoteConnection, error) {
	return s.RemoteConnectionInfo(ctx, profileID)
}

// UpsertRemoteConnection persists remote connection metadata for a profile.
func (s *Service) UpsertRemoteConnection(ctx context.Context, profileID, serverURL, hermesURL, instanceID, oidcIssuer string) (*RemoteConnection, error) {
	profileID = strings.TrimSpace(profileID)
	serverURL = strings.TrimSpace(serverURL)
	hermesURL = strings.TrimSpace(hermesURL)
	instanceID = strings.TrimSpace(instanceID)
	oidcIssuer = strings.TrimSpace(oidcIssuer)
	if profileID == "" || serverURL == "" || hermesURL == "" || instanceID == "" {
		return nil, fmt.Errorf("%w: profile, server_url, hermes_url and instance_id required", ErrValidation)
	}
	if strings.Contains(profileID, "\x00") || strings.Contains(serverURL, "\x00") || strings.Contains(hermesURL, "\x00") || strings.Contains(instanceID, "\x00") || strings.Contains(oidcIssuer, "\x00") {
		return nil, fmt.Errorf("%w: NUL byte not allowed", ErrValidation)
	}
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)
	// Check existing by id
	var existingID string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM hermes_remote_connections WHERE id = ?`, profileID).Scan(&existingID)
	if err == nil {
		_, err = s.db.ExecContext(ctx,
			`UPDATE hermes_remote_connections SET server_url = ?, hermes_url = ?, instance_id = ?, oidc_issuer = ?, updated_at = ? WHERE id = ?`,
			serverURL, hermesURL, instanceID, oidcIssuer, nowStr, profileID)
		if err != nil {
			return nil, fmt.Errorf("update remote connection: %w", err)
		}
		return &RemoteConnection{
			ID: profileID, ProfileID: profileID, ServerURL: serverURL, HermesURL: hermesURL,
			InstanceID: instanceID, OIDCIssuer: oidcIssuer, CreatedAt: now, UpdatedAt: now, GeneratedAt: now,
		}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		// Grace fallback for legacy schema with profile_id
		if strings.Contains(err.Error(), "no such column") {
			return s.upsertLegacy(ctx, profileID, serverURL, hermesURL, instanceID, oidcIssuer, now, nowStr)
		}
		return nil, fmt.Errorf("lookup remote connection: %w", err)
	}
	// Insert new row with id = profileID (preserves stable lookup)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO hermes_remote_connections (id, server_url, hermes_url, instance_id, oidc_issuer, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		profileID, serverURL, hermesURL, instanceID, oidcIssuer, nowStr, nowStr)
	if err != nil {
		// If table still has legacy schema (profile_id column not null), fallback
		if strings.Contains(err.Error(), "has no column") || strings.Contains(err.Error(), "no column") || strings.Contains(err.Error(), "table hermes_remote_connections has") {
			return s.upsertLegacy(ctx, profileID, serverURL, hermesURL, instanceID, oidcIssuer, now, nowStr)
		}
		return nil, fmt.Errorf("insert remote connection: %w", err)
	}
	_ = s.emit(ctx, "hermes.remote_connection.upserted", "info", domain.ID(profileID), "remote connection metadata upserted", map[string]any{"server_url": serverURL})
	return &RemoteConnection{
		ID: profileID, ProfileID: profileID, ServerURL: serverURL, HermesURL: hermesURL,
		InstanceID: instanceID, OIDCIssuer: oidcIssuer, CreatedAt: now, UpdatedAt: now, GeneratedAt: now,
	}, nil
}

func (s *Service) upsertLegacy(ctx context.Context, profileID, serverURL, hermesURL, instanceID, oidcIssuer string, now time.Time, nowStr string) (*RemoteConnection, error) {
	var existingID string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM hermes_remote_connections WHERE profile_id = ?`, profileID).Scan(&existingID)
	if err == nil {
		_, err = s.db.ExecContext(ctx,
			`UPDATE hermes_remote_connections SET server_url = ?, hermes_url = ?, instance_id = ?, oidc_issuer = ?, updated_at = ? WHERE profile_id = ?`,
			serverURL, hermesURL, instanceID, oidcIssuer, nowStr, profileID)
		if err != nil {
			return nil, fmt.Errorf("update remote connection: %w", err)
		}
		return &RemoteConnection{
			ID: existingID, ProfileID: profileID, ServerURL: serverURL, HermesURL: hermesURL,
			InstanceID: instanceID, OIDCIssuer: oidcIssuer, CreatedAt: now, UpdatedAt: now, GeneratedAt: now,
		}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("lookup remote connection legacy: %w", err)
	}
	id := newID()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO hermes_remote_connections (id, profile_id, server_url, hermes_url, instance_id, oidc_issuer, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, profileID, serverURL, hermesURL, instanceID, oidcIssuer, nowStr, nowStr)
	if err != nil {
		return nil, fmt.Errorf("insert remote connection legacy: %w", err)
	}
	_ = s.emit(ctx, "hermes.remote_connection.upserted", "info", domain.ID(profileID), "remote connection metadata upserted", map[string]any{"server_url": serverURL})
	return &RemoteConnection{
		ID: id, ProfileID: profileID, ServerURL: serverURL, HermesURL: hermesURL,
		InstanceID: instanceID, OIDCIssuer: oidcIssuer, CreatedAt: now, UpdatedAt: now, GeneratedAt: now,
	}, nil
}

// ProvisionRemoteConnection is a convenience that generates the official connection metadata.
func (s *Service) ProvisionRemoteConnection(ctx context.Context, profileID string) (*RemoteConnection, error) {
	return s.RemoteConnectionInfo(ctx, profileID)
}

// ConnectionInfo is alias for RemoteConnectionInfo.
func (s *Service) ConnectionInfo(ctx context.Context, profileID string) (*RemoteConnection, error) {
	return s.RemoteConnectionInfo(ctx, profileID)
}

// ProvisionConnection is alias for ProvisionRemoteConnection.
func (s *Service) ProvisionConnection(ctx context.Context, profileID string) (*RemoteConnection, error) {
	return s.ProvisionRemoteConnection(ctx, profileID)
}

func (s *Service) emit(ctx context.Context, typ, severity string, resourceID domain.ID, msg string, data map[string]any) error {
	if s.sink == nil {
		return nil
	}
	_ = s.sink.Emit(ctx, domain.Event{Type: typ, Severity: severity, ResourceID: resourceID, Message: msg, Data: data})
	return nil
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
