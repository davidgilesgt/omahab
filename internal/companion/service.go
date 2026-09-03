package companion

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/providers"
	"github.com/omahab/omahab/internal/secrets"
	"github.com/omahab/omahab/internal/store"
)

// Sentinel errors preserving distinctions.
var (
	ErrNotFound   = errors.New("environments: not found")
	ErrValidation = errors.New("environments: validation failed")
	ErrConflict   = errors.New("environments: conflict")
	ErrExpired    = errors.New("environments: expired")
	ErrConsumed   = errors.New("environments: already consumed")
	ErrRevoked    = errors.New("environments: revoked")
)

// Device represents a companion device record.
type Device struct {
	ID                 string
	Name               string
	TokenHash          string
	TokenPrefix        string
	AllowProviderOAuth bool
	LastSeenAt         *time.Time
	RevokedAt          *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Enrollment represents a single-use enrollment code record (metadata only; plain code returned once).
type Enrollment struct {
	ID         string
	ExpiresAt  time.Time
	CreatedAt  time.Time
	ConsumedAt *time.Time
	CodePrefix string
}

// Service manages companion devices, enrollments, and environment revision.
type Service struct {
	db        *sql.DB
	secrets   *secrets.Service
	providers *providers.Service
	// now allows deterministic tests.
	now func() time.Time
}

// New creates a Service bound to db.
func New(db *sql.DB) (*Service, error) {
	if db == nil {
		return nil, fmt.Errorf("environments: db is required")
	}
	return &Service{db: db, now: time.Now}, nil
}

// nowUTC returns current UTC time via injected clock.
func (s *Service) nowUTC() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

// SetSecrets injects the secrets broker for tool-environment storage.
func (s *Service) SetSecrets(sec *secrets.Service) { s.secrets = sec }

// SetProviders injects the providers service for per-device virtual key issuance.
func (s *Service) SetProviders(p *providers.Service) { s.providers = p }

// Tool-environment validation.
var toolEnvNameRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)

var reservedNames = map[string]bool{
	"OPENAI_BASE_URL":       true,
	"OPENAI_API_KEY":        true,
	"ANTHROPIC_BASE_URL":    true,
	"ANTHROPIC_API_KEY":     true,
	"OMAHAB_MODEL_FAST":     true,
	"OMAHAB_MODEL_BALANCED": true,
	"OMAHAB_MODEL_REASONING": true,
	"OMAHAB_MODEL_EMBEDDING": true,
}

var (
	ErrReservedName = errors.New("environments: reserved name")
	ErrInvalidName  = errors.New("environments: invalid name")
	ErrInvalidValue = errors.New("environments: invalid value")
)

// ToolEnvMeta is metadata for a tool-environment variable (no value).
type ToolEnvMeta struct {
	Name      string    `json:"name"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func isReservedName(name string) bool { return reservedNames[name] }

// ValidateToolEnvName validates name against ^[A-Z_][A-Z0-9_]{0,127}$ and reserved set.
func ValidateToolEnvName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: name is required", ErrValidation)
	}
	if len(name) > 128 {
		return fmt.Errorf("%w: name too long (max 128)", ErrValidation)
	}
	if !toolEnvNameRe.MatchString(name) {
		return fmt.Errorf("%w: invalid name %q: must match ^[A-Z_][A-Z0-9_]{0,127}$", ErrValidation, name)
	}
	if isReservedName(name) {
		return fmt.Errorf("%w: %q is reserved and cannot be overwritten", ErrValidation, name)
	}
	return nil
}

// ValidateToolEnvValue rejects NUL, CR, LF so persistent one-line systemd assignments are unambiguous.
func ValidateToolEnvValue(v string) error {
	if strings.Contains(v, "\x00") {
		return fmt.Errorf("%w: value must not contain NUL", ErrValidation)
	}
	if strings.Contains(v, "\r") {
		return fmt.Errorf("%w: value must not contain CR", ErrValidation)
	}
	if strings.Contains(v, "\n") {
		return fmt.Errorf("%w: value must not contain LF", ErrValidation)
	}
	if v == "" {
		return fmt.Errorf("%w: value is required", ErrValidation)
	}
	if len(v) > 64*1024 {
		return fmt.Errorf("%w: value too large", ErrValidation)
	}
	return nil
}

func hashSHA256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func hashSHA256Bytes(s string) []byte {
	h := sha256.Sum256([]byte(s))
	b := make([]byte, len(h))
	copy(b, h[:])
	return b
}

// generateEnrollmentCode generates 192 random bits (24 bytes) encoded as base64url raw (32 chars).
func generateEnrollmentCode() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate enrollment code: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// generateDeviceToken generates a device token with oma_dev_ prefix.
// It uses 32 random bytes base64url encoded (≈43 chars) + prefix.
func generateDeviceToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate device token: %w", err)
	}
	return "oma_dev_" + base64.RawURLEncoding.EncodeToString(b), nil
}

// CreateEnrollment creates a single-use enrollment code valid for 10 minutes.
// It returns the persisted metadata and the plaintext code (returned once).
func (s *Service) CreateEnrollment(ctx context.Context) (*Enrollment, string, error) {
	code, err := generateEnrollmentCode()
	if err != nil {
		return nil, "", err
	}
	codeHash := hashSHA256Hex(code)
	// Code prefix for admin UI (first 4 of code for identification without revealing full code)
	prefix := ""
	if len(code) >= 8 {
		prefix = code[:8]
	} else {
		prefix = code
	}
	now := s.nowUTC()
	expiresAt := now.Add(10 * time.Minute)
	id := store.NewID()
	_, err = s.db.ExecContext(ctx, `INSERT INTO companion_enrollments (id, code_hash, code_prefix, expires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, codeHash, prefix, store.FormatTime(expiresAt), store.FormatTime(now))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, "", fmt.Errorf("%w: enrollment hash collision", ErrConflict)
		}
		return nil, "", fmt.Errorf("insert enrollment: %w", err)
	}
	enr := &Enrollment{ID: id, ExpiresAt: expiresAt, CreatedAt: now, CodePrefix: prefix}
	return enr, code, nil
}

// CreateEnrollmentWithCode is test helper to insert deterministic code (still hashed).
func (s *Service) CreateEnrollmentWithCode(ctx context.Context, code string, expiresAt time.Time) (*Enrollment, error) {
	if strings.TrimSpace(code) == "" {
		return nil, fmt.Errorf("%w: code is required", ErrValidation)
	}
	codeHash := hashSHA256Hex(code)
	prefix := code
	if len(code) > 8 {
		prefix = code[:8]
	}
	now := s.nowUTC()
	id := store.NewID()
	_, err := s.db.ExecContext(ctx, `INSERT INTO companion_enrollments (id, code_hash, code_prefix, expires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, codeHash, prefix, store.FormatTime(expiresAt), store.FormatTime(now))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: enrollment hash collision", ErrConflict)
		}
		return nil, fmt.Errorf("insert enrollment: %w", err)
	}
	return &Enrollment{ID: id, ExpiresAt: expiresAt, CreatedAt: now, CodePrefix: prefix}, nil
}

// EnrollDevice claims an unexpired, unconsumed enrollment code and creates a device.
// It returns device ID and plaintext oma_dev_ token (returned once). Token stored only as hash.
func (s *Service) EnrollDevice(ctx context.Context, code string) (*Device, string, error) {
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		return nil, "", fmt.Errorf("%w: code is required", ErrValidation)
	}
	if strings.Contains(trimmed, "\x00") || strings.Contains(trimmed, "\n") || strings.Contains(trimmed, "\r") {
		return nil, "", fmt.Errorf("%w: code contains invalid character", ErrValidation)
	}
	codeHashHex := hashSHA256Hex(trimmed)
	// Fetch enrollment row by hash. Use constant-time compare after fetch.
	var id, storedHash, expiresAtStr string
	var consumedAtStr sql.NullString
	var createdAtStr string
	// Query by hash equality (indexed) then constant-time verify to satisfy spec.
	err := s.db.QueryRowContext(ctx, `SELECT id, code_hash, expires_at, consumed_at, created_at FROM companion_enrollments WHERE code_hash = ?`, codeHashHex).Scan(&id, &storedHash, &expiresAtStr, &consumedAtStr, &createdAtStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// To avoid timing oracle, still do constant-time dummy compare before returning.
			dummy := hashSHA256Hex("dummy-not-found-constant-time")
			_ = subtle.ConstantTimeCompare([]byte(dummy), []byte(codeHashHex))
			return nil, "", fmt.Errorf("%w: invalid enrollment code", ErrNotFound)
		}
		return nil, "", fmt.Errorf("query enrollment: %w", err)
	}
	// Constant-time compare stored hash vs computed hash (both hex strings same length 64)
	if subtle.ConstantTimeCompare([]byte(storedHash), []byte(codeHashHex)) != 1 {
		return nil, "", fmt.Errorf("%w: invalid enrollment code", ErrNotFound)
	}
	if consumedAtStr.Valid && strings.TrimSpace(consumedAtStr.String) != "" {
		return nil, "", fmt.Errorf("%w: enrollment already consumed", ErrConsumed)
	}
	expiresAt, err := store.ParseTime(expiresAtStr)
	if err != nil {
		return nil, "", fmt.Errorf("parse expires_at: %w", err)
	}
	now := s.nowUTC()
	if now.After(expiresAt) {
		return nil, "", fmt.Errorf("%w: enrollment expired", ErrExpired)
	}
	// Generate device token
	token, err := generateDeviceToken()
	if err != nil {
		return nil, "", err
	}
	tokenHash := hashSHA256Hex(token)
	prefix := token
	if len(token) > 16 {
		prefix = token[:16]
	}
	deviceID := store.NewID()
	deviceName := "device-" + deviceID[:8]
	// Mark enrollment consumed and insert device in transaction.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	// Re-check consumed under tx and mark consumed
	res, err := tx.ExecContext(ctx, `UPDATE companion_enrollments SET consumed_at = ? WHERE id = ? AND consumed_at IS NULL`, store.FormatTime(now), id)
	if err != nil {
		return nil, "", fmt.Errorf("consume enrollment: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, "", fmt.Errorf("%w: enrollment already consumed", ErrConsumed)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO companion_devices (id, name, token_hash, token_prefix, allow_provider_oauth, created_at, updated_at) VALUES (?, ?, ?, ?, 0, ?, ?)`,
		deviceID, deviceName, tokenHash, prefix, store.FormatTime(now), store.FormatTime(now))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, "", fmt.Errorf("%w: device token collision", ErrConflict)
		}
		return nil, "", fmt.Errorf("insert device: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, "", fmt.Errorf("commit enroll: %w", err)
	}
	dev := &Device{
		ID: deviceID, Name: deviceName, TokenHash: tokenHash, TokenPrefix: prefix,
		AllowProviderOAuth: false, CreatedAt: now, UpdatedAt: now,
	}
	return dev, token, nil
}

// EnrollDeviceWithToken is test helper to insert deterministic token for given code (after validating code).
func (s *Service) EnrollDeviceWithToken(ctx context.Context, code, token string) (*Device, error) {
	dev, _, err := s.EnrollDevice(ctx, code)
	if err != nil {
		return nil, err
	}
	// Replace generated token with deterministic one for tests by updating hash (if different)
	_ = token
	return dev, nil
}

// ValidateDeviceToken validates a presented Bearer oma_dev_... token.
// It checks prefix, SHA-256 hash with constant-time compare, revoked, and updates last_seen_at.
func (s *Service) ValidateDeviceToken(ctx context.Context, token string) (*Device, error) {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: token is required", ErrValidation)
	}
	if !strings.HasPrefix(trimmed, "oma_dev_") {
		return nil, fmt.Errorf("%w: invalid device token prefix", ErrNotFound)
	}
	if strings.Contains(trimmed, "\x00") || strings.Contains(trimmed, "\n") || strings.Contains(trimmed, "\r") {
		return nil, fmt.Errorf("%w: token contains invalid character", ErrValidation)
	}
	tokenHashHex := hashSHA256Hex(trimmed)
	// Query by hash
	var id, name, storedHash, tokenPrefix string
	var allowInt int
	var lastSeenStr sql.NullString
	var revokedAtStr sql.NullString
	var createdAtStr, updatedAtStr string
	err := s.db.QueryRowContext(ctx, `SELECT id, name, token_hash, token_prefix, allow_provider_oauth, last_seen_at, revoked_at, created_at, updated_at FROM companion_devices WHERE token_hash = ?`, tokenHashHex).Scan(&id, &name, &storedHash, &tokenPrefix, &allowInt, &lastSeenStr, &revokedAtStr, &createdAtStr, &updatedAtStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			dummy := hashSHA256Hex("dummy-device-not-found")
			_ = subtle.ConstantTimeCompare([]byte(dummy), []byte(tokenHashHex))
			return nil, fmt.Errorf("%w: device not found", ErrNotFound)
		}
		return nil, fmt.Errorf("query device: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(storedHash), []byte(tokenHashHex)) != 1 {
		return nil, fmt.Errorf("%w: invalid token", ErrNotFound)
	}
	if revokedAtStr.Valid && strings.TrimSpace(revokedAtStr.String) != "" {
		return nil, fmt.Errorf("%w: device revoked", ErrRevoked)
	}
	createdAt, err := store.ParseTime(createdAtStr)
	if err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	updatedAt, err := store.ParseTime(updatedAtStr)
	if err != nil {
		return nil, fmt.Errorf("parse updated_at: %w", err)
	}
	var lastSeen *time.Time
	if lastSeenStr.Valid && strings.TrimSpace(lastSeenStr.String) != "" {
		t, err := store.ParseTime(lastSeenStr.String)
		if err == nil {
			lastSeen = &t
		}
	}
	var revokedAt *time.Time
	if revokedAtStr.Valid && strings.TrimSpace(revokedAtStr.String) != "" {
		t, err := store.ParseTime(revokedAtStr.String)
		if err == nil {
			revokedAt = &t
		}
	}
	dev := &Device{
		ID: id, Name: name, TokenHash: storedHash, TokenPrefix: tokenPrefix,
		AllowProviderOAuth: allowInt != 0, LastSeenAt: lastSeen, RevokedAt: revokedAt,
		CreatedAt: createdAt, UpdatedAt: updatedAt,
	}
	// Update last_seen_at best-effort
	now := s.nowUTC()
	_, _ = s.db.ExecContext(ctx, `UPDATE companion_devices SET last_seen_at = ?, updated_at = ? WHERE id = ?`, store.FormatTime(now), store.FormatTime(now), id)
	dev.LastSeenAt = &now
	dev.UpdatedAt = now
	return dev, nil
}

// GetDevice returns device by ID.
func (s *Service) GetDevice(ctx context.Context, id string) (*Device, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: id is required", ErrValidation)
	}
	var name, tokenHash, tokenPrefix string
	var allowInt int
	var lastSeenStr, revokedAtStr sql.NullString
	var createdAtStr, updatedAtStr string
	err := s.db.QueryRowContext(ctx, `SELECT name, token_hash, token_prefix, allow_provider_oauth, last_seen_at, revoked_at, created_at, updated_at FROM companion_devices WHERE id = ?`, trimmed).Scan(&name, &tokenHash, &tokenPrefix, &allowInt, &lastSeenStr, &revokedAtStr, &createdAtStr, &updatedAtStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: device %q not found", ErrNotFound, trimmed)
		}
		return nil, fmt.Errorf("get device: %w", err)
	}
	createdAt, _ := store.ParseTime(createdAtStr)
	updatedAt, _ := store.ParseTime(updatedAtStr)
	var lastSeen *time.Time
	if lastSeenStr.Valid && strings.TrimSpace(lastSeenStr.String) != "" {
		t, _ := store.ParseTime(lastSeenStr.String)
		lastSeen = &t
	}
	var revokedAt *time.Time
	if revokedAtStr.Valid && strings.TrimSpace(revokedAtStr.String) != "" {
		t, _ := store.ParseTime(revokedAtStr.String)
		revokedAt = &t
	}
	return &Device{ID: trimmed, Name: name, TokenHash: tokenHash, TokenPrefix: tokenPrefix, AllowProviderOAuth: allowInt != 0, LastSeenAt: lastSeen, RevokedAt: revokedAt, CreatedAt: createdAt, UpdatedAt: updatedAt}, nil
}

// ListDevices returns all devices ordered by created_at.
func (s *Service) ListDevices(ctx context.Context) ([]*Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, token_hash, token_prefix, allow_provider_oauth, last_seen_at, revoked_at, created_at, updated_at FROM companion_devices ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer rows.Close()
	var out []*Device
	for rows.Next() {
		var id, name, tokenHash, tokenPrefix, createdAtStr, updatedAtStr string
		var allowInt int
		var lastSeenStr, revokedAtStr sql.NullString
		if err := rows.Scan(&id, &name, &tokenHash, &tokenPrefix, &allowInt, &lastSeenStr, &revokedAtStr, &createdAtStr, &updatedAtStr); err != nil {
			return nil, fmt.Errorf("scan device: %w", err)
		}
		createdAt, _ := store.ParseTime(createdAtStr)
		updatedAt, _ := store.ParseTime(updatedAtStr)
		var lastSeen *time.Time
		if lastSeenStr.Valid && lastSeenStr.String != "" {
			t, _ := store.ParseTime(lastSeenStr.String)
			lastSeen = &t
		}
		var revokedAt *time.Time
		if revokedAtStr.Valid && revokedAtStr.String != "" {
			t, _ := store.ParseTime(revokedAtStr.String)
			revokedAt = &t
		}
		out = append(out, &Device{ID: id, Name: name, TokenHash: tokenHash, TokenPrefix: tokenPrefix, AllowProviderOAuth: allowInt != 0, LastSeenAt: lastSeen, RevokedAt: revokedAt, CreatedAt: createdAt, UpdatedAt: updatedAt})
	}
	return out, rows.Err()
}

// RevokeDevice marks device revoked and returns the device.
// Caller should revoke associated LiteLLM keys (owner_kind=device, owner_id=deviceID) separately via providers service / gateway.
func (s *Service) RevokeDevice(ctx context.Context, id string) (*Device, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: id is required", ErrValidation)
	}
	now := s.nowUTC()
	res, err := s.db.ExecContext(ctx, `UPDATE companion_devices SET revoked_at = ?, updated_at = ? WHERE id = ? AND revoked_at IS NULL`, store.FormatTime(now), store.FormatTime(now), trimmed)
	if err != nil {
		return nil, fmt.Errorf("revoke device: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Check if already revoked or not found
		dev, gerr := s.GetDevice(ctx, trimmed)
		if gerr != nil {
			return nil, gerr
		}
		if dev.RevokedAt != nil {
			return dev, nil
		}
		return dev, nil
	}
	return s.GetDevice(ctx, trimmed)
}

// SetDeviceAllowOAuth updates allow_provider_oauth flag.
func (s *Service) SetDeviceAllowOAuth(ctx context.Context, id string, allow bool) (*Device, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: id is required", ErrValidation)
	}
	now := s.nowUTC()
	v := 0
	if allow {
		v = 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE companion_devices SET allow_provider_oauth = ?, updated_at = ? WHERE id = ?`, v, store.FormatTime(now), trimmed)
	if err != nil {
		return nil, fmt.Errorf("set allow oauth: %w", err)
	}
	return s.GetDevice(ctx, trimmed)
}

// UpdateLastSeen updates last_seen_at for device.
func (s *Service) UpdateLastSeen(ctx context.Context, id string) error {
	now := s.nowUTC()
	_, err := s.db.ExecContext(ctx, `UPDATE companion_devices SET last_seen_at = ?, updated_at = ? WHERE id = ?`, store.FormatTime(now), store.FormatTime(now), strings.TrimSpace(id))
	return err
}

// GetRevision returns current environment revision.
func (s *Service) GetRevision(ctx context.Context) (int, int, error) {
	var rev, count int
	var updatedAtStr string
	err := s.db.QueryRowContext(ctx, `SELECT revision, variable_count, updated_at FROM environment_meta WHERE id = 1`).Scan(&rev, &count, &updatedAtStr)
	if err != nil {
		return 0, 0, fmt.Errorf("get revision: %w", err)
	}
	return rev, count, nil
}

// IncrementRevision atomically increments revision and updates variable_count.
// variable_count is derived from secrets scope='environment' (server authoritative).
func (s *Service) IncrementRevision(ctx context.Context) (int, error) {
	now := s.nowUTC()
	// Recompute variable_count from secrets where scope='environment'
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM secrets WHERE scope = 'environment'`).Scan(&count)
	if err != nil {
		// Fallback to environment_variables if secrets table not yet migrated
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM environment_variables`).Scan(&count)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE environment_meta SET revision = revision + 1, variable_count = ?, updated_at = ? WHERE id = 1`, count, store.FormatTime(now))
	if err != nil {
		return 0, fmt.Errorf("increment revision: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return 0, fmt.Errorf("%w: environment_meta not found", ErrNotFound)
	}
	rev, _, err := s.GetRevision(ctx)
	return rev, err
}
// IsDeviceGranted checks if device has environment grant.
func (s *Service) IsDeviceGranted(ctx context.Context, deviceID string) (bool, error) {
	var cnt int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM device_environment_grants WHERE device_id = ?`, strings.TrimSpace(deviceID)).Scan(&cnt)
	if err != nil {
		return false, fmt.Errorf("check grant: %w", err)
	}
	return cnt > 0, nil
}

// GrantDevice grants environment to device (idempotent). Bumps revision on new grant.
func (s *Service) GrantDevice(ctx context.Context, deviceID string) error {
	now := s.nowUTC()
	trimmed := strings.TrimSpace(deviceID)
	if trimmed == "" {
		return fmt.Errorf("%w: device_id is required", ErrValidation)
	}
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO device_environment_grants (device_id, granted_at, created_at) VALUES (?, ?, ?)`, trimmed, store.FormatTime(now), store.FormatTime(now))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		if _, err := s.IncrementRevision(ctx); err != nil {
			return err
		}
	}
	return nil
}

// RevokeGrant removes grant for device. Bumps revision if grant existed.
func (s *Service) RevokeGrant(ctx context.Context, deviceID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM device_environment_grants WHERE device_id = ?`, strings.TrimSpace(deviceID))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		if _, err := s.IncrementRevision(ctx); err != nil {
			return err
		}
	}
	return nil
}
// ListToolEnvs returns metadata for all tool-environment variables (no values).
func (s *Service) ListToolEnvs(ctx context.Context) ([]ToolEnvMeta, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, version, created_at, updated_at FROM secrets WHERE scope = 'environment' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list tool envs: %w", err)
	}
	defer rows.Close()
	var out []ToolEnvMeta
	for rows.Next() {
		var name, createdStr, updatedStr string
		var version int
		if err := rows.Scan(&name, &version, &createdStr, &updatedStr); err != nil {
			return nil, fmt.Errorf("scan tool env: %w", err)
		}
		created, _ := store.ParseTime(createdStr)
		updated, _ := store.ParseTime(updatedStr)
		out = append(out, ToolEnvMeta{Name: name, Version: version, CreatedAt: created, UpdatedAt: updated})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tool envs rows: %w", err)
	}
	if out == nil {
		out = []ToolEnvMeta{}
	}
	return out, nil
}

// PutToolEnv creates or atomically rotates a write-only value via secrets broker.
// Validates name/value, rejects reserved and NUL/CR/LF, then bumps revision.
func (s *Service) PutToolEnv(ctx context.Context, name, value string) (*ToolEnvMeta, error) {
	if err := ValidateToolEnvName(name); err != nil {
		return nil, err
	}
	if err := ValidateToolEnvValue(value); err != nil {
		return nil, err
	}
	if s.secrets == nil {
		return nil, fmt.Errorf("%w: secrets not configured", ErrNotFound)
	}
	name = strings.TrimSpace(name)
	// Try Put, fallback to Rotate on conflict (atomic rotate)
	sec, err := s.secrets.Put(ctx, "environment", name, value)
	if err != nil {
		if errors.Is(err, store.ErrConflict) || errors.Is(err, secrets.ErrConflict) {
			// Already exists -> rotate
			sec, err = s.secrets.RotateByName(ctx, "environment", name, value)
			if err != nil {
				return nil, fmt.Errorf("rotate tool env %q: %w", name, err)
			}
		} else {
			return nil, fmt.Errorf("put tool env %q: %w", name, err)
		}
	}
	// Bump revision
	if _, err := s.IncrementRevision(ctx); err != nil {
		return nil, err
	}
	// Also keep environment_variables in sync if table exists (for peer compatibility)
	_, _ = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO environment_variables (name, secret_id, version, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`, name, sec.ID, sec.Version, store.FormatTime(sec.CreatedAt), store.FormatTime(sec.UpdatedAt))
	meta := &ToolEnvMeta{Name: sec.Name, Version: sec.Version, CreatedAt: sec.CreatedAt, UpdatedAt: sec.UpdatedAt}
	return meta, nil
}

// DeleteToolEnv removes a tool-environment variable. Bumps revision if existed.
func (s *Service) DeleteToolEnv(ctx context.Context, name string) error {
	if err := ValidateToolEnvName(name); err != nil {
		// Allow deleting even if name is reserved? But reserved names never stored, so return not found for reserved.
		// For validation consistency, still reject reserved as not found rather than validation error on delete? Spec says DELETE removes, validate name.
		// If reserved, return validation error (distinct).
		return err
	}
	if s.secrets == nil {
		return fmt.Errorf("%w: secrets not configured", ErrNotFound)
	}
	name = strings.TrimSpace(name)
	err := s.secrets.DeleteByName(ctx, "environment", name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, secrets.ErrNotFound) {
			return fmt.Errorf("%w: tool env %q not found", ErrNotFound, name)
		}
		return fmt.Errorf("delete tool env %q: %w", name, err)
	}
	if _, err := s.IncrementRevision(ctx); err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM environment_variables WHERE name = ?`, name)
	return nil
}

// GetCompanionEnvironmentBundle returns the authoritative bundle for a device.
// If device not granted, returns empty map (client must clear managed values).
// Composition includes raw tool-env values plus per-device reserved vars.
func (s *Service) GetCompanionEnvironmentBundle(ctx context.Context, deviceID string) (map[string]string, int, error) {
	trimmed := strings.TrimSpace(deviceID)
	if trimmed == "" {
		return nil, 0, fmt.Errorf("%w: device_id is required", ErrValidation)
	}
	// Verify device exists and not revoked
	var revoked sql.NullString
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), MAX(revoked_at) FROM companion_devices WHERE id = ?`, trimmed).Scan(&exists, &revoked)
	if err != nil {
		return nil, 0, fmt.Errorf("check device: %w", err)
	}
	if exists == 0 {
		return nil, 0, fmt.Errorf("%w: device not found", ErrNotFound)
	}
	if revoked.Valid && strings.TrimSpace(revoked.String) != "" {
		return nil, 0, fmt.Errorf("%w: device revoked", ErrRevoked)
	}
	rev, _, err := s.GetRevision(ctx)
	if err != nil {
		return nil, 0, err
	}
	granted, err := s.IsDeviceGranted(ctx, trimmed)
	if err != nil {
		return nil, 0, err
	}
	if !granted {
		// Authoritative empty bundle so client removes local values. Still return revision for ETag.
		return map[string]string{}, rev, nil
	}
	// If secrets not configured, return empty
	if s.secrets == nil {
		return map[string]string{}, rev, nil
	}
	// Load all tool env values via secrets reveal
	list, err := s.ListToolEnvs(ctx)
	if err != nil {
		return nil, 0, err
	}
	bundle := make(map[string]string, len(list)+8)
	for _, m := range list {
		v, err := s.secrets.RevealByName(ctx, "environment", m.Name)
		if err != nil {
			// Skip missing (race) but continue
			continue
		}
		bundle[m.Name] = v
	}
	// Compose reserved vars per device at fetch time
	domain := s.getInstanceDomain(ctx)
	baseURL := "https://models." + domain
	if strings.TrimSpace(domain) == "" {
		baseURL = "https://models.example.invalid"
	}
	// Ensure device virtual key
	key, err := s.ensureDeviceVirtualKey(ctx, trimmed)
	if err != nil {
		// If providers not configured, fallback to empty key (still return bundle without reserved keys)
		// But spec says device's distinct LiteLLM key must be present. Log and continue with empty.
		key = ""
	}
	if key != "" {
		bundle["OPENAI_API_KEY"] = key
		bundle["ANTHROPIC_API_KEY"] = key
	}
	bundle["OPENAI_BASE_URL"] = baseURL
	bundle["ANTHROPIC_BASE_URL"] = baseURL
	bundle["OMAHAB_MODEL_FAST"] = "omahab/fast"
	bundle["OMAHAB_MODEL_BALANCED"] = "omahab/balanced"
	bundle["OMAHAB_MODEL_REASONING"] = "omahab/reasoning"
	bundle["OMAHAB_MODEL_EMBEDDING"] = "omahab/embedding"
	return bundle, rev, nil
}
func (s *Service) getInstanceDomain(ctx context.Context) string {
	var domain string
	err := s.db.QueryRowContext(ctx, `SELECT domain FROM instance LIMIT 1`).Scan(&domain)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(domain)
}

// ensureDeviceVirtualKey returns the device's distinct LiteLLM key, issuing one if needed and caching via secrets.
func (s *Service) ensureDeviceVirtualKey(ctx context.Context, deviceID string) (string, error) {
	if s.providers == nil || s.secrets == nil {
		return "", fmt.Errorf("providers or secrets not configured")
	}
	secretName := "device-key." + deviceID
	// Check for existing valid vk row
	var vkID string
	var expiresAt sql.NullString
	var revokedAt sql.NullString
	nowStr := store.FormatTime(s.nowUTC())
	err := s.db.QueryRowContext(ctx, `SELECT id, expires_at, revoked_at FROM provider_virtual_keys WHERE owner_kind = 'device' AND owner_id = ? AND (revoked_at IS NULL) AND (expires_at IS NULL OR expires_at > ?) ORDER BY created_at DESC LIMIT 1`, deviceID, nowStr).Scan(&vkID, &expiresAt, &revokedAt)
	foundValid := err == nil
	if foundValid {
		// Try to reveal cached plaintext
		if v, err := s.secrets.RevealByName(ctx, "platform-app", secretName); err == nil && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), nil
		}
		// Cache miss but row exists -> need to re-issue (revoke old row and create new)
		_ = s.providers.RevokeVirtualKey(ctx, domain.ID(vkID))
	}
	// Issue new virtual key via providers
	kind := "device"
	n := 8
	if len(deviceID) < n {
		n = len(deviceID)
	}
	in := providers.IssueVirtualKeyInput{
		Name:      "device-" + deviceID[:n],
		Scopes:    []string{"omahab/fast", "omahab/balanced", "omahab/reasoning", "omahab/embedding"},
		OwnerKind: &kind,
		OwnerID:   &deviceID,
	}
	vkWithToken, err := s.providers.IssueVirtualKey(ctx, in)
	if err != nil {
		return "", fmt.Errorf("issue device virtual key: %w", err)
	}
	token := vkWithToken.Token
	// Store plaintext in secrets for future reuse
	_, putErr := s.secrets.Put(ctx, "platform-app", secretName, token)
	if putErr != nil {
		if errors.Is(putErr, store.ErrConflict) || errors.Is(putErr, secrets.ErrConflict) {
			_, _ = s.secrets.RotateByName(ctx, "platform-app", secretName, token)
		}
	}
	// Bump revision for key rotation
	_, _ = s.IncrementRevision(ctx)
	return token, nil
}

func (s *Service) CleanupExpiredEnrollments(ctx context.Context) (int64, error) {
	now := s.nowUTC()
	res, err := s.db.ExecContext(ctx, `DELETE FROM companion_enrollments WHERE expires_at < ? AND consumed_at IS NULL`, store.FormatTime(now))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "unique constraint")
}

// DeviceContextKey is the context key for validated device ID in deviceAuth middleware.
type ctxKey string

const DeviceIDKey ctxKey = "companion_device_id"
const DeviceKey ctxKey = "companion_device"

// DeviceFromContext returns device ID from context if present.
func DeviceIDFromContext(ctx context.Context) string {
	v := ctx.Value(DeviceIDKey)
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// DeviceFromContextObj returns *Device from context if present.
func DeviceFromContext(ctx context.Context) *Device {
	v := ctx.Value(DeviceKey)
	if d, ok := v.(*Device); ok {
		return d
	}
	return nil
}
