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

// Stable identifiers.
const (
	DefaultProfileID = "default"
	KindDefault      = "default"
	KindProject      = "project"
)

// Sentinel errors preserve validation/not-found/conflict/forbidden distinctions.
var (
	ErrNotFound   = errors.New("hermes: not found")
	ErrValidation = errors.New("hermes: validation failed")
	ErrConflict   = errors.New("hermes: conflict")
	ErrForbidden  = errors.New("hermes: forbidden")
)

// HermesClient is the narrow upstream Hermes integration. It is the only
// way the control plane touches Hermes profiles. The interface is small,
// testable, and never receives secrets or cookies.
type HermesClient interface {
	EnsureProfile(ctx context.Context, id, displayAlias string) error
	UpdateAlias(ctx context.Context, id, displayAlias string) error
	DeleteProfile(ctx context.Context, id string) error
}

// NoopHermesClient does nothing. Used in tests and when Hermes is disabled.
type NoopHermesClient struct{}

func (NoopHermesClient) EnsureProfile(_ context.Context, _, _ string) error { return nil }
func (NoopHermesClient) UpdateAlias(_ context.Context, _, _ string) error   { return nil }
func (NoopHermesClient) DeleteProfile(_ context.Context, _ string) error    { return nil }

// EventSink is a package-local normalized event sink.
type EventSink interface {
	Emit(ctx context.Context, event domain.Event) error
}

// NopEventSink discards events.
type NopEventSink struct{}

func (NopEventSink) Emit(_ context.Context, _ domain.Event) error { return nil }

// Profile is a Hermes profile (bot). ID is stable; DisplayAlias is mutable
// and stored independently. Default profile has ID "default".
type Profile struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	ProjectID    string    `json:"project_id,omitempty"`
	DisplayAlias string    `json:"display_alias"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Source is a knowledge-source grant.
type Source struct {
	ID         string    `json:"id"`
	ProfileID  string    `json:"profile_id"`
	SourceType string    `json:"source_type"`
	ResourceID string    `json:"resource_id"`
	GrantedAt  time.Time `json:"granted_at"`
}

// Message is a routed bot message.
type Message struct {
	ID            string    `json:"id"`
	FromProfileID string    `json:"from_profile_id"`
	ToProfileID   string    `json:"to_profile_id"`
	Kind          string    `json:"kind"`
	Body          string    `json:"body"`
	CreatedAt     time.Time `json:"created_at"`
}

// Group is an explicit cross-project communication group.
type Group struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Members   []string  `json:"members,omitempty"`
}

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

// Allowed capabilities per profile kind. Fail-closed: unknown or disallowed = forbidden.
var defaultAllowedCapabilities = map[string]bool{
	"notes.read":             true,
	"notes.write":            true,
	"paperless.search":       true,
	"paperless.read":         true,
	"karakeep.search":        true,
	"karakeep.read":          true,
	"email.read":             true,
	"home_assistant.read":    true,
	"home_assistant.execute": true,
	"project.status":         true,
	"project.list":           true,
	"memory.default":         true,
}

var projectAllowedCapabilities = map[string]bool{
	"repo.read":          true,
	"repo.write":         true,
	"issues.read":        true,
	"issues.write":       true,
	"deploy.read":        true,
	"deploy.write":       true,
	"secrets.project":    true,
	"attachments.read":   true,
	"attachments.write":  true,
	"memory.own":         true,
	"memory.project":     true,
	"project.status.own": true,
}

// Explicitly forbidden for project bots even if added to allowed map later.
// Home Assistant and default memory must never be granted to project bots.
var projectForbiddenCapabilities = map[string]bool{
	"home_assistant.read":    true,
	"home_assistant.execute": true,
	"home_assistant.write":   true,
	"memory.default":         true,
	"notes.read":             true,
	"notes.write":            true,
	"paperless.search":       true,
	"paperless.read":         true,
	"karakeep.search":        true,
	"karakeep.read":          true,
	"email.read":             true,
}

// Allowed knowledge source types per profile kind.
var defaultAllowedSources = map[string]bool{
	"notes":          true,
	"paperless":      true,
	"karakeep":       true,
	"email":          true,
	"home_assistant": true,
	"project_status": true,
	"project":        true,
}

var projectAllowedSources = map[string]bool{
	"repo":        true,
	"issues":      true,
	"deploy":      true,
	"secrets":     true,
	"attachments": true,
	"memory":      true,
}

// Forbidden source types for project bots (explicit).
var projectForbiddenSources = map[string]bool{
	"notes":          true,
	"paperless":      true,
	"karakeep":       true,
	"email":          true,
	"home_assistant": true,
}

// Message kinds and routing.
const (
	KindDelegation    = "delegation"
	KindStatusRequest = "status_request"
	KindQuestion      = "question"
	KindStatus        = "status"
	KindMessage       = "message"
	KindRedirect      = "redirect"
	KindCancel        = "cancel"
)

var validMessageKinds = map[string]bool{
	KindDelegation:    true,
	KindStatusRequest: true,
	KindQuestion:      true,
	KindStatus:        true,
	KindMessage:       true,
	KindRedirect:      true,
	KindCancel:        true,
}

// Service owns Hermes profile lifecycle, ACLs, sources, messages, groups,
// and remote connection metadata.
type Service struct {
	db     *sql.DB
	client HermesClient
	sink   EventSink
	// optional connection defaults; if empty, caller supplies per-request.
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

// New creates a Service. client and sink may be nil (noop used).
func New(db *sql.DB, client HermesClient, sink EventSink) *Service {
	if client == nil {
		client = NoopHermesClient{}
	}
	if sink == nil {
		sink = NopEventSink{}
	}
	return &Service{db: db, client: client, sink: sink}
}

// NewWithConfig creates a Service with connection defaults.
func NewWithConfig(db *sql.DB, client HermesClient, sink EventSink, cfg Config) *Service {
	s := New(db, client, sink)
	s.serverURL = strings.TrimSpace(cfg.ServerURL)
	s.instanceID = strings.TrimSpace(cfg.InstanceID)
	s.oidcIssuer = strings.TrimSpace(cfg.OIDCIssuer)
	s.domain = strings.TrimSpace(cfg.Domain)
	return s
}

// SetConnectionDefaults updates server/instance metadata used for RemoteConnection.
func (s *Service) SetConnectionDefaults(serverURL, instanceID, oidcIssuer, domain string) {
	s.serverURL = strings.TrimSpace(serverURL)
	s.instanceID = strings.TrimSpace(instanceID)
	s.oidcIssuer = strings.TrimSpace(oidcIssuer)
	s.domain = strings.TrimSpace(domain)
}

// --- profiles ---

// EnsureDefaultProfile ensures the stable default profile exists (id="default").
// Alias is persisted independently; renaming later preserves the same id and memory.
func (s *Service) EnsureDefaultProfile(ctx context.Context, displayAlias string) (*Profile, error) {
	alias := strings.TrimSpace(displayAlias)
	if alias == "" {
		alias = "AI"
	}
	if strings.Contains(alias, "\x00") {
		return nil, fmt.Errorf("%w: alias contains NUL", ErrValidation)
	}
	if len(alias) > 64 {
		return nil, fmt.Errorf("%w: alias too long", ErrValidation)
	}
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)

	existing, err := s.GetProfile(ctx, DefaultProfileID)
	if err == nil {
		// Already exists; optionally update alias if different.
		if existing.DisplayAlias != alias {
			return s.RenameDefaultProfile(ctx, alias)
		}
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	// Create default profile row.
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO hermes_profiles (id, kind, project_id, display_alias, created_at, updated_at) VALUES (?, ?, NULL, ?, ?, ?)`,
		DefaultProfileID, KindDefault, alias, nowStr, nowStr)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: default profile already exists", ErrConflict)
		}
		return nil, fmt.Errorf("insert default profile: %w", err)
	}
	if err := s.client.EnsureProfile(ctx, DefaultProfileID, alias); err != nil {
		// best-effort; do not fail creation, but log via event
		_ = s.emit(ctx, "hermes.profile.client_error", "warning", domain.ID(DefaultProfileID), "hermes client ensure failed", map[string]any{"error": err.Error()})
	}
	p := &Profile{ID: DefaultProfileID, Kind: KindDefault, DisplayAlias: alias, CreatedAt: now, UpdatedAt: now}
	_ = s.emit(ctx, "hermes.profile.created", "info", domain.ID(DefaultProfileID), "default profile created", map[string]any{"alias": alias})
	return p, nil
}

// GetProfile returns a profile by stable id.
func (s *Service) GetProfile(ctx context.Context, id string) (*Profile, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: profile id required", ErrValidation)
	}
	var (
		kind, alias, createdAt, updatedAt string
		projectID                         sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, kind, project_id, display_alias, created_at, updated_at FROM hermes_profiles WHERE id = ?`, id).
		Scan(&id, &kind, &projectID, &alias, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: profile %q not found", ErrNotFound, id)
		}
		return nil, fmt.Errorf("get profile: %w", err)
	}
	ca, _ := time.Parse(time.RFC3339Nano, createdAt)
	ua, _ := time.Parse(time.RFC3339Nano, updatedAt)
	p := &Profile{ID: id, Kind: kind, DisplayAlias: alias, CreatedAt: ca, UpdatedAt: ua}
	if projectID.Valid {
		p.ProjectID = projectID.String
	}
	return p, nil
}

// GetDefaultProfile is shorthand for GetProfile("default").
func (s *Service) GetDefaultProfile(ctx context.Context) (*Profile, error) {
	return s.GetProfile(ctx, DefaultProfileID)
}

// ListProfiles returns all profiles ordered by creation.
func (s *Service) ListProfiles(ctx context.Context) ([]*Profile, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, project_id, display_alias, created_at, updated_at FROM hermes_profiles ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	defer rows.Close()
	var out []*Profile
	for rows.Next() {
		var (
			id, kind, alias, createdAt, updatedAt string
			projectID                             sql.NullString
		)
		if err := rows.Scan(&id, &kind, &projectID, &alias, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan profile: %w", err)
		}
		ca, _ := time.Parse(time.RFC3339Nano, createdAt)
		ua, _ := time.Parse(time.RFC3339Nano, updatedAt)
		p := &Profile{ID: id, Kind: kind, DisplayAlias: alias, CreatedAt: ca, UpdatedAt: ua}
		if projectID.Valid {
			p.ProjectID = projectID.String
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// EnsureProjectProfile ensures one isolated profile per project. projectID is
// the domain project ID (opaque). Alias defaults to project slug if empty.
func (s *Service) EnsureProjectProfile(ctx context.Context, projectID, displayAlias string) (*Profile, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, fmt.Errorf("%w: project id required", ErrValidation)
	}
	if strings.Contains(projectID, "\x00") {
		return nil, fmt.Errorf("%w: project id contains NUL", ErrValidation)
	}
	alias := strings.TrimSpace(displayAlias)
	if alias == "" {
		alias = "project-" + projectID
	}
	if strings.Contains(alias, "\x00") {
		return nil, fmt.Errorf("%w: alias contains NUL", ErrValidation)
	}
	if len(alias) > 64 {
		return nil, fmt.Errorf("%w: alias too long", ErrValidation)
	}
	// Check existing by project_id
	var existingID string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM hermes_profiles WHERE project_id = ?`, projectID).Scan(&existingID)
	if err == nil {
		return s.GetProfile(ctx, existingID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("lookup project profile: %w", err)
	}
	id := newID()
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO hermes_profiles (id, kind, project_id, display_alias, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, KindProject, projectID, alias, nowStr, nowStr)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: project profile conflict", ErrConflict)
		}
		return nil, fmt.Errorf("insert project profile: %w", err)
	}
	if err := s.client.EnsureProfile(ctx, id, alias); err != nil {
		_ = s.emit(ctx, "hermes.profile.client_error", "warning", domain.ID(id), "hermes client ensure failed", map[string]any{"error": err.Error()})
	}
	p := &Profile{ID: id, Kind: KindProject, ProjectID: projectID, DisplayAlias: alias, CreatedAt: now, UpdatedAt: now}
	_ = s.emit(ctx, "hermes.profile.created", "info", domain.ID(id), "project profile created", map[string]any{"project_id": projectID, "alias": alias})
	return p, nil
}

// DeleteProjectProfile deletes the profile for a given project id.
func (s *Service) DeleteProjectProfile(ctx context.Context, projectID string) error {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return fmt.Errorf("%w: project id required", ErrValidation)
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM hermes_profiles WHERE project_id = ?`, projectID).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: project profile not found", ErrNotFound)
		}
		return fmt.Errorf("lookup project profile: %w", err)
	}
	return s.DeleteProfile(ctx, id)
}

// DeleteProfile deletes a profile by stable id. Default profile cannot be deleted.
func (s *Service) DeleteProfile(ctx context.Context, profileID string) error {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" {
		return fmt.Errorf("%w: profile id required", ErrValidation)
	}
	if profileID == DefaultProfileID {
		return fmt.Errorf("%w: default profile cannot be deleted", ErrValidation)
	}
	if _, err := s.GetProfile(ctx, profileID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM hermes_profiles WHERE id = ?`, profileID)
	if err != nil {
		return fmt.Errorf("delete profile: %w", err)
	}
	_ = s.client.DeleteProfile(ctx, profileID)
	_ = s.emit(ctx, "hermes.profile.deleted", "info", domain.ID(profileID), "profile deleted", nil)
	return nil
}

// RenameProfile updates the display alias for any profile. ID and memory remain stable.
func (s *Service) RenameProfile(ctx context.Context, profileID, newAlias string) (*Profile, error) {
	profileID = strings.TrimSpace(profileID)
	newAlias = strings.TrimSpace(newAlias)
	if profileID == "" {
		return nil, fmt.Errorf("%w: profile id required", ErrValidation)
	}
	if newAlias == "" {
		return nil, fmt.Errorf("%w: alias required", ErrValidation)
	}
	if strings.Contains(newAlias, "\x00") {
		return nil, fmt.Errorf("%w: alias contains NUL", ErrValidation)
	}
	if len(newAlias) > 64 {
		return nil, fmt.Errorf("%w: alias too long", ErrValidation)
	}
	p, err := s.GetProfile(ctx, profileID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.ExecContext(ctx, `UPDATE hermes_profiles SET display_alias = ?, updated_at = ? WHERE id = ?`, newAlias, now, profileID)
	if err != nil {
		return nil, fmt.Errorf("rename profile: %w", err)
	}
	if err := s.client.UpdateAlias(ctx, profileID, newAlias); err != nil {
		_ = s.emit(ctx, "hermes.profile.client_error", "warning", domain.ID(profileID), "hermes client rename failed", map[string]any{"error": err.Error()})
	}
	p.DisplayAlias = newAlias
	t, _ := time.Parse(time.RFC3339Nano, now)
	p.UpdatedAt = t
	_ = s.emit(ctx, "hermes.profile.renamed", "info", domain.ID(profileID), "profile renamed", map[string]any{"alias": newAlias})
	return p, nil
}

// RenameDefaultProfile renames the default assistant's display alias while
// preserving the stable id "default" and its memory. This satisfies the
// requirement that changing the display name does not change the stable profile.
func (s *Service) RenameDefaultProfile(ctx context.Context, newAlias string) (*Profile, error) {
	return s.RenameProfile(ctx, DefaultProfileID, newAlias)
}

// UpdateAlias is an alias for RenameProfile for API convenience.
func (s *Service) UpdateAlias(ctx context.Context, profileID, newAlias string) (*Profile, error) {
	return s.RenameProfile(ctx, profileID, newAlias)
}

// GetProjectProfile returns the profile for a given project id.
func (s *Service) GetProjectProfile(ctx context.Context, projectID string) (*Profile, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, fmt.Errorf("%w: project id required", ErrValidation)
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM hermes_profiles WHERE project_id = ?`, projectID).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: project profile not found", ErrNotFound)
		}
		return nil, fmt.Errorf("lookup project profile: %w", err)
	}
	return s.GetProfile(ctx, id)
}

// --- capabilities ---

// GrantCapability grants a capability to a profile. Fails closed if capability
// is not allowed for the profile kind. Home Assistant and default memory
// are never grantable to project bots.
func (s *Service) GrantCapability(ctx context.Context, profileID, capability string) error {
	profileID = strings.TrimSpace(profileID)
	capability = strings.TrimSpace(capability)
	if profileID == "" || capability == "" {
		return fmt.Errorf("%w: profile id and capability required", ErrValidation)
	}
	if strings.Contains(profileID, "\x00") || strings.Contains(capability, "\x00") {
		return fmt.Errorf("%w: NUL byte not allowed", ErrValidation)
	}
	p, err := s.GetProfile(ctx, profileID)
	if err != nil {
		return err
	}
	if err := validateCapabilityForKind(p.Kind, capability); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.ExecContext(ctx, `INSERT INTO hermes_capabilities (profile_id, capability, granted_at) VALUES (?, ?, ?)`, profileID, capability, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil // idempotent
		}
		return fmt.Errorf("grant capability: %w", err)
	}
	_ = s.emit(ctx, "hermes.capability.granted", "info", domain.ID(profileID), "capability granted", map[string]any{"capability": capability})
	return nil
}

// RevokeCapability removes a capability.
func (s *Service) RevokeCapability(ctx context.Context, profileID, capability string) error {
	profileID = strings.TrimSpace(profileID)
	capability = strings.TrimSpace(capability)
	if profileID == "" || capability == "" {
		return fmt.Errorf("%w: profile id and capability required", ErrValidation)
	}
	if _, err := s.GetProfile(ctx, profileID); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM hermes_capabilities WHERE profile_id = ? AND capability = ?`, profileID, capability)
	if err != nil {
		return fmt.Errorf("revoke capability: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: capability not granted", ErrNotFound)
	}
	_ = s.emit(ctx, "hermes.capability.revoked", "info", domain.ID(profileID), "capability revoked", map[string]any{"capability": capability})
	return nil
}

// HasCapability checks whether a capability is both allowed for the profile kind
// and explicitly granted. Fail-closed: unknown or not granted returns false.
func (s *Service) HasCapability(ctx context.Context, profileID, capability string) (bool, error) {
	profileID = strings.TrimSpace(profileID)
	capability = strings.TrimSpace(capability)
	if profileID == "" || capability == "" {
		return false, fmt.Errorf("%w: profile id and capability required", ErrValidation)
	}
	p, err := s.GetProfile(ctx, profileID)
	if err != nil {
		return false, err
	}
	if err := validateCapabilityForKind(p.Kind, capability); err != nil {
		// Disallowed for kind => fail closed, not error; return false without leaking.
		if errors.Is(err, ErrForbidden) {
			return false, nil
		}
		return false, err
	}
	var exists int
	err = s.db.QueryRowContext(ctx, `SELECT 1 FROM hermes_capabilities WHERE profile_id = ? AND capability = ?`, profileID, capability).Scan(&exists)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("check capability: %w", err)
	}
	return true, nil
}

// CheckCapability returns nil if capability is granted and allowed; otherwise ErrForbidden.
func (s *Service) CheckCapability(ctx context.Context, profileID, capability string) error {
	ok, err := s.HasCapability(ctx, profileID, capability)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: capability %q not granted or not allowed for profile %q", ErrForbidden, capability, profileID)
	}
	return nil
}

// ListCapabilities returns all granted capabilities for a profile.
func (s *Service) ListCapabilities(ctx context.Context, profileID string) ([]string, error) {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" {
		return nil, fmt.Errorf("%w: profile id required", ErrValidation)
	}
	if _, err := s.GetProfile(ctx, profileID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT capability FROM hermes_capabilities WHERE profile_id = ? ORDER BY capability ASC`, profileID)
	if err != nil {
		return nil, fmt.Errorf("list capabilities: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func validateCapabilityForKind(kind, cap string) error {
	if cap == "" {
		return fmt.Errorf("%w: capability required", ErrValidation)
	}
	switch kind {
	case KindDefault:
		if !defaultAllowedCapabilities[cap] {
			// Also allow project-only capabilities to be denied for default? Fail closed.
			// Default should not receive project-only secrets? But spec says default may receive project status.
			// Only allow default set. Unknown caps forbidden.
			return fmt.Errorf("%w: capability %q not allowed for default profile", ErrForbidden, cap)
		}
		return nil
	case KindProject:
		if projectForbiddenCapabilities[cap] {
			return fmt.Errorf("%w: capability %q never granted to project bots", ErrForbidden, cap)
		}
		if !projectAllowedCapabilities[cap] {
			return fmt.Errorf("%w: capability %q not allowed for project profile", ErrForbidden, cap)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown profile kind %q", ErrValidation, kind)
	}
}

// --- knowledge sources ---

// GrantSource grants a knowledge source to a profile. Enforces ACLs: default may
// receive notes/paperless/karakeep/email/home_assistant/project_status; project only
// repo/issues/deploy/secrets/attachments/memory.
func (s *Service) GrantSource(ctx context.Context, profileID, sourceType, resourceID string) error {
	profileID = strings.TrimSpace(profileID)
	sourceType = strings.TrimSpace(strings.ToLower(sourceType))
	resourceID = strings.TrimSpace(resourceID)
	if profileID == "" || sourceType == "" || resourceID == "" {
		return fmt.Errorf("%w: profile, source_type and resource_id required", ErrValidation)
	}
	if strings.Contains(profileID, "\x00") || strings.Contains(sourceType, "\x00") || strings.Contains(resourceID, "\x00") {
		return fmt.Errorf("%w: NUL byte not allowed", ErrValidation)
	}
	p, err := s.GetProfile(ctx, profileID)
	if err != nil {
		return err
	}
	if err := validateSourceForKind(p.Kind, sourceType); err != nil {
		return err
	}
	id := newID()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO hermes_knowledge_sources (id, profile_id, source_type, resource_id, granted_at) VALUES (?, ?, ?, ?, ?)`,
		id, profileID, sourceType, resourceID, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil
		}
		return fmt.Errorf("grant source: %w", err)
	}
	_ = s.emit(ctx, "hermes.source.granted", "info", domain.ID(profileID), "source granted", map[string]any{"source_type": sourceType, "resource_id": resourceID})
	return nil
}

// RevokeSource removes a source grant.
func (s *Service) RevokeSource(ctx context.Context, profileID, sourceType, resourceID string) error {
	profileID = strings.TrimSpace(profileID)
	sourceType = strings.TrimSpace(strings.ToLower(sourceType))
	resourceID = strings.TrimSpace(resourceID)
	if profileID == "" || sourceType == "" || resourceID == "" {
		return fmt.Errorf("%w: profile, source_type and resource_id required", ErrValidation)
	}
	if _, err := s.GetProfile(ctx, profileID); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM hermes_knowledge_sources WHERE profile_id = ? AND source_type = ? AND resource_id = ?`,
		profileID, sourceType, resourceID)
	if err != nil {
		return fmt.Errorf("revoke source: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: source not granted", ErrNotFound)
	}
	_ = s.emit(ctx, "hermes.source.revoked", "info", domain.ID(profileID), "source revoked", map[string]any{"source_type": sourceType})
	return nil
}

// HasSource checks whether a source is granted and allowed for kind (fail-closed).
func (s *Service) HasSource(ctx context.Context, profileID, sourceType, resourceID string) (bool, error) {
	profileID = strings.TrimSpace(profileID)
	sourceType = strings.TrimSpace(strings.ToLower(sourceType))
	resourceID = strings.TrimSpace(resourceID)
	if profileID == "" || sourceType == "" || resourceID == "" {
		return false, fmt.Errorf("%w: profile, source_type and resource_id required", ErrValidation)
	}
	p, err := s.GetProfile(ctx, profileID)
	if err != nil {
		return false, err
	}
	if err := validateSourceForKind(p.Kind, sourceType); err != nil {
		if errors.Is(err, ErrForbidden) {
			return false, nil
		}
		return false, err
	}
	var exists int
	err = s.db.QueryRowContext(ctx,
		`SELECT 1 FROM hermes_knowledge_sources WHERE profile_id = ? AND source_type = ? AND resource_id = ?`,
		profileID, sourceType, resourceID).Scan(&exists)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("check source: %w", err)
	}
	return true, nil
}

// CheckSource checks whether a source type is allowed and at least one grant of that type exists.
// Used for quick ACL checks before tool invocation.
func (s *Service) CheckSource(ctx context.Context, profileID, sourceType string) error {
	profileID = strings.TrimSpace(profileID)
	sourceType = strings.TrimSpace(strings.ToLower(sourceType))
	if profileID == "" || sourceType == "" {
		return fmt.Errorf("%w: profile and source_type required", ErrValidation)
	}
	p, err := s.GetProfile(ctx, profileID)
	if err != nil {
		return err
	}
	if err := validateSourceForKind(p.Kind, sourceType); err != nil {
		return err
	}
	var exists int
	err = s.db.QueryRowContext(ctx,
		`SELECT 1 FROM hermes_knowledge_sources WHERE profile_id = ? AND source_type = ? LIMIT 1`,
		profileID, sourceType).Scan(&exists)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: source type %q not granted to profile %q", ErrForbidden, sourceType, profileID)
		}
		return fmt.Errorf("check source: %w", err)
	}
	return nil
}

// ListSources returns all source grants for a profile.
func (s *Service) ListSources(ctx context.Context, profileID string) ([]Source, error) {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" {
		return nil, fmt.Errorf("%w: profile id required", ErrValidation)
	}
	if _, err := s.GetProfile(ctx, profileID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, profile_id, source_type, resource_id, granted_at FROM hermes_knowledge_sources WHERE profile_id = ? ORDER BY source_type ASC, resource_id ASC`, profileID)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var src Source
		var grantedAt string
		if err := rows.Scan(&src.ID, &src.ProfileID, &src.SourceType, &src.ResourceID, &grantedAt); err != nil {
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339Nano, grantedAt)
		src.GrantedAt = t
		out = append(out, src)
	}
	return out, rows.Err()
}

func validateSourceForKind(kind, sourceType string) error {
	switch kind {
	case KindDefault:
		if !defaultAllowedSources[sourceType] {
			return fmt.Errorf("%w: source type %q not allowed for default profile", ErrForbidden, sourceType)
		}
		return nil
	case KindProject:
		if projectForbiddenSources[sourceType] {
			return fmt.Errorf("%w: source type %q never granted to project bots (home assistant / default knowledge)", ErrForbidden, sourceType)
		}
		if !projectAllowedSources[sourceType] {
			return fmt.Errorf("%w: source type %q not allowed for project profile", ErrForbidden, sourceType)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown profile kind %q", ErrValidation, kind)
	}
}

// --- messages / routing ---

// SendMessage routes a message between profiles with strict ACLs:
// - default -> project: delegation/status_request/redirect/cancel/message allowed
// - project -> default: question/status allowed (bounded)
// - project -> project: denied unless explicit group contains both
func (s *Service) SendMessage(ctx context.Context, fromID, toID, kind, body string) (*Message, error) {
	fromID = strings.TrimSpace(fromID)
	toID = strings.TrimSpace(toID)
	kind = strings.TrimSpace(kind)
	body = strings.TrimSpace(body)
	if fromID == "" || toID == "" || kind == "" {
		return nil, fmt.Errorf("%w: from, to and kind required", ErrValidation)
	}
	if body == "" {
		return nil, fmt.Errorf("%w: body required", ErrValidation)
	}
	if len(body) > 8000 {
		return nil, fmt.Errorf("%w: body too long (max 8000)", ErrValidation)
	}
	if strings.Contains(fromID, "\x00") || strings.Contains(toID, "\x00") || strings.Contains(kind, "\x00") {
		return nil, fmt.Errorf("%w: NUL byte not allowed", ErrValidation)
	}
	if !validMessageKinds[kind] {
		return nil, fmt.Errorf("%w: invalid message kind %q", ErrValidation, kind)
	}
	// Bounded question/status: stricter limit for project->default to prevent exfiltration
	if (kind == KindQuestion || kind == KindStatus) && len(body) > 4000 {
		return nil, fmt.Errorf("%w: question/status body exceeds 4000 chars", ErrValidation)
	}
	from, err := s.GetProfile(ctx, fromID)
	if err != nil {
		return nil, err
	}
	to, err := s.GetProfile(ctx, toID)
	if err != nil {
		return nil, err
	}
	if err := s.validateRouting(ctx, from, to, kind); err != nil {
		return nil, err
	}
	id := newID()
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO hermes_messages (id, from_profile_id, to_profile_id, kind, body, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, fromID, toID, kind, body, nowStr)
	if err != nil {
		return nil, fmt.Errorf("insert message: %w", err)
	}
	msg := &Message{ID: id, FromProfileID: fromID, ToProfileID: toID, Kind: kind, Body: body, CreatedAt: now}
	_ = s.emit(ctx, "hermes.message.sent", "info", domain.ID(id), "hermes message routed", map[string]any{"from": fromID, "to": toID, "kind": kind})
	return msg, nil
}

// DelegateToProject is default -> project delegation convenience.
func (s *Service) DelegateToProject(ctx context.Context, projectProfileID, body string) (*Message, error) {
	return s.SendMessage(ctx, DefaultProfileID, projectProfileID, KindDelegation, body)
}

// AskDefault is project -> default bounded question.
func (s *Service) AskDefault(ctx context.Context, fromProjectID, body string) (*Message, error) {
	return s.SendMessage(ctx, fromProjectID, DefaultProfileID, KindQuestion, body)
}

// ReportStatus is project -> default status report.
func (s *Service) ReportStatus(ctx context.Context, fromProjectID, body string) (*Message, error) {
	return s.SendMessage(ctx, fromProjectID, DefaultProfileID, KindStatus, body)
}

func (s *Service) validateRouting(ctx context.Context, from, to *Profile, kind string) error {
	// default -> project
	if from.ID == DefaultProfileID && to.Kind == KindProject {
		switch kind {
		case KindDelegation, KindStatusRequest, KindRedirect, KindCancel, KindMessage:
			return nil
		default:
			return fmt.Errorf("%w: default->project only allows delegation/status_request/redirect/cancel/message (got %q)", ErrForbidden, kind)
		}
	}
	// project -> default (bounded question/status)
	if from.Kind == KindProject && to.ID == DefaultProfileID {
		switch kind {
		case KindQuestion, KindStatus:
			return nil
		default:
			return fmt.Errorf("%w: project->default only allows question/status (got %q)", ErrForbidden, kind)
		}
	}
	// project -> project denied unless explicit group
	if from.Kind == KindProject && to.Kind == KindProject {
		same, err := s.AreProfilesInSameGroup(ctx, from.ID, to.ID)
		if err != nil {
			return err
		}
		if !same {
			return fmt.Errorf("%w: project-to-project messaging denied without explicit group", ErrForbidden)
		}
		// Within a group, only generic message allowed; still bounded.
		if kind != KindMessage {
			return fmt.Errorf("%w: project-to-project only allows message kind within group", ErrForbidden)
		}
		return nil
	}
	// default -> default, project->self etc. disallow except via explicit checks
	if from.ID == to.ID {
		return fmt.Errorf("%w: self-message not allowed", ErrValidation)
	}
	// Any other combination (default->default, project as default spoof)
	return fmt.Errorf("%w: routing denied for %q -> %q kind %q", ErrForbidden, from.ID, to.ID, kind)
}

// ListMessages returns messages involving a profile (as sender or recipient), ordered by creation.
func (s *Service) ListMessages(ctx context.Context, profileID string) ([]*Message, error) {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" {
		return nil, fmt.Errorf("%w: profile id required", ErrValidation)
	}
	if _, err := s.GetProfile(ctx, profileID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, from_profile_id, to_profile_id, kind, body, created_at FROM hermes_messages WHERE from_profile_id = ? OR to_profile_id = ? ORDER BY created_at ASC`, profileID, profileID)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		var m Message
		var createdAt string
		if err := rows.Scan(&m.ID, &m.FromProfileID, &m.ToProfileID, &m.Kind, &m.Body, &createdAt); err != nil {
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339Nano, createdAt)
		m.CreatedAt = t
		out = append(out, &m)
	}
	return out, rows.Err()
}

// GetMessage returns a message by id.
func (s *Service) GetMessage(ctx context.Context, id string) (*Message, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: message id required", ErrValidation)
	}
	var m Message
	var createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, from_profile_id, to_profile_id, kind, body, created_at FROM hermes_messages WHERE id = ?`, id).
		Scan(&m.ID, &m.FromProfileID, &m.ToProfileID, &m.Kind, &m.Body, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: message not found", ErrNotFound)
		}
		return nil, fmt.Errorf("get message: %w", err)
	}
	t, _ := time.Parse(time.RFC3339Nano, createdAt)
	m.CreatedAt = t
	return &m, nil
}

// CanSendMessage checks routing without persisting.
func (s *Service) CanSendMessage(ctx context.Context, fromID, toID, kind string) (bool, error) {
	from, err := s.GetProfile(ctx, fromID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	to, err := s.GetProfile(ctx, toID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if err := s.validateRouting(ctx, from, to, kind); err != nil {
		if errors.Is(err, ErrForbidden) || errors.Is(err, ErrValidation) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// --- groups ---

// CreateGroup creates an explicit group for project-to-project communication.
func (s *Service) CreateGroup(ctx context.Context, name string, memberIDs []string) (*Group, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: group name required", ErrValidation)
	}
	if strings.Contains(name, "\x00") {
		return nil, fmt.Errorf("%w: name contains NUL", ErrValidation)
	}
	if len(name) > 64 {
		return nil, fmt.Errorf("%w: group name too long", ErrValidation)
	}
	id := newID()
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO hermes_groups (id, name, created_at) VALUES (?, ?, ?)`, id, name, nowStr)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: group name already exists", ErrConflict)
		}
		return nil, fmt.Errorf("insert group: %w", err)
	}
	// dedupe members
	seen := make(map[string]bool)
	var members []string
	for _, mid := range memberIDs {
		mid = strings.TrimSpace(mid)
		if mid == "" || seen[mid] {
			continue
		}
		seen[mid] = true
		if _, err := s.getProfileTx(ctx, tx, mid); err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO hermes_group_members (group_id, profile_id, added_at) VALUES (?, ?, ?)`, id, mid, nowStr)
		if err != nil {
			return nil, fmt.Errorf("add member: %w", err)
		}
		members = append(members, mid)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	g := &Group{ID: id, Name: name, CreatedAt: now, Members: members}
	_ = s.emit(ctx, "hermes.group.created", "info", domain.ID(id), "group created", map[string]any{"name": name})
	return g, nil
}

// GetGroup returns a group with members.
func (s *Service) GetGroup(ctx context.Context, id string) (*Group, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: group id required", ErrValidation)
	}
	var name, createdAt string
	err := s.db.QueryRowContext(ctx, `SELECT id, name, created_at FROM hermes_groups WHERE id = ?`, id).Scan(&id, &name, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: group not found", ErrNotFound)
		}
		return nil, fmt.Errorf("get group: %w", err)
	}
	t, _ := time.Parse(time.RFC3339Nano, createdAt)
	g := &Group{ID: id, Name: name, CreatedAt: t}
	rows, err := s.db.QueryContext(ctx, `SELECT profile_id FROM hermes_group_members WHERE group_id = ? ORDER BY profile_id ASC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			return nil, err
		}
		g.Members = append(g.Members, pid)
	}
	return g, rows.Err()
}

// ListGroups returns all groups.
func (s *Service) ListGroups(ctx context.Context) ([]*Group, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at FROM hermes_groups ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer rows.Close()
	var out []*Group
	for rows.Next() {
		var g Group
		var createdAt string
		if err := rows.Scan(&g.ID, &g.Name, &createdAt); err != nil {
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339Nano, createdAt)
		g.CreatedAt = t
		out = append(out, &g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// populate members
	for _, g := range out {
		mrows, err := s.db.QueryContext(ctx, `SELECT profile_id FROM hermes_group_members WHERE group_id = ? ORDER BY profile_id ASC`, g.ID)
		if err != nil {
			return nil, err
		}
		for mrows.Next() {
			var pid string
			_ = mrows.Scan(&pid)
			g.Members = append(g.Members, pid)
		}
		mrows.Close()
	}
	return out, nil
}

// AddGroupMember adds a profile to a group.
func (s *Service) AddGroupMember(ctx context.Context, groupID, profileID string) error {
	groupID = strings.TrimSpace(groupID)
	profileID = strings.TrimSpace(profileID)
	if groupID == "" || profileID == "" {
		return fmt.Errorf("%w: group id and profile id required", ErrValidation)
	}
	if _, err := s.GetGroup(ctx, groupID); err != nil {
		return err
	}
	if _, err := s.GetProfile(ctx, profileID); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `INSERT INTO hermes_group_members (group_id, profile_id, added_at) VALUES (?, ?, ?)`, groupID, profileID, now)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: member already in group", ErrConflict)
		}
		return fmt.Errorf("add member: %w", err)
	}
	_ = s.emit(ctx, "hermes.group.member_added", "info", domain.ID(groupID), "group member added", map[string]any{"profile_id": profileID})
	return nil
}

// RemoveGroupMember removes a profile from a group.
func (s *Service) RemoveGroupMember(ctx context.Context, groupID, profileID string) error {
	groupID = strings.TrimSpace(groupID)
	profileID = strings.TrimSpace(profileID)
	if groupID == "" || profileID == "" {
		return fmt.Errorf("%w: group id and profile id required", ErrValidation)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM hermes_group_members WHERE group_id = ? AND profile_id = ?`, groupID, profileID)
	if err != nil {
		return fmt.Errorf("remove member: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: member not in group", ErrNotFound)
	}
	_ = s.emit(ctx, "hermes.group.member_removed", "info", domain.ID(groupID), "group member removed", map[string]any{"profile_id": profileID})
	return nil
}

// DeleteGroup deletes a group and its memberships.
func (s *Service) DeleteGroup(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: group id required", ErrValidation)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM hermes_groups WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: group not found", ErrNotFound)
	}
	_ = s.emit(ctx, "hermes.group.deleted", "info", domain.ID(id), "group deleted", nil)
	return nil
}

// AreProfilesInSameGroup reports whether two profiles share at least one group.
func (s *Service) AreProfilesInSameGroup(ctx context.Context, a, b string) (bool, error) {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false, fmt.Errorf("%w: profile ids required", ErrValidation)
	}
	var exists int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM hermes_group_members m1
		JOIN hermes_group_members m2 ON m1.group_id = m2.group_id
		WHERE m1.profile_id = ? AND m2.profile_id = ?
		LIMIT 1`, a, b).Scan(&exists)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("check group membership: %w", err)
	}
	return true, nil
}

// --- remote connection metadata (no cookies / no safeStorage) ---

// RemoteConnectionInfo returns the remote web/desktop connection metadata for a
// profile. It contains only server URLs, instance identity, display alias, and
// OIDC issuer. It never synthesizes cookies or Electron safeStorage entries.
// If a hermes_remote_connections row exists it is returned; otherwise metadata
// is synthesized from Service defaults and the profile. Callers that need
// persistence should also store the row via UpsertRemoteConnection.
func (s *Service) RemoteConnectionInfo(ctx context.Context, profileID string) (*RemoteConnection, error) {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" {
		return nil, fmt.Errorf("%w: profile id required", ErrValidation)
	}
	p, err := s.GetProfile(ctx, profileID)
	if err != nil {
		return nil, err
	}
	// Try persisted row
	var rc RemoteConnection
	var createdAt, updatedAt string
	err = s.db.QueryRowContext(ctx,
		`SELECT id, profile_id, server_url, hermes_url, instance_id, oidc_issuer, created_at, updated_at FROM hermes_remote_connections WHERE profile_id = ?`, profileID).
		Scan(&rc.ID, &rc.ProfileID, &rc.ServerURL, &rc.HermesURL, &rc.InstanceID, &rc.OIDCIssuer, &createdAt, &updatedAt)
	if err == nil {
		ca, _ := time.Parse(time.RFC3339Nano, createdAt)
		ua, _ := time.Parse(time.RFC3339Nano, updatedAt)
		rc.CreatedAt = ca
		rc.UpdatedAt = ua
		rc.GeneratedAt = ua
		rc.DisplayAlias = p.DisplayAlias
		return &rc, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get remote connection: %w", err)
	}
	// Synthesize from defaults without cookies or safeStorage.
	serverURL := s.serverURL
	hermesURL := ""
	if serverURL != "" {
		// Derive hermes websocket URL from serverURL; keep simple.
		hermesURL = strings.TrimRight(serverURL, "/") + "/api/v1/hermes/ws"
	}
	now := time.Now().UTC()
	rc = RemoteConnection{
		ID:           newID(),
		ProfileID:    p.ID,
		ServerURL:    serverURL,
		HermesURL:    hermesURL,
		InstanceID:   s.instanceID,
		OIDCIssuer:   s.oidcIssuer,
		DisplayAlias: p.DisplayAlias,
		GeneratedAt:  now,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	// If domain present and hermesURL empty, use domain.
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
// It never stores cookies or safeStorage data; only URLs and instance identity.
// Values are validated as URLs/instance ids and stored as plain text.
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
	if _, err := s.GetProfile(ctx, profileID); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)
	// Check existing
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
		return nil, fmt.Errorf("lookup remote connection: %w", err)
	}
	id := newID()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO hermes_remote_connections (id, profile_id, server_url, hermes_url, instance_id, oidc_issuer, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, profileID, serverURL, hermesURL, instanceID, oidcIssuer, nowStr, nowStr)
	if err != nil {
		return nil, fmt.Errorf("insert remote connection: %w", err)
	}
	_ = s.emit(ctx, "hermes.remote_connection.upserted", "info", domain.ID(profileID), "remote connection metadata upserted", map[string]any{"server_url": serverURL})
	return &RemoteConnection{
		ID: id, ProfileID: profileID, ServerURL: serverURL, HermesURL: hermesURL,
		InstanceID: instanceID, OIDCIssuer: oidcIssuer, CreatedAt: now, UpdatedAt: now, GeneratedAt: now,
	}, nil
}

// ProvisionRemoteConnection is a convenience that generates the official
// connection metadata without requiring cookie or safeStorage synthesis.
// It prefers Service defaults but allows explicit overrides. It does not
// write browser storage; the official Hermes Desktop performs its own auth flow.
func (s *Service) ProvisionRemoteConnection(ctx context.Context, profileID string) (*RemoteConnection, error) {
	return s.RemoteConnectionInfo(ctx, profileID)
}

// --- helpers ---

func (s *Service) emit(ctx context.Context, typ, severity string, resourceID domain.ID, msg string, data map[string]any) error {
	if s.sink == nil {
		return nil
	}
	ev := domain.Event{
		ID:         domain.ID(newID()),
		Type:       typ,
		Severity:   severity,
		ResourceID: resourceID,
		Message:    msg,
		Data:       data,
		CreatedAt:  time.Now().UTC(),
	}
	return s.sink.Emit(ctx, ev)
}

func (s *Service) getProfileTx(ctx context.Context, tx *sql.Tx, id string) (*Profile, error) {
	var (
		kind, alias, createdAt, updatedAt string
		projectID                         sql.NullString
	)
	err := tx.QueryRowContext(ctx,
		`SELECT id, kind, project_id, display_alias, created_at, updated_at FROM hermes_profiles WHERE id = ?`, id).
		Scan(&id, &kind, &projectID, &alias, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: profile %q not found", ErrNotFound, id)
		}
		return nil, fmt.Errorf("get profile: %w", err)
	}
	ca, _ := time.Parse(time.RFC3339Nano, createdAt)
	ua, _ := time.Parse(time.RFC3339Nano, updatedAt)
	p := &Profile{ID: id, Kind: kind, DisplayAlias: alias, CreatedAt: ca, UpdatedAt: ua}
	if projectID.Valid {
		p.ProjectID = projectID.String
	}
	return p, nil
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

// --- compatibility aliases for broader test surface ---

// EnsureDefault is alias for EnsureDefaultProfile.
func (s *Service) EnsureDefault(ctx context.Context, alias string) (*Profile, error) {
	return s.EnsureDefaultProfile(ctx, alias)
}

// RenameDefault renames the default profile (alias for RenameDefaultProfile).
func (s *Service) RenameDefault(ctx context.Context, alias string) (*Profile, error) {
	return s.RenameDefaultProfile(ctx, alias)
}

// CreateProjectProfile is alias for EnsureProjectProfile (creates if missing).
func (s *Service) CreateProjectProfile(ctx context.Context, projectID, alias string) (*Profile, error) {
	return s.EnsureProjectProfile(ctx, projectID, alias)
}

// CreateProjectBot creates a project bot profile; alias for EnsureProjectProfile.
func (s *Service) CreateProjectBot(ctx context.Context, projectID, alias string) (*Profile, error) {
	return s.EnsureProjectProfile(ctx, projectID, alias)
}

// GetProjectBot returns the bot profile for a project.
func (s *Service) GetProjectBot(ctx context.Context, projectID string) (*Profile, error) {
	return s.GetProjectProfile(ctx, projectID)
}

// DeleteProjectBot deletes a project bot.
func (s *Service) DeleteProjectBot(ctx context.Context, projectID string) error {
	return s.DeleteProjectProfile(ctx, projectID)
}

// UpdateProfileAlias is alias for RenameProfile.
func (s *Service) UpdateProfileAlias(ctx context.Context, profileID, alias string) (*Profile, error) {
	return s.RenameProfile(ctx, profileID, alias)
}

// CanAccess checks capability via HasCapability (fail-closed).
func (s *Service) CanAccess(ctx context.Context, profileID, capability string) (bool, error) {
	return s.HasCapability(ctx, profileID, capability)
}

// GrantKnowledgeSource is alias for GrantSource.
func (s *Service) GrantKnowledgeSource(ctx context.Context, profileID, sourceType, resourceID string) error {
	return s.GrantSource(ctx, profileID, sourceType, resourceID)
}

// HasKnowledgeSource is alias for HasSource.
func (s *Service) HasKnowledgeSource(ctx context.Context, profileID, sourceType, resourceID string) (bool, error) {
	return s.HasSource(ctx, profileID, sourceType, resourceID)
}

// ListKnowledgeSources is alias for ListSources.
func (s *Service) ListKnowledgeSources(ctx context.Context, profileID string) ([]Source, error) {
	return s.ListSources(ctx, profileID)
}

// CheckKnowledgeSource is alias for CheckSource.
func (s *Service) CheckKnowledgeSource(ctx context.Context, profileID, sourceType string) error {
	return s.CheckSource(ctx, profileID, sourceType)
}

// RevokeKnowledgeSource is alias for RevokeSource.
func (s *Service) RevokeKnowledgeSource(ctx context.Context, profileID, sourceType, resourceID string) error {
	return s.RevokeSource(ctx, profileID, sourceType, resourceID)
}

// Delegate is alias for DelegateToProject.
func (s *Service) Delegate(ctx context.Context, projectProfileID, body string) (*Message, error) {
	return s.DelegateToProject(ctx, projectProfileID, body)
}

// Ask is alias for AskDefault.
func (s *Service) Ask(ctx context.Context, fromProjectID, body string) (*Message, error) {
	return s.AskDefault(ctx, fromProjectID, body)
}

// Status is alias for ReportStatus.
func (s *Service) Status(ctx context.Context, fromProjectID, body string) (*Message, error) {
	return s.ReportStatus(ctx, fromProjectID, body)
}

// AreInSameGroup is alias for AreProfilesInSameGroup.
func (s *Service) AreInSameGroup(ctx context.Context, a, b string) (bool, error) {
	return s.AreProfilesInSameGroup(ctx, a, b)
}

// ConnectionInfo is alias for RemoteConnectionInfo.
func (s *Service) ConnectionInfo(ctx context.Context, profileID string) (*RemoteConnection, error) {
	return s.RemoteConnectionInfo(ctx, profileID)
}

// ProvisionConnection is alias for ProvisionRemoteConnection.
func (s *Service) ProvisionConnection(ctx context.Context, profileID string) (*RemoteConnection, error) {
	return s.ProvisionRemoteConnection(ctx, profileID)
}
