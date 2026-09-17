package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"maps"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/health"
	"github.com/omahab/omahab/internal/providers"
	"github.com/omahab/omahab/internal/secrets"
	"github.com/omahab/omahab/internal/store"
)

const (
	hindsightDBPasswordName = "hindsight_db_password"
	hindsightAPIKeyName     = "hindsight_api_key"
	hindsightEmbedKeyName   = "hindsight_embedding_key"
	hindsightLLMKeyName     = "hindsight_litellm_key"

	hindsightKeyOwnerID = "hindsight"
	hindsightBankID     = "hermes"
)

var hindsightSecretNames = []string{
	hindsightDBPasswordName,
	hindsightAPIKeyName,
	hindsightEmbedKeyName,
	hindsightLLMKeyName,
}

func hindsightBundlePresent(bundles []apps.Bundle) bool {
	for _, bd := range bundles {
		if bd.ID == "hindsight" && bd.Default {
			return true
		}
	}
	return false
}

func (b *Backend) ensureHindsightSecrets(ctx context.Context) error {
	if b.secrets == nil {
		return nil
	}
	for _, name := range []string{hindsightDBPasswordName, hindsightAPIKeyName, hindsightEmbedKeyName} {
		v, err := b.secrets.RevealByName(ctx, "platform-app", name)
		if err == nil && strings.TrimSpace(v) != "" {
			continue
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := upsertSecret(ctx, b.secrets, "platform-app", name, generateRandomBase64URL(32)); err != nil {
			return err
		}
	}
	return nil
}

func hindsightDBURL(password string) string {
	return fmt.Sprintf("postgresql://hindsight:%s@127.0.0.1:5432/hindsight", password)
}

func (b *Backend) ensureHindsightPostgresAuth(ctx context.Context) error {
	password := ""
	if b.secrets != nil {
		if v, err := b.secrets.RevealByName(ctx, "platform-app", hindsightDBPasswordName); err == nil {
			password = strings.TrimSpace(v)
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	if !validPostgresPassword(password) {
		return fmt.Errorf("hindsight_db_password missing or outside safe alphabet")
	}
	stmt := "ALTER ROLE \"hindsight\" WITH PASSWORD '" + password + "'"
	if out, err := runPostgresAlterRole(ctx, "hindsight", stmt); err != nil {
		log.Printf("hindsight postgres auth failed: %s", health.RedactDetail(strings.TrimSpace(out+" "+err.Error())))
		return fmt.Errorf("hindsight postgres auth failed")
	}
	for _, ext := range []string{"vector", "pg_trgm"} {
		extStmt := "CREATE EXTENSION IF NOT EXISTS \"" + ext + "\""
		if out, err := runPostgresAlterRole(ctx, "hindsight", extStmt); err != nil {
			log.Printf("hindsight extension %s failed: %s", ext, health.RedactDetail(strings.TrimSpace(out+" "+err.Error())))
			return fmt.Errorf("hindsight extension %s unavailable", ext)
		}
	}
	return nil
}

func (b *Backend) ensureHindsightLiteLLMKey(ctx context.Context) (string, error) {
	if b.providers == nil || b.secrets == nil {
		return "", fmt.Errorf("providers/secrets not configured")
	}
	nowStr := store.FormatTime(time.Now().UTC())
	var vkID, scopesStr string
	var expiresAt, revokedAt sql.NullString
	err := b.db.QueryRowContext(ctx, `SELECT id, scopes, expires_at, revoked_at FROM provider_virtual_keys WHERE owner_kind = 'harness' AND owner_id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?) ORDER BY created_at DESC LIMIT 1`, hindsightKeyOwnerID, nowStr).Scan(&vkID, &scopesStr, &expiresAt, &revokedAt)
	if err == nil {
		scopes := []string{}
		for _, s := range strings.Split(scopesStr, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				scopes = append(scopes, s)
			}
		}
		if len(scopes) == 1 && scopes[0] == providers.AliasMemory {
			if v, rerr := b.secrets.RevealByName(ctx, "platform-app", hindsightLLMKeyName); rerr == nil && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v), nil
			} else if rerr != nil && !errors.Is(rerr, store.ErrNotFound) {
				return "", rerr
			}
		}
		_ = b.providers.RevokeVirtualKey(ctx, domain.ID(vkID))
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	kind := providers.OwnerKindHarness
	ownerID := hindsightKeyOwnerID
	res, err := b.providers.IssueVirtualKey(ctx, providers.IssueVirtualKeyInput{
		Name:      "hindsight",
		Scopes:    []string{providers.AliasMemory},
		OwnerKind: &kind,
		OwnerID:   &ownerID,
	})
	if err != nil {
		return "", fmt.Errorf("issue hindsight virtual key: %w", err)
	}
	if _, err := b.secrets.Put(ctx, "platform-app", hindsightLLMKeyName, res.Token); err != nil {
		if errors.Is(err, store.ErrConflict) || errors.Is(err, secrets.ErrConflict) {
			if _, rerr := b.secrets.RotateByName(ctx, "platform-app", hindsightLLMKeyName, res.Token); rerr != nil {
				return "", rerr
			}
		} else {
			return "", err
		}
	}
	return res.Token, nil
}

func hindsightDesiredEnv(dbURL, llmKey, embedKey, apiKey string) map[string]string {
	return map[string]string{
		"HINDSIGHT_ENABLE_API":                        "true",
		"HINDSIGHT_ENABLE_CP":                         "false",
		"HINDSIGHT_API_HOST":                          "127.0.0.1",
		"HINDSIGHT_API_PORT":                          "8888",
		"HINDSIGHT_API_HEALTH_URL":                    "http://127.0.0.1:8888/health",
		"HINDSIGHT_API_DATABASE_URL":                  dbURL,
		"HINDSIGHT_API_VECTOR_EXTENSION":              "pgvector",
		"HINDSIGHT_API_TEXT_SEARCH_EXTENSION":         "native",
		"HINDSIGHT_API_LLM_PROVIDER":                  "openai",
		"HINDSIGHT_API_LLM_BASE_URL":                  "http://127.0.0.1:4000/v1",
		"HINDSIGHT_API_LLM_MODEL":                     "omahab/memory",
		"HINDSIGHT_API_LLM_API_KEY":                   llmKey,
		"HINDSIGHT_API_LLM_REASONING_EFFORT":          "none",
		"HINDSIGHT_API_LLM_TEMPERATURE":               "none",
		"HINDSIGHT_API_EMBEDDINGS_PROVIDER":           "openai",
		"HINDSIGHT_API_EMBEDDINGS_OPENAI_BASE_URL":    "http://127.0.0.1:8484/api/v1/hindsight",
		"HINDSIGHT_API_EMBEDDINGS_OPENAI_MODEL":       "omahab-embed-english",
		"HINDSIGHT_API_EMBEDDINGS_OPENAI_API_KEY":     embedKey,
		"HINDSIGHT_API_EMBEDDINGS_OPENAI_BATCH_SIZE":  "1",
		"HINDSIGHT_API_EMBEDDINGS_QUERY_PREFIX":       "search_query: ",
		"HINDSIGHT_API_EMBEDDINGS_PASSAGE_PREFIX":     "search_document: ",
		"HINDSIGHT_API_EMBEDDINGS_MAX_CONCURRENT_REQUESTS": "1",
		"HINDSIGHT_API_RERANKER_PROVIDER":             "rrf",
		"HINDSIGHT_API_ENABLE_RERANKING":              "false",
		"HINDSIGHT_API_TENANT_EXTENSION":              "hindsight_api.extensions.builtin.tenant:ApiKeyTenantExtension",
		"HINDSIGHT_API_TENANT_API_KEY":                apiKey,
		"HINDSIGHT_API_WORKERS":                       "1",
		"HINDSIGHT_API_WORKER_ID":                     "omahab-hindsight",
		"HINDSIGHT_API_WORKER_MAX_SLOTS":              "2",
		"HINDSIGHT_API_WORKER_CONSOLIDATION_RESERVED_SLOTS": "1",
		"HINDSIGHT_API_LLM_MAX_CONCURRENT":            "2",
		"HINDSIGHT_API_RETAIN_MAX_CONCURRENT":         "1",
		"HINDSIGHT_API_RETAIN_SUBBATCH_CONCURRENCY":   "1",
		"HINDSIGHT_API_RECALL_MAX_CONCURRENT":         "1",
		"HINDSIGHT_API_RECALL_CONNECTION_BUDGET":      "2",
		"HINDSIGHT_API_DB_POOL_MIN_SIZE":              "1",
		"HINDSIGHT_API_DB_POOL_MAX_SIZE":              "10",
		"HINDSIGHT_API_LLM_TRACE_ENABLED":             "false",
	}
}

func normalizeHindsightEnvForCompare(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	for _, k := range []string{"HINDSIGHT_API_EMBEDDINGS_QUERY_PREFIX", "HINDSIGHT_API_EMBEDDINGS_PASSAGE_PREFIX"} {
		if v, ok := out[k]; ok {
			out[k] = strings.TrimSpace(v)
		}
	}
	return out
}

func (b *Backend) renderHindsightEnv(ctx context.Context, llmToken string) (bool, error) {
	llmToken = strings.TrimSpace(llmToken)
	if llmToken == "" {
		return false, fmt.Errorf("hindsight litellm key is required")
	}
	if b.secrets == nil {
		return false, fmt.Errorf("secrets not configured")
	}
	dbPassword, err := b.secrets.RevealByName(ctx, "platform-app", hindsightDBPasswordName)
	if err != nil {
		return false, err
	}
	dbPassword = strings.TrimSpace(dbPassword)
	if !validPostgresPassword(dbPassword) {
		return false, fmt.Errorf("hindsight_db_password missing or outside safe alphabet")
	}
	embedKey, err := b.secrets.RevealByName(ctx, "platform-app", hindsightEmbedKeyName)
	if err != nil {
		return false, err
	}
	embedKey = strings.TrimSpace(embedKey)
	if embedKey == "" {
		return false, fmt.Errorf("hindsight_embedding_key missing")
	}
	apiKey, err := b.secrets.RevealByName(ctx, "platform-app", hindsightAPIKeyName)
	if err != nil {
		return false, err
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return false, fmt.Errorf("hindsight_api_key missing")
	}
	desired := hindsightDesiredEnv(hindsightDBURL(dbPassword), llmToken, embedKey, apiKey)
	prev, err := b.readAppEnv("hindsight")
	if err != nil {
		return false, fmt.Errorf("read hindsight appenv: %w", err)
	}
	if maps.Equal(normalizeHindsightEnvForCompare(prev), normalizeHindsightEnvForCompare(desired)) {
		return false, nil
	}
	if err := b.writeAppEnv("hindsight", desired, ""); err != nil {
		return false, fmt.Errorf("write hindsight appenv: %w", err)
	}
	return true, nil
}

func (b *Backend) ensureHindsightDependentApp(ctx context.Context) error {
	if b.apps == nil {
		return fmt.Errorf("apps not configured")
	}
	var hindsightBundle *apps.Bundle
	for _, bd := range b.apps.CatalogBundles() {
		if bd.ID == "hindsight" {
			if !bd.Default {
				return fmt.Errorf("Hindsight bundle is not default; enable Hindsight to use Hermes memory")
			}
			c := bd
			hindsightBundle = &c
			break
		}
	}
	if hindsightBundle == nil {
		hasDefaultHermes := false
		for _, bd := range b.apps.CatalogBundles() {
			if bd.ID == "hermes" && bd.Default {
				hasDefaultHermes = true
				break
			}
		}
		if hasDefaultHermes {
			return fmt.Errorf("Hindsight bundle missing; default Hermes requires Hindsight for memory")
		}
		return nil
	}
	if m, err := apps.GetMaintenance(ctx, b.db, "hindsight"); err == nil {
		log.Printf("setup: bundle %q paused for %s; skipping hindsight provisioning", "hindsight", m.Reason)
		return fmt.Errorf("setup cannot activate memory during maintenance (hindsight paused)")
	}
	if m, err := apps.GetMaintenance(ctx, b.db, "hermes"); err == nil {
		log.Printf("setup: bundle %q paused for %s; skipping hindsight provisioning", "hermes", m.Reason)
		return fmt.Errorf("setup cannot activate memory during maintenance (hermes paused)")
	}
	if b.gateway == nil {
		return fmt.Errorf("Hindsight requires omahab/memory. Configure this model in LiteLLM, then retry setup.")
	}
	native, err := b.gateway.ListModels(ctx)
	if err != nil {
		return fmt.Errorf("Hindsight requires omahab/memory. Configure this model in LiteLLM, then retry setup.")
	}
	foundMemory := false
	foundMode := ""
	for _, d := range native {
		if strings.TrimSpace(d.ModelName) != providers.AliasMemory {
			continue
		}
		foundMemory = true
		foundMode = strings.ToLower(strings.TrimSpace(d.ModelInfo.Mode))
		if foundMode == "" {
			if litellmProvider := strings.ToLower(strings.TrimSpace(d.ModelInfo.LitellmProvider)); litellmProvider != "" {
				foundMode = "chat"
			}
		}
		break
	}
	if !foundMemory {
		return fmt.Errorf("Hindsight requires omahab/memory. Configure this model in LiteLLM, then retry setup.")
	}
	if foundMode != "" && foundMode != "chat" {
		return fmt.Errorf("Hindsight requires omahab/memory in chat mode (found %s)", foundMode)
	}
	if err := b.checkHindsightEmbeddings(ctx); err != nil {
		log.Printf("hindsight embedding readiness failed: %v", err)
		return fmt.Errorf("Hindsight requires the shared Nomic worker: omahab-embed-english, 768 dimensions. Configure that worker, then retry setup.")
	}
	if err := b.ensureHindsightPostgresAuth(ctx); err != nil {
		return err
	}
	llmToken, err := b.ensureHindsightLiteLLMKey(ctx)
	if err != nil {
		return err
	}
	changed, err := b.renderHindsightEnv(ctx, llmToken)
	if err != nil {
		return err
	}
	if changed {
		if err := b.redeployBundle(ctx, "hindsight"); err != nil {
			log.Printf("hindsight redeploy after env change: %v", health.RedactDetail(err.Error()))
		}
	}
	domainName := ""
	if inst, err := b.store.Instance(ctx); err == nil {
		domainName = strings.TrimSpace(inst.Domain)
	}
	if err := b.ensureDefaultApp(ctx, *hindsightBundle, domainName); err != nil {
		return err
	}
	list, err := b.apps.List(ctx)
	if err != nil {
		return fmt.Errorf("list apps: %w", err)
	}
	for _, st := range list {
		if st.BundleID != "hindsight" {
			continue
		}
		if st.ObservedState != apps.ObservedRunning || st.Health != domain.HealthHealthy {
			return fmt.Errorf("hindsight not healthy; retry setup after it becomes healthy")
		}
		return nil
	}
	return fmt.Errorf("hindsight app not found after install")
}
