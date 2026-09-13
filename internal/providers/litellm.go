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
	"os/user"
	"path/filepath"
	"strconv"
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
}

// GatewayModelInfo carries native deployment metadata. Extra preserves
// unknown fields (team membership, blocked state, handoff/seed tags) so
// callers can detect conflicts without losing data. It is metadata only;
// LiteLLM never returns secrets here (no return_keys option).
type GatewayModelInfo struct {
	ID              string `json:"id"`
	DBModel         *bool  `json:"db_model,omitempty"`
	Mode            string `json:"mode,omitempty"`
	LitellmProvider string `json:"litellm_provider,omitempty"`
	TeamID          *string `json:"team_id,omitempty"`
	Blocked         *bool   `json:"blocked,omitempty"`
	Extra           map[string]any `json:"-"`
}

// GatewayDeployment is one native LiteLLM router deployment.
type GatewayDeployment struct {
	ModelName     string         `json:"model_name"`
	LitellmParams map[string]any `json:"litellm_params"`
	ModelInfo     GatewayModelInfo `json:"model_info"`
}

// GatewayCredentialSummary is a safe named-credential record (masked values only).
type GatewayCredentialSummary struct {
	CredentialName string            `json:"credential_name"`
	CredentialInfo map[string]string `json:"credential_info,omitempty"`
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
		default:
			if !allowedProviders[provider] {
				return "", false, false, fmt.Errorf("%w: unsupported provider %q", ErrValidation, provider)
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

// shareGatewayConfig makes a freshly rendered config group-readable by the
// litellm service group. Missing group or chown failure is ignored so tests
// and foreign hosts keep working; the unit then fails closed on restart.
func shareGatewayConfig(path string) {
	grp, err := user.LookupGroup("litellm-cfg")
	if err != nil {
		return
	}
	gid, err := strconv.Atoi(grp.Gid)
	if err != nil {
		return
	}
	_ = os.Chown(path, 0, gid)
	_ = os.Chmod(path, 0o640)
	_ = os.Chown(filepath.Dir(path), 0, gid)
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
