package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/omahab/omahab/internal/health"
	"golang.org/x/crypto/pbkdf2"
)

// djangoPBKDF2Iterations mirrors Django 5.2's default PBKDF2 hasher
// (django.contrib.auth.hashers.PBKDF2PasswordHasher), which paperless-ngx
// uses for auth_user passwords.
const djangoPBKDF2Iterations = 1_000_000

// paperlessInitialAdminUser is the fallback superuser created on fresh
// SSO-configured instances (see ensurePaperlessInitialAdmin).
const paperlessInitialAdminUser = "admin"

// djangoPasswordHash renders a Django-compatible pbkdf2_sha256 password hash
// ("pbkdf2_sha256$<iter>$<salt>$<b64>") so setup can seed a paperless
// superuser via SQL without shelling into paperless-manage. Salt must not
// contain "$" (Django splits the encoding on it).
func djangoPasswordHash(password, salt string, iterations int) (string, error) {
	if password == "" {
		return "", fmt.Errorf("password empty")
	}
	if salt == "" || strings.Contains(salt, "$") {
		return "", fmt.Errorf("salt must be nonempty without $")
	}
	if iterations <= 0 {
		return "", fmt.Errorf("iterations must be positive")
	}
	dk := pbkdf2.Key([]byte(password), []byte(salt), iterations, 32, sha256.New)
	return fmt.Sprintf("pbkdf2_sha256$%d$%s$%s", iterations, salt, base64.StdEncoding.EncodeToString(dk)), nil
}

// generateAlphanumeric returns n cryptographically random [A-Za-z0-9]
// characters (Django salt alphabet; also safe inside SQL string literals).
func generateAlphanumeric(n int) (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	if n <= 0 {
		return "", fmt.Errorf("length must be positive")
	}
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	for i := range raw {
		raw[i] = alphabet[int(raw[i])%len(alphabet)]
	}
	return string(raw), nil
}

// psqlCountLine extracts the data value from default aligned psql output
// (header, dashes, value, "(1 row)" footer) for single-column SELECTs.
var psqlCountLine = regexp.MustCompile(`(?m)^\s*(\d+)\s*$`)

// psqlCount parses the row count out of `SELECT count(*) ...` output.
func psqlCount(out string) (int, error) {
	m := psqlCountLine.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("no count in psql output")
	}
	n := 0
	for _, c := range m[1] {
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// psqlHashLine matches a Django pbkdf2_sha256 hash data line in default
// aligned psql output (leading padding included).
var psqlHashLine = regexp.MustCompile(`(?m)^\s*(pbkdf2_sha256\$\d+\$[^$]+\$[A-Za-z0-9+/=]+)\s*$`)

// psqlHash extracts the password hash from `SELECT password ...` output.
// Empty means no such row (or unparseable output).
func psqlHash(out string) string {
	if m := psqlHashLine.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// ensurePaperlessInitialAdmin seeds the fallback superuser on fresh
// paperless instances (zero non-system users). Without any user, paperless's
// FIRST_INSTALL login template forwards every visitor to the local signup
// page, and that forward beats REDIRECT_LOGIN_TO_SSO's auto-submit in real
// browsers (live 2026-09-13) — so SSO never engages by default. Creating the
// initial admin retires the funnel: /accounts/login/ auto-POSTs to Pocket ID.
//
// The password is random, stored in platform-app/paperless_admin_password
// (plus the secrets file tree), and works at /admin/ (Django's ModelBackend,
// unaffected by PAPERLESS_DISABLE_REGULAR_LOGIN, which only gates allauth).
// First SSO logins remain plain users; promote them via /admin/ or the shell.
// Idempotent: a present user (even 'admin' owned by someone else) skips.
func (b *Backend) ensurePaperlessInitialAdmin(ctx context.Context) error {
	countOut, err := runPostgresAlterRole(ctx, "paperless", "SELECT count(*) FROM auth_user WHERE username NOT IN ('consumer','AnonymousUser')")
	if err != nil {
		return fmt.Errorf("count paperless users: %s", health.RedactDetail(err.Error()))
	}
	n, err := psqlCount(countOut)
	if err != nil {
		return fmt.Errorf("parse paperless user count: %s", health.RedactDetail(err.Error()))
	}
	if n != 0 {
		return nil
	}
	password := generateRandomBase64URL(32)
	salt, err := generateAlphanumeric(22)
	if err != nil {
		return fmt.Errorf("generate salt: %w", err)
	}
	hash, err := djangoPasswordHash(password, salt, djangoPBKDF2Iterations)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	if strings.ContainsAny(hash, "'\\") {
		return fmt.Errorf("hash carries SQL-unsafe characters")
	}
	insert := fmt.Sprintf("INSERT INTO auth_user (password, last_login, is_superuser, username, first_name, last_name, email, is_staff, is_active, date_joined) VALUES ('%s', NULL, true, '%s', '', '', '', true, true, now()) ON CONFLICT (username) DO NOTHING", hash, paperlessInitialAdminUser)
	if _, err := runPostgresAlterRole(ctx, "paperless", insert); err != nil {
		return fmt.Errorf("insert paperless admin: %s", health.RedactDetail(err.Error()))
	}
	verifyOut, err := runPostgresAlterRole(ctx, "paperless", "SELECT password FROM auth_user WHERE username = '"+paperlessInitialAdminUser+"'")
	if err != nil {
		return fmt.Errorf("verify paperless admin: %s", health.RedactDetail(err.Error()))
	}
	if psqlHash(verifyOut) != hash {
		return fmt.Errorf("paperless admin username taken concurrently, kept existing row")
	}
	if b.secrets != nil {
		if err := upsertSecret(ctx, b.secrets, "platform-app", "paperless_admin_password", password); err != nil {
			return fmt.Errorf("store paperless admin password: %w", err)
		}
	}
	secretsDir := filepath.Join(b.cfg.StateDir, "secrets")
	if strings.TrimSpace(b.cfg.StateDir) == "" {
		secretsDir = "/var/lib/omahab/secrets"
	}
	_ = os.MkdirAll(secretsDir, 0o700)
	if err := atomicReplaceSecretFile(secretsDir, "paperless_admin_password", password); err != nil {
		return fmt.Errorf("write paperless admin password file: %w", err)
	}
	log.Printf("setup oidc: paperless initial admin created; password in platform-app/paperless_admin_password, usable at /admin/")
	return nil
}
