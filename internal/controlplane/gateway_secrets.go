package controlplane

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
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

// projectProviderSecrets delivers omahab-managed API key material to files the
// LiteLLM gateway can actually read: <configDir>/secrets/provider_<credID>.
// The renderer only emits file:// refs; without this projection the refs
// dangle and chat 401s pre-upstream.
//
// It is convergent: files for credentials absent from creds are pruned (only
// provider_* names are ever touched). Callers MUST invoke it immediately
// before every successful-path ReconcileModels so refs resolve on disk before
// the config goes live, and treat a projection error as fail-closed (clean up
// like a reconcile failure, never reconcile without the material).
//
// Key material is never logged; errors name credential IDs only.
func (b *Backend) projectProviderSecrets(ctx context.Context, creds []providers.Credential) error {
	if b.secrets == nil {
		return fmt.Errorf("gateway secrets: secrets not configured")
	}
	dir := filepath.Join(b.gatewayConfigDir(), "secrets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("gateway secrets mkdir: %w", err)
	}
	shareGatewaySecretsDir(dir)
	live := make(map[string]bool, len(creds))
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
			return fmt.Errorf("gateway secrets: credential with empty id")
		}
		var material string
		var err error
		if strings.TrimSpace(string(c.SecretID)) != "" {
			material, err = b.secrets.Reveal(ctx, c.SecretID)
		} else {
			material, err = b.secrets.RevealByName(ctx, "provider", "credential."+id)
		}
		if err != nil {
			return fmt.Errorf("gateway secrets: cannot reveal material for credential %s: %w", id, err)
		}
		path := filepath.Join(dir, "provider_"+id)
		if err := os.WriteFile(path, []byte(material), 0o600); err != nil {
			return fmt.Errorf("gateway secrets: cannot write file for credential %s: %w", id, err)
		}
		shareGatewaySecretFile(path)
		live["provider_"+id] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("gateway secrets: cannot list %s: %w", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "provider_") || live[name] {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
	return nil
}

// removeProviderSecretFile explicitly drops one projected file (used after
// credential delete, covering paths where no reconcile ran, e.g. nil gateway).
// Best-effort: convergent prune on the next projection is the backstop.
func (b *Backend) removeProviderSecretFile(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	_ = os.Remove(filepath.Join(b.gatewayConfigDir(), "secrets", "provider_"+id))
}

// shareGatewaySecretsDir makes the secrets dir traversable by the litellm
// service group. Best-effort: unit tests and non-NixOS hosts lack the group,
// in which case files stay root-only and the unit fails loudly on restart.
func shareGatewaySecretsDir(dir string) {
	gid, ok := litellmCfgGID()
	if !ok {
		return
	}
	_ = os.Chown(dir, 0, gid)
	_ = os.Chmod(dir, 0o750)
}

// shareGatewaySecretFile makes one projected key file group-readable by the
// litellm service group. Same best-effort semantics as shareGatewaySecretsDir.
func shareGatewaySecretFile(path string) {
	gid, ok := litellmCfgGID()
	if !ok {
		return
	}
	_ = os.Chown(path, 0, gid)
	_ = os.Chmod(path, 0o640)
}

func litellmCfgGID() (int, bool) {
	grp, err := user.LookupGroup("litellm-cfg")
	if err != nil {
		return 0, false
	}
	gid, err := strconv.Atoi(grp.Gid)
	if err != nil {
		return 0, false
	}
	return gid, true
}
