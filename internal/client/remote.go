package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
)
var (
	ErrInstanceMismatch = errors.New("server instance mismatch")
	ErrPublicFallback   = errors.New("public fallback rejected")
	ErrNotAuthenticated = errors.New("not authenticated")
)

// RemoteClient talks to omahabd over HTTPS with instance-ID and Tailscale
// identity pinning. It never logs credentials.
type RemoteClient struct {
	baseURL    string
	pinnedID   string
	httpClient *http.Client
	creds      CredentialStore
}

// RemoteClientConfig configures the remote client.
type RemoteClientConfig struct {
	ServerURL        string
	PinnedInstanceID string
	CredentialStore  CredentialStore
	HTTPClient       *http.Client
}

// NewRemoteClient creates a RemoteClient. If HTTPClient is nil a default with
// secure TLS is used. ServerURL must be https in production; http is allowed
// for tests via loopback.
func NewRemoteClient(cfg RemoteClientConfig) (*RemoteClient, error) {
	if strings.TrimSpace(cfg.ServerURL) == "" {
		return nil, fmt.Errorf("server_url required")
	}
	u, err := url.Parse(cfg.ServerURL)
	if err != nil {
		return nil, fmt.Errorf("invalid server_url: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("server_url must be https or http")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					MinVersion: tls.VersionTLS12,
				},
			},
		}
	}
	return &RemoteClient{
		baseURL:    strings.TrimRight(cfg.ServerURL, "/"),
		pinnedID:   cfg.PinnedInstanceID,
		httpClient: hc,
		creds:      cfg.CredentialStore,
	}, nil
}

// BaseURL returns the configured base URL.
func (c *RemoteClient) BaseURL() string { return c.baseURL }

// PinnedID returns the pinned instance ID (may be empty until enrollment).
func (c *RemoteClient) PinnedID() string { return c.pinnedID }

// SetPinnedID updates the pinned instance (e.g., after enrollment).
func (c *RemoteClient) SetPinnedID(id string) { c.pinnedID = id }

func (c *RemoteClient) authHeader() (string, error) {
	if c.creds == nil {
		return "", nil
	}
	tok, err := c.creds.Get(CredentialService, CredentialAccount)
	if err != nil {
		if errors.Is(err, ErrCredentialNotFound) {
			return "", nil
		}
		return "", err
	}
	return "Bearer " + tok, nil
}

func (c *RemoteClient) deviceAuthHeader() (string, error) {
	if c.creds == nil {
		return "", nil
	}
	tok, err := c.creds.Get(CredentialService, CredentialDeviceAccount)
	if err != nil {
		if errors.Is(err, ErrCredentialNotFound) {
			return "", nil
		}
		return "", err
	}
	if strings.TrimSpace(tok) == "" {
		return "", nil
	}
	return "Bearer " + strings.TrimSpace(tok), nil
}

func (c *RemoteClient) doWithAuth(ctx context.Context, method, path string, auth string, body io.Reader, out any) error {
	u := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.pinnedID != "" {
		parsed, _ := url.Parse(c.baseURL)
		if parsed != nil && parsed.Scheme == "http" && !isLoopbackHost(parsed.Host) {
			return fmt.Errorf("%w: pinned instance requires https or loopback", ErrPublicFallback)
		}
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrNotAuthenticated
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("remote %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	if c.pinnedID != "" {
		if instID := extractInstanceID(data); instID != "" && instID != c.pinnedID {
			return fmt.Errorf("%w: got %s want %s", ErrInstanceMismatch, instID, c.pinnedID)
		}
	}
	return nil
}

func (c *RemoteClient) do(ctx context.Context, method, path string, out any) error {
	u := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return err
	}
	if h, err := c.authHeader(); err != nil {
		return err
	} else if h != "" {
		req.Header.Set("Authorization", h)
	}
	req.Header.Set("Accept", "application/json")

	// Reject downgrade from pinned https to http (public fallback).
	if c.pinnedID != "" {
		parsed, _ := url.Parse(c.baseURL)
		if parsed != nil && parsed.Scheme == "http" && !isLoopbackHost(parsed.Host) {
			return fmt.Errorf("%w: pinned instance requires https or loopback", ErrPublicFallback)
		}
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return ErrNotAuthenticated
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("remote %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	// Instance pinning check for instance/status payloads.
	if c.pinnedID != "" {
		if instID := extractInstanceID(data); instID != "" && instID != c.pinnedID {
			return fmt.Errorf("%w: got %s want %s", ErrInstanceMismatch, instID, c.pinnedID)
		}
	}
	return nil
}

func isLoopbackHost(host string) bool {
	h := host
	if idx := strings.LastIndex(h, ":"); idx != -1 {
		h = h[:idx]
	}
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

func extractInstanceID(data []byte) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	// Try top-level "id" (instance) and "instance_id" (status)
	for _, k := range []string{"instance_id", "id"} {
		if raw, ok := m[k]; ok {
			var s string
			if err := json.Unmarshal(raw, &s); err == nil {
				return s
			}
		}
	}
	// Nested "instance": {"id": "..."}
	if raw, ok := m["instance"]; ok {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(raw, &inner); err == nil {
			if v, ok := inner["id"]; ok {
				var s string
				if err := json.Unmarshal(v, &s); err == nil {
					return s
				}
			}
		}
	}
	return ""
}

// GetInstance fetches /api/instance (or /api/status fallback).
func (c *RemoteClient) GetInstance(ctx context.Context) (*domain.Instance, error) {
	var inst domain.Instance
	if err := c.do(ctx, http.MethodGet, "/api/instance", &inst); err == nil {
		return &inst, nil
	}
	// Fallback: some deployments expose instance via status.
	var st domain.Status
	if err := c.do(ctx, http.MethodGet, "/api/status", &st); err != nil {
		return nil, err
	}
	return &domain.Instance{ID: st.InstanceID}, nil
}

// GetStatus fetches the server status.
func (c *RemoteClient) GetStatus(ctx context.Context) (*domain.Status, error) {
	var st domain.Status
	if err := c.do(ctx, http.MethodGet, "/api/status", &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// GetEvents fetches recent events.
func (c *RemoteClient) GetEvents(ctx context.Context) ([]domain.Event, error) {
	var evs []domain.Event
	if err := c.do(ctx, http.MethodGet, "/api/events", &evs); err != nil {
		return nil, err
	}
	return evs, nil
}

// GetProjects fetches projects (for sidebar/sync).
func (c *RemoteClient) GetProjects(ctx context.Context) ([]domain.Project, error) {
	var ps []domain.Project
	if err := c.do(ctx, http.MethodGet, "/api/projects", &ps); err != nil {
		return nil, err
	}
	return ps, nil
}

// CheckPocketID probes pocket-id reachability via the server's instance host.
func (c *RemoteClient) CheckPocketID(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/api/instance", nil)
}

// EnrollCompanion claims a single-use enrollment code and returns a device token (oma_dev_...).
// It is unauthenticated (no bearer) and must be called with the code from POST /api/v1/companion-enrollments.
// On success it also returns per-device machine-backup credentials when the server provides them.
type EnrollResult struct {
	Token          string `json:"token"`
	TokenPrefix    string `json:"token_prefix"`
	ResticRepo     string `json:"restic_repo"`
	ResticPassword string `json:"restic_password"`
	RestUser       string `json:"rest_user"`
	RestPassword   string `json:"rest_password"`
}

func (c *RemoteClient) EnrollCompanion(ctx context.Context, code string) (*EnrollResult, error) {
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		return nil, fmt.Errorf("code is required")
	}
	payload, _ := json.Marshal(map[string]string{"code": trimmed})
	var resp EnrollResult
	if err := c.doWithAuth(ctx, http.MethodPost, "/api/v1/companion/enroll", "", bytes.NewReader(payload), &resp); err != nil {
		return nil, err
	}
	tok := strings.TrimSpace(resp.Token)
	if tok == "" {
		return nil, fmt.Errorf("empty token in enroll response")
	}
	if !strings.HasPrefix(tok, "oma_dev_") {
		return nil, fmt.Errorf("invalid device token prefix")
	}
	return &resp, nil
}

// EnrollCompanionToken is a convenience wrapper that returns only the token string (legacy).
func (c *RemoteClient) EnrollCompanionToken(ctx context.Context, code string) (string, error) {
	res, err := c.EnrollCompanion(ctx, code)
	if err != nil {
		return "", err
	}
	return res.Token, nil
}

// GetCompanionStatus fetches status via device-authenticated endpoint.
func (c *RemoteClient) GetCompanionStatus(ctx context.Context) (*domain.Status, error) {
	auth, err := c.deviceAuthHeader()
	if err != nil {
		return nil, err
	}
	if auth == "" {
		return nil, ErrNotAuthenticated
	}
	var st domain.Status
	if err := c.doWithAuth(ctx, http.MethodGet, "/api/v1/companion/status", auth, nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// GetCompanionEvents fetches events via device endpoint.
func (c *RemoteClient) GetCompanionEvents(ctx context.Context) ([]domain.Event, error) {
	auth, err := c.deviceAuthHeader()
	if err != nil {
		return nil, err
	}
	if auth == "" {
		return nil, ErrNotAuthenticated
	}
	var out struct {
		Items []domain.Event `json:"items"`
	}
	if err := c.doWithAuth(ctx, http.MethodGet, "/api/v1/companion/events", auth, nil, &out); err != nil {
		// Fallback: server may return plain array
		var evs []domain.Event
		if err2 := c.doWithAuth(ctx, http.MethodGet, "/api/v1/companion/events", auth, nil, &evs); err2 == nil {
			return evs, nil
		}
		return nil, err
	}
	if out.Items != nil {
		return out.Items, nil
	}
	return []domain.Event{}, nil
}

// GetCompanionProjects fetches projects via device endpoint.
func (c *RemoteClient) GetCompanionProjects(ctx context.Context) ([]domain.Project, error) {
	auth, err := c.deviceAuthHeader()
	if err != nil {
		return nil, err
	}
	if auth == "" {
		return nil, ErrNotAuthenticated
	}
	var out struct {
		Items []domain.Project `json:"items"`
	}
	if err := c.doWithAuth(ctx, http.MethodGet, "/api/v1/companion/projects", auth, nil, &out); err != nil {
		var ps []domain.Project
		if err2 := c.doWithAuth(ctx, http.MethodGet, "/api/v1/companion/projects", auth, nil, &ps); err2 == nil {
			return ps, nil
		}
		return nil, err
	}
	if out.Items != nil {
		return out.Items, nil
	}
	return []domain.Project{}, nil
}

// GetCompanionWorkspaces fetches workspaces via device endpoint.
func (c *RemoteClient) GetCompanionWorkspaces(ctx context.Context) ([]domain.Workspace, error) {
	auth, err := c.deviceAuthHeader()
	if err != nil {
		return nil, err
	}
	if auth == "" {
		return nil, ErrNotAuthenticated
	}
	var out struct {
		Items []domain.Workspace `json:"items"`
	}
	if err := c.doWithAuth(ctx, http.MethodGet, "/api/v1/companion/workspaces", auth, nil, &out); err != nil {
		return nil, err
	}
	if out.Items != nil {
		return out.Items, nil
	}
	return []domain.Workspace{}, nil
}

// CreateCompanionWorkspace creates a workspace via device endpoint.
func (c *RemoteClient) CreateCompanionWorkspace(ctx context.Context, projectSlug, title, instructions string) (*domain.Workspace, error) {
	auth, err := c.deviceAuthHeader()
	if err != nil {
		return nil, err
	}
	if auth == "" {
		return nil, ErrNotAuthenticated
	}
	body := map[string]string{
		"project_slug": projectSlug,
		"title":        title,
		"instructions": instructions,
	}
	b, _ := json.Marshal(body)
	var out domain.Workspace
	if err := c.doWithAuth(ctx, http.MethodPost, "/api/v1/companion/workspaces", auth, strings.NewReader(string(b)), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StopCompanionWorkspace stops a workspace via device endpoint.
func (c *RemoteClient) StopCompanionWorkspace(ctx context.Context, id string) error {
	auth, err := c.deviceAuthHeader()
	if err != nil {
		return err
	}
	if auth == "" {
		return ErrNotAuthenticated
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("workspace id required")
	}
	path := "/api/v1/companion/workspaces/" + url.PathEscape(id) + "/stop"
	return c.doWithAuth(ctx, http.MethodPost, path, auth, strings.NewReader("{}"), nil)
}

// GetCompanionEnvironment fetches the tool-environment bundle via device endpoint.
// It returns a map of name->value and ETag for caching (If-None-Match -> 304).
func (c *RemoteClient) GetCompanionEnvironment(ctx context.Context) (map[string]string, string, error) {
	auth, err := c.deviceAuthHeader()
	if err != nil {
		return nil, "", err
	}
	if auth == "" {
		return nil, "", ErrNotAuthenticated
	}
	u := c.baseURL + "/api/v1/companion/environment"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, resp.Header.Get("ETag"), nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, "", ErrNotAuthenticated
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", fmt.Errorf("remote GET /api/v1/companion/environment: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var bundle map[string]string
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(data, &bundle); err != nil {
		// Try envelope {items: ...} or {environment: ...}
		var env struct {
			Items map[string]string `json:"items"`
			Env   map[string]string `json:"environment"`
		}
		if err2 := json.Unmarshal(data, &env); err2 == nil {
			if env.Items != nil {
				bundle = env.Items
			} else if env.Env != nil {
				bundle = env.Env
			}
		}
		if bundle == nil {
			return nil, "", fmt.Errorf("decode companion/environment: %w", err)
		}
	}
	return bundle, resp.Header.Get("ETag"), nil
}

// WatchCompanionEvents streams SSE events from the device-authenticated endpoint.
// It handles context cancellation and parses SSE data payloads as domain.Event.
// Caller should provide since for replay after reconnect; empty means live only.
func (c *RemoteClient) WatchCompanionEvents(ctx context.Context, since domain.ID, out chan<- domain.Event) error {
	auth, err := c.deviceAuthHeader()
	if err != nil {
		return err
	}
	if auth == "" {
		return ErrNotAuthenticated
	}
	u := c.baseURL + "/api/v1/companion/events/stream"
	if since != "" {
		u += "?lastEventId=" + url.QueryEscape(string(since))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "text/event-stream")
	if since != "" {
		req.Header.Set("Last-Event-ID", string(since))
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrNotAuthenticated
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("remote GET /api/v1/companion/events/stream: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return parseRemoteSSE(ctx, resp.Body, out)
}

func parseRemoteSSE(ctx context.Context, r io.Reader, out chan<- domain.Event) error {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	var dataLines []string
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				idx := bytes.Index(buf, []byte("\n"))
				if idx < 0 {
					break
				}
				line := string(bytes.TrimRight(buf[:idx], "\r"))
				buf = buf[idx+1:]
				if line == "" {
					if len(dataLines) > 0 {
						payload := strings.Join(dataLines, "\n")
						dataLines = nil
						var ev domain.Event
						if jerr := json.Unmarshal([]byte(payload), &ev); jerr == nil {
							select {
							case out <- ev:
							case <-ctx.Done():
								return ctx.Err()
							}
						}
					}
					continue
				}
				if strings.HasPrefix(line, "data:") {
					dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}
