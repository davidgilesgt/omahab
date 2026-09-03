package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/health"
	"github.com/omahab/omahab/internal/store"
)

const defaultPodmanSocketPath = "/run/omahab-builder/podman.sock"

func (b *Backend) podmanSocketPathOrDefault() string {
	if strings.TrimSpace(b.podmanSocketPath) != "" {
		return strings.TrimSpace(b.podmanSocketPath)
	}
	return defaultPodmanSocketPath
}

func (b *Backend) woodpeckerHTTPClientOrDefault() *http.Client {
	if b.woodpeckerHTTPClient != nil {
		return b.woodpeckerHTTPClient
	}
	return &http.Client{Timeout: 5 * time.Second}
}

func (b *Backend) woodpeckerBaseURLForCheck(ctx context.Context, domain string) string {
	if strings.TrimSpace(b.woodpeckerBaseURLOverride) != "" {
		return strings.TrimRight(strings.TrimSpace(b.woodpeckerBaseURLOverride), "/")
	}
	if b.secrets != nil {
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "woodpecker_base_url"); err == nil && strings.TrimSpace(v) != "" {
			return strings.TrimRight(strings.TrimSpace(v), "/")
		}
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "woodpecker_url"); err == nil && strings.TrimSpace(v) != "" {
			return strings.TrimRight(strings.TrimSpace(v), "/")
		}
	}
	if v := strings.TrimSpace(os.Getenv("OMAHAB_WOODPECKER_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	if v := strings.TrimSpace(os.Getenv("WOODPECKER_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	if domain != "" && domain != "example.com" && domain != "not-configured.invalid" {
		return "https://ci." + domain
	}
	return ""
}

// woodpeckerConnectionCheck implements the operator-owned woodpecker_connection setup check.
// It remains pending until PAT validates, user is admin, podman socket answers _ping,
// and Woodpecker reports a connected local agent.
// Failed probes return failed with redacted detail, never exposing the token.
func (b *Backend) woodpeckerConnectionCheck(ctx context.Context) apitypes.SetupCheck {
	c := apitypes.SetupCheck{ID: "woodpecker_connection"}

	if b.store == nil {
		c.Status = "pending"
		c.Detail = "store not configured"
		return c
	}
	if b.secrets == nil {
		c.Status = "pending"
		c.Detail = "secrets service not configured"
		return c
	}

	// Instance needed for domain derivation.
	inst, instErr := b.store.Instance(ctx)
	domain := ""
	if instErr == nil {
		domain = strings.TrimSpace(inst.Domain)
	}

	// Reveal secrets. If missing, remains pending awaiting operator handoff.
	token, tokenErr := b.secrets.RevealByName(ctx, "platform-app", "woodpecker_token")
	token = strings.TrimSpace(token)
	username, userErr := b.secrets.RevealByName(ctx, "platform-app", "woodpecker_admin_username")
	username = strings.TrimSpace(username)

	missingToken := tokenErr != nil || token == ""
	missingUser := userErr != nil || username == ""

	// If either not found, treat as pending. Use store.IsNotFound to distinguish
	// but any error with empty value is pending (operator has not submitted).
	if missingToken || missingUser {
		// If error is not NotFound, still surface as pending unless it's a DB error we can detect?
		// For DB errors that are not NotFound, still report pending to avoid leaking internals.
		c.Status = "pending"
		c.Detail = "woodpecker not connected: submit PAT via setup"
		return c
	}

	// Derive Woodpecker base URL.
	baseURL := b.woodpeckerBaseURLForCheck(ctx, domain)
	if baseURL == "" {
		c.Status = "pending"
		c.Detail = "domain not configured; cannot verify woodpecker"
		return c
	}

	// Probe Woodpecker user (PAT validation + admin)
	login, isAdmin, err := b.probeWoodpeckerUser(ctx, baseURL, token)
	if err != nil {
		c.Status = "failed"
		c.Detail = health.RedactDetail(err.Error())
		return c
	}
	if login != username {
		c.Status = "failed"
		c.Detail = health.RedactDetail(fmt.Sprintf("woodpecker token username mismatch: have %s want %s", login, username))
		return c
	}
	if !isAdmin {
		c.Status = "failed"
		c.Detail = health.RedactDetail("woodpecker user is not admin")
		return c
	}

	// Probe podman socket
	if err := b.probePodmanSocket(ctx); err != nil {
		c.Status = "failed"
		c.Detail = health.RedactDetail("podman socket not ready: " + err.Error())
		return c
	}

	// Probe agents
	connected, err := b.probeWoodpeckerAgents(ctx, baseURL, token)
	if err != nil {
		c.Status = "failed"
		c.Detail = health.RedactDetail("woodpecker agent probe failed: " + err.Error())
		return c
	}
	if !connected {
		c.Status = "failed"
		c.Detail = health.RedactDetail("no connected woodpecker agent")
		return c
	}

	c.Status = "ok"
	c.Detail = "woodpecker connected"
	return c
}

func (b *Backend) probeWoodpeckerUser(ctx context.Context, baseURL, token string) (string, bool, error) {
	client := b.woodpeckerHTTPClientOrDefault()
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	url := strings.TrimRight(baseURL, "/") + "/api/user"
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return "", false, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("woodpecker api user request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", false, fmt.Errorf("woodpecker token rejected: status %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("woodpecker api user status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Login string `json:"login"`
		Admin bool   `json:"admin"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", false, fmt.Errorf("woodpecker user parse error: %w", err)
	}
	login := strings.TrimSpace(out.Login)
	if login == "" {
		return "", false, fmt.Errorf("woodpecker user response missing login")
	}
	return login, out.Admin, nil
}

func (b *Backend) probeWoodpeckerAgents(ctx context.Context, baseURL, token string) (bool, error) {
	client := b.woodpeckerHTTPClientOrDefault()
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	url := strings.TrimRight(baseURL, "/") + "/api/agents"
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("agents status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var agents []struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		LastContact int64  `json:"last_contact"`
		LastWork    int64  `json:"last_work"`
		Created     int64  `json:"created"`
		Updated     int64  `json:"updated"`
	}
	if err := json.Unmarshal(body, &agents); err != nil {
		return false, fmt.Errorf("agents parse error: %w", err)
	}
	if len(agents) == 0 {
		return false, nil
	}
	now := time.Now().Unix()
	for _, a := range agents {
		if a.LastContact == 0 {
			// If no timestamp, presence implies ok (older server may not populate)
			return true, nil
		}
		if now-a.LastContact < 600 {
			return true, nil
		}
	}
	return false, nil
}

func (b *Backend) probePodmanSocket(ctx context.Context) error {
	path := b.podmanSocketPathOrDefault()
	if strings.TrimSpace(path) == "" {
		path = defaultPodmanSocketPath
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("socket %s not found", path)
		}
		// Check for store not found? not relevant
		if strings.Contains(err.Error(), "no such file") {
			return fmt.Errorf("socket %s not found", path)
		}
		return fmt.Errorf("socket stat error: %w", err)
	}
	endpoints := []string{"http://d/_ping", "http://d/v1.40/_ping", "http://d/v4.0.0/libpod/_ping"}
	var lastErr string
	for _, ep := range endpoints {
		if err := b.pingPodmanEndpoint(ctx, path, ep); err == nil {
			return nil
		} else {
			lastErr = err.Error()
			// If context canceled, return immediately
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Check if error is due to file not found already handled
			if strings.Contains(lastErr, "not found") {
				// continue to try next endpoint? still fail
			}
		}
	}
	if lastErr == "" {
		lastErr = "no endpoint succeeded"
	}
	return fmt.Errorf("podman _ping failed: %s", lastErr)
}

func (b *Backend) pingPodmanEndpoint(ctx context.Context, socketPath, url string) error {
	tr := &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 2 * time.Second}
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	// Ensure we close idle connections after
	defer tr.CloseIdleConnections()

	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		// Wrap for redaction safety? health.Redact will handle later
		if errors.Is(err, store.ErrNotFound) {
			return err
		}
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	bstr := strings.TrimSpace(string(body))
	// Docker/Podman _ping returns "OK"
	if bstr == "" {
		// Empty 200 is still considered OK for some versions
		return nil
	}
	if strings.Contains(bstr, "OK") {
		return nil
	}
	// Some podman versions return JSON? Accept any 200 as success
	return nil
}

var _ = store.ErrNotFound
func (b *Backend) SetupWoodpecker(ctx context.Context, req apitypes.SetupWoodpeckerRequest) error {
	username := strings.TrimSpace(req.Username)
	token := strings.TrimSpace(req.Token)
	if username == "" || token == "" {
		return fmt.Errorf("%w: username and token are required", apitypes.ErrValidation)
	}
	if len(username) < 1 || len(username) > 255 {
		return fmt.Errorf("%w: username must be 1-255 characters", apitypes.ErrValidation)
	}
	if len(token) < 1 || len(token) > 4096 {
		return fmt.Errorf("%w: token must be 1-4096 characters", apitypes.ErrValidation)
	}
	if b.secrets == nil || b.store == nil {
		return fmt.Errorf("%w: secrets or store not configured", apitypes.ErrValidation)
	}
	inst, _ := b.store.Instance(ctx)
	domain := ""
	if inst.Domain != "" {
		domain = strings.TrimSpace(inst.Domain)
	}
	baseURL := b.woodpeckerBaseURLForCheck(ctx, domain)
	if baseURL == "" {
		return fmt.Errorf("%w: woodpecker url not configured; domain missing", apitypes.ErrValidation)
	}
	// Initial validation: token must be valid and login must match username. Do not check admin yet;
	// admin is granted via WOODPECKER_ADMIN_USERNAME after redeploy.
	login, _, err := b.probeWoodpeckerUser(ctx, baseURL, token)
	if err != nil {
		return fmt.Errorf("%w: %s", apitypes.ErrValidation, health.RedactDetail(err.Error()))
	}
	if login != username {
		return fmt.Errorf("%w: woodpecker username mismatch: token belongs to different user", apitypes.ErrValidation)
	}
	// Persist encrypted secrets. Never log token.
	if err := upsertSecret(ctx, b.secrets, "platform-app", "woodpecker_token", token); err != nil {
		return fmt.Errorf("store woodpecker_token: %w", err)
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "woodpecker_admin_username", username); err != nil {
		return fmt.Errorf("store woodpecker_admin_username: %w", err)
	}
	// Redeploy Woodpecker so WOODPECKER_ADMIN_USERNAME takes effect.
	if b.apps != nil {
		if err := b.redeployBundle(ctx, "woodpecker"); err != nil {
			return fmt.Errorf("%w: woodpecker redeploy failed: %s", apitypes.ErrValidation, health.RedactDetail(err.Error()))
		}
	}
	// Revalidate that the user is now admin, with brief retry for container restart.
	var finalLogin string
	var finalAdmin bool
	var finalErr error
	deadline := time.Now().Add(30 * time.Second)
	for {
		l, a, e := b.probeWoodpeckerUser(ctx, baseURL, token)
		if e == nil {
			finalLogin = l
			finalAdmin = a
			finalErr = nil
			if l == username && a {
				break
			}
			if l != username {
				finalErr = fmt.Errorf("woodpecker username mismatch after redeploy")
				break
			}
			if !a {
				finalErr = fmt.Errorf("woodpecker user is not admin")
			}
		} else {
			finalErr = e
		}
		if time.Now().After(deadline) {
			if finalErr == nil {
				finalErr = fmt.Errorf("woodpecker admin not verified: admin=%v", finalAdmin)
			}
			break
		}
		if finalErr != nil && strings.Contains(finalErr.Error(), "mismatch") {
			break
		}
		if finalLogin == username && finalAdmin {
			finalErr = nil
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
	if finalErr != nil {
		return fmt.Errorf("%w: %s", apitypes.ErrValidation, health.RedactDetail(finalErr.Error()))
	}
	if finalLogin != username || !finalAdmin {
		return fmt.Errorf("%w: woodpecker admin verification failed", apitypes.ErrValidation)
	}
	if err := b.bindSCM(ctx); err != nil {
		return fmt.Errorf("bind scm after woodpecker setup: %w", err)
	}
	return nil
}


