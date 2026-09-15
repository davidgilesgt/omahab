package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/config"
	"github.com/omahab/omahab/internal/secrets"
	"github.com/omahab/omahab/internal/store"
)

func phraseTestBackend(t *testing.T) *Backend {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for _, m := range [][]store.Migration{store.Migrations(), secrets.Migrations()} {
		if err := st.Migrate(ctx, m...); err != nil {
			t.Fatal(err)
		}
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}
	sec, err := secrets.New(st.DB(), key)
	if err != nil {
		t.Fatal(err)
	}
	var mk [32]byte
	for i := range mk {
		mk[i] = byte(i + 1)
	}
	return &Backend{
		cfg:       config.Config{StateDir: t.TempDir()},
		store:     st,
		db:        st.DB(),
		secrets:   sec,
		masterKey: mk,
	}
}

func confirmTestPhrase(t *testing.T, b *Backend) string {
	t.Helper()
	words, seed, err := secrets.GenerateRecoveryPhrase()
	if err != nil {
		t.Fatal(err)
	}
	phrase := strings.Join(words, " ")
	sum_fp := seedFingerprint(seed)
	if err := b.persistRecoveryKit(context.Background(), sum_fp, seed); err != nil {
		t.Fatalf("persist kit: %v", err)
	}
	return phrase
}

// No phrase and no kit: the operation cannot proceed silently.
func TestAuthenticateRequiresPhrase(t *testing.T) {
	b := phraseTestBackend(t)
	if _, err := b.authenticateRecoverySeed(""); !errors.Is(err, ErrRecoveryPhraseRequired) {
		t.Fatalf("err = %v, want recovery_phrase_required", err)
	}
}

// A valid-format phrase that does not match the saved kit is a mismatch,
// never an authentication.
func TestAuthenticateMismatch(t *testing.T) {
	b := phraseTestBackend(t)
	confirmTestPhrase(t, b)
	words2, _, err := secrets.GenerateRecoveryPhrase()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.authenticateRecoverySeed(strings.Join(words2, " ")); !errors.Is(err, ErrRecoveryPhraseMismatch) {
		t.Fatalf("err = %v, want recovery_phrase_mismatch", err)
	}
}

// The confirmed phrase authenticates against the saved kit.
func TestAuthenticateAgainstKit(t *testing.T) {
	b := phraseTestBackend(t)
	phrase := confirmTestPhrase(t, b)
	seed, err := b.authenticateRecoverySeed(phrase)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	words := strings.Fields(phrase)
	want, err := secrets.PhraseToSeed(words)
	if err != nil {
		t.Fatal(err)
	}
	if seed != want {
		t.Fatal("authenticated seed does not match phrase seed")
	}
}

// After a restart (memory cache lost), the phrase alone still authenticates
// via the kit; without any phrase the daemon refuses random material.
func TestHetznerRequiresPhraseAfterRestart(t *testing.T) {
	b := phraseTestBackend(t)
	confirmTestPhrase(t, b)
	// Simulate restart: drop the in-memory cache.
	recoveryMu.Lock()
	lastConfirmedSeed = nil
	lastConfirmedSeedTime = time.Time{}
	lastConfirmedFingerprint = ""
	recoveryMu.Unlock()
	_, err := b.CreateBackupRepository(context.Background(), apitypes.CreateBackupRepositoryRequest{
		Kind: "hetzner_storagebox", Label: "hetzner",
		Username: "u123", Host: "u123.your-storagebox.de", SubAccountPassword: "x",
	})
	if !errors.Is(err, ErrRecoveryPhraseRequired) {
		t.Fatalf("err = %v, want recovery_phrase_required (never random fallback)", err)
	}
}

func TestCreateRepositoryRejectsBadMode(t *testing.T) {
	b := phraseTestBackend(t)
	_, err := b.CreateBackupRepository(context.Background(), apitypes.CreateBackupRepositoryRequest{
		Kind: "hetzner_storagebox", Label: "h", Mode: "overwrite",
	})
	if err == nil || !strings.Contains(err.Error(), "mode must be create|attach") {
		t.Fatalf("err = %v, want mode validation", err)
	}
}

func TestSanitizeSecretName(t *testing.T) {
	if got := sanitizeSecretName("abc123"); got != "abc123" {
		t.Fatalf("got %q", got)
	}
	if got := sanitizeSecretName("a/b\\c:d"); got != "a-b-c-d" {
		t.Fatalf("got %q", got)
	}
	if got := sanitizeSecretName(""); got != "repo" {
		t.Fatalf("got %q", got)
	}
}
