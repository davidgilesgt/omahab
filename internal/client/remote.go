package client

import (
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

// Sentinel errors for callers that need typed distinctions.
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
