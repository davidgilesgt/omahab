package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/providers"
	"github.com/omahab/omahab/internal/secrets"
	"github.com/omahab/omahab/internal/store"
)

// Karakeep AI env keys rendered into appenv/karakeep.env. Karakeep workers
// speak to any OpenAI-compatible endpoint, so they point at the LiteLLM
// gateway with a karakeep-scoped virtual key; INFERENCE_*_MODEL carries the
// gateway routing alias (a bare model-name string to Karakeep).
const (
	karakeepInferenceBaseURL = "http://127.0.0.1:4000/v1"
	karakeepSecretName       = "karakeep_litellm_key"
	karakeepKeyOwnerID       = "karakeep"
)

// karakeepAIKeys lists every env key owned by the AI render. ensureKarakeepOIDC
// carries these across OIDC re-ensures (writeAppEnv replaces the whole file).
var karakeepAIKeys = []string{
	"OPENAI_BASE_URL",
	"OPENAI_API_KEY",
	"INFERENCE_TEXT_MODEL",
	"INFERENCE_IMAGE_MODEL",
}

// mergeKarakeepAIEnv overlays the AI keys for token onto existing, returning
// the merged map and whether anything changed. OIDC-owned keys pass through
// untouched. An empty token leaves the map unchanged (AI stays off, never
// half-configured: Karakeep enables inference when a provider key is set).
func mergeKarakeepAIEnv(existing map[string]string, token string) (map[string]string, bool) {
	merged := make(map[string]string, len(existing)+len(karakeepAIKeys))
	for k, v := range existing {
		merged[k] = v
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return merged, false
	}
	want := map[string]string{
		"OPENAI_BASE_URL":       karakeepInferenceBaseURL,
		"OPENAI_API_KEY":        token,
		"INFERENCE_TEXT_MODEL":  providers.AliasKarakeep,
		"INFERENCE_IMAGE_MODEL": providers.AliasKarakeep,
	}
	changed := false
	for k, v := range want {
		if merged[k] != v {
			merged[k] = v
			changed = true
		}
	}
	return merged, changed
}

// ensureKarakeepLiteLLMKey issues a LiteLLM virtual key scoped to
// omahab/karakeep and renders it into karakeep.env alongside the gateway
// address and model alias, then redeploys Karakeep. Skip-nil when the admin
// has not mapped the alias yet (AI stays off) or Karakeep is not enrolled
// (no karakeep.env); hard-fail on real errors so a broken render never runs
// silently AI-less. Owner kind harness (never hermes: the hermes key lookup
// matches on kind alone and would adopt a hermes-kind karakeep key).
func (b *Backend) ensureKarakeepLiteLLMKey(ctx context.Context) error {
	if b.providers == nil || b.secrets == nil {
		log.Printf("setup dependent_apps: providers/secrets not configured; skipping karakeep key")
		return nil
	}
	if !b.nativeAliasAvailable(ctx, providers.AliasKarakeep) {
		log.Printf("setup dependent_apps: omahab/karakeep alias not mapped; karakeep AI stays off")
		return nil
	}
	existing, err := b.readAppEnv("karakeep")
	if err != nil {
		return fmt.Errorf("read karakeep appenv: %w", err)
	}
	if len(existing) == 0 {
		log.Printf("setup dependent_apps: karakeep not enrolled; skipping karakeep key")
		return nil
	}
	secretName := karakeepSecretName
	var vkID string
	var expiresAt, revokedAt sql.NullString
	nowStr := store.FormatTime(time.Now().UTC())
	err = b.db.QueryRowContext(ctx, `SELECT id, expires_at, revoked_at FROM provider_virtual_keys WHERE owner_kind = 'harness' AND owner_id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?) ORDER BY created_at DESC LIMIT 1`, karakeepKeyOwnerID, nowStr).Scan(&vkID, &expiresAt, &revokedAt)
	if err == nil {
		if v, rerr := b.secrets.RevealByName(ctx, "platform-app", secretName); rerr == nil && strings.TrimSpace(v) != "" {
			return b.renderKarakeepAIEnv(ctx, existing, strings.TrimSpace(v))
		}
		_ = b.providers.RevokeVirtualKey(ctx, domain.ID(vkID))
	}
	kind := providers.OwnerKindHarness
	ownerID := karakeepKeyOwnerID
	res, err := b.providers.IssueVirtualKey(ctx, providers.IssueVirtualKeyInput{
		Name:      "karakeep",
		Scopes:    []string{providers.AliasKarakeep},
		OwnerKind: &kind,
		OwnerID:   &ownerID,
	})
	if err != nil {
		return fmt.Errorf("issue karakeep virtual key: %w", err)
	}
	if _, err := b.secrets.Put(ctx, "platform-app", secretName, res.Token); err != nil {
		if errors.Is(err, store.ErrConflict) || errors.Is(err, secrets.ErrConflict) {
			_, _ = b.secrets.RotateByName(ctx, "platform-app", secretName, res.Token)
		}
	}
	return b.renderKarakeepAIEnv(ctx, existing, res.Token)
}

// renderKarakeepAIEnv merges the AI keys into the existing karakeep env and
// redeploys only on change, so converged re-runs never bounce the units.
func (b *Backend) renderKarakeepAIEnv(ctx context.Context, existing map[string]string, token string) error {
	merged, changed := mergeKarakeepAIEnv(existing, token)
	if !changed {
		return nil
	}
	if err := b.writeAppEnv("karakeep", merged, "karakeep"); err != nil {
		return fmt.Errorf("write karakeep appenv: %w", err)
	}
	if err := b.redeployBundle(ctx, "karakeep"); err != nil {
		return fmt.Errorf("reload karakeep config: %w", err)
	}
	log.Printf("setup dependent_apps: karakeep AI env rendered")
	return nil
}
