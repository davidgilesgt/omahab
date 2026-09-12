package providers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/domain"
)

// renderTestConfig reconciles the given aliases/creds into a temp dir and
// returns the rendered litellm.yaml content.
func renderTestConfig(t *testing.T, aliases []Alias, creds []Credential) string {
	t.Helper()
	dir := t.TempDir()
	gw, err := NewLiteLLMGateway(struct{}{}, GatewayOptions{ConfigDir: dir})
	if err != nil {
		t.Fatalf("NewLiteLLMGateway: %v", err)
	}
	if err := gw.ReconcileModels(context.Background(), aliases, creds); err != nil {
		t.Fatalf("ReconcileModels: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "litellm.yaml"))
	if err != nil {
		t.Fatalf("read rendered config: %v", err)
	}
	return string(raw)
}

func apiKeyCred(id, provider string) Credential {
	return Credential{
		ID:             domain.ID(id),
		Provider:       provider,
		CredentialType: CredentialTypeAPIKey,
		SecretID:       domain.ID("sec-" + id),
		ManagedBy:      ManagedByOmahab,
	}
}

// TestReconcileModels_OpenRouterSlashModelGetsPrefix ensures a slash-carrying
// model on an OpenRouter API-key credential is routed via the openrouter
// provider instead of passing through verbatim (which breaks routing).
func TestReconcileModels_OpenRouterSlashModelGetsPrefix(t *testing.T) {
	content := renderTestConfig(t,
		[]Alias{{Name: AliasFast, CredentialID: "cred1", Model: "deepseek/deepseek-v4-flash-0731"}},
		[]Credential{apiKeyCred("cred1", ProviderOpenRouter)},
	)
	if !strings.Contains(content, "model: openrouter/deepseek/deepseek-v4-flash-0731") {
		t.Fatalf("slash model missing openrouter/ prefix:\n%s", content)
	}
	if strings.Contains(content, "model: deepseek/deepseek-v4-flash-0731\n") {
		t.Fatalf("slash model passed through verbatim:\n%s", content)
	}
}

// TestReconcileModels_APIKeyPrefixBehavior pins per-provider prefixing:
// plain models gain their provider prefix, already-prefixed models are
// unchanged, and non-leading slashes no longer suppress the prefix.
func TestReconcileModels_APIKeyPrefixBehavior(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		model    string
		want     string
	}{
		{"openai plain", ProviderOpenAI, "gpt-4o", "model: openai/gpt-4o"},
		{"anthropic plain", ProviderAnthropic, "claude-sonnet", "model: anthropic/claude-sonnet"},
		{"openrouter plain", ProviderOpenRouter, "llama-3", "model: openrouter/llama-3"},
		{"openai slash", ProviderOpenAI, "org/gpt-4o", "model: openai/org/gpt-4o"},
		{"anthropic slash", ProviderAnthropic, "org/claude", "model: anthropic/org/claude"},
		{"openai already prefixed", ProviderOpenAI, "openai/gpt-4o", "model: openai/gpt-4o"},
		{"openrouter already prefixed", ProviderOpenRouter, "openrouter/deepseek/x", "model: openrouter/deepseek/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := renderTestConfig(t,
				[]Alias{{Name: AliasFast, CredentialID: "cred1", Model: tc.model}},
				[]Credential{apiKeyCred("cred1", tc.provider)},
			)
			if !strings.Contains(content, tc.want+"\n") && !strings.Contains(content, tc.want+" ") {
				t.Fatalf("want %q in:\n%s", tc.want, content)
			}
			// Already-prefixed values must not gain a doubled prefix.
			if strings.HasPrefix(strings.ToLower(tc.model), tc.provider+"/") &&
				strings.Contains(content, tc.provider+"/"+tc.model) {
				t.Fatalf("doubled prefix for %q:\n%s", tc.model, content)
			}
		})
	}
}
