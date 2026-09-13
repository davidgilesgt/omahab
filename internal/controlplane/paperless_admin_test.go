package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/identity"
)

// Golden vector produced by Django 5.2's real PBKDF2PasswordHasher
// (paperless-ngx 3.1.0 closure, live box):
// encode('secret-test-pw', 'abc123SALTxyz', 1000).
func TestDjangoPasswordHashGoldenVector(t *testing.T) {
	t.Parallel()
	got, err := djangoPasswordHash("secret-test-pw", "abc123SALTxyz", 1000)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	want := "pbkdf2_sha256$1000$abc123SALTxyz$3+h1vspAY2dtvUQcSmc0yGiSOsyqgCJJVJMrP5kBEqE="
	if got != want {
		t.Fatalf("hash = %q, want %q", got, want)
	}
}

func TestDjangoPasswordHashRejectsBadInput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		password string
		salt     string
		iter     int
	}{
		{"empty password", "", "abc", 1000},
		{"empty salt", "pw", "", 1000},
		{"dollar salt", "pw", "a$b", 1000},
		{"nonpositive iterations", "pw", "abc", 0},
	} {
		if _, err := djangoPasswordHash(tc.password, tc.salt, tc.iter); err == nil {
			t.Fatalf("%s: expected error, got nil", tc.name)
		}
	}
}

// The stub replays realistic aligned psql output so the count/hash parsers
// are exercised, and echoes the inserted hash back on the verify SELECT.
func TestEnsurePaperlessInitialAdminCreatesAdmin(t *testing.T) {
	b, _ := newSetupBackend(t, nil)
	b.cfg.StateDir = t.TempDir()
	var stmts []string
	insertHash := ""
	oldAlter := runPostgresAlterRole
	runPostgresAlterRole = func(_ context.Context, db, stmt string) (string, error) {
		if db != "paperless" {
			return "", errors.New("unexpected db " + db)
		}
		stmts = append(stmts, stmt)
		switch {
		case strings.Contains(stmt, "count(*)"):
			return " count \n-------\n     0\n(1 row)\n", nil
		case strings.HasPrefix(stmt, "INSERT INTO auth_user"):
			idx := strings.Index(stmt, "pbkdf2_sha256$")
			if idx < 0 {
				return "", errors.New("insert carries no hash")
			}
			rest := stmt[idx+len("pbkdf2_sha256$"):]
			end := strings.Index(rest, "'")
			if end < 0 {
				return "", errors.New("insert hash unterminated")
			}
			insertHash = "pbkdf2_sha256$" + rest[:end]
			return "INSERT 0 1\n", nil
		case strings.Contains(stmt, "SELECT password"):
			return " password \n----------\n " + insertHash + "\n(1 row)\n", nil
		default:
			return "", errors.New("unexpected stmt " + stmt)
		}
	}
	t.Cleanup(func() { runPostgresAlterRole = oldAlter })

	if err := b.ensurePaperlessInitialAdmin(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	var insert string
	for _, s := range stmts {
		if strings.HasPrefix(s, "INSERT INTO auth_user") {
			insert = s
		}
	}
	for _, want := range []string{"'admin'", "true, 'admin', '', '', '', true, true", "ON CONFLICT (username) DO NOTHING"} {
		if !strings.Contains(insert, want) {
			t.Fatalf("insert missing %q:\n%s", want, insert)
		}
	}
	stored, err := b.secrets.RevealByName(context.Background(), "platform-app", "paperless_admin_password")
	if err != nil || strings.TrimSpace(stored) == "" {
		t.Fatalf("admin password not stored: %v", err)
	}
	fileRaw, err := os.ReadFile(filepath.Join(b.cfg.StateDir, "secrets", "paperless_admin_password"))
	if err != nil {
		t.Fatalf("admin password file: %v", err)
	}
	if string(fileRaw) != stored {
		t.Fatal("secret file content differs from stored secret")
	}
}

// An existing user (even one) must skip creation entirely: no INSERT, no
// stored password, no error.
func TestEnsurePaperlessInitialAdminSkipsWhenUsersExist(t *testing.T) {
	b, _ := newSetupBackend(t, nil)
	b.cfg.StateDir = t.TempDir()
	oldAlter := runPostgresAlterRole
	runPostgresAlterRole = func(_ context.Context, _, stmt string) (string, error) {
		if strings.Contains(stmt, "count(*)") {
			return " count \n-------\n     3\n(1 row)\n", nil
		}
		return "", errors.New("must not run " + stmt)
	}
	t.Cleanup(func() { runPostgresAlterRole = oldAlter })

	if err := b.ensurePaperlessInitialAdmin(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := b.secrets.RevealByName(context.Background(), "platform-app", "paperless_admin_password"); err == nil {
		t.Fatal("password stored despite existing users")
	}
}

// With an enrolled Pocket ID owner, the seeded row carries their username
// and email instead of the fallback.
func TestEnsurePaperlessInitialAdminUsesOwnerIdentity(t *testing.T) {
	b, _ := newSetupBackend(t, nil)
	b.cfg.StateDir = t.TempDir()
	oldLookup := paperlessOwnerLookup
	paperlessOwnerLookup = func(context.Context, *identity.PocketIDClient) (string, string, error) {
		return "david", "david@davidgiles.net", nil
	}
	t.Cleanup(func() { paperlessOwnerLookup = oldLookup })
	var stmts []string
	insertHash := ""
	oldAlter := runPostgresAlterRole
	runPostgresAlterRole = func(_ context.Context, db, stmt string) (string, error) {
		stmts = append(stmts, stmt)
		switch {
		case strings.Contains(stmt, "count(*)"):
			return " count \n-------\n     0\n(1 row)\n", nil
		case strings.HasPrefix(stmt, "INSERT INTO auth_user"):
			idx := strings.Index(stmt, "pbkdf2_sha256$")
			rest := stmt[idx+len("pbkdf2_sha256$"):]
			insertHash = "pbkdf2_sha256$" + rest[:strings.Index(rest, "'")]
			return "INSERT 0 1\n", nil
		case strings.Contains(stmt, "SELECT password"):
			return " password \n----------\n " + insertHash + "\n(1 row)\n", nil
		default:
			return "", errors.New("unexpected stmt " + stmt)
		}
	}
	t.Cleanup(func() { runPostgresAlterRole = oldAlter })

	if err := b.ensurePaperlessInitialAdmin(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	var insert string
	for _, s := range stmts {
		if strings.HasPrefix(s, "INSERT INTO auth_user") {
			insert = s
		}
	}
	for _, want := range []string{"'david'", "'david@davidgiles.net'"} {
		if !strings.Contains(insert, want) {
			t.Fatalf("insert missing %q:\n%s", want, insert)
		}
	}
	if strings.Contains(insert, "'admin'") {
		t.Fatalf("insert must not use fallback admin:\n%s", insert)
	}
}

// A lookup failure (or unusable name) falls back to 'admin' rather than
// failing setup.
func TestEnsurePaperlessInitialAdminFallsBackWithoutOwner(t *testing.T) {
	b, _ := newSetupBackend(t, nil)
	b.cfg.StateDir = t.TempDir()
	oldLookup := paperlessOwnerLookup
	paperlessOwnerLookup = func(context.Context, *identity.PocketIDClient) (string, string, error) {
		return "", "", errors.New("no users enrolled")
	}
	t.Cleanup(func() { paperlessOwnerLookup = oldLookup })
	oldAlter := runPostgresAlterRole
	var insertHash string
	runPostgresAlterRole = func(_ context.Context, _, stmt string) (string, error) {
		if strings.Contains(stmt, "count(*)") {
			return " count \n-------\n     0\n(1 row)\n", nil
		}
		if strings.HasPrefix(stmt, "INSERT INTO auth_user") {
			if !strings.Contains(stmt, "'admin'") {
				return "", errors.New("expected fallback admin")
			}
			idx := strings.Index(stmt, "pbkdf2_sha256$")
			rest := stmt[idx+len("pbkdf2_sha256$"):]
			insertHash = "pbkdf2_sha256$" + rest[:strings.Index(rest, "'")]
			return "INSERT 0 1\n", nil
		}
		if strings.Contains(stmt, "SELECT password") {
			return " password \n----------\n " + insertHash + "\n(1 row)\n", nil
		}
		return "", errors.New("unexpected stmt " + stmt)
	}
	t.Cleanup(func() { runPostgresAlterRole = oldAlter })

	if err := b.ensurePaperlessInitialAdmin(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
}
