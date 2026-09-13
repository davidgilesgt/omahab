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
	"github.com/omahab/omahab/internal/identity"
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

// ensurePaperlessInitialAdmin seeds the initial superuser on fresh paperless
// instances (zero non-system users). Without any user, paperless's
// FIRST_INSTALL login template forwards every visitor to the local signup
// page, and that forward beats REDIRECT_LOGIN_TO_SSO's auto-submit in real
// browsers (live 2026-09-13) — so SSO never engages by default. Creating the
// initial admin retires the funnel: /accounts/login/ auto-POSTs to Pocket ID.
//
// The row carries the first Pocket ID user's username and email (fallback
// 'admin' when nobody enrolled yet), so the owner's identity owns the admin
// seat. The password is random, stored in
// platform-app/paperless_admin_password (plus the secrets file tree), and
// works at /admin/ (Django's ModelBackend, unaffected by
// PAPERLESS_DISABLE_REGULAR_LOGIN, which only gates allauth).
// Idempotent: any present user skips creation entirely.

// paperlessOwnerLookup resolves the owner identity for the seeded superuser.
// Assigned to a var so tests can stub the Pocket ID boundary.
var paperlessOwnerLookup = func(ctx context.Context, pc *identity.PocketIDClient) (username, email string, err error) {
	if pc == nil {
		return "", "", fmt.Errorf("pocket client not configured")
	}
	return pc.OwnerCandidate(ctx)
}

// validPaperlessUsername mirrors Django's UnicodeUsernameValidator
// (^[\w.@+-]+$, 150 chars): Pocket ID names outside it cannot be seeded.
var paperlessUsernameRe = regexp.MustCompile(`^[\w.@+-]+$`)

func validPaperlessUsername(s string) bool {
	return s != "" && len(s) <= 150 && paperlessUsernameRe.MatchString(s)
}

// sqlSafeLiteral reports whether s interpolates safely into a SQL string
// literal (no quote or backslash).
func sqlSafeLiteral(s string) bool {
	return !strings.ContainsAny(s, "'\\")
}

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
	// Seed with the first Pocket ID user's identity so the owner's first SSO
	// login can attach to (or at least match) an admin row instead of an
	// orphaned fallback. Falls back to 'admin' when nobody enrolled yet.
	username, email := paperlessInitialAdminUser, ""
	if ou, oe, oerr := paperlessOwnerLookup(ctx, b.pocketClient); oerr == nil && validPaperlessUsername(ou) && sqlSafeLiteral(ou) && sqlSafeLiteral(oe) {
		username, email = ou, oe
	} else if oerr != nil {
		log.Printf("setup oidc: paperless owner lookup failed, using fallback admin: %s", health.RedactDetail(oerr.Error()))
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
	insert := fmt.Sprintf("INSERT INTO auth_user (password, last_login, is_superuser, username, first_name, last_name, email, is_staff, is_active, date_joined) VALUES ('%s', NULL, true, '%s', '', '', '%s', true, true, now()) ON CONFLICT (username) DO NOTHING", hash, username, email)
	if _, err := runPostgresAlterRole(ctx, "paperless", insert); err != nil {
		return fmt.Errorf("insert paperless admin: %s", health.RedactDetail(err.Error()))
	}
	verifyOut, err := runPostgresAlterRole(ctx, "paperless", "SELECT password FROM auth_user WHERE username = '"+username+"'")
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
	log.Printf("setup oidc: paperless initial admin %q created; password in platform-app/paperless_admin_password, usable at /admin/", username)
	return nil
}
