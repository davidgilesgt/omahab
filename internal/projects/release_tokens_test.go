package projects

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestIssueVerifyRoundTrip(t *testing.T) {
	svc := newService(t, &fakeRunner{healthOK: true}, &fakeRecorder{}, nil)
	ctx := context.Background()
	proj, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tok, err := svc.IssueReleaseToken(ctx, proj.ID)
	if err != nil {
		t.Fatalf("IssueReleaseToken: %v", err)
	}
	if tok == "" {
		t.Fatal("token empty")
	}
	if _, err := base64.RawURLEncoding.DecodeString(tok); err != nil {
		t.Fatalf("token not base64url: %v (%q)", err, tok)
	}
	if b, _ := base64.RawURLEncoding.DecodeString(tok); len(b) != 32 {
		t.Fatalf("token decoded length = %d, want 32", len(b))
	}
	if err := svc.VerifyReleaseToken(ctx, proj.ID, tok); err != nil {
		t.Fatalf("VerifyReleaseToken: %v", err)
	}
	var last sqlNullString
	// last_used_at should be set after verify
	row := svc.db.QueryRowContext(ctx, `SELECT last_used_at FROM project_release_tokens WHERE project_id = ?`, string(proj.ID))
	if err := row.Scan(&last); err != nil {
		t.Fatalf("query last_used_at: %v", err)
	}
	if !last.Valid || strings.TrimSpace(last.String) == "" {
		t.Fatalf("last_used_at not recorded after verify, got %#v", last)
	}
	if _, err := time.Parse(time.RFC3339Nano, last.String); err != nil {
		t.Fatalf("last_used_at not RFC3339Nano: %v", err)
	}
}

func TestVerifyWrongProject(t *testing.T) {
	svc := newService(t, &fakeRunner{healthOK: true}, &fakeRecorder{}, nil)
	ctx := context.Background()
	p1, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create p1: %v", err)
	}
	p2, err := svc.Create(ctx, CreateParams{Slug: "other", Name: "Other", RepositoryURL: "https://forgejo.example.com/acme/other", Image: "forgejo.example.com/acme/other"})
	if err != nil {
		t.Fatalf("Create p2: %v", err)
	}
	tok, err := svc.IssueReleaseToken(ctx, p1.ID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	err = svc.VerifyReleaseToken(ctx, p2.ID, tok)
	if err == nil {
		t.Fatal("verify with wrong project should fail")
	}
	if !errors.Is(err, ErrReleaseMismatch) && !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong project error should be ErrReleaseMismatch or ErrUnauthorized, got %v", err)
	}
	if strings.Contains(err.Error(), tok) {
		t.Fatal("token leaked in error")
	}
}

func TestVerifyTamperedToken(t *testing.T) {
	svc := newService(t, &fakeRunner{healthOK: true}, &fakeRecorder{}, nil)
	ctx := context.Background()
	proj, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tok, err := svc.IssueReleaseToken(ctx, proj.ID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if len(tok) < 4 {
		t.Fatalf("token too short")
	}
	tampered := tok[:len(tok)-2] + "AA"
	if tampered == tok {
		tampered = tok[:len(tok)-1] + "B"
	}
	if err := svc.VerifyReleaseToken(ctx, proj.ID, tampered); err == nil {
		t.Fatalf("tampered token should fail")
	} else {
		if !errors.Is(err, ErrReleaseMismatch) && !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("tampered error should be mismatch/unauthorized, got %v", err)
		}
		if strings.Contains(err.Error(), tok) || strings.Contains(err.Error(), tampered) {
			t.Fatal("token leaked in error")
		}
	}
	// original still verifies
	if err := svc.VerifyReleaseToken(ctx, proj.ID, tok); err != nil {
		t.Fatalf("original token should still verify after tampered attempt: %v", err)
	}
}

func TestPersistenceStoresHashOnly(t *testing.T) {
	svc := newService(t, &fakeRunner{healthOK: true}, &fakeRecorder{}, nil)
	ctx := context.Background()
	proj, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tok, err := svc.IssueReleaseToken(ctx, proj.ID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	h := sha256.Sum256([]byte(tok))
	wantHash := hex.EncodeToString(h[:])
	var storedHash, storedPrefix, createdAt, updatedAt string
	row := svc.db.QueryRowContext(ctx, `SELECT token_hash, token_prefix, created_at, updated_at FROM project_release_tokens WHERE project_id = ?`, string(proj.ID))
	if err := row.Scan(&storedHash, &storedPrefix, &createdAt, &updatedAt); err != nil {
		t.Fatalf("query token row: %v", err)
	}
	if storedHash != wantHash {
		t.Fatalf("stored hash mismatch: got %q want %q", storedHash, wantHash)
	}
	if storedHash == tok {
		t.Fatal("stored hash equals raw token, should be hash only")
	}
	// raw token must not appear anywhere in table via direct equality search
	var count int
	if err := svc.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_release_tokens WHERE token_hash = ?`, tok).Scan(&count); err != nil {
		t.Fatalf("count raw: %v", err)
	}
	if count != 0 {
		t.Fatal("raw token found as hash")
	}
	if storedPrefix == "" {
		t.Fatal("token_prefix should be stored")
	}
	if !strings.HasPrefix(tok, storedPrefix) {
		t.Fatalf("token_prefix %q not prefix of token %q", storedPrefix, tok)
	}
	if createdAt == "" || updatedAt == "" {
		t.Fatal("timestamps empty")
	}
}

func TestRotateInvalidatesOld(t *testing.T) {
	svc := newService(t, &fakeRunner{healthOK: true}, &fakeRecorder{}, nil)
	ctx := context.Background()
	proj, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	oldTok, err := svc.IssueReleaseToken(ctx, proj.ID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := svc.VerifyReleaseToken(ctx, proj.ID, oldTok); err != nil {
		t.Fatalf("verify old before rotate: %v", err)
	}
	newTok, err := svc.RotateReleaseToken(ctx, proj.ID)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if newTok == oldTok {
		t.Fatal("rotated token should differ")
	}
	if err := svc.VerifyReleaseToken(ctx, proj.ID, oldTok); err == nil {
		t.Fatal("old token should fail after rotate")
	}
	if err := svc.VerifyReleaseToken(ctx, proj.ID, newTok); err != nil {
		t.Fatalf("new token should verify: %v", err)
	}
	var cnt int
	if err := svc.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_release_tokens WHERE project_id = ?`, string(proj.ID)).Scan(&cnt); err != nil {
		t.Fatalf("count: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("should have exactly one token row after rotate, got %d", cnt)
	}
}

func TestIssueRequiresExistingProject(t *testing.T) {
	svc := newService(t, &fakeRunner{healthOK: true}, &fakeRecorder{}, nil)
	ctx := context.Background()
	_, err := svc.IssueReleaseToken(ctx, "does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown project, got %v", err)
	}
	_, err = svc.RotateReleaseToken(ctx, "does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Rotate unknown should be ErrNotFound, got %v", err)
	}
}

func TestVerifyEmptyTokenFails(t *testing.T) {
	svc := newService(t, &fakeRunner{healthOK: true}, &fakeRecorder{}, nil)
	ctx := context.Background()
	proj, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, _ = svc.IssueReleaseToken(ctx, proj.ID)
	for _, tok := range []string{"", "   ", "\t"} {
		err := svc.VerifyReleaseToken(ctx, proj.ID, tok)
		if err == nil {
			t.Fatalf("empty token %q should fail", tok)
		}
		if !errors.Is(err, ErrUnauthorized) && !errors.Is(err, ErrReleaseMismatch) {
			t.Fatalf("empty token error should be unauthorized/mismatch, got %v", err)
		}
	}
}

func TestServiceImplementsVerifier(t *testing.T) {
	var _ ReleaseTokenVerifier = (*Service)(nil)
	svc := newService(t, &fakeRunner{healthOK: true}, &fakeRecorder{}, nil)
	ctx := context.Background()
	proj, _ := svc.Create(ctx, validCreate())
	tok, _ := svc.IssueReleaseToken(ctx, proj.ID)
	var v ReleaseTokenVerifier = svc
	if err := v.VerifyReleaseToken(ctx, proj.ID, tok); err != nil {
		t.Fatalf("interface verify: %v", err)
	}
}
func TestVerifyWithoutIssueFails(t *testing.T) {
	svc := newService(t, &fakeRunner{healthOK: true}, &fakeRecorder{}, nil)
	ctx := context.Background()
	proj, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	err = svc.VerifyReleaseToken(ctx, proj.ID, "some-token-that-was-never-issued")
	if err == nil {
		t.Fatal("verify without issue should fail")
	}
	if !errors.Is(err, ErrUnauthorized) && !errors.Is(err, ErrReleaseMismatch) {
		t.Fatalf("want unauthorized/mismatch, got %v", err)
	}
}

func TestIssueTwiceInvalidatesFirst(t *testing.T) {
	svc := newService(t, &fakeRunner{healthOK: true}, &fakeRecorder{}, nil)
	ctx := context.Background()
	proj, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tok1, err := svc.IssueReleaseToken(ctx, proj.ID)
	if err != nil {
		t.Fatalf("Issue1: %v", err)
	}
	tok2, err := svc.IssueReleaseToken(ctx, proj.ID)
	if err != nil {
		t.Fatalf("Issue2: %v", err)
	}
	if tok1 == tok2 {
		t.Fatal("second issue should produce different token")
	}
	if err := svc.VerifyReleaseToken(ctx, proj.ID, tok1); err == nil {
		t.Fatal("first token should be invalid after second issue")
	}
	if err := svc.VerifyReleaseToken(ctx, proj.ID, tok2); err != nil {
		t.Fatalf("second token should verify: %v", err)
	}
}

func TestVerifyUpdatesLastUsed(t *testing.T) {
	svc := newService(t, &fakeRunner{healthOK: true}, &fakeRecorder{}, nil)
	ctx := context.Background()
	proj, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tok, err := svc.IssueReleaseToken(ctx, proj.ID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	var before sqlNullString
	_ = svc.db.QueryRowContext(ctx, `SELECT last_used_at FROM project_release_tokens WHERE project_id = ?`, string(proj.ID)).Scan(&before)
	if before.Valid {
		t.Fatalf("last_used_at should be NULL before verify, got %q", before.String)
	}
	if err := svc.VerifyReleaseToken(ctx, proj.ID, tok); err != nil {
		t.Fatalf("verify1: %v", err)
	}
	var first sqlNullString
	if err := svc.db.QueryRowContext(ctx, `SELECT last_used_at FROM project_release_tokens WHERE project_id = ?`, string(proj.ID)).Scan(&first); err != nil {
		t.Fatalf("query first last_used: %v", err)
	}
	if !first.Valid {
		t.Fatal("last_used_at should be set after first verify")
	}
	t1, err := time.Parse(time.RFC3339Nano, first.String)
	if err != nil {
		t.Fatalf("parse first last_used: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := svc.VerifyReleaseToken(ctx, proj.ID, tok); err != nil {
		t.Fatalf("verify2: %v", err)
	}
	var second sqlNullString
	if err := svc.db.QueryRowContext(ctx, `SELECT last_used_at FROM project_release_tokens WHERE project_id = ?`, string(proj.ID)).Scan(&second); err != nil {
		t.Fatalf("query second last_used: %v", err)
	}
	t2, err := time.Parse(time.RFC3339Nano, second.String)
	if err != nil {
		t.Fatalf("parse second last_used: %v", err)
	}
	if !t2.After(t1) && !t2.Equal(t1) {
		// Equal is allowed if clock granularity is low, but should not be before.
		t.Fatalf("second verify should not move last_used backwards: %v vs %v", t2, t1)
	}
}


// sqlNullString mirrors database/sql NullString without importing sql in test header duplication
type sqlNullString struct {
	String string
	Valid  bool
}

func (n *sqlNullString) Scan(value any) error {
	if value == nil {
		n.String, n.Valid = "", false
		return nil
	}
	switch v := value.(type) {
	case string:
		n.String, n.Valid = v, true
	case []byte:
		n.String, n.Valid = string(v), true
	default:
		n.String, n.Valid = "", false
	}
	return nil
}
