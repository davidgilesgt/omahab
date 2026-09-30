package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/providers"
)

var seedMu sync.Mutex

var seedRoles = []string{
	providers.AliasFast,
	providers.AliasBalanced,
	providers.AliasReasoning,
	providers.AliasSummarization,
}

var modelSetupAliases = []string{
	providers.AliasFast,
	providers.AliasBalanced,
	providers.AliasReasoning,
	providers.AliasSummarization,
	providers.AliasEmbedding,
	providers.AliasKarakeep,
	providers.AliasMemory,
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func seedCredentialName(sourceID string) string {
	return "omahab-seed-" + sha256Hex(strings.TrimSpace(sourceID))
}

func seedRoleID(alias string) string {
	return "omahab-seed-" + sha256Hex(strings.TrimSpace(alias))
}

// --- salt ---

// ensureLitellmSalt resolves the effective LITELLM_SALT_KEY per freeze semantics
// and persists it through the broker. It never regenerates during setup and
// never logs secret material. Returns the chosen salt (possibly empty when
// neither active nor broker material exists yet; caller then leaves env alone).
func (b *Backend) ensureLitellmSalt(ctx context.Context, existing map[string]string) string {
	active := strings.TrimSpace(existing["LITELLM_SALT_KEY"])
	if active != "" {
		// Keep byte-for-byte; mirror into broker so retries converge.
		if b.secrets != nil {
			if cur, err := b.secrets.RevealByName(ctx, "platform-app", "litellm_salt_key"); err != nil || strings.TrimSpace(cur) != active {
				_ = upsertSecret(ctx, b.secrets, "platform-app", "litellm_salt_key", active)
			}
		}
		return active
	}
	// No active salt: freeze current effective master key before any change.
	effectiveMaster := strings.TrimSpace(existing["LITELLM_MASTER_KEY"])
	if effectiveMaster == "" && b.secrets != nil {
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "litellm_master_key"); err == nil {
			effectiveMaster = strings.TrimSpace(v)
		}
	}
	if effectiveMaster != "" {
		if b.secrets != nil {
			_ = upsertSecret(ctx, b.secrets, "platform-app", "litellm_salt_key", effectiveMaster)
		}
		return effectiveMaster
	}
	// Fresh install: use already-declared broker secret (materialized from
	// catalog secret_sources); never generate here.
	if b.secrets != nil {
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "litellm_salt_key"); err == nil && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// --- provider identity ---

var standardAPIHosts = map[string]string{
	providers.ProviderOpenAI:     "api.openai.com",
	providers.ProviderAnthropic:  "api.anthropic.com",
	providers.ProviderOpenRouter: "openrouter.ai",
	providers.ProviderXAI:        "api.x.ai",
}

func normalizeOrigin(apiBase string) string {
	apiBase = strings.TrimSpace(apiBase)
	if apiBase == "" {
		return ""
	}
	u, err := url.Parse(apiBase)
	if err != nil || strings.TrimSpace(u.Host) == "" {
		// Fall back to raw lowercased host-like prefix.
		lower := strings.ToLower(apiBase)
		lower = strings.TrimPrefix(lower, "https://")
		lower = strings.TrimPrefix(lower, "http://")
		if i := strings.Index(lower, "/"); i >= 0 {
			lower = lower[:i]
		}
		if i := strings.Index(lower, ":"); i >= 0 {
			lower = lower[:i]
		}
		return strings.TrimSpace(lower)
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	return host
}

func knownProvider(p string) bool {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case providers.ProviderOpenAI, providers.ProviderAnthropic, providers.ProviderOpenRouter, providers.ProviderChatGPT, providers.ProviderXAI:
		return true
	}
	return false
}

// resolveProviderIdentity maps native deployment metadata to a consent identity.
// Built-in standard endpoints preserve plain provider keys; custom api_base
// endpoints qualify by normalized origin so consent to [OI] is never reused
// for a different [OI]-compatible host. Unknown/opaque routing is unresolved.
func resolveProviderIdentity(d providers.GatewayDeployment) (string, bool) {
	params := d.LitellmParams
	if params == nil {
		params = map[string]any{}
	}
	modelStr, _ := params["model"].(string)
	modelStr = strings.TrimSpace(modelStr)
	customProvider := ""
	if v, ok := params["custom_llm_provider"].(string); ok {
		customProvider = strings.ToLower(strings.TrimSpace(v))
	}
	infoProvider := strings.ToLower(strings.TrimSpace(d.ModelInfo.LitellmProvider))
	apiBase := ""
	if v, ok := params["api_base"].(string); ok {
		apiBase = strings.TrimSpace(v)
	}
	base := ""
	if knownProvider(customProvider) {
		base = customProvider
	} else if knownProvider(infoProvider) {
		base = infoProvider
	} else if modelStr != "" {
		if i := strings.Index(modelStr, "/"); i > 0 {
			prefix := strings.ToLower(strings.TrimSpace(modelStr[:i]))
			if knownProvider(prefix) {
				base = prefix
			}
		}
	}
	if base == "" {
		return "", false
	}
	// Normalize chatgpt/xai synonyms that LiteLLM may enrich.
	if base == "xai" {
		base = providers.ProviderXAI
	}
	if apiBase == "" {
		return base, true
	}
	host := normalizeOrigin(apiBase)
	if host == "" {
		return "", false
	}
	if std, ok := standardAPIHosts[base]; ok && (host == std || strings.HasSuffix(host, "."+std)) {
		return base, true
	}
	// ChatGPT/XAI subscription deployments carry no api_base; a present
	// api_base on those providers is custom by definition.
	return base + "@" + host, true
}

func deploymentMode(d providers.GatewayDeployment) string {
	return strings.ToLower(strings.TrimSpace(d.ModelInfo.Mode))
}

func isSelectableDeployment(d providers.GatewayDeployment) (provider, mode string, ok bool) {
	if strings.TrimSpace(d.ModelInfo.ID) == "" || strings.TrimSpace(d.ModelName) == "" {
		return "", "", false
	}
	if d.ModelInfo.DBModel != nil && !*d.ModelInfo.DBModel {
		return "", "", false
	}
	if d.ModelInfo.DBModel == nil {
		// Absent db_model is unknown capability; do not guess.
		return "", "", false
	}
	if d.ModelInfo.TeamID != nil && strings.TrimSpace(*d.ModelInfo.TeamID) != "" {
		return "", "", false
	}
	if d.ModelInfo.Blocked != nil && *d.ModelInfo.Blocked {
		return "", "", false
	}
	if strings.Contains(d.ModelName, "*") {
		return "", "", false
	}
	if params := d.LitellmParams; params != nil {
		if m, ok := params["model"].(string); ok && strings.Contains(m, "*") {
			return "", "", false
		}
	}
	// Synthetic router deployments never seed chat roles.
	lowerID := strings.ToLower(strings.TrimSpace(d.ModelInfo.ID))
	lowerName := strings.ToLower(strings.TrimSpace(d.ModelName))
	if strings.HasPrefix(lowerID, "router-") || strings.HasPrefix(lowerName, "router-") {
		return "", "", false
	}
	m := deploymentMode(d)
	if m != "chat" && m != "responses" {
		return "", "", false
	}
	params := d.LitellmParams
	if params == nil {
		return "", "", false
	}
	modelStr, _ := params["model"].(string)
	if strings.TrimSpace(modelStr) == "" {
		return "", "", false
	}
	// Embedding-only deployments are never chat sources.
	if m == "embedding" {
		return "", "", false
	}
	prov, ok := resolveProviderIdentity(d)
	if !ok || strings.TrimSpace(prov) == "" {
		return "", "", false
	}
	return prov, m, true
}

// --- management URL ---

func (b *Backend) litellmManagementURL(ctx context.Context) string {
	bundles, domainName, err := b.launchContext(ctx)
	if err != nil || b.apps == nil {
		return ""
	}
	bd, ok := bundles["litellm"]
	if !ok {
		return ""
	}
	list, err := b.apps.List(ctx)
	if err != nil {
		return ""
	}
	for _, st := range list {
		if st.BundleID != "litellm" {
			continue
		}
		if u := applicationLaunchURL(st.Application, bd, domainName); u != "" {
			return u
		}
		return ""
	}
	return ""
}

// --- setup-models endpoints ---

// GetModelSetup returns the safe native model inventory for onboarding.
func (b *Backend) GetModelSetup(ctx context.Context) (apitypes.ModelSetupStatus, error) {
	st := apitypes.ModelSetupStatus{
		Deployments: []apitypes.ModelSetupDeployment{},
		Aliases:     []apitypes.ModelSetupAlias{},
	}
	st.ManagementURL = b.litellmManagementURL(ctx)
	if b.gateway == nil {
		return apitypes.ModelSetupStatus{}, translateError(fmt.Errorf("%w: gateway not configured", ErrNotConfigured))
	}
	native, err := b.gateway.ListModels(ctx)
	if err != nil {
		return apitypes.ModelSetupStatus{}, translateError(fmt.Errorf("%w: gateway unavailable: %v", apitypes.ErrServiceUnavailable, err))
	}
	for _, d := range native {
		prov, mode, ok := isSelectableDeployment(d)
		if !ok {
			continue
		}
		modelStr, _ := d.LitellmParams["model"].(string)
		st.Deployments = append(st.Deployments, apitypes.ModelSetupDeployment{
			ID:       strings.TrimSpace(d.ModelInfo.ID),
			Name:     strings.TrimSpace(d.ModelName),
			Model:    strings.TrimSpace(modelStr),
			Provider: prov,
			Mode:     mode,
		})
	}
	byNameProviders := map[string][]string{}
	byNameSeen := map[string]map[string]bool{}
	for _, d := range native {
		name := strings.TrimSpace(d.ModelName)
		if name == "" {
			continue
		}
		prov, ok := resolveProviderIdentity(d)
		if !ok {
			prov = "unknown"
		}
		if byNameSeen[name] == nil {
			byNameSeen[name] = map[string]bool{}
		}
		if !byNameSeen[name][prov] {
			byNameSeen[name][prov] = true
			byNameProviders[name] = append(byNameProviders[name], prov)
		}
	}
	for _, name := range modelSetupAliases {
		provs := byNameProviders[name]
		if provs == nil {
			provs = []string{}
		}
		st.Aliases = append(st.Aliases, apitypes.ModelSetupAlias{
			Name:       name,
			Configured: len(byNameProviders[name]) > 0,
			Providers:  provs,
		})
	}
	return st, nil
}

// SeedModelAliases seeds missing chat-role aliases from one native deployment.
func (b *Backend) SeedModelAliases(ctx context.Context, req apitypes.SeedModelAliasesRequest) (apitypes.ModelSetupStatus, error) {
	seedMu.Lock()
	defer seedMu.Unlock()
	modelID := strings.TrimSpace(req.ModelID)
	if modelID == "" {
		return apitypes.ModelSetupStatus{}, translateError(fmt.Errorf("%w: model_id is required", apitypes.ErrValidation))
	}
	if b.gateway == nil {
		return apitypes.ModelSetupStatus{}, translateError(fmt.Errorf("%w: gateway unavailable", apitypes.ErrServiceUnavailable))
	}
	src, err := b.gateway.GetModel(ctx, modelID)
	if err != nil {
		if errors.Is(err, providers.ErrNotFound) || strings.Contains(strings.ToLower(err.Error()), "not found") {
			return apitypes.ModelSetupStatus{}, translateError(fmt.Errorf("%w: model %q not found", apitypes.ErrNotFound, modelID))
		}
		return apitypes.ModelSetupStatus{}, translateError(fmt.Errorf("%w: gateway unavailable: %v", apitypes.ErrServiceUnavailable, err))
	}
	if _, _, ok := isSelectableDeployment(src); !ok {
		return apitypes.ModelSetupStatus{}, translateError(fmt.Errorf("%w: model %q is not a selectable chat deployment", apitypes.ErrValidation, modelID))
	}
	native, err := b.gateway.ListModels(ctx)
	if err != nil {
		return apitypes.ModelSetupStatus{}, translateError(fmt.Errorf("%w: gateway unavailable: %v", apitypes.ErrServiceUnavailable, err))
	}
	presentNames := map[string]bool{}
	byID := map[string]providers.GatewayDeployment{}
	for _, d := range native {
		presentNames[strings.TrimSpace(d.ModelName)] = true
		byID[strings.TrimSpace(d.ModelInfo.ID)] = d
	}
	var missing []string
	for _, role := range seedRoles {
		if !presentNames[role] {
			missing = append(missing, role)
		}
	}
	if len(missing) == 0 {
		return b.GetModelSetup(ctx)
	}
	needsCredential := !isOAuthSource(src)
	credName := ""
	if needsCredential {
		credName = seedCredentialName(modelID)
		gwCreds, err := b.gateway.ListGatewayCredentials(ctx)
		if err != nil {
			return apitypes.ModelSetupStatus{}, translateError(fmt.Errorf("%w: gateway unavailable: %v", apitypes.ErrServiceUnavailable, err))
		}
		found := false
		for _, gc := range gwCreds {
			if strings.TrimSpace(gc.CredentialName) != credName {
				continue
			}
			found = true
			if gc.CredentialInfoString("omahab_seed_source_id") != modelID {
				return apitypes.ModelSetupStatus{}, translateError(fmt.Errorf("%w: credential %q already exists for a different source model; delete it in LiteLLM or re-seed from the original source", apitypes.ErrConflict, credName))
			}
			break
		}
		if !found {
			in := providers.GatewayCredentialInput{
				CredentialName: credName,
				CredentialInfo: map[string]string{"omahab_seed_source_id": modelID},
				ModelID:        modelID,
			}
			if err := b.gateway.CreateGatewayCredential(ctx, in); err != nil {
				return apitypes.ModelSetupStatus{}, translateError(fmt.Errorf("seed credential %q: %w", credName, err))
			}
		}
	}
	srcParams := src.LitellmParams
	if srcParams == nil {
		srcParams = map[string]any{}
	}
	for _, role := range missing {
		// Skip any name that appeared since the pre-creation inventory read.
		fresh, err := b.gateway.ListModels(ctx)
		if err != nil {
			return apitypes.ModelSetupStatus{}, translateError(fmt.Errorf("%w: gateway unavailable: %v", apitypes.ErrServiceUnavailable, err))
		}
		freshNames := map[string]bool{}
		freshIDs := map[string]providers.GatewayDeployment{}
		for _, d := range fresh {
			freshNames[strings.TrimSpace(d.ModelName)] = true
			freshIDs[strings.TrimSpace(d.ModelInfo.ID)] = d
		}
		if freshNames[role] {
			continue
		}
		roleID := seedRoleID(role)
		if d, ok := freshIDs[roleID]; ok && strings.TrimSpace(d.ModelName) != role {
			return apitypes.ModelSetupStatus{}, translateError(fmt.Errorf("%w: model id %q already in use", apitypes.ErrConflict, roleID))
		}
		if _, ok := freshIDs[roleID]; ok {
			continue // deterministic object already present
		}
		params := map[string]any{}
		if m, ok := srcParams["model"].(string); ok && strings.TrimSpace(m) != "" {
			params["model"] = strings.TrimSpace(m)
		} else {
			return apitypes.ModelSetupStatus{}, translateError(fmt.Errorf("%w: source model %q has no model", apitypes.ErrValidation, modelID))
		}
		for _, k := range []string{"custom_llm_provider", "use_xai_oauth", "use_chat_completions_api", "api_base", "api_version", "region_name"} {
			if v, ok := srcParams[k]; ok {
				params[k] = v
			}
		}
		if needsCredential {
			params["litellm_credential_name"] = credName
		}
		dbTrue := true
		dep := providers.GatewayDeployment{
			ModelName:     role,
			LitellmParams: params,
			ModelInfo: providers.GatewayModelInfo{
				ID:              roleID,
				DBModel:         &dbTrue,
				Mode:            deploymentMode(src),
				LitellmProvider: strings.TrimSpace(src.ModelInfo.LitellmProvider),
			},
		}
		// Preserve explicit provider prefix signal when info provider empty.
		if strings.TrimSpace(dep.ModelInfo.LitellmProvider) == "" {
			if prov, ok := resolveProviderIdentity(src); ok {
				if i := strings.Index(prov, "@"); i >= 0 {
					prov = prov[:i]
				}
				dep.ModelInfo.LitellmProvider = prov
			}
		}
		if err := b.gateway.CreateModel(ctx, dep); err != nil {
			return apitypes.ModelSetupStatus{}, translateError(fmt.Errorf("seed alias %q: %w", role, err))
		}
	}
	return b.GetModelSetup(ctx)
}

func isOAuthSource(d providers.GatewayDeployment) bool {
	if d.LitellmParams != nil {
		if v, ok := d.LitellmParams["use_xai_oauth"].(bool); ok && v {
			return true
		}
	}
	modelStr := ""
	if d.LitellmParams != nil {
		modelStr, _ = d.LitellmParams["model"].(string)
	}
	lower := strings.ToLower(strings.TrimSpace(modelStr))
	prov := strings.ToLower(strings.TrimSpace(d.ModelInfo.LitellmProvider))
	if cp, ok := d.LitellmParams["custom_llm_provider"].(string); ok && strings.TrimSpace(cp) != "" {
		prov = strings.ToLower(strings.TrimSpace(cp))
	}
	if strings.HasPrefix(lower, "chatgpt/") || prov == providers.ProviderChatGPT {
		// ChatGPT subscription uses token files, no API key.
		if _, hasKey := d.LitellmParams["api_key"]; !hasKey {
			if _, hasCred := d.LitellmParams["litellm_credential_name"]; !hasCred {
				return true
			}
		}
	}
	if strings.HasPrefix(lower, "xai/") || prov == providers.ProviderXAI {
		if _, hasKey := d.LitellmParams["api_key"]; !hasKey {
			if _, hasCred := d.LitellmParams["litellm_credential_name"]; !hasCred {
				return true
			}
		}
	}
	return false
}

// nativeAliasAvailable reports whether a native omahab/* model_name deployment
// exists. It backs Karakeep gating.
func (b *Backend) nativeAliasAvailable(ctx context.Context, alias string) bool {
	if b.gateway == nil {
		return false
	}
	models, err := b.gateway.ListModels(ctx)
	if err != nil {
		return false
	}
	for _, d := range models {
		if strings.TrimSpace(d.ModelName) == strings.TrimSpace(alias) {
			return true
		}
	}
	return false
}

// nativeProbeModel chooses a live native deployment for OAuth probing,
// preferring omahab/fast. Empty means no native model is configured.
func (b *Backend) nativeProbeModel(ctx context.Context, provider string) string {
	if b.gateway == nil {
		return ""
	}
	models, err := b.gateway.ListModels(ctx)
	if err != nil || len(models) == 0 {
		return ""
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	fallback := ""
	for _, d := range models {
		ident, ok := resolveProviderIdentity(d)
		if !ok {
			continue
		}
		base := ident
		if i := strings.Index(base, "@"); i >= 0 {
			base = base[:i]
		}
		if !strings.EqualFold(base, provider) {
			continue
		}
		name := strings.TrimSpace(d.ModelName)
		if name == "" {
			continue
		}
		if name == providers.AliasFast {
			return name
		}
		if fallback == "" {
			fallback = name
		}
	}
	return fallback
}
