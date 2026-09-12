package providers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/domain"
)

// TestReconcileModels_APIKeyRefPointsUnderConfigDir ensures the rendered
// api_key file:// ref resolves to the projected secrets dir under the
// gateway's own configDir (controlplane writes provider_<id> there next to
// each reconcile). A dangling absolute ref (e.g. /run/secrets, which nothing
// projects) 401s pre-upstream.
func TestReconcileModels_APIKeyRefPointsUnderConfigDir(t *testing.T) {
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
	want := "file://" + filepath.Join(dir, "secrets", "provider_cred1")
	if !strings.Contains(content, want) {
		t.Fatalf("want api_key ref %q in:\n%s", want, content)
	}
	if strings.Contains(content, "/run/secrets") {
		t.Fatalf("rendered config must not reference /run/secrets:\n%s", content)
	}
}
