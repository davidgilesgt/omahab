package controlplane

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/omahab/omahab/internal/providers"
)

// TestProjectProviderSecrets_RoundTrip covers the key-material delivery the
// gateway renderer depends on: omahab-managed api_key credentials are written
// to <configDir>/secrets/provider_<id> with exact content, litellm-managed
// oauth credentials produce no file, and a second projection with a smaller
// live set prunes stale provider_* files while leaving unrelated files alone.
func TestProjectProviderSecrets_RoundTrip(t *testing.T) {
	b := newTestBackend(t, nil)
	ctx := context.Background()

	sec1, err := b.secrets.Put(ctx, "provider", "credential.cred1", "sk-live-key-1")
	if err != nil {
		t.Fatalf("put secret cred1: %v", err)
	}
	sec2, err := b.secrets.Put(ctx, "provider", "credential.cred2", "sk-live-key-2")
	if err != nil {
		t.Fatalf("put secret cred2: %v", err)
	}
	// cred3 exercises the by-name fallback (empty SecretID).
	if _, err := b.secrets.Put(ctx, "provider", "credential.cred3", "sk-live-key-3"); err != nil {
		t.Fatalf("put secret cred3: %v", err)
	}
	oauthRef := providers.ExternalRefChatGPT
	creds := []providers.Credential{
		{ID: "cred1", Provider: providers.ProviderOpenAI, CredentialType: providers.CredentialTypeAPIKey, SecretID: sec1.ID, ManagedBy: providers.ManagedByOmahab},
		{ID: "cred2", Provider: providers.ProviderAnthropic, CredentialType: providers.CredentialTypeAPIKey, SecretID: sec2.ID, ManagedBy: providers.ManagedByOmahab},
		{ID: "cred3", Provider: providers.ProviderOpenRouter, CredentialType: providers.CredentialTypeAPIKey, ManagedBy: providers.ManagedByOmahab},
		{ID: "credOA", Provider: providers.ProviderChatGPT, CredentialType: providers.CredentialTypeOAuth, ManagedBy: providers.ManagedByLiteLLM, ExternalRef: &oauthRef},
	}
	if err := b.projectProviderSecrets(ctx, creds); err != nil {
		t.Fatalf("projectProviderSecrets: %v", err)
	}
	dir := filepath.Join(b.gatewayConfigDir(), "secrets")
	for id, want := range map[string]string{
		"cred1": "sk-live-key-1",
		"cred2": "sk-live-key-2",
		"cred3": "sk-live-key-3",
	} {
		raw, err := os.ReadFile(filepath.Join(dir, "provider_"+id))
		if err != nil {
			t.Fatalf("read projected file %s: %v", id, err)
		}
		if string(raw) != want {
			t.Fatalf("projected file %s = %q, want %q", id, raw, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "provider_credOA")); !os.IsNotExist(err) {
		t.Fatalf("oauth credential must not produce a projected file (stat err = %v)", err)
	}
	// Mode: 0640 where the service group exists, else the 0600 we wrote.
	wantMode := os.FileMode(0o600)
	if _, err := user.LookupGroup("litellm-cfg"); err == nil {
		wantMode = os.FileMode(0o640)
	}
	fi, err := os.Stat(filepath.Join(dir, "provider_cred1"))
	if err != nil {
		t.Fatalf("stat projected file: %v", err)
	}
	if fi.Mode().Perm() != wantMode {
		t.Fatalf("projected file mode = %o, want %o", fi.Mode().Perm(), wantMode)
	}

	// Convergent prune: stale provider_* goes, unrelated files stay.
	if err := os.WriteFile(filepath.Join(dir, "provider_stale"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed stale file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatalf("seed unrelated file: %v", err)
	}
	if err := b.projectProviderSecrets(ctx, creds[:1]); err != nil {
		t.Fatalf("re-project: %v", err)
	}
	for _, pruned := range []string{"provider_cred2", "provider_cred3", "provider_stale"} {
		if _, err := os.Stat(filepath.Join(dir, pruned)); !os.IsNotExist(err) {
			t.Fatalf("%s survived prune (stat err = %v)", pruned, err)
		}
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "keep.txt")); err != nil || string(raw) != "keep" {
		t.Fatalf("unrelated file disturbed: content=%q err=%v", raw, err)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "provider_cred1")); err != nil || string(raw) != "sk-live-key-1" {
		t.Fatalf("live file disturbed: content=%q err=%v", raw, err)
	}
}

// TestProjectProviderSecrets_FailClosed ensures a missing secret aborts
// projection with an error (callers translate and roll back; they never
// reconcile a config whose refs dangle).
func TestProjectProviderSecrets_FailClosed(t *testing.T) {
	b := newTestBackend(t, nil)
	creds := []providers.Credential{
		{ID: "ghost", Provider: providers.ProviderOpenAI, CredentialType: providers.CredentialTypeAPIKey, ManagedBy: providers.ManagedByOmahab},
	}
	if err := b.projectProviderSecrets(context.Background(), creds); err == nil {
		t.Fatal("projectProviderSecrets with missing secret = nil, want error")
	}
}
