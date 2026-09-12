package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/omahab/omahab/internal/providers"
)

// TestSyncLiteLLMProviderEnv_RoundTrip covers the key-material delivery the
// gateway renderer depends on: omahab-managed api_key credentials land in
// appenv/litellm.env as OMAHAB_PROVIDER_<ID> vars, litellm-managed oauth
// credentials produce no var, platform vars (master key, DB URL) survive, and
// a second sync with a smaller live set prunes stale provider vars while
// leaving everything else alone.
func TestSyncLiteLLMProviderEnv_RoundTrip(t *testing.T) {
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
	// Seed platform vars so preservation is exercised.
	if err := b.writeAppEnv("litellm", map[string]string{
		"LITELLM_MASTER_KEY": "mk-test",
		"DATABASE_URL":       "postgres://db",
	}, "litellm"); err != nil {
		t.Fatalf("seed litellm env: %v", err)
	}
	oauthRef := providers.ExternalRefChatGPT
	creds := []providers.Credential{
		{ID: "cred1", Provider: providers.ProviderOpenAI, CredentialType: providers.CredentialTypeAPIKey, SecretID: sec1.ID, ManagedBy: providers.ManagedByOmahab},
		{ID: "cred2", Provider: providers.ProviderAnthropic, CredentialType: providers.CredentialTypeAPIKey, SecretID: sec2.ID, ManagedBy: providers.ManagedByOmahab},
		{ID: "cred3", Provider: providers.ProviderOpenRouter, CredentialType: providers.CredentialTypeAPIKey, ManagedBy: providers.ManagedByOmahab},
		{ID: "credOA", Provider: providers.ProviderChatGPT, CredentialType: providers.CredentialTypeOAuth, ManagedBy: providers.ManagedByLiteLLM, ExternalRef: &oauthRef},
	}
	if err := b.syncLiteLLMProviderEnv(ctx, creds); err != nil {
		t.Fatalf("syncLiteLLMProviderEnv: %v", err)
	}
	env, err := b.readAppEnv("litellm")
	if err != nil {
		t.Fatalf("read litellm env: %v", err)
	}
	for k, want := range map[string]string{
		"LITELLM_MASTER_KEY":          "mk-test",
		"DATABASE_URL":                "postgres://db",
		"OMAHAB_PROVIDER_CRED1":       "sk-live-key-1",
		"OMAHAB_PROVIDER_CRED2":       "sk-live-key-2",
		"OMAHAB_PROVIDER_CRED3":       "sk-live-key-3",
	} {
		if env[k] != want {
			t.Fatalf("litellm env[%s] = %q, want %q (full env: %v)", k, env[k], want, env)
		}
	}
	if _, ok := env["OMAHAB_PROVIDER_CREDOA"]; ok {
		t.Fatalf("oauth credential must not produce a provider var (env: %v)", env)
	}
	// Mode 0640: the litellm unit reads via EnvironmentFile as its own user.
	fi, err := os.Stat(filepath.Join(b.appEnvDir(), "litellm.env"))
	if err != nil {
		t.Fatalf("stat litellm env: %v", err)
	}
	if fi.Mode().Perm() != os.FileMode(0o640) {
		t.Fatalf("litellm env mode = %o, want 640", fi.Mode().Perm())
	}

	// Convergent prune: stale provider vars go, platform vars stay. Legacy
	// per-credential files prune too while unrelated files stay.
	secretsDir := filepath.Join(b.gatewayConfigDir(), "secrets")
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		t.Fatalf("mkdir legacy secrets dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "provider_stale"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed stale legacy file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatalf("seed unrelated file: %v", err)
	}
	if err := b.syncLiteLLMProviderEnv(ctx, creds[:1]); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	env, err = b.readAppEnv("litellm")
	if err != nil {
		t.Fatalf("read litellm env after prune: %v", err)
	}
	for _, pruned := range []string{"OMAHAB_PROVIDER_CRED2", "OMAHAB_PROVIDER_CRED3"} {
		if _, ok := env[pruned]; ok {
			t.Fatalf("%s survived prune (env: %v)", pruned, env)
		}
	}
	for k, want := range map[string]string{
		"LITELLM_MASTER_KEY":    "mk-test",
		"DATABASE_URL":          "postgres://db",
		"OMAHAB_PROVIDER_CRED1": "sk-live-key-1",
	} {
		if env[k] != want {
			t.Fatalf("after prune env[%s] = %q, want %q", k, env[k], want)
		}
	}
	if _, err := os.Stat(filepath.Join(secretsDir, "provider_stale")); !os.IsNotExist(err) {
		t.Fatalf("legacy provider_stale survived prune (stat err = %v)", err)
	}
	if raw, err := os.ReadFile(filepath.Join(secretsDir, "keep.txt")); err != nil || string(raw) != "keep" {
		t.Fatalf("unrelated file disturbed: content=%q err=%v", raw, err)
	}
}

// TestSyncLiteLLMProviderEnv_FailClosed ensures a missing secret aborts the
// sync with an error and leaves the existing env file untouched (callers
// translate and roll back; they never reconcile a config whose refs dangle).
func TestSyncLiteLLMProviderEnv_FailClosed(t *testing.T) {
	b := newTestBackend(t, nil)
	if err := b.writeAppEnv("litellm", map[string]string{
		"LITELLM_MASTER_KEY": "mk-test",
	}, "litellm"); err != nil {
		t.Fatalf("seed litellm env: %v", err)
	}
	creds := []providers.Credential{
		{ID: "ghost", Provider: providers.ProviderOpenAI, CredentialType: providers.CredentialTypeAPIKey, ManagedBy: providers.ManagedByOmahab},
	}
	if err := b.syncLiteLLMProviderEnv(context.Background(), creds); err == nil {
		t.Fatal("syncLiteLLMProviderEnv with missing secret = nil, want error")
	}
	env, err := b.readAppEnv("litellm")
	if err != nil {
		t.Fatalf("read litellm env: %v", err)
	}
	if len(env) != 1 || env["LITELLM_MASTER_KEY"] != "mk-test" {
		t.Fatalf("failed sync disturbed env: %v", env)
	}
}

// TestMergeLiteLLMProviderEnv pins the single-writer merge: live vars add and
// update, stale OMAHAB_PROVIDER_* prune, everything else (master, db, foreign
// keys) is preserved, and the existing map is not mutated.
func TestMergeLiteLLMProviderEnv(t *testing.T) {
	existing := map[string]string{
		"LITELLM_MASTER_KEY":  "mk",
		"DATABASE_URL":        "db",
		"FOREIGN":             "keep",
		"OMAHAB_PROVIDER_OLD": "old",
	}
	live := map[string]string{
		"OMAHAB_PROVIDER_OLD": "new",
		"OMAHAB_PROVIDER_ADD": "add",
	}
	merged := mergeLiteLLMProviderEnv(existing, live)
	want := map[string]string{
		"LITELLM_MASTER_KEY":  "mk",
		"DATABASE_URL":        "db",
		"FOREIGN":             "keep",
		"OMAHAB_PROVIDER_OLD": "new",
		"OMAHAB_PROVIDER_ADD": "add",
	}
	if len(merged) != len(want) {
		t.Fatalf("merged = %v, want %v", merged, want)
	}
	for k, v := range want {
		if merged[k] != v {
			t.Fatalf("merged[%s] = %q, want %q", k, merged[k], v)
		}
	}
	if existing["OMAHAB_PROVIDER_OLD"] != "old" {
		t.Fatalf("merge mutated existing map: %v", existing)
	}
	// Prune: live without OLD drops it.
	pruned := mergeLiteLLMProviderEnv(existing, map[string]string{})
	if _, ok := pruned["OMAHAB_PROVIDER_OLD"]; ok {
		t.Fatalf("stale provider var survived prune: %v", pruned)
	}
	for k, v := range map[string]string{"LITELLM_MASTER_KEY": "mk", "DATABASE_URL": "db", "FOREIGN": "keep"} {
		if pruned[k] != v {
			t.Fatalf("prune disturbed %s = %q, want %q", k, pruned[k], v)
		}
	}
}

// TestReadAppEnv_RoundTrip pins the minimal KEY=VALUE reader: missing file is
// empty (not an error), and a written file reads back exactly.
func TestReadAppEnv_RoundTrip(t *testing.T) {
	b := newTestBackend(t, nil)
	env, err := b.readAppEnv("litellm")
	if err != nil {
		t.Fatalf("read missing env: %v", err)
	}
	if len(env) != 0 {
		t.Fatalf("missing env = %v, want empty", env)
	}
	want := map[string]string{
		"LITELLM_MASTER_KEY":    "mk",
		"DATABASE_URL":          "postgres://db",
		"OMAHAB_PROVIDER_CRED1": "sk-key",
	}
	if err := b.writeAppEnv("litellm", want, "litellm"); err != nil {
		t.Fatalf("write env: %v", err)
	}
	got, err := b.readAppEnv("litellm")
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("read env = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("read env[%s] = %q, want %q", k, got[k], v)
		}
	}
}
