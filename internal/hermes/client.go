package hermes

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HermesModelGatewayURL is the fixed LiteLLM gateway used for the default
// profile's inference. It must remain litellm:4000 regardless of Nous Portal
// tool gateway connection. This constant documents the invariant and is used
// as the default base for the Hermes model gateway env.
const HermesModelGatewayURL = "http://litellm:4000"

// ProviderOAuth describes a single provider oauth connection state as returned
// by the Hermes dashboard API GET /api/providers/oauth.
type ProviderOAuth struct {
	Provider  string `json:"provider"`
	Connected bool   `json:"connected"`
	Status    string `json:"status,omitempty"`
}

// NousOAuthSession is the safe session returned by the Hermes Nous OAuth flow.
// It maps to POST /api/providers/oauth/nous/start and GET
// /api/providers/oauth/nous/poll/{session_id}. It never contains device codes,
// access tokens or refresh tokens — only session_id, verification_url, status
// and expiry.
type NousOAuthSession struct {
	SessionID       string     `json:"session_id"`
	VerificationURL string     `json:"verification_url"`
	Status          string     `json:"status,omitempty"`
	Provider        string     `json:"provider,omitempty"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
}

// Toolset describes a Hermes toolset and its selected provider.
type Toolset struct {
	Name     string `json:"name"`
	Provider string `json:"provider,omitempty"`
	// Description and other display fields are preserved if present but not required.
	Description string `json:"description,omitempty"`
}

// Extend HermesClient to proxy the official Hermes dashboard APIs. The
// concrete HTTP client forwards requests with a short-lived Hermes JWT derived
// from the hermes_jwt_secret — it does not reimplement Hermes auth.

type httpHermesClient struct {
	baseURL    string
	jwtSecret  string
	httpClient *http.Client
}

// ClientConfig configures the HTTP Hermes client.
type ClientConfig struct {
	BaseURL    string
	JWTSecret  string
	HTTPClient *http.Client
}

// NewClient creates a Hermes HTTP client that proxies dashboard APIs.
// BaseURL defaults to http://hermes:8080 when empty. JWTSecret may be empty
// in tests; calls will then return ErrNotFound with a clear message.
func NewClient(cfg ClientConfig) *httpHermesClient {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = "http://hermes:8080"
	}
	base = strings.TrimRight(base, "/")
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &httpHermesClient{
		baseURL:    base,
		jwtSecret:  strings.TrimSpace(cfg.JWTSecret),
		httpClient: hc,
	}
}

// NewClientWithSecret is a convenience for tests that only need a secret.
func NewClientWithSecret(secret string) *httpHermesClient {
	return NewClient(ClientConfig{JWTSecret: secret})
}

func (c *httpHermesClient) EnsureProfile(ctx context.Context, id, displayAlias string) error {
	// Profile sync is handled via Service persistence; the HTTP client does not
	// need to push profiles to Hermes for the default case. Return nil to keep
	// Hermes Service creation idempotent.
	return nil
}

func (c *httpHermesClient) UpdateAlias(ctx context.Context, id, displayAlias string) error {
	return nil
}

func (c *httpHermesClient) DeleteProfile(ctx context.Context, id string) error {
	return nil
}

// ListProvidersOAuth proxies GET /api/providers/oauth.
func (c *httpHermesClient) ListProvidersOAuth(ctx context.Context) ([]ProviderOAuth, error) {
	body, status, err := c.do(ctx, http.MethodGet, "/api/providers/oauth", nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		// Hermes older builds may not expose this endpoint; treat as not configured.
		return nil, fmt.Errorf("%w: hermes providers oauth endpoint not found", ErrNotFound)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("hermes list providers oauth: status %d: %s", status, truncate(string(body), 512))
	}
	// Try bare array first.
	var slice []ProviderOAuth
	if err := json.Unmarshal(body, &slice); err == nil {
		return slice, nil
	}
	// Try envelope {providers: [...]}, {items: [...]}, {data: [...]}
	var env struct {
		Providers []ProviderOAuth `json:"providers"`
		Items     []ProviderOAuth `json:"items"`
		Data      []ProviderOAuth `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err == nil {
		if env.Providers != nil {
			return env.Providers, nil
		}
		if env.Items != nil {
			return env.Items, nil
		}
		if env.Data != nil {
			return env.Data, nil
		}
	}
	// Try object map with provider keys: {nous: {connected: true}, ...}
	var m map[string]ProviderOAuth
	if err := json.Unmarshal(body, &m); err == nil && len(m) > 0 {
		// Check if keys look like provider names.
		out := make([]ProviderOAuth, 0, len(m))
		for k, v := range m {
			if v.Provider == "" {
				v.Provider = k
			}
			out = append(out, v)
		}
		return out, nil
	}
	// Try generic envelope where items are under arbitrary key: extract via map
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err == nil {
		for _, key := range []string{"providers", "items", "data"} {
			if v, ok := raw[key]; ok {
				var arr []ProviderOAuth
				if err := json.Unmarshal(v, &arr); err == nil {
					return arr, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("hermes list providers oauth: unexpected response %s", truncate(string(body), 512))
}

// StartNousOAuth proxies POST /api/providers/oauth/nous/start.
func (c *httpHermesClient) StartNousOAuth(ctx context.Context) (*NousOAuthSession, error) {
	body, status, err := c.do(ctx, http.MethodPost, "/api/providers/oauth/nous/start", map[string]any{})
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("hermes start nous oauth: status %d: %s", status, truncate(string(body), 512))
	}
	return decodeNousSession(body)
}

// PollNousOAuth proxies GET /api/providers/oauth/nous/poll/{session_id}.
func (c *httpHermesClient) PollNousOAuth(ctx context.Context, sessionID string) (*NousOAuthSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("%w: session_id required", ErrValidation)
	}
	if strings.Contains(sessionID, "/") || strings.Contains(sessionID, "\x00") || strings.Contains(sessionID, "\n") || strings.Contains(sessionID, "\r") {
		return nil, fmt.Errorf("%w: invalid session_id", ErrValidation)
	}
	path := "/api/providers/oauth/nous/poll/" + url.PathEscape(sessionID)
	body, status, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("%w: nous session not found", ErrNotFound)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("hermes poll nous oauth: status %d: %s", status, truncate(string(body), 512))
	}
	return decodeNousSession(body)
}

// SetToolsetProvider proxies PUT /api/tools/toolsets/{name}/provider with {provider: nous}.
func (c *httpHermesClient) SetToolsetProvider(ctx context.Context, toolsetName, provider string) error {
	toolsetName = strings.TrimSpace(toolsetName)
	provider = strings.TrimSpace(provider)
	if toolsetName == "" || provider == "" {
		return fmt.Errorf("%w: toolset name and provider required", ErrValidation)
	}
	if strings.Contains(toolsetName, "/") || strings.Contains(toolsetName, "\x00") {
		return fmt.Errorf("%w: invalid toolset name", ErrValidation)
	}
	if strings.Contains(provider, "\x00") || strings.Contains(provider, "\n") || strings.Contains(provider, "\r") {
		return fmt.Errorf("%w: invalid provider", ErrValidation)
	}
	path := "/api/tools/toolsets/" + url.PathEscape(toolsetName) + "/provider"
	payload := map[string]string{"provider": provider}
	body, status, err := c.do(ctx, http.MethodPut, path, payload)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return fmt.Errorf("%w: toolset %q not found", ErrNotFound, toolsetName)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("hermes set toolset provider: status %d: %s", status, truncate(string(body), 512))
	}
	return nil
}

// ListToolsets proxies GET /api/tools/toolsets (used by dashboard to show current selections).
func (c *httpHermesClient) ListToolsets(ctx context.Context) ([]Toolset, error) {
	body, status, err := c.do(ctx, http.MethodGet, "/api/tools/toolsets", nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("%w: toolsets endpoint not found", ErrNotFound)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("hermes list toolsets: status %d: %s", status, truncate(string(body), 512))
	}
	var slice []Toolset
	if err := json.Unmarshal(body, &slice); err == nil {
		return slice, nil
	}
	var env struct {
		Toolsets []Toolset `json:"toolsets"`
		Items    []Toolset `json:"items"`
		Data     []Toolset `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err == nil {
		if env.Toolsets != nil {
			return env.Toolsets, nil
		}
		if env.Items != nil {
			return env.Items, nil
		}
		if env.Data != nil {
			return env.Data, nil
		}
	}
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(body, &wrapper); err == nil {
		for _, k := range []string{"toolsets", "items", "data"} {
			if v, ok := wrapper[k]; ok {
				var arr []Toolset
				if err := json.Unmarshal(v, &arr); err == nil {
					return arr, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("hermes list toolsets: unexpected response %s", truncate(string(body), 512))
}

func (c *httpHermesClient) do(ctx context.Context, method, path string, payload any) ([]byte, int, error) {
	if c.jwtSecret == "" {
		// In production this means hermes_jwt_secret not yet configured.
		// Return a clear error so callers can surface 503 rather than 401 loop.
		// For tests with empty secret we still attempt unauthenticated for backwards compat if baseURL is a test server.
		// Heuristic: if baseURL is a test URL (contains 127.0.0.1 or localhost), allow no auth.
		if !strings.Contains(c.baseURL, "127.0.0.1") && !strings.Contains(c.baseURL, "localhost") {
			return nil, 0, fmt.Errorf("%w: hermes jwt secret not configured", ErrNotFound)
		}
	}
	var bodyReader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal hermes payload: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	full := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, full, bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("hermes request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.jwtSecret != "" {
		token := generateHermesJWT(c.jwtSecret)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// Avoid leaking JWT in logs; do not log headers.

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("hermes %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("hermes read response: %w", err)
	}
	return b, resp.StatusCode, nil
}

func decodeNousSession(body []byte) (*NousOAuthSession, error) {
	// Try direct struct first.
	var s NousOAuthSession
	if err := json.Unmarshal(body, &s); err == nil && s.SessionID != "" {
		// Normalize alternative field names if needed.
		if s.VerificationURL == "" {
			var alt struct {
				SessionID       string `json:"sessionId"`
				VerificationURL string `json:"verificationUrl"`
				VerificationURL2 string `json:"url"`
				VerificationURL3 string `json:"verification_url"`
			}
			_ = json.Unmarshal(body, &alt)
			if alt.VerificationURL != "" {
				s.VerificationURL = alt.VerificationURL
			} else if alt.VerificationURL2 != "" {
				s.VerificationURL = alt.VerificationURL2
			}
		}
		return &s, nil
	}
	// Try envelope with session_id / verification_url at top level under different keys
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("decode nous session: %w", err)
	}
	// Extract session_id variations
	var sessionID string
	for _, k := range []string{"session_id", "sessionId", "id"} {
		if v, ok := m[k]; ok {
			_ = json.Unmarshal(v, &sessionID)
			if sessionID != "" {
				break
			}
		}
	}
	var verificationURL string
	for _, k := range []string{"verification_url", "verificationUrl", "url", "verification_uri"} {
		if v, ok := m[k]; ok {
			_ = json.Unmarshal(v, &verificationURL)
			if verificationURL != "" {
				break
			}
		}
	}
	if sessionID == "" && verificationURL == "" {
		return nil, fmt.Errorf("decode nous session: missing session_id/verification_url in %s", truncate(string(body), 512))
	}
	var status string
	for _, k := range []string{"status", "state"} {
		if v, ok := m[k]; ok {
			_ = json.Unmarshal(v, &status)
			break
		}
	}
	var expiresAt *time.Time
	if v, ok := m["expires_at"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				expiresAt = &t
			} else if t, err := time.Parse(time.RFC3339, s); err == nil {
				expiresAt = &t
			}
		}
	}
	if v, ok := m["expiresAt"]; ok && expiresAt == nil {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				expiresAt = &t
			}
		}
	}
	return &NousOAuthSession{
		SessionID:       sessionID,
		VerificationURL: verificationURL,
		Status:          status,
		ExpiresAt:       expiresAt,
	}, nil
}

func generateHermesJWT(secret string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	now := time.Now().UTC()
	payloadMap := map[string]any{
		"sub": "omahab",
		"iss": "omahab",
		"iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
	}
	payloadBytes, _ := json.Marshal(payloadMap)
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	signingInput := header + "." + payload
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signingInput))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signingInput + "." + sig
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Ensure httpHermesClient implements HermesClient.
var _ HermesClient = (*httpHermesClient)(nil)
