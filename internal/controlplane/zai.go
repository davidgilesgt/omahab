package controlplane

import (
	"context"
	"fmt"
	"strings"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/providers"
	"github.com/omahab/omahab/internal/store"
)

// Z.ai connection: explicit plan selection with an authorized Coding Plan
// option. PAYG uses the payg endpoint; Coding Plan uses only the coding
// endpoint after an explicit authorization acknowledgement (recorded with
// timestamp, but the acknowledgement is not proof Z.ai granted
// authorization). Unauthorized Coding Plan saves fail; entitlement failure
// stays visible with no silent fallback to paid API usage. The connection
// scopes to explicitly selected model deployments, never silently replacing
// household aliases.

// ConnectZAI creates a scoped Z.ai connection: secrets-backed API-key
// credential, one native deployment rendering openai/<glm-model> with the
// plan's explicit api_base, and a real probe validation of the selected
// model. No fabricated model inventory or default entitlement is claimed.
func (b *Backend) ConnectZAI(ctx context.Context, req apitypes.ConnectZAIRequest) (apitypes.ZAIConnection, error) {
	if b.providers == nil || b.gateway == nil || b.secrets == nil {
		return apitypes.ZAIConnection{}, translateError(fmt.Errorf("%w: providers not configured", ErrNotConfigured))
	}
	plan := strings.ToLower(strings.TrimSpace(req.Plan))
	if plan == "" {
		plan = providers.ZAIPlanPAYG
	}
	if plan != providers.ZAIPlanPAYG && plan != providers.ZAIPlanCoding {
		return apitypes.ZAIConnection{}, translateError(fmt.Errorf("%w: plan must be payg|coding", store.ErrValidation))
	}
	if plan == providers.ZAIPlanCoding && !req.AuthorizedCodingUse {
		return apitypes.ZAIConnection{}, translateError(fmt.Errorf("%w: Coding Plan use requires explicit authorization acknowledgement (see %s)", store.ErrValidation, providers.ZAITermsURL))
	}
	key := strings.TrimSpace(req.APIKey)
	if key == "" {
		return apitypes.ZAIConnection{}, translateError(fmt.Errorf("%w: api_key is required (create one at %s)", store.ErrValidation, providers.ZAIKeyURL))
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "glm-4.6"
	}
	nativeModel, _, _, err := providers.NormalizeNativeModel(providers.ProviderZAI, providers.CredentialTypeAPIKey, providers.ManagedByOmahab, "", model)
	if err != nil {
		return apitypes.ZAIConnection{}, translateError(err)
	}
	apiBase := providers.ZAIPAYGBase
	if plan == providers.ZAIPlanCoding {
		apiBase = providers.ZAICodingBase
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		displayName = "z.ai " + plan
	}
	// Secrets-backed credential first (key never persists outside the broker).
	secretName := "zai-" + plan + "-" + strings.ToLower(store.NewID())[:8]
	sec, err := b.secrets.Put(ctx, "platform-app", secretName, key)
	key = ""
	if err != nil {
		return apitypes.ZAIConnection{}, translateError(err)
	}
	cred, err := b.providers.CreateCredential(ctx, providers.CreateCredentialInput{
		Provider:            providers.ProviderZAI,
		CredentialType:      providers.CredentialTypeAPIKey,
		DisplayName:         displayName,
		SecretID:            sec.ID,
		ManagedBy:           providers.ManagedByOmahab,
		Plan:                plan,
		AuthorizedCodingUse: plan == providers.ZAIPlanCoding && req.AuthorizedCodingUse,
	})
	if err != nil {
		return apitypes.ZAIConnection{}, translateError(err)
	}
	// Native LiteLLM credential + one explicitly selected deployment.
	credName := "omahab-zai-" + plan + "-" + strings.ToLower(string(cred.ID))[:8]
	secretValue, err := b.secrets.Reveal(ctx, sec.ID)
	if err != nil {
		return apitypes.ZAIConnection{}, translateError(err)
	}
	if err := b.gateway.CreateGatewayCredential(ctx, providers.GatewayCredentialInput{
		CredentialName:   credName,
		CredentialValues: map[string]string{"api_key": secretValue},
	}); err != nil {
		return apitypes.ZAIConnection{}, translateError(err)
	}
	secretValue = ""
	deploymentName := strings.TrimSpace(req.Deployment)
	if deploymentName == "" {
		deploymentName = "omahab/zai-" + plan
	}
	if err := b.gateway.CreateModel(ctx, providers.GatewayDeployment{
		ModelName: deploymentName,
		LitellmParams: map[string]any{
			"model":                  nativeModel,
			"api_base":               apiBase,
			"litellm_credential_name": credName,
		},
	}); err != nil {
		return apitypes.ZAIConnection{}, translateError(err)
	}
	// Validate the selected model through the probe machinery. The probe key
	// is unscoped and short-lived (revoked below): z.ai deployments are not
	// routing aliases, so no alias scope applies.
	ownerKind, ownerID := "harness", "probe-"+string(store.NewID())
	probeReq := providers.IssueVirtualKeyInput{Name: "probe-zai"}
	probeReq.OwnerKind = &ownerKind
	probeReq.OwnerID = &ownerID
	vk, err := b.providers.IssueVirtualKey(ctx, probeReq)
	if err != nil {
		return apitypes.ZAIConnection{}, translateError(err)
	}
	defer func() { _ = b.providers.RevokeVirtualKey(ctx, vk.VirtualKey.ID) }()
	if err := b.gateway.ProbeModel(ctx, deploymentName, vk.Token); err != nil {
		_ = b.providers.UpdateHealth(ctx, cred.ID, domain.HealthDegraded, providers.EntitlementUnknown, fmt.Sprintf("z.ai probe failed: %v", err))
		return apitypes.ZAIConnection{}, translateError(err)
	}
	_ = b.providers.UpdateHealth(ctx, cred.ID, domain.HealthHealthy, providers.EntitlementEntitled, "")
	return apitypes.ZAIConnection{
		CredentialID: cred.ID,
		Provider:     providers.ProviderZAI,
		Plan:         plan,
		APIBase:      apiBase,
		Model:        nativeModel,
		Deployment:   deploymentName,
	}, nil
}

// DisconnectProvider revokes/deletes local saved auth via a provider-owned
// operation. Unrelated gateway credentials and models are untouched.
func (b *Backend) DisconnectProvider(ctx context.Context, provider string) error {
	switch provider {
	case providers.ProviderChatGPT, providers.ProviderXAI:
		if err := b.gateway.Disconnect(ctx, provider); err != nil {
			return translateError(err)
		}
		return b.markSubscriptionCredential(ctx, provider, domain.HealthUnknown, providers.EntitlementUnknown, "disconnected by operator")
	case providers.ProviderZAI:
		return b.disconnectZAI(ctx)
	default:
		return translateError(fmt.Errorf("%w: disconnect supports chatgpt|xai|zai", store.ErrValidation))
	}
}

// disconnectZAI removes the connection's own credential and secret. Aliases
// still referencing it block deletion instead of silently breaking routing;
// unrelated credentials, models and deployments are untouched.
func (b *Backend) disconnectZAI(ctx context.Context) error {
	if b.providers == nil {
		return nil
	}
	creds, err := b.providers.ListCredentialsByProvider(ctx, providers.ProviderZAI)
	if err != nil {
		return translateError(err)
	}
	for _, c := range creds {
		if c == nil {
			continue
		}
		secretID := strings.TrimSpace(string(c.SecretID))
		if err := b.providers.DeleteCredential(ctx, c.ID); err != nil {
			return translateError(err)
		}
		if secretID != "" && b.secrets != nil {
			if meta, err := b.secrets.Get(ctx, c.SecretID); err == nil {
				_ = b.secrets.DeleteByName(ctx, meta.Scope, meta.Name)
			}
		}
	}
	return nil
}



func (b *Backend) markSubscriptionCredential(ctx context.Context, provider string, health domain.Health, entitlement, message string) error {
	if b.providers == nil {
		return nil
	}
	creds, err := b.providers.ListCredentials(ctx)
	if err != nil {
		return err
	}
	for _, c := range creds {
		if c == nil || !strings.EqualFold(c.Provider, provider) || c.ManagedBy != providers.ManagedByLiteLLM {
			continue
		}
		_ = b.providers.UpdateHealth(ctx, c.ID, health, entitlement, message)
	}
	return nil
}
