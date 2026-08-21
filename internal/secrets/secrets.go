package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

// Service is the AES-256-GCM envelope secrets broker.
type Service struct {
	db  *sql.DB
	key [32]byte
	gcm cipher.AEAD
}

// New creates a broker bound to db and the 32-byte master key.
// Key is copied; caller may zero the input slice after.
func New(db *sql.DB, key []byte) (*Service, error) {
	if db == nil {
		return nil, store.Validation("secrets: db is required")
	}
	if len(key) != 32 {
		return nil, store.Validation("secrets: master key must be 32 bytes")
	}
	var k [32]byte
	copy(k[:], key)
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, fmt.Errorf("secrets: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: create GCM: %w", err)
	}
	return &Service{db: db, key: k, gcm: gcm}, nil
}

// Close best-effort zeroes the in-memory key.
func (s *Service) Close() {
	for i := range s.key {
		s.key[i] = 0
	}
}

var allowedScopes = map[string]bool{
	"provider":     true,
	"platform-app": true,
	"project":      true,
	"user":         true,
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._\-]*$`)

// Sentinel re-exports for callers that reference secrets package directly.
// Underlying values are store sentinels so errors.Is works.
var (
	ErrNotFound   = store.ErrNotFound
	ErrConflict   = store.ErrConflict
	ErrValidation = store.ErrValidation
)

func validateScope(scope string) error {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return store.Validation("secret scope is required")
	}
	if !allowedScopes[scope] {
		return store.Validationf("invalid secret scope %q: must be one of provider, platform-app, project, user", scope)
	}
	return nil
}

func validateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return store.Validation("secret name is required")
	}
	if len(name) > 128 {
		return store.Validation("secret name too long")
	}
	if !nameRe.MatchString(name) {
		return store.Validationf("invalid secret name %q", name)
	}
	return nil
}

func validateValue(v string) error {
	if v == "" {
		return store.Validation("secret value is required")
	}
	if len(v) > 64*1024 {
		return store.Validation("secret value too large")
	}
	return nil
}

// encrypt encrypts plaintext with a fresh random nonce.
func (s *Service) encrypt(plaintext string) (nonce []byte, ciphertext []byte, err error) {
	nonce = make([]byte, s.gcm.NonceSize()) // 12 for GCM
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext = s.gcm.Seal(nil, nonce, []byte(plaintext), nil)
	return nonce, ciphertext, nil
}

// decrypt authenticates and decrypts. Wrong key/nonce fails closed.
func (s *Service) decrypt(nonce, ciphertext []byte) (string, error) {
	if len(nonce) != s.gcm.NonceSize() {
		return "", fmt.Errorf("invalid nonce size")
	}
	plain, err := s.gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// Authentication failed: wrong key or tampered data.
		return "", store.Validation("decrypt failed: authentication error")
	}
	return string(plain), nil
}

// Put creates a new secret at scope/name with value.
// It fails with ErrConflict if scope/name already exists. Use Rotate for updates.
func (s *Service) Put(ctx context.Context, scope, name, value string) (*domain.Secret, error) {
	if err := validateScope(scope); err != nil {
		return nil, err
	}
	if err := validateName(name); err != nil {
		return nil, err
	}
	if err := validateValue(value); err != nil {
		return nil, err
	}
	nonce, ct, err := s.encrypt(value)
	if err != nil {
		return nil, err
	}
	id := store.NewID()
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO secrets (id, scope, name, version, nonce, ciphertext, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, strings.TrimSpace(scope), strings.TrimSpace(name), 1, nonce, ct, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, store.Conflictf("secret %s/%s already exists", scope, name)
		}
		return nil, fmt.Errorf("put secret: %w", err)
	}
	return &domain.Secret{
		ID:        domain.ID(id),
		Scope:     strings.TrimSpace(scope),
		Name:      strings.TrimSpace(name),
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// Get returns redacted metadata for the secret with id.
func (s *Service) Get(ctx context.Context, id domain.ID) (*domain.Secret, error) {
	if strings.TrimSpace(string(id)) == "" {
		return nil, store.Validation("secret id is required")
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT id, scope, name, version, created_at, updated_at FROM secrets WHERE id = ?`, string(id))
	sec, err := scanSecret(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.NotFoundf("secret %s not found", id)
		}
		return nil, fmt.Errorf("get secret: %w", err)
	}
	return sec, nil
}

// GetByName returns redacted metadata by scope/name.
func (s *Service) GetByName(ctx context.Context, scope, name string) (*domain.Secret, error) {
	if err := validateScope(scope); err != nil {
		return nil, err
	}
	if err := validateName(name); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT id, scope, name, version, created_at, updated_at FROM secrets WHERE scope = ? AND name = ?`, strings.TrimSpace(scope), strings.TrimSpace(name))
	sec, err := scanSecret(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.NotFoundf("secret %s/%s not found", scope, name)
		}
		return nil, fmt.Errorf("get secret by name: %w", err)
	}
	return sec, nil
}

// List returns all secrets as redacted metadata ordered by scope, name.
func (s *Service) List(ctx context.Context) ([]domain.Secret, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, scope, name, version, created_at, updated_at FROM secrets ORDER BY scope, name`)
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	defer rows.Close()
	var out []domain.Secret
	for rows.Next() {
		sec, err := scanSecret(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	if out == nil {
		out = []domain.Secret{}
	}
	return out, nil
}

// Reveal returns the plaintext for the secret with id.
// It fails closed on wrong key/auth (authentication error).
func (s *Service) Reveal(ctx context.Context, id domain.ID) (string, error) {
	if strings.TrimSpace(string(id)) == "" {
		return "", store.Validation("secret id is required")
	}
	var nonce, ct []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT nonce, ciphertext FROM secrets WHERE id = ?`, string(id)).Scan(&nonce, &ct)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", store.NotFoundf("secret %s not found", id)
		}
		return "", fmt.Errorf("reveal secret: %w", err)
	}
	plain, err := s.decrypt(nonce, ct)
	if err != nil {
		return "", err
	}
	return plain, nil
}

// RevealByName returns the plaintext for scope/name.
func (s *Service) RevealByName(ctx context.Context, scope, name string) (string, error) {
	if err := validateScope(scope); err != nil {
		return "", err
	}
	if err := validateName(name); err != nil {
		return "", err
	}
	var nonce, ct []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT nonce, ciphertext FROM secrets WHERE scope = ? AND name = ?`, strings.TrimSpace(scope), strings.TrimSpace(name)).Scan(&nonce, &ct)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", store.NotFoundf("secret %s/%s not found", scope, name)
		}
		return "", fmt.Errorf("reveal secret: %w", err)
	}
	return s.decrypt(nonce, ct)
}

// Rotate atomically re-encrypts the secret with a new value and increments version.
func (s *Service) Rotate(ctx context.Context, id domain.ID, newValue string) (*domain.Secret, error) {
	if strings.TrimSpace(string(id)) == "" {
		return nil, store.Validation("secret id is required")
	}
	if err := validateValue(newValue); err != nil {
		return nil, err
	}
	nonce, ct, err := s.encrypt(newValue)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin rotate: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Ensure secret exists and capture metadata.
	var scope, name string
	var version int
	var createdAtStr string
	err = tx.QueryRowContext(ctx,
		`SELECT scope, name, version, created_at FROM secrets WHERE id = ?`, string(id)).Scan(&scope, &name, &version, &createdAtStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.NotFoundf("secret %s not found", id)
		}
		return nil, fmt.Errorf("rotate select: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE secrets SET nonce = ?, ciphertext = ?, version = ?, updated_at = ? WHERE id = ?`,
		nonce, ct, version+1, now.Format(time.RFC3339Nano), string(id))
	if err != nil {
		return nil, fmt.Errorf("rotate update: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, store.NotFoundf("secret %s not found", id)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit rotate: %w", err)
	}
	createdAt, _ := time.Parse(time.RFC3339Nano, createdAtStr)
	return &domain.Secret{
		ID:        id,
		Scope:     scope,
		Name:      name,
		Version:   version + 1,
		CreatedAt: createdAt,
		UpdatedAt: now,
	}, nil
}

// RotateByName rotates by scope/name.
func (s *Service) RotateByName(ctx context.Context, scope, name, newValue string) (*domain.Secret, error) {
	sec, err := s.GetByName(ctx, scope, name)
	if err != nil {
		return nil, err
	}
	return s.Rotate(ctx, sec.ID, newValue)
}

// Delete removes a secret.
func (s *Service) Delete(ctx context.Context, id domain.ID) error {
	if strings.TrimSpace(string(id)) == "" {
		return store.Validation("secret id is required")
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE id = ?`, string(id))
	if err != nil {
		return fmt.Errorf("delete secret: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.NotFoundf("secret %s not found", id)
	}
	return nil
}

// DeleteByName removes by scope/name.
func (s *Service) DeleteByName(ctx context.Context, scope, name string) error {
	if err := validateScope(scope); err != nil {
		return err
	}
	if err := validateName(name); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE scope = ? AND name = ?`, strings.TrimSpace(scope), strings.TrimSpace(name))
	if err != nil {
		return fmt.Errorf("delete secret: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.NotFoundf("secret %s/%s not found", scope, name)
	}
	return nil
}

// Project writes the plaintext of the secret to a temporary file with 0600
// permissions and returns the path and a cleanup function that removes the file.
// dir is the directory for the temporary file; if empty, os.TempDir is used.
func (s *Service) Project(ctx context.Context, id domain.ID, dir string) (string, func() error, error) {
	plain, err := s.Reveal(ctx, id)
	if err != nil {
		return "", nil, err
	}
	return writeProtectedFile(plain, dir)
}

// ProjectToPath writes the secret to the exact path with 0600 and returns a cleanup func.
func (s *Service) ProjectToPath(ctx context.Context, id domain.ID, path string) (func() error, error) {
	if strings.TrimSpace(path) == "" {
		return nil, store.Validation("project path is required")
	}
	plain, err := s.Reveal(ctx, id)
	if err != nil {
		return nil, err
	}
	p := strings.TrimSpace(path)
	// If path is an existing directory, create a temp file inside it.
	if fi, statErr := os.Stat(p); statErr == nil && fi.IsDir() {
		_, cleanup, werr := writeProtectedFile(plain, p)
		if werr != nil {
			return nil, werr
		}
		return cleanup, nil
	}
	// Ensure parent directory exists.
	dir := strings.TrimSpace(filepath.Dir(p))
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create project dir: %w", err)
		}
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("project secret: %w", err)
	}
	// Enforce 0600 regardless of umask.
	_ = f.Chmod(0o600)
	if _, err := io.WriteString(f, plain); err != nil {
		_ = f.Close()
		_ = os.Remove(p)
		return nil, fmt.Errorf("write secret file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(p)
		return nil, fmt.Errorf("close secret file: %w", err)
	}
	cleanup := func() error {
		return os.Remove(p)
	}
	return cleanup, nil
}

func writeProtectedFile(plain, dir string) (string, func() error, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("create project dir: %w", err)
	}
	f, err := os.CreateTemp(dir, "omahab-secret-*")
	if err != nil {
		return "", nil, fmt.Errorf("create secret file: %w", err)
	}
	path := f.Name()
	// Enforce 0600.
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", nil, fmt.Errorf("chmod secret file: %w", err)
	}
	if _, err := io.WriteString(f, plain); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", nil, fmt.Errorf("write secret file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", nil, fmt.Errorf("close secret file: %w", err)
	}
	cleanup := func() error {
		return os.Remove(path)
	}
	return path, cleanup, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanSecret(row scanner) (*domain.Secret, error) {
	var id, scope, name, createdAtStr, updatedAtStr string
	var version int
	if err := row.Scan(&id, &scope, &name, &version, &createdAtStr, &updatedAtStr); err != nil {
		return nil, err
	}
	createdAt, _ := time.Parse(time.RFC3339Nano, createdAtStr)
	updatedAt, _ := time.Parse(time.RFC3339Nano, updatedAtStr)
	return &domain.Secret{
		ID:        domain.ID(id),
		Scope:     scope,
		Name:      name,
		Version:   version,
		CreatedAt: createdAt.UTC(),
		UpdatedAt: updatedAt.UTC(),
	}, nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "unique constraint") || strings.Contains(msg, "UNIQUE constraint")
}
