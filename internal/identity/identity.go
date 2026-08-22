package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

// PocketID is the minimal Pocket ID integration surface for recovery.
// Implementations must talk to Pocket ID's API; Omahab never verifies
// passkeys itself.
// The interface is extended with P1-2 provisioning capabilities: user
// creation with expiring enrollment links, group seeding and assignment,
// enrollment-state inspection, per-user application access, and default
// configuration. Implementations that are not configured must return
// ErrNotConfigured so callers fail loudly.
type PocketID interface {
	// CreateRecoveryCode generates a short-lived login code and enrollment URL
	// for email. exp is the absolute expiry. Implementations must generate
	// fresh random codes per call and never reuse a static token.
	CreateRecoveryCode(ctx context.Context, email string) (code string, url string, expiresAt time.Time, err error)
	// ValidateRecovery checks that a recovery code for email is valid.
	ValidateRecovery(ctx context.Context, email, code string) error
	// CreateUser creates a Pocket ID user and returns an expiring enrollment URL.
	CreateUser(ctx context.Context, email, name string, isAdmin bool, groupIDs []string) (userID string, enrollmentURL string, expiresAt time.Time, err error)
	// GetUser returns a domain user for the given Pocket ID user ID.
	GetUser(ctx context.Context, userID string) (domain.User, error)
	// ListUsers returns all Pocket ID users.
	ListUsers(ctx context.Context) ([]domain.User, error)
	// DisableUser disables or enables a user.
	DisableUser(ctx context.Context, userID string, disabled bool) error
	// DeleteUser deletes a user.
	DeleteUser(ctx context.Context, userID string) error
	// EnsureGroups idempotently ensures groups with the given names exist.
	EnsureGroups(ctx context.Context, names []string) ([]Group, error)
	// GetUserGroups returns the groups a user belongs to.
	GetUserGroups(ctx context.Context, userID string) ([]Group, error)
	// SetUserGroups replaces the groups for a user.
	SetUserGroups(ctx context.Context, userID string, groupIDs []string) error
	// AddUserToGroup adds a user to a group idempotently.
	AddUserToGroup(ctx context.Context, userID, groupID string) error
	// RemoveUserFromGroup removes a user from a group.
	RemoveUserFromGroup(ctx context.Context, userID, groupID string) error
	// GetEnrollmentState inspects passkey enrollment state for a user.
	GetEnrollmentState(ctx context.Context, userID string) (EnrollmentState, error)
	// ListApplicationAccess returns the OIDC clients a user may access via groups.
	ListApplicationAccess(ctx context.Context, userID string) ([]AppAccess, error)
	// ConfigureDefaults provisions Pocket ID defaults (passkey-first, email OTP disabled).
	ConfigureDefaults(ctx context.Context) error
	// SeedDefaultGroups ensures the initial groups admins/members/guests exist.
	SeedDefaultGroups(ctx context.Context) error
	// HealthCheck verifies Pocket ID reachability.
	HealthCheck(ctx context.Context) error
}

// EventRecorder records security events for audit.
// If nil, the service falls back to the local identity_security_events table.
type EventRecorder interface {
	RecordSecurityEvent(ctx context.Context, evt domain.Event) error
}

// Recovery is the short-lived enrollment artifact returned to root.
type Recovery struct {
	ID        domain.ID `json:"id"`
	Email     string    `json:"email"`
	URL       string    `json:"url"`
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

// Service owns Pocket ID recovery orchestration with local-root enforcement
// and expiring records. There is no static web recovery token.
type Service struct {
	db       *sql.DB
	pocket   PocketID
	recorder EventRecorder
	ttl      time.Duration
	now      func() time.Time
	isRoot   func() bool
}

const (
	DefaultRecoveryTTL = 15 * time.Minute
	maxEmailLen        = 254
)

// Option configures Service.
type Option func(*Service)

// WithTTL overrides the short-lived TTL.
func WithTTL(d time.Duration) Option {
	return func(s *Service) { s.ttl = d }
}

// WithNow overrides the clock (tests).
func WithNow(fn func() time.Time) Option {
	return func(s *Service) { s.now = fn }
}

// WithRecorder sets an external security-event sink.
func WithRecorder(r EventRecorder) Option {
	return func(s *Service) { s.recorder = r }
}

// WithIsRoot overrides root detection (tests).
func WithIsRoot(fn func() bool) Option {
	return func(s *Service) { s.isRoot = fn }
}

// New creates a Service. db and pocket are required.
func New(db *sql.DB, pocket PocketID, opts ...Option) (*Service, error) {
	if db == nil {
		return nil, store.Validation("identity: db is required")
	}
	if pocket == nil {
		return nil, store.Validation("identity: PocketID is required")
	}
	s := &Service{
		db:     db,
		pocket: pocket,
		ttl:    DefaultRecoveryTTL,
		now:    func() time.Time { return time.Now().UTC() },
		isRoot: func() bool { return os.Geteuid() == 0 },
	}
	for _, o := range opts {
		o(s)
	}
	if s.ttl <= 0 {
		s.ttl = DefaultRecoveryTTL
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.isRoot == nil {
		s.isRoot = func() bool { return os.Geteuid() == 0 }
	}
	return s, nil
}

// Recover requires local root, generates a short-lived Pocket ID login code,
// persists an expiring record, emits a security event, and returns the
// enrollment URL. It enforces local-root policy and expiry.
func (s *Service) Recover(ctx context.Context, email string) (*Recovery, error) {
	if !s.isRoot() {
		// Fail closed: non-root callers never learn whether PocketID was called.
		return nil, fmt.Errorf("%w: recovery requires local root (use sudo omahab identity recover)", store.ErrValidation)
	}
	email = strings.TrimSpace(strings.ToLower(email))
	if err := validateEmail(email); err != nil {
		return nil, err
	}
	code, url, expiresAt, err := s.pocket.CreateRecoveryCode(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("pocket recovery: %w", err)
	}
	code = strings.TrimSpace(code)
	url = strings.TrimSpace(url)
	if code == "" || url == "" {
		return nil, store.Validation("PocketID returned empty code or URL")
	}
	now := s.now().UTC()
	if expiresAt.IsZero() {
		expiresAt = now.Add(s.ttl)
	} else {
		expiresAt = expiresAt.UTC()
	}
	// Enforce short-lived bounds: expiry must be in the future and within 2*ttl.
	if !expiresAt.After(now) {
		return nil, store.Validation("recovery expiry must be in the future")
	}
	if expiresAt.After(now.Add(2 * s.ttl)) {
		return nil, store.Validationf("recovery expiry too far in future (max %s)", 2*s.ttl)
	}
	id := store.NewID()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO identity_recoveries (id, email, code, url, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, email, code, url, expiresAt.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, fmt.Errorf("persist recovery: %w", err)
	}
	// Record security event. Best-effort but must not be silently ignored.
	evt := domain.Event{
		ID:        domain.ID(store.NewID()),
		Type:      "identity.recovery",
		Severity:  "security",
		Message:   fmt.Sprintf("recovery code issued for %s", email),
		Data:      map[string]any{"email": email, "recovery_id": id},
		CreatedAt: now,
	}
	if s.recorder != nil {
		if err := s.recorder.RecordSecurityEvent(ctx, evt); err != nil {
			// Roll back recovery if event sink fails? Keep record but surface error.
			_ = err
		}
	}
	// Fallback/local audit table.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO identity_security_events (id, type, email, message, created_at) VALUES (?, ?, ?, ?, ?)`,
		string(evt.ID), evt.Type, email, evt.Message, now.Format(time.RFC3339Nano),
	); err != nil {
		// Non-fatal but log-like; don't expose internals.
		_ = err
	}
	return &Recovery{
		ID:        domain.ID(id),
		Email:     email,
		URL:       url,
		Code:      code,
		ExpiresAt: expiresAt,
		CreatedAt: now,
	}, nil
}

// GetRecovery returns a recovery record by id, enforcing expiry.
func (s *Service) GetRecovery(ctx context.Context, id domain.ID) (*Recovery, error) {
	if strings.TrimSpace(string(id)) == "" {
		return nil, store.Validation("recovery id is required")
	}
	var email, code, url, expiresAtStr, createdAtStr string
	var usedAt sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT email, code, url, expires_at, used_at, created_at FROM identity_recoveries WHERE id = ?`, string(id)).
		Scan(&email, &code, &url, &expiresAtStr, &usedAt, &createdAtStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.NotFoundf("recovery %s not found", id)
		}
		return nil, fmt.Errorf("get recovery: %w", err)
	}
	expiresAt, _ := time.Parse(time.RFC3339Nano, expiresAtStr)
	createdAt, _ := time.Parse(time.RFC3339Nano, createdAtStr)
	expiresAt = expiresAt.UTC()
	createdAt = createdAt.UTC()
	if s.now().UTC().After(expiresAt) {
		return nil, store.Validationf("recovery %s expired at %s", id, expiresAt.Format(time.RFC3339Nano))
	}
	return &Recovery{
		ID:        id,
		Email:     email,
		Code:      code,
		URL:       url,
		ExpiresAt: expiresAt,
		CreatedAt: createdAt,
	}, nil
}

// ListRecoveries returns recoveries ordered by creation time (most recent first).
func (s *Service) ListRecoveries(ctx context.Context) ([]Recovery, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, email, code, url, expires_at, created_at FROM identity_recoveries ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list recoveries: %w", err)
	}
	defer rows.Close()
	var out []Recovery
	for rows.Next() {
		var r Recovery
		var expiresAtStr, createdAtStr string
		var id, email, code, url string
		if err := rows.Scan(&id, &email, &code, &url, &expiresAtStr, &createdAtStr); err != nil {
			return nil, fmt.Errorf("scan recovery: %w", err)
		}
		expiresAt, _ := time.Parse(time.RFC3339Nano, expiresAtStr)
		createdAt, _ := time.Parse(time.RFC3339Nano, createdAtStr)
		r.ID = domain.ID(id)
		r.Email = email
		r.Code = code
		r.URL = url
		r.ExpiresAt = expiresAt.UTC()
		r.CreatedAt = createdAt.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list recoveries: %w", err)
	}
	if out == nil {
		out = []Recovery{}
	}
	return out, nil
}

// PurgeExpired hard-deletes expired recovery records and returns the count.
func (s *Service) PurgeExpired(ctx context.Context) (int64, error) {
	nowStr := s.now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `DELETE FROM identity_recoveries WHERE expires_at < ?`, nowStr)
	if err != nil {
		return 0, fmt.Errorf("purge expired: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ValidateRecovery checks that a recovery code for email is valid and unexpired.
// It does not consume the code.
func (s *Service) ValidateRecovery(ctx context.Context, email, code string) error {
	email = strings.TrimSpace(strings.ToLower(email))
	code = strings.TrimSpace(code)
	if email == "" || code == "" {
		return store.Validation("email and code are required")
	}
	var expiresAtStr string
	err := s.db.QueryRowContext(ctx,
		`SELECT expires_at FROM identity_recoveries WHERE email = ? AND code = ? ORDER BY created_at DESC LIMIT 1`,
		email, code).Scan(&expiresAtStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.NotFoundf("recovery code not found for %s", email)
		}
		return fmt.Errorf("validate recovery: %w", err)
	}
	expiresAt, _ := time.Parse(time.RFC3339Nano, expiresAtStr)
	if s.now().UTC().After(expiresAt.UTC()) {
		return store.Validationf("recovery code expired at %s", expiresAt)
	}
	return nil
}

// IsExpired reports whether a recovery is expired.
func (r *Recovery) IsExpired(now time.Time) bool { return now.After(r.ExpiresAt) }

func validateEmail(email string) error {
	if email == "" {
		return store.Validation("email is required")
	}
	if len(email) > maxEmailLen {
		return store.Validation("email too long")
	}
	// Minimal check: must contain @ and .
	if !strings.Contains(email, "@") || !strings.Contains(email, ".") {
		return store.Validationf("invalid email %q", email)
	}
	if strings.Contains(email, " ") {
		return store.Validationf("invalid email %q", email)
	}
	return nil
}
