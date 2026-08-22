package projects

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
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

func generateReleaseToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate release token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashReleaseToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func (s *Service) ensureReleaseTokensTable(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS project_release_tokens (
    project_id TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL,
    token_prefix TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    last_used_at TEXT
) STRICT`)
	if err != nil {
		_, err2 := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS project_release_tokens (
    project_id TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL,
    token_prefix TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    last_used_at TEXT
)`)
		if err2 != nil {
			return fmt.Errorf("ensure release tokens table: %w", err)
		}
	}
	_, _ = s.db.ExecContext(ctx, `ALTER TABLE project_release_tokens ADD COLUMN last_used_at TEXT`)
	_, _ = s.db.ExecContext(ctx, `ALTER TABLE project_release_tokens ADD COLUMN last_use TEXT`)
	_, _ = s.db.ExecContext(ctx, `ALTER TABLE project_release_tokens ADD COLUMN token_prefix TEXT NOT NULL DEFAULT ''`)
	return nil
}

func (s *Service) storeReleaseTokenHash(ctx context.Context, projectID domain.ID, token string) error {
	if err := s.ensureReleaseTokensTable(ctx); err != nil {
		return err
	}
	hash := hashReleaseToken(token)
	prefix := token
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO project_release_tokens (project_id, token_hash, token_prefix, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(project_id) DO UPDATE SET
    token_hash = excluded.token_hash,
    token_prefix = excluded.token_prefix,
    updated_at = excluded.updated_at,
    last_used_at = NULL
`, string(projectID), hash, prefix, now, now)
	if err != nil {
		return fmt.Errorf("store release token: %w", err)
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE project_release_tokens SET last_use = NULL WHERE project_id = ?`, string(projectID))
	return nil
}

// IssueReleaseToken creates a per-project release token. The raw token is
// returned exactly once and only the sha256 hex hash is persisted.
func (s *Service) IssueReleaseToken(ctx context.Context, projectID domain.ID) (string, error) {
	if strings.TrimSpace(string(projectID)) == "" {
		return "", invalidf("project_id", "must not be empty")
	}
	if _, err := s.fetchProject(ctx, "id = ?", string(projectID)); err != nil {
		return "", err
	}
	tok, err := generateReleaseToken()
	if err != nil {
		return "", err
	}
	if err := s.storeReleaseTokenHash(ctx, projectID, tok); err != nil {
		return "", err
	}
	return tok, nil
}

// RotateReleaseToken revokes the existing token and issues a new one.
func (s *Service) RotateReleaseToken(ctx context.Context, projectID domain.ID) (string, error) {
	if strings.TrimSpace(string(projectID)) == "" {
		return "", invalidf("project_id", "must not be empty")
	}
	if _, err := s.fetchProject(ctx, "id = ?", string(projectID)); err != nil {
		return "", err
	}
	tok, err := generateReleaseToken()
	if err != nil {
		return "", err
	}
	if err := s.storeReleaseTokenHash(ctx, projectID, tok); err != nil {
		return "", err
	}
	return tok, nil
}

func tokenVerifyErr(msg string) error {
	return fmt.Errorf("%w: %w: %s", ErrUnauthorized, ErrReleaseMismatch, msg)
}

// VerifyReleaseToken checks that token authorizes projectID. The hash
// comparison is constant-time and a successful verification records last use.
func (s *Service) VerifyReleaseToken(ctx context.Context, projectID domain.ID, token string) error {
	if strings.TrimSpace(string(projectID)) == "" {
		return tokenVerifyErr("project id required")
	}
	if strings.TrimSpace(token) == "" {
		return tokenVerifyErr("release token required")
	}
	if err := s.ensureReleaseTokensTable(ctx); err != nil {
		return err
	}
	var stored string
	err := s.db.QueryRowContext(ctx, `SELECT token_hash FROM project_release_tokens WHERE project_id = ?`, string(projectID)).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		dummy := hashReleaseToken(strings.TrimSpace(token))
		_ = subtle.ConstantTimeCompare([]byte(dummy), []byte(dummy))
		return tokenVerifyErr("release token not configured")
	}
	if err != nil {
		return fmt.Errorf("lookup release token: %w", err)
	}
	got := hashReleaseToken(strings.TrimSpace(token))
	if subtle.ConstantTimeCompare([]byte(got), []byte(stored)) != 1 {
		return tokenVerifyErr("invalid release token")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = s.db.ExecContext(ctx, `UPDATE project_release_tokens SET last_used_at = ?, updated_at = ?, last_use = ? WHERE project_id = ?`, now, now, now, string(projectID))
	_, _ = s.db.ExecContext(ctx, `UPDATE project_release_tokens SET last_use = ? WHERE project_id = ?`, now, string(projectID))
	return nil
}

var _ ReleaseTokenVerifier = (*Service)(nil)
