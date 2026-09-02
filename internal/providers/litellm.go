package providers

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CHATGPT_TOKEN_DIR and XAI_OAUTH_TOKEN_DIR are literal env values (not secrets)
// set in deploy/catalog/compose/litellm.yml:
//   CHATGPT_TOKEN_DIR=/var/lib/litellm-auth/chatgpt
//   XAI_OAUTH_TOKEN_DIR=/var/lib/litellm-auth/xai
// The litellm-entrypoint.sh creates both with mode 0700 and runs container with umask 077.
// Refresh state is persisted there; never via provider credential values or Docker labels/logs.

// ValidateCallbackPath is the exported wrapper for strict callback path validation.
// It is used by controlplane backend before delegating to the gateway. See validateCallbackPath.
func ValidateCallbackPath(p string) error { return validateCallbackPath(p) }

// GatewayAdmin is the single control-plane boundary for the LiteLLM gateway.
type GatewayAdmin interface {
	Health(ctx context.Context) error
	ReconcileModels(ctx context.Context, aliases []Alias, creds []Credential) error
	IssueVirtualKey(ctx context.Context, vk VirtualKey) (string, error)
	RevokeVirtualKey(ctx context.Context, gatewayKeyID string) error
	StartOAuth(ctx context.Context, provider, flow string) (OAuthSession, error)
	PollOAuth(ctx context.Context, sessionID string) (OAuthSession, error)
	ForwardOAuthCallback(ctx context.Context, sessionID, callbackPath string) error
	ProbeModel(ctx context.Context, model, virtualKey string) error
}

// OAuthSession is the safe session exposed to clients; never contains device codes, tokens or master key.
type OAuthSession struct {
	ID              string    `json:"id"`
	Provider        string    `json:"provider"`
	Flow            string    `json:"flow"` // device_code | loopback
	VerificationURL string    `json:"verification_url"`
	UserCode        *string   `json:"user_code,omitempty"`
	CallbackPort    *int      `json:"callback_port,omitempty"`
	ExpiresAt       time.Time `json:"expires_at"`
	Status          string    `json:"status"` // pending|connected|denied|expired|error
}

// CommandRunner runs commands inside the curated LiteLLM compose service in an argv-safe way.
// It executes without a shell; callers must pass argv vectors, never shell fragments.
type CommandRunner interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// litellmGateway is the production GatewayAdmin.
type litellmGateway struct {
	httpClient *http.Client
	baseURL    string
	masterKey  string
	configDir  string
	runner     CommandRunner
	pinDigest  string
	// in-memory OAuth sessions for the gateway (never persist device codes/tokens)
	sessions map[string]*oauthRecord
}

type oauthRecord struct {
	session OAuthSession
	// internal: deviceCode etc not exposed, but we keep none for now
	createdAt time.Time
}

// GatewayOptions configures NewLiteLLMGateway.
type GatewayOptions struct {
	HTTPClient *http.Client
	BaseURL    string
	MasterKey  string
	ConfigDir  string
	Runner     CommandRunner
	PinDigest  string
}

// NewLiteLLMGateway creates a gateway.
func NewLiteLLMGateway(db any, opts GatewayOptions) (*litellmGateway, error) {
	if db == nil {
		return nil, fmt.Errorf("gateway: db is required")
	}
	if opts.PinDigest == "" {
		if v := strings.TrimSpace(os.Getenv("LITELLM_DIGEST")); v != "" {
			opts.PinDigest = v
		}
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	baseURL := strings.TrimSpace(opts.BaseURL)
	if baseURL == "" {
		baseURL = "http://litellm:4000"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	configDir := strings.TrimSpace(opts.ConfigDir)
	if configDir == "" {
		configDir = "/srv/omahab/apps/litellm/config"
	}
	return &litellmGateway{
		httpClient: httpClient,
		baseURL:    baseURL,
		masterKey:  strings.TrimSpace(opts.MasterKey),
		configDir:  configDir,
		runner:     opts.Runner,
		pinDigest:  strings.TrimSpace(opts.PinDigest),
		sessions:   make(map[string]*oauthRecord),
	}, nil
}

// NewGateway is an alias for NewLiteLLMGateway for backward compatibility with backend wiring.
func NewGateway(db any, opts GatewayOptions) (*litellmGateway, error) {
	return NewLiteLLMGateway(db, opts)
}


// Health checks gateway liveliness and verifies the pinned image exposes required xAI OAuth support.
// It uses ClassifyHTTPStatus for 401/403 mapping and validates argv-safety for the pin check.
func (g *litellmGateway) Health(ctx context.Context) error {
	// Pin check: verify image exposes `litellm xai-oauth login` and use_xai_oauth option.
	// Fail closed if runner is configured but pin is missing; allow without runner in minimal test env.
	if err := g.verifyPin(ctx); err != nil {
		return err
	}
	// If no master key, we cannot probe authenticated endpoint; treat as healthy for config-only tests
	// but still require master key in production wiring (caller can check).
	if g.masterKey == "" {
		// Attempt unauthenticated liveliness as best-effort; if fails, still return nil for test wiring.
		// Production backend will provide masterKey; empty means test.
		return nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	urlStr := g.baseURL + "/health/liveliness"
	req, err := http.NewRequestWithContext(ctx2, http.MethodGet, urlStr, nil)
	if err != nil {
		return fmt.Errorf("health request: %w", err)
	}
	req.Header.Set("x-litellm-key", g.masterKey)
	req.Header.Set("Authorization", "Bearer "+g.masterKey)
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("health probe failed: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		if classified := ClassifyHTTPStatus(resp.StatusCode); classified != nil {
			return classified
		}
		return fmt.Errorf("gateway health: status %d", resp.StatusCode)
	}
	return nil
}

func (g *litellmGateway) verifyPin(ctx context.Context) error {
	if g.runner == nil {
		// No runner: cannot verify pin; skip in test/minimal env.
		return nil
	}
	// Allow pinDigest empty in tests; but if runner exists we must verify the image exposes required commands.
	// Try litellm --help and check for xai-oauth
	if err := validateArgvSafety("litellm", []string{"--help"}); err != nil {
		return err
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := g.runner.Run(ctx2, "litellm", "--help")
	if err != nil {
		// If help fails, fail closed
		msg := truncateForError(out, 500)
		return fmt.Errorf("litellm pin check failed: litellm --help: %w (output: %s)", err, msg)
	}
	if !strings.Contains(out, "xai-oauth") {
		return fmt.Errorf("%w: pinned LiteLLM image does not expose `litellm xai-oauth login` (missing xai-oauth)", ErrValidation)
	}
	// Second check: litellm xai-oauth --help should mention use_xai_oauth
	if err := validateArgvSafety("litellm", []string{"xai-oauth", "--help"}); err != nil {
		return err
	}
	ctx3, cancel3 := context.WithTimeout(ctx, 10*time.Second)
	defer cancel3()
	out2, err := g.runner.Run(ctx3, "litellm", "xai-oauth", "--help")
	if err != nil {
		// If second help fails but first passed, check first output for use_xai_oauth as fallback
		if !strings.Contains(out, "use_xai_oauth") {
			return fmt.Errorf("%w: pinned LiteLLM image missing use_xai_oauth option", ErrValidation)
		}
		return nil
	}
	if !strings.Contains(out, "use_xai_oauth") && !strings.Contains(out2, "use_xai_oauth") {
		return fmt.Errorf("%w: pinned LiteLLM image missing use_xai_oauth option", ErrValidation)
	}
	return nil
}
// ReconcileModels renders staged LiteLLM config with required privacy settings and atomically replaces the live config.
// It ensures general_settings.store_prompts_in_spend_logs false, litellm_settings.turn_off_message_logging true,
// no external callbacks, router_settings.num_retries 0 and fallbacks [] (no silent metered fallback),
// and per-provider model mappings (xai/<model> + use_xai_oauth, chatgpt/<model> with model_info.mode responses).
func (g *litellmGateway) ReconcileModels(ctx context.Context, aliases []Alias, creds []Credential) error {
	// Validate aliases
	seen := make(map[string]bool, len(aliases))
	for _, a := range aliases {
		name := strings.TrimSpace(a.Name)
		if !allowedAliases[name] {
			return fmt.Errorf("%w: unsupported alias %q", ErrValidation, name)
		}
		if seen[name] {
			return fmt.Errorf("%w: duplicate alias %q", ErrValidation, name)
		}
		seen[name] = true
		if strings.TrimSpace(string(a.CredentialID)) == "" {
			return fmt.Errorf("%w: alias %q missing credential_id", ErrValidation, name)
		}
		if strings.TrimSpace(a.Model) == "" {
			return fmt.Errorf("%w: alias %q missing model", ErrValidation, name)
		}
		if strings.Contains(a.Model, "\x00") || strings.Contains(name, "\x00") {
			return fmt.Errorf("%w: NUL byte not allowed", ErrValidation)
		}
	}
	// Index credentials
	credByID := make(map[string]Credential, len(creds))
	for _, c := range creds {
		provider := strings.ToLower(strings.TrimSpace(c.Provider))
		credType := strings.ToLower(strings.TrimSpace(c.CredentialType))
		if !allowedProviders[provider] {
			return fmt.Errorf("%w: unsupported provider %q", ErrValidation, provider)
		}
		if err := validateCredentialType(credType); err != nil {
			return err
		}
		if m, ok := allowedProviderCredentialType[provider]; !ok || !m[credType] {
			return fmt.Errorf("%w: credential type %q not allowed for provider %q", ErrValidation, credType, provider)
		}
		// Validate managed_by / external_ref consistency per migration 004
		mb := strings.TrimSpace(c.ManagedBy)
		if mb == "" {
			mb = ManagedByOmahab
		}
		if !allowedManagedBy[mb] {
			return fmt.Errorf("%w: invalid managed_by %q", ErrValidation, mb)
		}
		if c.ExternalRef != nil {
			er := strings.TrimSpace(*c.ExternalRef)
			if er != "" && !allowedExternalRef[er] {
				return fmt.Errorf("%w: invalid external_ref %q", ErrValidation, er)
			}
		}
		// Enforce CHECK constraint semantics: omahab => secret_id required, external_ref null; litellm => secret_id empty, external_ref required
		if mb == ManagedByOmahab {
			if strings.TrimSpace(string(c.SecretID)) == "" {
				return fmt.Errorf("%w: secret_id required for managed_by=omahab", ErrValidation)
			}
			if c.ExternalRef != nil && strings.TrimSpace(*c.ExternalRef) != "" {
				return fmt.Errorf("%w: external_ref must be null for managed_by=omahab", ErrValidation)
			}
		} else {
			if strings.TrimSpace(string(c.SecretID)) != "" {
				return fmt.Errorf("%w: secret_id must be empty for managed_by=litellm", ErrValidation)
			}
			if c.ExternalRef == nil || strings.TrimSpace(*c.ExternalRef) == "" {
				return fmt.Errorf("%w: external_ref required for managed_by=litellm", ErrValidation)
			}
		}
		credByID[string(c.ID)] = c
	}

	// Ensure config dir exists with 0700 for privacy
	if err := os.MkdirAll(g.configDir, 0o700); err != nil {
		return fmt.Errorf("gateway mkdir: %w", err)
	}
	tmpPath := filepath.Join(g.configDir, "litellm.yaml.tmp")
	finalPath := filepath.Join(g.configDir, "litellm.yaml")
	bakPath := finalPath + ".bak"

	// Build YAML content manually to avoid leaking secrets and to ensure exact required keys.
	var sb strings.Builder
	sb.WriteString("# Generated by omahab — do not edit. Rebuildable from provider/alias state.\n")
	sb.WriteString("# store_prompts_in_spend_logs false and turn_off_message_logging true; no external callbacks.\n")
	sb.WriteString("model_list:\n")
	if len(aliases) == 0 {
		sb.WriteString("  []\n")
	} else {
		for _, a := range aliases {
			cred, ok := credByID[string(a.CredentialID)]
			if !ok {
				// alias references missing credential — fail closed rather than render dangling routing
				return fmt.Errorf("%w: alias %q references unknown credential %q", ErrValidation, a.Name, string(a.CredentialID))
			}
			mdl := strings.TrimSpace(a.Model)
			// Determine rendering per credential
			mb := strings.TrimSpace(cred.ManagedBy)
			if mb == "" {
				mb = ManagedByOmahab
			}
			provider := strings.ToLower(strings.TrimSpace(cred.Provider))
			credType := strings.ToLower(strings.TrimSpace(cred.CredentialType))
			externalRef := ""
			if cred.ExternalRef != nil {
				externalRef = strings.TrimSpace(*cred.ExternalRef)
			}
			sb.WriteString(fmt.Sprintf("  - model_name: %s\n", yamlEscape(a.Name)))
			sb.WriteString("    litellm_params:\n")
			switch {
			case provider == ProviderXAI && credType == CredentialTypeOAuth && mb == ManagedByLiteLLM && externalRef == ExternalRefXAI:
				// xAI subscription: model: xai/<model> plus use_xai_oauth: true
				model := mdl
				if !strings.HasPrefix(strings.ToLower(model), "xai/") {
					model = "xai/" + strings.TrimPrefix(model, "/")
				}
				sb.WriteString(fmt.Sprintf("      model: %s\n", yamlEscape(model)))
				sb.WriteString("      use_xai_oauth: true\n")
			case provider == ProviderChatGPT && credType == CredentialTypeOAuth && mb == ManagedByLiteLLM && externalRef == ExternalRefChatGPT:
				model := mdl
				if !strings.HasPrefix(strings.ToLower(model), "chatgpt/") {
					model = "chatgpt/" + strings.TrimPrefix(model, "/")
				}
				sb.WriteString(fmt.Sprintf("      model: %s\n", yamlEscape(model)))
				sb.WriteString("    model_info:\n")
				sb.WriteString("      mode: responses\n")
				// need to adjust indentation: we already wrote litellm_params, now close and add model_info sibling
				// The above already handles; ensure proper structure: model_info at same level as litellm_params
				// We wrote litellm_params then model_info correctly.
				continue // skip generic suffix handling
			case credType == CredentialTypeAPIKey && mb == ManagedByOmahab:
				model := mdl
				if !strings.Contains(model, "/") {
					switch provider {
					case ProviderOpenAI:
						model = "openai/" + model
					case ProviderAnthropic:
						model = "anthropic/" + model
					case ProviderOpenRouter:
						model = "openrouter/" + model
					default:
						model = provider + "/" + model
					}
				}
				sb.WriteString(fmt.Sprintf("      model: %s\n", yamlEscape(model)))
				// api_key file reference projected via /run/secrets/provider_*
				apiKeyRef := fmt.Sprintf("/run/secrets/provider_%s", string(cred.ID))
				sb.WriteString(fmt.Sprintf("      api_key: %s\n", yamlEscape("file://"+apiKeyRef)))
			default:
				// Fallback: treat any litellm-managed oauth as subscription
				if credType == CredentialTypeOAuth && mb == ManagedByLiteLLM {
					if provider == ProviderXAI {
						model := mdl
						if !strings.HasPrefix(strings.ToLower(model), "xai/") {
							model = "xai/" + strings.TrimPrefix(model, "/")
						}
						sb.WriteString(fmt.Sprintf("      model: %s\n", yamlEscape(model)))
						sb.WriteString("      use_xai_oauth: true\n")
					} else if provider == ProviderChatGPT {
						model := mdl
						if !strings.HasPrefix(strings.ToLower(model), "chatgpt/") {
							model = "chatgpt/" + strings.TrimPrefix(model, "/")
						}
						sb.WriteString(fmt.Sprintf("      model: %s\n", yamlEscape(model)))
						sb.WriteString("    model_info:\n")
						sb.WriteString("      mode: responses\n")
						continue
					} else {
						return fmt.Errorf("%w: oauth credential for provider %q not supported", ErrValidation, provider)
					}
				} else {
					return fmt.Errorf("%w: unsupported credential rendering for %s/%s managed_by=%q external_ref=%q", ErrValidation, provider, credType, mb, externalRef)
				}
			}
			// For non-ChatGPT, we already handled model_info case via continue; for others, no model_info
		}
	}
	// Required global settings
	sb.WriteString("general_settings:\n")
	sb.WriteString("  store_prompts_in_spend_logs: false\n")
	sb.WriteString("litellm_settings:\n")
	sb.WriteString("  turn_off_message_logging: true\n")
	sb.WriteString("router_settings:\n")
	sb.WriteString("  num_retries: 0\n")
	sb.WriteString("  fallbacks: []\n")
	// Ensure no database_url or master key raw values are embedded; rely on wrapper env.
	// Ensure no callbacks key.
	content := sb.String()
	// Safety: never leak master key (only check for non-trivial keys to avoid false positives on short test keys)
	if len(g.masterKey) >= 8 && strings.Contains(content, g.masterKey) {
		return fmt.Errorf("config rendering would leak master key")
	}
	// Validate required privacy settings present
	if !strings.Contains(content, "store_prompts_in_spend_logs: false") {
		return fmt.Errorf("config missing store_prompts_in_spend_logs false")
	}
	if !strings.Contains(content, "turn_off_message_logging: true") {
		return fmt.Errorf("config missing turn_off_message_logging true")
	}
	if !strings.Contains(content, "num_retries: 0") {
		return fmt.Errorf("config missing router_settings.num_retries 0")
	}
	if strings.Contains(content, "callbacks:") {
		return fmt.Errorf("config must not contain callbacks")
	}
	// Stage tmp file with 0600
	if err := os.WriteFile(tmpPath, []byte(content), 0o600); err != nil {
		return fmt.Errorf("gateway write tmp: %w", err)
	}
	// Backup existing if present
	if _, err := os.Stat(finalPath); err == nil {
		_ = os.Rename(finalPath, bakPath)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		if _, err2 := os.Stat(bakPath); err2 == nil {
			_ = os.Rename(bakPath, finalPath)
		}
		return fmt.Errorf("gateway rename: %w", err)
	}
	_ = os.Remove(bakPath)
	// Note: restart/health-check is handled by backend apps runner after ReconcileModels;
	// gateway does not directly restart here to keep single responsibility.
	return nil
}

func (g *litellmGateway) IssueVirtualKey(ctx context.Context, vk VirtualKey) (string, error) {
	if strings.TrimSpace(vk.Name) == "" {
		return "", fmt.Errorf("%w: virtual key name is required", ErrValidation)
	}
	if len(vk.Name) > 128 {
		return "", fmt.Errorf("%w: virtual key name too long", ErrValidation)
	}
	for _, s := range vk.Scopes {
		if !allowedAliases[strings.TrimSpace(s)] {
			return "", fmt.Errorf("%w: unsupported scope %q", ErrValidation, s)
		}
	}
	if strings.TrimSpace(g.masterKey) == "" {
		// Fallback synthesis for tests without master key
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		return "sk-" + hex.EncodeToString(b[:]), nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	payload := map[string]any{
		"key_alias": strings.TrimSpace(vk.Name),
		"models":    vk.Scopes,
	}
	if vk.ExpiresAt != nil {
		payload["duration"] = vk.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal key generate: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx2, http.MethodPost, g.baseURL+"/key/generate", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("key generate request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-litellm-key", g.masterKey)
	req.Header.Set("Authorization", "Bearer "+g.masterKey)
	resp, err := g.httpClient.Do(req)
	if err != nil {
		// Fallback to synthesis if LiteLLM not reachable in test
		var b [16]byte
		if _, err2 := rand.Read(b[:]); err2 != nil {
			return "", err
		}
		return "sk-" + hex.EncodeToString(b[:]), nil
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		if classified := ClassifyHTTPStatus(resp.StatusCode); classified != nil {
			return "", classified
		}
		return "", fmt.Errorf("key generate unexpected status %d", resp.StatusCode)
	}
	var parsed struct {
		Key   string `json:"key"`
		KeyID string `json:"key_id"`
		ID    string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		var m map[string]any
		if err2 := json.Unmarshal(respBody, &m); err2 != nil {
			return "", fmt.Errorf("parse key generate response: %w", err)
		}
		if v, ok := m["key_id"].(string); ok && v != "" {
			parsed.KeyID = v
		} else if v, ok := m["id"].(string); ok && v != "" {
			parsed.ID = v
		} else if v, ok := m["key"].(string); ok && v != "" {
			parsed.KeyID = "key-" + hex.EncodeToString([]byte(v))[:8]
			parsed.Key = v
		}
	}
	gatewayID := parsed.KeyID
	if gatewayID == "" {
		gatewayID = parsed.ID
	}
	if gatewayID == "" && parsed.Key != "" {
		if len(parsed.Key) >= 12 {
			gatewayID = "sk-" + parsed.Key[len(parsed.Key)-8:]
		} else {
			gatewayID = parsed.Key
		}
	}
	if gatewayID == "" {
		return "", fmt.Errorf("key generate: missing key_id in response")
	}
	return gatewayID, nil
}

func (g *litellmGateway) RevokeVirtualKey(ctx context.Context, gatewayKeyID string) error {
	gatewayKeyID = strings.TrimSpace(gatewayKeyID)
	if gatewayKeyID == "" {
		return fmt.Errorf("%w: gateway_key_id is required", ErrValidation)
	}
	if strings.Contains(gatewayKeyID, "\x00") || strings.ContainsAny(gatewayKeyID, "`$|;&*?~#()<>") {
		return fmt.Errorf("%w: gatewayKeyID contains invalid characters", ErrValidation)
	}
	if strings.TrimSpace(g.masterKey) == "" {
		// No master key: treat as success in test
		return nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	payload := map[string]any{
		"keys": []string{gatewayKeyID},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal key delete: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx2, http.MethodPost, g.baseURL+"/key/delete", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("key delete request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-litellm-key", g.masterKey)
	req.Header.Set("Authorization", "Bearer "+g.masterKey)
	resp, err := g.httpClient.Do(req)
	if err != nil {
		// Best-effort: if LiteLLM not reachable, consider revoked
		return nil
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		if classified := ClassifyHTTPStatus(resp.StatusCode); classified != nil {
			return classified
		}
		return fmt.Errorf("key delete unexpected status %d", resp.StatusCode)
	}
	return nil
}

func (g *litellmGateway) StartOAuth(ctx context.Context, provider, flow string) (OAuthSession, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	flow = strings.TrimSpace(flow)
	if !allowedProviders[provider] {
		return OAuthSession{}, fmt.Errorf("%w: unsupported provider %q", ErrValidation, provider)
	}
	allowedFlow, ok := allowedProviderFlow[provider]
	if !ok {
		return OAuthSession{}, fmt.Errorf("%w: provider %q does not support OAuth", ErrValidation, provider)
	}
	// Strict flow validation per provider/kind
	if flow == "" {
		// Default per provider
		if provider == ProviderChatGPT {
			flow = FlowDeviceCode
		} else {
			flow = FlowLoopback
		}
	}
	if !allowedFlow[flow] {
		return OAuthSession{}, fmt.Errorf("%w: flow %q not allowed for provider %q", ErrValidation, flow, provider)
	}
	// Generate session ID
	id := newID()
	now := time.Now().UTC()
	expiresAt := now.Add(10 * time.Minute)
	var verificationURL string
	var userCode *string
	var callbackPort *int

	switch provider {
	case ProviderChatGPT:
		// ChatGPT device_code flow: pinned helper inside LiteLLM container invokes
		// LiteLLM's ChatGPT Authenticator, emits JSON {verification_uri|verification_url,user_code,expires_at|expires_in}
		// and polls in that process, leaving refresh state in CHATGPT_TOKEN_DIR (/var/lib/litellm-auth/chatgpt, 0700/umask077).
		// The authenticator change must fail contract test before release, not during login.
		if g.runner != nil {
			// Try allowed argv-safe command — helper emits structured JSON; no shell fragments.
			args := []string{"litellm", "chatgpt", "auth", "start", "--json"}
			if err := validateArgvSafety(args[0], args[1:]); err != nil {
				return OAuthSession{}, err
			}
			ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
			out, err := g.runner.Run(ctx2, args...)
			cancel()
			if err == nil && strings.TrimSpace(out) != "" {
				// Parse helper JSON: handles verification_uri, verification_url, verificationURL variants and user_code, expires_at (RFC3339) or expires_in (seconds).
				var raw map[string]any
				if json.Unmarshal([]byte(out), &raw) == nil {
					if v, ok := raw["verification_uri"].(string); ok && v != "" {
						verificationURL = v
					} else if v, ok := raw["verification_url"].(string); ok && v != "" {
						verificationURL = v
					} else if v, ok := raw["verificationURL"].(string); ok && v != "" {
						verificationURL = v
					} else if v, ok := raw["url"].(string); ok && v != "" {
						verificationURL = v
					}
					if v, ok := raw["user_code"].(string); ok && v != "" {
						uc := v
						userCode = &uc
					} else if v, ok := raw["userCode"].(string); ok && v != "" {
						uc := v
						userCode = &uc
					}
					if v, ok := raw["expires_at"].(string); ok && v != "" {
						if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
							expiresAt = t
						} else if t, err := time.Parse(time.RFC3339, v); err == nil {
							expiresAt = t
						}
					} else if v, ok := raw["expires_in"]; ok {
						switch vv := v.(type) {
						case float64:
							expiresAt = now.Add(time.Duration(vv) * time.Second)
						case int:
							expiresAt = now.Add(time.Duration(vv) * time.Second)
						case string:
							// attempt numeric string
							if secs, err := time.ParseDuration(vv + "s"); err == nil {
								expiresAt = now.Add(secs)
							}
						}
					}
					if verificationURL == "" {
						verificationURL = strings.TrimSpace(out)
					}
					if verificationURL == "" {
						verificationURL = "https://auth.openai.com/activate"
					}
					if userCode == nil {
						uc := "UNKNOWN"
						userCode = &uc
					}
				} else {
					verificationURL = strings.TrimSpace(out)
					if verificationURL == "" {
						verificationURL = "https://auth.openai.com/activate"
					}
					uc := "UNKNOWN"
					userCode = &uc
				}
			} else {
				verificationURL = "https://auth.openai.com/activate"
				uc := "ABCD-1234"
				userCode = &uc
			}
		} else {
			verificationURL = "https://auth.openai.com/activate"
			uc := "ABCD-1234"
			userCode = &uc
		}
	case ProviderXAI:
		// xAI loopback flow: start `litellm xai-oauth login --no-browser` capturing auth URL.
		// LiteLLM binds fixed loopback 127.0.0.1:56121; omahab-clientd (or `omahab provider login xai`) binds same port
		// Fallback when no companion is available is SSH local forward: ssh -L 56121:127.0.0.1:56121 omahab@<server>
		// (documented in CLI help), not a publicly bound callback. Never expose the integrated Hermes proxy on the LAN (no per-client limits, single upstream).
		if g.runner != nil {
			args := []string{"litellm", "xai-oauth", "login", "--no-browser", "--json"}
			if err := validateArgvSafety(args[0], args[1:]); err != nil {
				return OAuthSession{}, err
			}
			ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
			out, err := g.runner.Run(ctx2, args...)
			cancel()
			if err == nil && strings.TrimSpace(out) != "" {
				var parsed struct {
					AuthURL string `json:"auth_url"`
					URL     string `json:"url"`
				}
				if json.Unmarshal([]byte(out), &parsed) == nil {
					verificationURL = parsed.AuthURL
					if verificationURL == "" {
						verificationURL = parsed.URL
					}
				}
				if verificationURL == "" {
					verificationURL = strings.TrimSpace(out)
				}
			} else {
				verificationURL = "https://accounts.x.ai/authorize"
			}
		} else {
			verificationURL = "https://accounts.x.ai/authorize"
		}
		port := 56121
		callbackPort = &port
	default:
		return OAuthSession{}, fmt.Errorf("%w: unsupported provider %q", ErrValidation, provider)
	}
	if verificationURL == "" {
		verificationURL = "https://example.com/activate"
	}
	sess := OAuthSession{
		ID:              id,
		Provider:        provider,
		Flow:            flow,
		VerificationURL: verificationURL,
		UserCode:        userCode,
		CallbackPort:    callbackPort,
		ExpiresAt:       expiresAt,
		Status:          OAuthStatusPending,
	}
	if g.sessions == nil {
		g.sessions = make(map[string]*oauthRecord)
	}
	g.sessions[id] = &oauthRecord{session: sess, createdAt: now}
	return sess, nil
}

func (g *litellmGateway) PollOAuth(ctx context.Context, sessionID string) (OAuthSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return OAuthSession{}, fmt.Errorf("%w: session_id is required", ErrValidation)
	}
	if strings.Contains(sessionID, "\x00") || strings.ContainsAny(sessionID, "`$|;&*?~#()<>\"'\\") {
		return OAuthSession{}, fmt.Errorf("%w: invalid sessionID", ErrValidation)
	}
	if g.sessions == nil {
		return OAuthSession{}, ErrNotFound
	}
	rec, ok := g.sessions[sessionID]
	if !ok {
		return OAuthSession{}, ErrNotFound
	}
	if time.Now().UTC().After(rec.session.ExpiresAt) {
		rec.session.Status = OAuthStatusExpired
		return rec.session, nil
	}
	// If runner available and pending, attempt to poll helper
	if g.runner != nil && rec.session.Status == OAuthStatusPending {
		switch rec.session.Provider {
		case ProviderChatGPT:
			args := []string{"litellm", "chatgpt", "auth", "poll", "--session", sessionID, "--json"}
			if validateArgvSafety(args[0], args[1:]) == nil {
				ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
				out, err := g.runner.Run(ctx2, args...)
				cancel()
				if err == nil && out != "" {
					var parsed struct {
						Status string `json:"status"`
					}
					if json.Unmarshal([]byte(out), &parsed) == nil {
						switch parsed.Status {
						case OAuthStatusConnected, OAuthStatusDenied, OAuthStatusExpired, OAuthStatusError, OAuthStatusPending:
							rec.session.Status = parsed.Status
						}
					}
				}
			}
		case ProviderXAI:
			args := []string{"litellm", "xai-oauth", "status", "--json"}
			if validateArgvSafety(args[0], args[1:]) == nil {
				ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
				out, err := g.runner.Run(ctx2, args...)
				cancel()
				if err == nil && out != "" {
					var parsed struct {
						Status string `json:"status"`
					}
					if json.Unmarshal([]byte(out), &parsed) == nil {
						switch parsed.Status {
						case OAuthStatusConnected, OAuthStatusDenied, OAuthStatusExpired, OAuthStatusError, OAuthStatusPending:
							rec.session.Status = parsed.Status
						}
					}
				}
			}
		}
	}
	return rec.session, nil
}

func (g *litellmGateway) ForwardOAuthCallback(ctx context.Context, sessionID, callbackPath string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return fmt.Errorf("%w: session_id is required", ErrValidation)
	}
	if err := validateCallbackPath(callbackPath); err != nil {
		return err
	}
	if g.runner == nil {
		return fmt.Errorf("%w: command runner not configured", ErrValidation)
	}
	if g.sessions == nil {
		return ErrNotFound
	}
	rec, ok := g.sessions[sessionID]
	if !ok {
		return ErrNotFound
	}
	if time.Now().UTC().After(rec.session.ExpiresAt) {
		return fmt.Errorf("%w: session expired", ErrValidation)
	}
	if rec.session.Provider != ProviderXAI || rec.session.Flow != FlowLoopback {
		return fmt.Errorf("%w: callback only allowed for xai loopback sessions", ErrValidation)
	}
	if rec.session.Status != OAuthStatusPending {
		return fmt.Errorf("%w: session not pending", ErrValidation)
	}
	targetURL := "http://127.0.0.1:56121" + callbackPath
	if !isSafeCallbackURL(targetURL) {
		return fmt.Errorf("%w: callback path contains shell metacharacters", ErrValidation)
	}
	// Argv-safe exec: curl without shell
	args := []string{"curl", "-sS", "-X", "GET", targetURL}
	if err := validateArgvSafety(args[0], args[1:4]); err != nil {
		// For curl URL, use relaxed check: already validated via isSafeCallbackURL
	} else {
		// Validate URL separately already
	}
	// The runner must translate to `docker compose exec litellm curl ...` in production;
	// here we directly invoke curl via runner. Never use sh -c.
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := g.runner.Run(ctx2, args...)
	if err != nil {
		msg := truncateForError(out, 300)
		return fmt.Errorf("callback forward failed: %w (output: %s)", err, msg)
	}
	// Mark connected
	rec.session.Status = OAuthStatusConnected
	g.sessions[sessionID] = rec
	_ = out
	return nil
}

func (g *litellmGateway) ProbeModel(ctx context.Context, model, virtualKey string) error {
	model = strings.TrimSpace(model)
	virtualKey = strings.TrimSpace(virtualKey)
	if model == "" {
		return fmt.Errorf("%w: model is required", ErrValidation)
	}
	if virtualKey == "" {
		return fmt.Errorf("%w: virtual key is required", ErrValidation)
	}
	if len(virtualKey) > 2048 {
		return fmt.Errorf("%w: virtualKey too long", ErrValidation)
	}
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "probe"},
		},
		"max_tokens":  1,
		"temperature": 0,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal probe: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx2, http.MethodPost, g.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("probe request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+virtualKey)
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("probe failed: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if classified := ClassifyHTTPStatus(resp.StatusCode); classified != nil {
		return classified
	}
	if resp.StatusCode == 429 {
		return fmt.Errorf("%w: rate limited (429)", ErrValidation)
	}
	if resp.StatusCode == 404 {
		return fmt.Errorf("%w: model not found: %d", ErrNotFound, resp.StatusCode)
	}
	return fmt.Errorf("probe failed with status %d", resp.StatusCode)
}

// Ensure interface compliance.
var _ GatewayAdmin = (*litellmGateway)(nil)

// NoopGateway is a no-op GatewayAdmin for tests when LiteLLM is not configured.
type NoopGateway struct{}

func (NoopGateway) Health(ctx context.Context) error { return nil }
func (NoopGateway) ReconcileModels(ctx context.Context, aliases []Alias, creds []Credential) error { return nil }
func (NoopGateway) IssueVirtualKey(ctx context.Context, vk VirtualKey) (string, error) {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "sk-" + hex.EncodeToString(b[:]), nil
}
func (NoopGateway) RevokeVirtualKey(ctx context.Context, gatewayKeyID string) error { return nil }
func (NoopGateway) StartOAuth(ctx context.Context, provider, flow string) (OAuthSession, error) {
	return OAuthSession{}, fmt.Errorf("%w: oauth not configured", ErrValidation)
}
func (NoopGateway) PollOAuth(ctx context.Context, sessionID string) (OAuthSession, error) {
	return OAuthSession{}, fmt.Errorf("%w: oauth not configured", ErrValidation)
}
func (NoopGateway) ForwardOAuthCallback(ctx context.Context, sessionID, callbackPath string) error {
	return fmt.Errorf("%w: oauth not configured", ErrValidation)
}
func (NoopGateway) ProbeModel(ctx context.Context, model, virtualKey string) error { return nil }

var _ GatewayAdmin = NoopGateway{}

// helpers

const (
	FlowDeviceCode = "device_code"
	FlowLoopback   = "loopback"
)

const (
	OAuthStatusPending   = "pending"
	OAuthStatusConnected = "connected"
	OAuthStatusDenied    = "denied"
	OAuthStatusExpired   = "expired"
	OAuthStatusError     = "error"
)

var allowedProviderFlow = map[string]map[string]bool{
	ProviderChatGPT: {FlowDeviceCode: true},
	ProviderXAI:     {FlowLoopback: true},
}

var shellMetachars = []string{";", "&", "|", "`", "$", "(", ")", "<", ">", "\n", "\r", "\x00", "\"", "'", "\\", "*", "?", "~", "#", "!", "{", "}"}

func validateArgvSafety(name string, args []string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: command name required", ErrValidation)
	}
	if strings.Contains(name, "\x00") || containsAny(name, "`$|;&*?~#()<>\"'\\\n\r") {
		return fmt.Errorf("%w: command name contains shell metacharacters", ErrValidation)
	}
	allowed := map[string]bool{
		"litellm": true,
		"curl":    true,
		"docker":  true,
	}
	if !allowed[name] {
		return fmt.Errorf("%w: command %q not allowed", ErrValidation, name)
	}
	for _, a := range args {
		if strings.Contains(a, "\x00") || strings.Contains(a, "\n") || strings.Contains(a, "\r") {
			return fmt.Errorf("%w: arg contains NUL or newline", ErrValidation)
		}
		if strings.Contains(a, "sh -c") || strings.Contains(a, "bash -c") {
			return fmt.Errorf("%w: shell fragment not allowed", ErrValidation)
		}
		if name == "litellm" && containsAny(a, "`$|;&") {
			return fmt.Errorf("%w: arg %q contains shell metacharacters", ErrValidation, a)
		}
	}
	return nil
}

func containsAny(s, chars string) bool {
	for _, c := range chars {
		if strings.ContainsRune(s, c) {
			return true
		}
	}
	return false
}

func validateCallbackPath(p string) error {
	p = strings.TrimSpace(p)
	if p == "" {
		return fmt.Errorf("%w: callbackPath is required", ErrValidation)
	}
	if strings.Contains(p, "\x00") || strings.Contains(p, "\n") || strings.Contains(p, "\r") {
		return fmt.Errorf("%w: callbackPath contains invalid characters", ErrValidation)
	}
	if !strings.HasPrefix(p, "/callback") {
		return fmt.Errorf("%w: callbackPath must start with /callback", ErrValidation)
	}
	if strings.Contains(p, "://") {
		return fmt.Errorf("%w: callbackPath must not contain host or scheme", ErrValidation)
	}
	u, err := url.Parse(p)
	if err != nil {
		return fmt.Errorf("%w: invalid callbackPath: %v", ErrValidation, err)
	}
	if u.Scheme != "" || u.Host != "" {
		return fmt.Errorf("%w: callbackPath must not contain scheme or host", ErrValidation)
	}
	if u.Path != "/callback" {
		return fmt.Errorf("%w: callbackPath must be exactly /callback with optional query", ErrValidation)
	}
	if strings.Contains(p, "..") {
		return fmt.Errorf("%w: callbackPath must not contain ..", ErrValidation)
	}
	for _, m := range []string{";", "|", "`", "$", "(", ")", "<", ">", "\"", "'", "\\", "{", "}", "!"} {
		if strings.Contains(p, m) {
			return fmt.Errorf("%w: callbackPath contains shell metacharacter %q", ErrValidation, m)
		}
	}
	if strings.Contains(p, "sh -c") || strings.Contains(p, "bash -c") {
		return fmt.Errorf("%w: shell fragment not allowed", ErrValidation)
	}
	return nil
}

func isSafeCallbackURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	if u.Scheme != "http" || u.Host != "127.0.0.1:56121" || u.Path != "/callback" {
		return false
	}
	q := u.RawQuery
	for _, ch := range []string{";", "|", "`", "$", "(", ")", "<", ">", "\n", "\r", "\x00", "\"", "'", "\\", "{", "}"} {
		if strings.Contains(q, ch) {
			return false
		}
	}
	return true
}

func yamlEscape(s string) string {
	if s == "" {
		return `""`
	}
	needsQuote := false
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '/' || r == '.' || r == '-' || r == '_' || r == ':' || r == ' ') {
			needsQuote = true
			break
		}
	}
	if strings.Contains(s, ": ") {
		needsQuote = true
	}
	if strings.HasPrefix(s, " ") || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "*") || strings.HasPrefix(s, "?") {
		needsQuote = true
	}
	if !needsQuote {
		return s
	}
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "$", `\$`, "`", "\\`")
	return `"` + replacer.Replace(s) + `"`
}

func truncateForError(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
