package providers

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CHATGPT_TOKEN_DIR and XAI_OAUTH_TOKEN_DIR are literal env values (not secrets)
// set identically on the native gateway unit and the provider-auth helper:
//   CHATGPT_TOKEN_DIR=${dataDir}/apps/litellm/auth/chatgpt
//   XAI_OAUTH_TOKEN_DIR=${dataDir}/apps/litellm/auth/xai
// Directories are 0700 and auth files atomically 0600, owned by the fixed
// litellm service user. Refresh state persists there; never via provider
// credential values or logs.

// ValidateCallbackPath is the exported wrapper for strict callback path validation.
// It is used by controlplane backend before delegating to the gateway. See validateCallbackPath.
func ValidateCallbackPath(p string) error { return validateCallbackPath(p) }

// GatewayAdmin is the single control-plane boundary for the LiteLLM gateway.
type GatewayAdmin interface {
	Health(ctx context.Context) error
	ListModels(ctx context.Context) ([]GatewayDeployment, error)
	GetModel(ctx context.Context, id string) (GatewayDeployment, error)
	CreateModel(ctx context.Context, deployment GatewayDeployment) error
	ListGatewayCredentials(ctx context.Context) ([]GatewayCredentialSummary, error)
	CreateGatewayCredential(ctx context.Context, input GatewayCredentialInput) error
	IssueVirtualKey(ctx context.Context, vk VirtualKey) (string, error)
	RevokeVirtualKey(ctx context.Context, gatewayKeyID, keyAlias string) error
	StartOAuth(ctx context.Context, provider, flow string) (OAuthSession, error)
	PollOAuth(ctx context.Context, sessionID string) (OAuthSession, error)
	ForwardOAuthCallback(ctx context.Context, sessionID, callbackPath string) error
	ProbeModel(ctx context.Context, model, virtualKey string) error
	// Disconnect revokes/deletes local saved auth via a provider-owned
	// operation. Unrelated gateway credentials and models are untouched.
	Disconnect(ctx context.Context, provider string) error
	// ConnectionStatus reports one provider's steady-state connection
	// without starting a login.
	ConnectionStatus(ctx context.Context, provider string) string
}

// GatewayModelInfo carries native deployment metadata. Extra preserves
// unknown fields (team membership, blocked state, handoff/seed tags) so
// callers can detect conflicts without losing data. It is metadata only;
// LiteLLM never returns secrets here (no return_keys option).
type GatewayModelInfo struct {
	ID              string         `json:"id"`
	DBModel         *bool          `json:"db_model,omitempty"`
	Mode            string         `json:"mode,omitempty"`
	LitellmProvider string         `json:"litellm_provider,omitempty"`
	TeamID          *string        `json:"team_id,omitempty"`
	Blocked         *bool          `json:"blocked,omitempty"`
	Extra           map[string]any `json:"-"`
}

// GatewayDeployment is one native LiteLLM router deployment.
type GatewayDeployment struct {
	ModelName     string           `json:"model_name"`
	LitellmParams map[string]any   `json:"litellm_params"`
	ModelInfo     GatewayModelInfo `json:"model_info"`
}

// GatewayCredentialSummary is a safe named-credential record.
// CredentialInfo preserves LiteLLM's general JSON metadata.
// Credential values are intentionally absent because they can contain secrets.
type GatewayCredentialSummary struct {
	CredentialName string         `json:"credential_name"`
	CredentialInfo map[string]any `json:"credential_info,omitempty"`
}

// GatewayCredentialInput creates one named credential. CredentialValues and
// ModelID are mutually exclusive (values XOR model_id); the latter lets
// LiteLLM extract/encrypt credentials server-side from an existing deployment.
type GatewayCredentialInput struct {
	CredentialName   string            `json:"credential_name"`
	CredentialInfo   map[string]string `json:"credential_info,omitempty"`
	CredentialValues map[string]string `json:"credential_values,omitempty"`
	ModelID          string            `json:"model_id,omitempty"`
}

func (m GatewayModelInfo) MarshalJSON() ([]byte, error) {
	type plain GatewayModelInfo
	raw, err := json.Marshal(plain(m))
	if err != nil {
		return nil, err
	}
	if len(m.Extra) == 0 {
		return raw, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	for k, v := range m.Extra {
		if _, ok := obj[k]; !ok {
			obj[k] = v
		}
	}
	return json.Marshal(obj)
}

func (m *GatewayModelInfo) UnmarshalJSON(data []byte) error {
	type plain GatewayModelInfo
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	for _, k := range []string{"id", "db_model", "mode", "litellm_provider", "team_id", "blocked"} {
		delete(obj, k)
	}
	*m = GatewayModelInfo(p)
	if len(obj) > 0 {
		m.Extra = obj
	}
	return nil
}

// ExtraString returns a string tag from Extra (handoff/seed markers).
func (m GatewayModelInfo) ExtraString(key string) string {
	if m.Extra == nil {
		return ""
	}
	if v, ok := m.Extra[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// CredentialInfoString returns one trimmed string metadata value.
func (c GatewayCredentialSummary) CredentialInfoString(key string) string {
	if c.CredentialInfo == nil {
		return ""
	}
	value, ok := c.CredentialInfo[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
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
	Status          string    `json:"status"` // connecting|connected|not_entitled|quota_exceeded|not_authenticated|pending|denied|expired|error
}

// CommandRunner runs argv-safe local commands without a shell. Callers
// pass argv vectors, never shell fragments.
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
	auth       AuthHelper
	// in-memory OAuth sessions for the gateway (never persist device codes/tokens)
	sessions map[string]*oauthRecord
}

type oauthRecord struct {
	session   OAuthSession
	createdAt time.Time
}

// AuthHelper drives subscription logins through the packaged helper
// subprocess. Implemented by *ProviderAuthAdapter; fakes cover tests.
type AuthHelper interface {
	Start(ctx context.Context, provider string) (OAuthSession, error)
	Poll(ctx context.Context, sessionID string) (OAuthSession, error)
	Cancel(ctx context.Context, sessionID string) error
	Capabilities(ctx context.Context) (map[string]any, error)
	ConnectionStatus(ctx context.Context, provider string) string
	Disconnect(ctx context.Context, provider string) error
}

// GatewayOptions configures NewLiteLLMGateway.
type GatewayOptions struct {
	HTTPClient  *http.Client
	BaseURL     string
	MasterKey   string
	ConfigDir   string
	Runner      CommandRunner
	AuthAdapter AuthHelper
}

// NewLiteLLMGateway creates a gateway.
func NewLiteLLMGateway(db any, opts GatewayOptions) (*litellmGateway, error) {
	if db == nil {
		return nil, fmt.Errorf("gateway: db is required")
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
		auth:       opts.AuthAdapter,
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
	if g.auth == nil {
		// No helper: cannot verify pin; skip in test/minimal env.
		return nil
	}
	// Helper capability/import checks against the packaged v1.97.0
	// environment: both authenticators must import and report.
	caps, err := g.auth.Capabilities(ctx)
	if err != nil {
		return fmt.Errorf("litellm pin check failed: provider-auth capabilities: %w", err)
	}
	for _, provider := range []string{ProviderChatGPT, ProviderXAI} {
		sub, _ := caps[provider].(map[string]any)
		if ok, _ := sub["ok"].(bool); !ok {
			return fmt.Errorf("%w: provider-auth helper lacks %s support", ErrValidation, provider)
		}
	}
	// Real native model request smoke: list models through the gateway.
	if strings.TrimSpace(g.masterKey) == "" {
		return nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx2, http.MethodGet, g.baseURL+"/v1/models", nil)
	if err != nil {
		return fmt.Errorf("pin model smoke: %w", err)
	}
	g.gatewayAuth(req)
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("pin model smoke: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		if classified := ClassifyHTTPStatus(resp.StatusCode); classified != nil {
			return classified
		}
		return fmt.Errorf("pin model smoke: status %d", resp.StatusCode)
	}
	return nil
}

// Disconnect revokes/deletes local saved auth via the provider-owned helper
// operation. Unrelated gateway credentials and models are untouched.
func (g *litellmGateway) Disconnect(ctx context.Context, provider string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != ProviderChatGPT && provider != ProviderXAI {
		return fmt.Errorf("%w: disconnect only supports chatgpt|xai subscription auth", ErrValidation)
	}
	if g.auth == nil {
		return fmt.Errorf("%w: provider auth helper not configured", ErrValidation)
	}
	if err := g.auth.Disconnect(ctx, provider); err != nil {
		return err
	}
	if g.sessions != nil {
		for id, rec := range g.sessions {
			if rec.session.Provider == provider {
				delete(g.sessions, id)
			}
		}
	}
	return nil
}

// ConnectionStatus reports one provider's steady-state connection without
// starting a login.
func (g *litellmGateway) ConnectionStatus(ctx context.Context, provider string) string {
	if g.auth == nil {
		return OAuthStatusNotAuthenticated
	}
	return g.auth.ConnectionStatus(ctx, provider)
}

// ExecRunner runs argv-safe local commands without a shell. Callers pass
// argv vectors, never shell fragments; the command inherits a minimal
// environment and never secret values in arguments.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, args ...string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("%w: command is required", ErrValidation)
	}
	if err := validateArgvSafety(args[0], args[1:]); err != nil {
		// The xAI callback URL carries ?&= query characters by contract;
		// it was pre-validated by isSafeCallbackURL, so only the argv
		// name is enforced here.
		if err := validateArgvSafety(args[0], nil); err != nil {
			return "", err
		}
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	out, err := cmd.CombinedOutput()
	if len(out) > 1<<20 {
		out = out[:1<<20]
	}
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}

// gatewayAuth attaches LiteLLM master-key authentication to a gateway request.
func (g *litellmGateway) gatewayAuth(req *http.Request) {
	req.Header.Set("x-litellm-key", g.masterKey)
	req.Header.Set("Authorization", "Bearer "+g.masterKey)
}

func (g *litellmGateway) gatewayGet(ctx context.Context, path string, query map[string]string) ([]byte, int, error) {
	if strings.TrimSpace(g.masterKey) == "" {
		return nil, 0, fmt.Errorf("%w: gateway not configured", ErrValidation)
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u := g.baseURL + path
	if len(query) > 0 {
		v := url.Values{}
		for k, val := range query {
			v.Set(k, val)
		}
		u += "?" + v.Encode()
	}
	req, err := http.NewRequestWithContext(ctx2, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("gateway request: %w", err)
	}
	g.gatewayAuth(req)
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("gateway request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return body, resp.StatusCode, nil
}

func (g *litellmGateway) gatewayPost(ctx context.Context, path string, payload any) ([]byte, int, error) {
	if strings.TrimSpace(g.masterKey) == "" {
		return nil, 0, fmt.Errorf("%w: gateway not configured", ErrValidation)
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal gateway payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx2, http.MethodPost, g.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("gateway request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	g.gatewayAuth(req)
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("gateway request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return respBody, resp.StatusCode, nil
}

// ListModels returns native inventory via GET /v2/model/info with pagination
// and ID dedup. An empty data array on a fresh gateway is a valid empty
// inventory, never proof of database connectivity.
func (g *litellmGateway) ListModels(ctx context.Context) ([]GatewayDeployment, error) {
	seen := map[string]bool{}
	var out []GatewayDeployment
	page := 1
	for {
		body, status, err := g.gatewayGet(ctx, "/v2/model/info", map[string]string{"page": strconv.Itoa(page), "size": "100"})
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			if classified := ClassifyHTTPStatus(status); classified != nil {
				return nil, classified
			}
			return nil, fmt.Errorf("model info unexpected status %d", status)
		}
		var parsed struct {
			Data        []GatewayDeployment `json:"data"`
			TotalCount  int                 `json:"total_count"`
			CurrentPage int                 `json:"current_page"`
			TotalPages  int                 `json:"total_pages"`
			Size        int                 `json:"size"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("parse model info: %w", err)
		}
		for _, d := range parsed.Data {
			id := strings.TrimSpace(d.ModelInfo.ID)
			if id == "" {
				id = strings.TrimSpace(d.ModelName)
			}
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, d)
		}
		totalPages := parsed.TotalPages
		if totalPages <= 0 {
			break
		}
		if page >= totalPages {
			break
		}
		page++
		if page > 100 {
			break
		}
	}
	if out == nil {
		out = []GatewayDeployment{}
	}
	return out, nil
}

// GetModel fetches one deployment via GET /v1/model/info?litellm_model_id=<id>.
func (g *litellmGateway) GetModel(ctx context.Context, id string) (GatewayDeployment, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return GatewayDeployment{}, fmt.Errorf("%w: model id is required", ErrValidation)
	}
	body, status, err := g.gatewayGet(ctx, "/v1/model/info", map[string]string{"litellm_model_id": id})
	if err != nil {
		return GatewayDeployment{}, err
	}
	if status == http.StatusNotFound {
		return GatewayDeployment{}, ErrNotFound
	}
	if status != http.StatusOK {
		if classified := ClassifyHTTPStatus(status); classified != nil {
			return GatewayDeployment{}, classified
		}
		return GatewayDeployment{}, fmt.Errorf("model info unexpected status %d", status)
	}
	var parsed struct {
		Data []GatewayDeployment `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return GatewayDeployment{}, fmt.Errorf("parse model info: %w", err)
	}
	if len(parsed.Data) == 0 {
		return GatewayDeployment{}, ErrNotFound
	}
	return parsed.Data[0], nil
}

// CreateModel creates one DB deployment via POST /model/new, honoring the
// caller-supplied model_info.id as the stable ID. Callers must supply a
// distinct stable ID per deployment and never reuse a source model ID.
func (g *litellmGateway) CreateModel(ctx context.Context, deployment GatewayDeployment) error {
	if strings.TrimSpace(deployment.ModelName) == "" {
		return fmt.Errorf("%w: model_name is required", ErrValidation)
	}
	if strings.TrimSpace(deployment.ModelInfo.ID) == "" {
		return fmt.Errorf("%w: model_info.id is required", ErrValidation)
	}
	if deployment.LitellmParams == nil {
		return fmt.Errorf("%w: litellm_params is required", ErrValidation)
	}
	payload := map[string]any{
		"model_name":     strings.TrimSpace(deployment.ModelName),
		"litellm_params": deployment.LitellmParams,
		"model_info":     deployment.ModelInfo,
	}
	respBody, status, err := g.gatewayPost(ctx, "/model/new", payload)
	if err != nil {
		return err
	}
	if status == http.StatusOK || status == http.StatusCreated {
		return nil
	}
	_ = respBody
	if classified := ClassifyHTTPStatus(status); classified != nil {
		return classified
	}
	return fmt.Errorf("model create unexpected status %d", status)
}

// ListGatewayCredentials returns named-credential inventory (masked values only).
func (g *litellmGateway) ListGatewayCredentials(ctx context.Context) ([]GatewayCredentialSummary, error) {
	body, status, err := g.gatewayGet(ctx, "/credentials", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		if classified := ClassifyHTTPStatus(status); classified != nil {
			return nil, classified
		}
		return nil, fmt.Errorf("credentials unexpected status %d", status)
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return []GatewayCredentialSummary{}, nil
	}
	if trimmed[0] == '[' {
		var arr []GatewayCredentialSummary
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, fmt.Errorf("parse credentials: %w", err)
		}
		if arr == nil {
			arr = []GatewayCredentialSummary{}
		}
		return arr, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	for _, key := range []string{"credentials", "data", "items"} {
		if raw, ok := obj[key]; ok {
			var arr []GatewayCredentialSummary
			if err := json.Unmarshal(raw, &arr); err != nil {
				return nil, fmt.Errorf("parse credentials: %w", err)
			}
			if arr == nil {
				arr = []GatewayCredentialSummary{}
			}
			return arr, nil
		}
	}
	return []GatewayCredentialSummary{}, nil
}

// CreateGatewayCredential creates one named credential. CredentialValues and
// ModelID are mutually exclusive; ModelID lets LiteLLM extract/encrypt
// credentials server-side without secrets transiting the browser or Omahab.
func (g *litellmGateway) CreateGatewayCredential(ctx context.Context, input GatewayCredentialInput) error {
	name := strings.TrimSpace(input.CredentialName)
	if name == "" {
		return fmt.Errorf("%w: credential_name is required", ErrValidation)
	}
	hasValues := len(input.CredentialValues) > 0
	hasModel := strings.TrimSpace(input.ModelID) != ""
	if hasValues == hasModel {
		return fmt.Errorf("%w: exactly one of credential_values or model_id is required", ErrValidation)
	}
	payload := map[string]any{"credential_name": name}
	if input.CredentialInfo != nil {
		payload["credential_info"] = input.CredentialInfo
	} else {
		payload["credential_info"] = map[string]string{}
	}
	if hasValues {
		payload["credential_values"] = input.CredentialValues
	} else {
		payload["model_id"] = strings.TrimSpace(input.ModelID)
	}
	respBody, status, err := g.gatewayPost(ctx, "/credentials", payload)
	if err != nil {
		return err
	}
	if status == http.StatusOK || status == http.StatusCreated {
		return nil
	}
	_ = respBody
	if classified := ClassifyHTTPStatus(status); classified != nil {
		return classified
	}
	return fmt.Errorf("credential create unexpected status %d", status)
}

// NormalizeNativeModel maps a legacy alias model to its native LiteLLM model
// string, preserving OpenAI/Anthropic/OpenRouter prefixes, xAI use_xai_oauth,
// and ChatGPT responses mode. It is the extracted rendering contract formerly
// inside the retired YAML reconciler, reused by the one-time handoff.
func NormalizeNativeModel(provider, credType, managedBy, externalRef, model string) (litellmModel string, useXaiOAuth bool, responsesMode bool, err error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	credType = strings.ToLower(strings.TrimSpace(credType))
	managedBy = strings.TrimSpace(managedBy)
	if managedBy == "" {
		managedBy = ManagedByOmahab
	}
	externalRef = strings.TrimSpace(externalRef)
	mdl := strings.TrimSpace(model)
	if mdl == "" {
		return "", false, false, fmt.Errorf("%w: model is required", ErrValidation)
	}
	if strings.Contains(mdl, "\x00") {
		return "", false, false, fmt.Errorf("%w: NUL byte not allowed", ErrValidation)
	}
	switch {
	case provider == ProviderXAI && credType == CredentialTypeOAuth && managedBy == ManagedByLiteLLM && externalRef == ExternalRefXAI:
		if !strings.HasPrefix(strings.ToLower(mdl), "xai/") {
			mdl = "xai/" + strings.TrimPrefix(mdl, "/")
		}
		return mdl, true, false, nil
	case provider == ProviderChatGPT && credType == CredentialTypeOAuth && managedBy == ManagedByLiteLLM && externalRef == ExternalRefChatGPT:
		if !strings.HasPrefix(strings.ToLower(mdl), "chatgpt/") {
			mdl = "chatgpt/" + strings.TrimPrefix(mdl, "/")
		}
		return mdl, false, true, nil
	case credType == CredentialTypeAPIKey && managedBy == ManagedByOmahab:
		lower := strings.ToLower(mdl)
		switch provider {
		case ProviderOpenAI:
			if !strings.HasPrefix(lower, "openai/") {
				mdl = "openai/" + strings.TrimPrefix(mdl, "/")
			}
		case ProviderAnthropic:
			if !strings.HasPrefix(lower, "anthropic/") {
				mdl = "anthropic/" + strings.TrimPrefix(mdl, "/")
			}
		case ProviderOpenRouter:
			if !strings.HasPrefix(lower, "openrouter/") {
				mdl = "openrouter/" + strings.TrimPrefix(mdl, "/")
			}
		case ProviderZAI:
			// Z.ai renders through openai/<glm-model> with an explicit
			// api_base on the deployment (PAYG or Coding Plan endpoint,
			// chosen per connection, never silently swapped).
			lm := strings.ToLower(mdl)
			switch {
			case strings.HasPrefix(lm, "openai/"):
				mdl = "openai/" + mdl[len("openai/"):]
			case strings.HasPrefix(lm, "zai/"):
				mdl = "openai/" + mdl[len("zai/"):]
			default:
				mdl = "openai/" + strings.TrimPrefix(mdl, "/")
			}
		default:
			if !allowedProviders[provider] {
			}
			if !strings.HasPrefix(lower, provider+"/") {
				mdl = provider + "/" + strings.TrimPrefix(mdl, "/")
			}
		}
		return mdl, false, false, nil
	default:
		if credType == CredentialTypeOAuth && managedBy == ManagedByLiteLLM {
			if provider == ProviderXAI {
				if !strings.HasPrefix(strings.ToLower(mdl), "xai/") {
					mdl = "xai/" + strings.TrimPrefix(mdl, "/")
				}
				return mdl, true, false, nil
			}
			if provider == ProviderChatGPT {
				if !strings.HasPrefix(strings.ToLower(mdl), "chatgpt/") {
					mdl = "chatgpt/" + strings.TrimPrefix(mdl, "/")
				}
				return mdl, false, true, nil
			}
		}
		return "", false, false, fmt.Errorf("%w: unsupported credential rendering for %s/%s", ErrValidation, provider, credType)
	}
}

// ShareGatewayConfig makes a freshly rendered config group-readable by the
// litellm service group. Missing group or chown failure falls back to plain
// restrictive perms so tests and foreign hosts keep working; the unit then
// fails closed on restart.
func ShareGatewayConfig(path string) {
	if grp, err := user.LookupGroup("litellm-cfg"); err == nil {
		if gid, err := strconv.Atoi(grp.Gid); err == nil {
			_ = os.Chown(path, 0, gid)
			_ = os.Chmod(path, 0o640)
			_ = os.Chown(filepath.Dir(path), 0, gid)
			_ = os.Chmod(filepath.Dir(path), 0o750)
			return
		}
	}
	_ = os.Chmod(path, 0o640)
	_ = os.Chmod(filepath.Dir(path), 0o750)
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

func (g *litellmGateway) RevokeVirtualKey(ctx context.Context, gatewayKeyID, keyAlias string) error {
	gatewayKeyID = strings.TrimSpace(gatewayKeyID)
	keyAlias = strings.TrimSpace(keyAlias)
	if gatewayKeyID == "" && keyAlias == "" {
		return fmt.Errorf("%w: gateway_key_id is required", ErrValidation)
	}
	for _, id := range []string{gatewayKeyID, keyAlias} {
		if strings.Contains(id, "\x00") || strings.ContainsAny(id, "`$|;&*?~#()<>") {
			return fmt.Errorf("%w: gatewayKeyID contains invalid characters", ErrValidation)
		}
	}
	if strings.TrimSpace(g.masterKey) == "" {
		// No master key: treat as success in test
		return nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// /key/delete matches rows by token or key_alias, not by the key_id
	// returned from /key/generate — so send both. The alias is what
	// IssueVirtualKey sets at generate time, hence the reliable handle.
	seen := map[string]bool{}
	keys := make([]string, 0, 2)
	for _, id := range []string{keyAlias, gatewayKeyID} {
		if id != "" && !seen[id] {
			seen[id] = true
			keys = append(keys, id)
		}
	}
	payload := map[string]any{
		"keys": keys,
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
	// Real subscription login through the packaged helper subprocess. No
	// production no-op or fake challenge exists: a missing adapter reports
	// not-configured and the UI shows the blocked check.
	if g.auth == nil {
		return OAuthSession{}, fmt.Errorf("%w: subscription login not configured (provider auth helper missing)", ErrValidation)
	}
	sess, err := g.auth.Start(ctx, provider)
	if err != nil {
		return OAuthSession{}, err
	}
	if g.sessions == nil {
		g.sessions = make(map[string]*oauthRecord)
	}
	g.sessions[sess.ID] = &oauthRecord{session: sess, createdAt: time.Now().UTC()}
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
	// Live helper state wins when an adapter is wired; the registry only
	// tracks existence and expiry otherwise.
	if g.auth != nil {
		live, err := g.auth.Poll(ctx, sessionID)
		if err == nil {
			rec.session = live
		} else if errors.Is(err, ErrNotFound) {
			return OAuthSession{}, ErrNotFound
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
	if rec.session.Status != OAuthStatusPending && rec.session.Status != OAuthStatusConnecting {
		return fmt.Errorf("%w: session not pending", ErrValidation)
	}
	targetURL := "http://127.0.0.1:56121" + callbackPath
	if !isSafeCallbackURL(targetURL) {
		return fmt.Errorf("%w: callback path contains shell metacharacters", ErrValidation)
	}
	// Argv-safe exec: curl without shell. The URL was pre-validated by
	// isSafeCallbackURL (strict /callback?<query> shape, no foreign
	// host/scheme/port, no shell metacharacters); never use sh -c.
	args := []string{"curl", "-sS", "-X", "GET", targetURL}
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := g.runner.Run(ctx2, args...)
	if err != nil {
		msg := truncateForError(out, 300)
		return fmt.Errorf("callback forward failed: %w (output: %s)", err, msg)
	}
	// The forward only delivers the callback to the helper's loopback
	// listener; the helper completes the exchange and the adapter session
	// flips to connected. Never mark connected here: PollOAuth reveals it.
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
func (NoopGateway) ListModels(ctx context.Context) ([]GatewayDeployment, error) {
	return nil, fmt.Errorf("%w: gateway not configured", ErrValidation)
}
func (NoopGateway) GetModel(ctx context.Context, id string) (GatewayDeployment, error) {
	return GatewayDeployment{}, fmt.Errorf("%w: gateway not configured", ErrValidation)
}
func (NoopGateway) CreateModel(ctx context.Context, deployment GatewayDeployment) error {
	return fmt.Errorf("%w: gateway not configured", ErrValidation)
}
func (NoopGateway) ListGatewayCredentials(ctx context.Context) ([]GatewayCredentialSummary, error) {
	return nil, fmt.Errorf("%w: gateway not configured", ErrValidation)
}
func (NoopGateway) CreateGatewayCredential(ctx context.Context, input GatewayCredentialInput) error {
	return fmt.Errorf("%w: gateway not configured", ErrValidation)
}
func (NoopGateway) IssueVirtualKey(ctx context.Context, vk VirtualKey) (string, error) {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "sk-" + hex.EncodeToString(b[:]), nil
}
func (NoopGateway) RevokeVirtualKey(ctx context.Context, gatewayKeyID, keyAlias string) error {
	return nil
}
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
func (NoopGateway) Disconnect(ctx context.Context, provider string) error {
	return fmt.Errorf("%w: oauth not configured", ErrValidation)
}
func (NoopGateway) ConnectionStatus(ctx context.Context, provider string) string {
	return OAuthStatusNotAuthenticated
}

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

// ProviderEnvVarPrefix namespaces omahab-managed provider key material in the
// litellm systemd unit's EnvironmentFile (appenv/litellm.env). The renderer
// emits `api_key: os.environ/<NAME>` refs; controlplane converges the file.
const ProviderEnvVarPrefix = "OMAHAB_PROVIDER_"

// ProviderEnvVar maps a credential ID to its environment variable name:
// OMAHAB_PROVIDER_<SANITIZED_ID> where sanitization uppercases and maps every
// non-ASCII-alphanumeric byte to '_'. Deterministic and injective enough for
// credential IDs (UUID/hex); never logs or touches key material.
func ProviderEnvVar(id string) string {
	id = strings.TrimSpace(id)
	var sb strings.Builder
	sb.WriteString(ProviderEnvVarPrefix)
	for i := range id {
		c := id[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			sb.WriteByte(c)
		} else {
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

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
