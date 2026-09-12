package providers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/domain"
)

// TestReconcileModels_APIKeyRefUsesEnviron ensures the rendered api_key ref
// resolves via the litellm unit's EnvironmentFile (controlplane converges
// appenv/litellm.env next to each reconcile). A literal file:// ref would pass
// through unresolved and 401 pre-upstream.
func TestReconcileModels_APIKeyRefUsesEnviron(t *testing.T) {
	dir := t.TempDir()
	gw, err := NewLiteLLMGateway(struct{}{}, GatewayOptions{ConfigDir: dir})
	if err != nil {
		t.Fatalf("NewLiteLLMGateway: %v", err)
	}
	aliases := []Alias{{Name: AliasFast, CredentialID: domain.ID("cred1"), Model: "gpt-4o"}}
	creds := []Credential{apiKeyCred("cred1", ProviderOpenAI)}
	if err := gw.ReconcileModels(context.Background(), aliases, creds); err != nil {
		t.Fatalf("ReconcileModels: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "litellm.yaml"))
	if err != nil {
		t.Fatalf("read rendered config: %v", err)
	}
	content := string(raw)
	want := "api_key: os.environ/" + ProviderEnvVar("cred1")
	if !strings.Contains(content, want) {
		t.Fatalf("want api_key ref %q in:\n%s", want, content)
	}
	for _, banned := range []string{"file://", "/run/secrets"} {
		if strings.Contains(content, banned) {
			t.Fatalf("rendered config must not contain %q:\n%s", banned, content)
		}
	}
}

// TestProviderEnvVar_Sanitize pins the OMAHAB_PROVIDER_<SANITIZED_ID> mapping:
// uppercase, non-alphanumeric to '_', surrounding whitespace trimmed.
func TestProviderEnvVar_Sanitize(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want string
	}{
		{"plain", "cred1", "OMAHAB_PROVIDER_CRED1"},
		{"uppercases", "Cred-AbC", "OMAHAB_PROVIDER_CRED_ABC"},
		{"uuid", "9f2a4e1c-3b7d-4f8a-9c0d-1e2f3a4b5c6d", "OMAHAB_PROVIDER_9F2A4E1C_3B7D_4F8A_9C0D_1E2F3A4B5C6D"},
		{"dots and slashes", "a.b/c", "OMAHAB_PROVIDER_A_B_C"},
		{"spaces", "a b", "OMAHAB_PROVIDER_A_B"},
		{"trims whitespace", "  cred1\n", "OMAHAB_PROVIDER_CRED1"},
		{"empty", "", "OMAHAB_PROVIDER_"},
		{"digits preserved", "123", "OMAHAB_PROVIDER_123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProviderEnvVar(tc.id); got != tc.want {
				t.Fatalf("ProviderEnvVar(%q) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}
