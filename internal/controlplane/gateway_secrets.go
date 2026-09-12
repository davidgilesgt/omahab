package controlplane

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/omahab/omahab/internal/providers"
)

// gatewayConfigDir mirrors the gateway wiring in backend.go: the LiteLLM
// config tree lives under <DataDir>/apps/litellm/config.
func (b *Backend) gatewayConfigDir() string {
	if strings.TrimSpace(b.cfg.DataDir) == "" {
		return "/srv/omahab/apps/litellm/config"
	}
	return b.cfg.DataDir + "/apps/litellm/config"
}

// revealProviderEnv reveals omahab-managed API key material keyed by
// environment variable name (providers.ProviderEnvVar). litellm-managed oauth
// credentials carry no key material and contribute nothing. Fail-closed: any
// reveal error aborts with an error naming the credential ID only.
//
// Key material is never logged; errors name credential IDs only.
func (b *Backend) revealProviderEnv(ctx context.Context, creds []providers.Credential) (map[string]string, error) {
	if b.secrets == nil {
		return nil, fmt.Errorf("gateway secrets: secrets not configured")
	}
	live := make(map[string]string, len(creds))
	for _, c := range creds {
		if strings.ToLower(strings.TrimSpace(c.CredentialType)) != providers.CredentialTypeAPIKey {
			continue
		}
		mb := strings.TrimSpace(c.ManagedBy)
		if mb == "" {
			mb = providers.ManagedByOmahab
		}
		if mb != providers.ManagedByOmahab {
			continue
		}
		id := strings.TrimSpace(string(c.ID))
		if id == "" {
			return nil, fmt.Errorf("gateway secrets: credential with empty id")
		}
		var material string
		var err error
		if strings.TrimSpace(string(c.SecretID)) != "" {
			material, err = b.secrets.Reveal(ctx, c.SecretID)
		} else {
			material, err = b.secrets.RevealByName(ctx, "provider", "credential."+id)
		}
		if err != nil {
			return nil, fmt.Errorf("gateway secrets: cannot reveal material for credential %s: %w", id, err)
		}
		live[providers.ProviderEnvVar(id)] = material
	}
	return live, nil
}

// mergeLiteLLMProviderEnv is the single-writer merge for appenv/litellm.env,
// shared by the runtime projection (syncLiteLLMProviderEnv) and the setup
// render (renderNativeAppEnv). It copies existing (preserving everything else,
// including master key and DB URL), sets every live OMAHAB_PROVIDER_* var, and
// prunes stale OMAHAB_PROVIDER_* keys absent from live. No other prefix is
// ever touched.
func mergeLiteLLMProviderEnv(existing, live map[string]string) map[string]string {
	merged := make(map[string]string, len(existing)+len(live))
	for k, v := range existing {
		merged[k] = v
	}
	for k := range merged {
		if !strings.HasPrefix(k, providers.ProviderEnvVarPrefix) {
			continue
		}
		if _, ok := live[k]; !ok {
			delete(merged, k)
		}
	}
	for k, v := range live {
		merged[k] = v
	}
	return merged
}

// syncLiteLLMProviderEnv converges appenv/litellm.env to the existing platform
// vars (master key, DB URL, untouched) plus the live provider vars revealed
// from creds, pruning stale OMAHAB_PROVIDER_* entries. The renderer only emits
// os.environ/<NAME> refs; without this projection the refs dangle and chat
// 401s pre-upstream. Restart re-reads the EnvironmentFile, so the existing
// reload ordering (env → reconcile → restart → health) just works.
//
// Callers MUST invoke it immediately before every successful-path
// ReconcileModels so refs resolve via the environment before the config goes
// live, and treat a sync error as fail-closed (clean up like a reconcile
// failure, never reconcile without the material). It also convergently prunes
// legacy per-credential files (<configDir>/secrets/provider_*).
//
// Key material is never logged; errors name credential IDs only.
func (b *Backend) syncLiteLLMProviderEnv(ctx context.Context, creds []providers.Credential) error {
	live, err := b.revealProviderEnv(ctx, creds)
	if err != nil {
		return err
	}
	existing, err := b.readAppEnv("litellm")
	if err != nil {
		return fmt.Errorf("gateway secrets: cannot read litellm env: %w", err)
	}
	merged := mergeLiteLLMProviderEnv(existing, live)
	if len(merged) == 0 {
		// Nothing to gate the unit on yet; leave a missing env file missing.
		pruneOrphanProviderSecretFiles(b.gatewayConfigDir())
		return nil
	}
	if err := b.writeAppEnv("litellm", merged, "litellm"); err != nil {
		return fmt.Errorf("gateway secrets: cannot write litellm env: %w", err)
	}
	pruneOrphanProviderSecretFiles(b.gatewayConfigDir())
	return nil
}

// pruneOrphanProviderSecretFiles removes legacy per-credential files
// (<configDir>/secrets/provider_*) left over from file://-ref delivery.
// Best-effort; only provider_* names are ever touched. Missing dir is a no-op.
func pruneOrphanProviderSecretFiles(configDir string) {
	dir := filepath.Join(configDir, "secrets")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "provider_") {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// removeProviderSecretFile explicitly drops one legacy projected file (used
// after credential delete, covering paths where no reconcile ran, e.g. nil
// gateway). Best-effort: convergent prune on the next sync is the backstop.
func (b *Backend) removeProviderSecretFile(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	_ = os.Remove(filepath.Join(b.gatewayConfigDir(), "secrets", "provider_"+id))
}
