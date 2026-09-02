package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/api"
	"github.com/omahab/omahab/internal/domain"
)

// Client is the HTTP/JSON client for omahabd's /api/v1.
// Contract: JSON errors {"error":{"code":"...","message":"..."}},
// Bearer auth required except /up, mutating requests require Content-Type application/json.
type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
	UserAgent  string
}

// New creates a client with the given base URL and token.
// BaseURL may include or omit trailing slash; /api/v1 is appended if missing.
func New(baseURL, token string) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL != "" && !strings.HasSuffix(baseURL, "/api/v1") && !strings.HasSuffix(baseURL, "/api") {
		// Ensure prefix; if caller passed host only, add /api/v1
		if !strings.Contains(baseURL, "/api/") {
			baseURL += "/api/v1"
		}
	}
	// Normalize: if ends with /api, expand to /api/v1
	if strings.HasSuffix(baseURL, "/api") {
		baseURL += "/v1"
	}
	return &Client{
		BaseURL:    baseURL,
		Token:      strings.TrimSpace(token),
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		UserAgent:  "omahab-cli/0.1.0",
	}
}

// ListEnvelope is the generic list wrapper used by the API.
type ListEnvelope[T any] struct {
	Items []T `json:"items"`
}

// --- low level ---

func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	// path must start with /
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	// /up lives at the root (outside /api/v1). Strip the prefix for that one path
	// so New("http://host:8484") + "/up" → "http://host:8484/up" not "/api/v1/up".
	base := c.BaseURL
	if path == "/up" {
		base = strings.TrimSuffix(strings.TrimSuffix(base, "/api/v1"), "/api")
	}
	full := base + path
	var rBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, full, rBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	// Bearer auth required except /up
	if path != "/up" && c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return req, nil
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseAPIError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	// Allow empty body for 204
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	// Limit JSON size to 10MB
	limited := io.LimitReader(resp.Body, 10<<20)
	return json.NewDecoder(limited).Decode(out)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	req, err := c.newRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *Client) put(ctx context.Context, path string, body any, out any) error {
	req, err := c.newRequest(ctx, http.MethodPut, path, body)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *Client) del(ctx context.Context, path string, out any) error {
	req, err := c.newRequest(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

// --- status / up / doctor ---

func (c *Client) Up(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	// /up is unauthenticated per contract: we explicitly strip auth in newRequest for this path
	if err := c.get(ctx, "/up", &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) Status(ctx context.Context) (*domain.Status, error) {
	var out domain.Status
	if err := c.get(ctx, "/status", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DoctorCheck is a single health diagnostic.
type DoctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

type DoctorResult struct {
	Healthy bool          `json:"healthy"`
	Checks  []DoctorCheck `json:"checks"`
}

func (c *Client) Doctor(ctx context.Context) (*DoctorResult, error) {
	var out DoctorResult
	if err := c.get(ctx, "/doctor", &out); err != nil {
		// Fallback: try /health if /doctor missing; map status to checks
		if apiErr, ok := err.(*APIError); ok && apiErr.StatusCode == http.StatusNotFound {
			st, sErr := c.Status(ctx)
			if sErr != nil {
				return nil, err
			}
			return &DoctorResult{
				Healthy: st.Health == domain.HealthHealthy,
				Checks: []DoctorCheck{
					{Name: "api", Status: string(st.Health), Message: fmt.Sprintf("version %s", st.Version)},
				},
			}, nil
		}
		return nil, err
	}
	return &out, nil
}

// --- applications ---

func (c *Client) ListApplications(ctx context.Context) ([]domain.Application, error) {
	var env ListEnvelope[domain.Application]
	if err := c.get(ctx, "/applications", &env); err != nil {
		return nil, err
	}
	return env.Items, nil
}

func (c *Client) GetApplication(ctx context.Context, id string) (*domain.Application, error) {
	var out domain.Application
	if err := c.get(ctx, "/applications/"+url.PathEscape(id), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type AppActionResponse struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

func (c *Client) RestartApplication(ctx context.Context, id string) (*AppActionResponse, error) {
	var out AppActionResponse
	if err := c.post(ctx, "/applications/"+url.PathEscape(id)+"/restart", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- exposure ---

type ExposureRequest struct {
	Target   string `json:"target"`   // "app:<id>" or "project:<id>" or service name
	Exposure string `json:"exposure"` // private|shared|public
	Hostname string `json:"hostname,omitempty"`
}

type ExposureResponse struct {
	Target   string `json:"target"`
	Exposure string `json:"exposure"`
	Hostname string `json:"hostname"`
	Message  string `json:"message,omitempty"`
}

func (c *Client) GetExposure(ctx context.Context, target string) (*ExposureResponse, error) {
	path := "/exposure"
	if target != "" {
		path += "?target=" + url.QueryEscape(target)
	}
	var out ExposureResponse
	if err := c.get(ctx, path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListExposures(ctx context.Context) ([]ExposureResponse, error) {
	var env ListEnvelope[ExposureResponse]
	if err := c.get(ctx, "/exposure", &env); err != nil {
		// fallback: single object
		var single ExposureResponse
		if err2 := c.get(ctx, "/exposures", &env); err2 == nil {
			return env.Items, nil
		}
		_ = single
		return nil, err
	}
	return env.Items, nil
}

func (c *Client) SetExposure(ctx context.Context, req ExposureRequest) (*ExposureResponse, error) {
	var out ExposureResponse
	if err := c.put(ctx, "/exposure", req, &out); err != nil {
		// try POST fallback
		if apiErr, ok := err.(*APIError); ok && apiErr.StatusCode == http.StatusNotFound {
			if err2 := c.post(ctx, "/exposure", req, &out); err2 == nil {
				return &out, nil
			}
		}
		return nil, err
	}
	return &out, nil
}

// Application-specific exposure helpers (for CLI ergonomics)

func (c *Client) GetAppExposure(ctx context.Context, appID string) (*ExposureResponse, error) {
	var out ExposureResponse
	if err := c.get(ctx, "/applications/"+url.PathEscape(appID)+"/exposure", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) SetAppExposure(ctx context.Context, appID, exposure string) (*ExposureResponse, error) {
	var out ExposureResponse
	if err := c.put(ctx, "/applications/"+url.PathEscape(appID)+"/exposure", map[string]string{"exposure": exposure}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetProjectExposure(ctx context.Context, projectID string) (*ExposureResponse, error) {
	var out ExposureResponse
	if err := c.get(ctx, "/projects/"+url.PathEscape(projectID)+"/exposure", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) SetProjectExposure(ctx context.Context, projectID, exposure string) (*ExposureResponse, error) {
	var out ExposureResponse
	if err := c.put(ctx, "/projects/"+url.PathEscape(projectID)+"/exposure", map[string]string{"exposure": exposure}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- projects & releases ---

type CreateProjectRequest struct {
	Name string `json:"name"`
	Slug string `json:"slug,omitempty"`
}

func (c *Client) ListProjects(ctx context.Context) ([]domain.Project, error) {
	var env ListEnvelope[domain.Project]
	if err := c.get(ctx, "/projects", &env); err != nil {
		return nil, err
	}
	return env.Items, nil
}

func (c *Client) GetProject(ctx context.Context, idOrSlug string) (*domain.Project, error) {
	var out domain.Project
	if err := c.get(ctx, "/projects/"+url.PathEscape(idOrSlug), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreateProject(ctx context.Context, req CreateProjectRequest) (*domain.Project, error) {
	var out domain.Project
	if err := c.post(ctx, "/projects", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) DeleteProject(ctx context.Context, id string) error {
	return c.del(ctx, "/projects/"+url.PathEscape(id), nil)
}

func (c *Client) ListReleases(ctx context.Context, projectID string) ([]domain.Release, error) {
	var env ListEnvelope[domain.Release]
	if err := c.get(ctx, "/projects/"+url.PathEscape(projectID)+"/releases", &env); err != nil {
		return nil, err
	}
	return env.Items, nil
}

func (c *Client) GetRelease(ctx context.Context, projectID, releaseID string) (*domain.Release, error) {
	var out domain.Release
	if err := c.get(ctx, "/projects/"+url.PathEscape(projectID)+"/releases/"+url.PathEscape(releaseID), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type CreateReleaseRequest struct {
	Commit string `json:"commit"`
	Digest string `json:"digest"`
}

func (c *Client) CreateRelease(ctx context.Context, projectID string, req CreateReleaseRequest) (*domain.Release, error) {
	var out domain.Release
	if err := c.post(ctx, "/projects/"+url.PathEscape(projectID)+"/releases", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) RollbackRelease(ctx context.Context, projectID, releaseID string) (*domain.Release, error) {
	var out domain.Release
	if err := c.post(ctx, "/projects/"+url.PathEscape(projectID)+"/releases/"+url.PathEscape(releaseID)+"/rollback", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- secrets metadata ---

func (c *Client) ListSecrets(ctx context.Context) ([]domain.Secret, error) {
	var env ListEnvelope[domain.Secret]
	if err := c.get(ctx, "/secrets", &env); err != nil {
		return nil, err
	}
	return env.Items, nil
}

func (c *Client) GetSecret(ctx context.Context, id string) (*domain.Secret, error) {
	var out domain.Secret
	if err := c.get(ctx, "/secrets/"+url.PathEscape(id), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- backups ---

type CreateBackupRequest struct {
	Repository string `json:"repository,omitempty"`
}

func (c *Client) ListBackups(ctx context.Context) ([]domain.Backup, error) {
	var env ListEnvelope[domain.Backup]
	if err := c.get(ctx, "/backups", &env); err != nil {
		return nil, err
	}
	return env.Items, nil
}

func (c *Client) GetBackup(ctx context.Context, id string) (*domain.Backup, error) {
	var out domain.Backup
	if err := c.get(ctx, "/backups/"+url.PathEscape(id), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreateBackup(ctx context.Context, req CreateBackupRequest) (*domain.Backup, error) {
	var out domain.Backup
	if err := c.post(ctx, "/backups", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) RestoreBackup(ctx context.Context, id string) (*domain.Backup, error) {
	var out domain.Backup
	if err := c.post(ctx, "/backups/"+url.PathEscape(id)+"/restore", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) VerifyBackup(ctx context.Context, id string) (*domain.Backup, error) {
	var out domain.Backup
	if err := c.post(ctx, "/backups/"+url.PathEscape(id)+"/verify", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) VerifyLatestBackup(ctx context.Context) (*domain.Backup, error) {
	var out domain.Backup
	if err := c.post(ctx, "/backups/verify", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- events ---

type EventListParams struct {
	Limit      int
	UnreadOnly bool
	Type       string
}

func (c *Client) ListEvents(ctx context.Context, params EventListParams) ([]domain.Event, error) {
	q := url.Values{}
	if params.Limit > 0 {
		q.Set("limit", fmt.Sprint(params.Limit))
	}
	if params.UnreadOnly {
		q.Set("unread", "true")
	}
	if params.Type != "" {
		q.Set("type", params.Type)
	}
	path := "/events"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var env ListEnvelope[domain.Event]
	if err := c.get(ctx, path, &env); err != nil {
		return nil, err
	}
	return env.Items, nil
}

func (c *Client) GetEvent(ctx context.Context, id string) (*domain.Event, error) {
	var out domain.Event
	if err := c.get(ctx, "/events/"+url.PathEscape(id), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) AckEvent(ctx context.Context, id string) error {
	return c.post(ctx, "/events/"+url.PathEscape(id)+"/ack", map[string]any{}, nil)
}

// WatchEvents streams SSE events. Caller handles context cancellation.
// Each SSE data payload is a domain.Event JSON object.
func (c *Client) WatchEvents(ctx context.Context, out chan<- domain.Event) error {
	req, err := c.newRequest(ctx, http.MethodGet, "/events/stream", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return parseAPIError(resp)
	}
	// Minimal SSE parser: look for "data: " lines
	return parseSSE(ctx, resp.Body, out)
}

func parseSSE(ctx context.Context, r io.Reader, out chan<- domain.Event) error {
	// Very small SSE parser: accumulate lines until empty line
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
			// Process lines
			for {
				idx := bytes.Index(buf, []byte("\n"))
				if idx < 0 {
					break
				}
				line := string(bytes.TrimRight(buf[:idx], "\r"))
				buf = buf[idx+1:]
				if line == "" {
					// Dispatch
					if len(dataLines) > 0 {
						payload := strings.Join(dataLines, "\n")
						dataLines = nil
						// Try to decode as Event; if fails, wrap as generic event with message
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
			// Check for context cancellation
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}

// --- sync folders ---

type CreateSyncFolderRequest struct {
	Name        string `json:"name"`
	ServerPath  string `json:"server_path"`
	ShareWithAI bool   `json:"share_with_ai"`
}

func (c *Client) ListSyncFolders(ctx context.Context) ([]domain.SyncFolder, error) {
	var env ListEnvelope[domain.SyncFolder]
	if err := c.get(ctx, "/sync/folders", &env); err != nil {
		return nil, err
	}
	return env.Items, nil
}

func (c *Client) GetSyncFolder(ctx context.Context, id string) (*domain.SyncFolder, error) {
	var out domain.SyncFolder
	if err := c.get(ctx, "/sync/folders/"+url.PathEscape(id), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreateSyncFolder(ctx context.Context, req CreateSyncFolderRequest) (*domain.SyncFolder, error) {
	var out domain.SyncFolder
	if err := c.post(ctx, "/sync/folders", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) DeleteSyncFolder(ctx context.Context, id string) error {
	return c.del(ctx, "/sync/folders/"+url.PathEscape(id), nil)
}

// --- workspaces / runners (aliases) ---

type CreateWorkspaceRequest struct {
	ProjectID string `json:"project_id"`
	Branch    string `json:"branch,omitempty"`
	Agent     string `json:"agent,omitempty"`
}

func (c *Client) ListWorkspaces(ctx context.Context) ([]domain.Workspace, error) {
	var env ListEnvelope[domain.Workspace]
	// Try /workspaces then /runners fallback
	if err := c.get(ctx, "/workspaces", &env); err != nil {
		var env2 ListEnvelope[domain.Workspace]
		if err2 := c.get(ctx, "/runners", &env2); err2 == nil {
			return env2.Items, nil
		}
		return nil, err
	}
	return env.Items, nil
}

func (c *Client) GetWorkspace(ctx context.Context, id string) (*domain.Workspace, error) {
	var out domain.Workspace
	if err := c.get(ctx, "/workspaces/"+url.PathEscape(id), &out); err != nil {
		if apiErr, ok := err.(*APIError); ok && apiErr.StatusCode == http.StatusNotFound {
			// try runner alias
			var out2 domain.Workspace
			if err2 := c.get(ctx, "/runners/"+url.PathEscape(id), &out2); err2 == nil {
				return &out2, nil
			}
		}
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreateWorkspace(ctx context.Context, req CreateWorkspaceRequest) (*domain.Workspace, error) {
	var out domain.Workspace
	if err := c.post(ctx, "/workspaces", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Backward compat alias
func (c *Client) CreateRunner(ctx context.Context, req CreateWorkspaceRequest) (*domain.Workspace, error) {
	return c.CreateWorkspace(ctx, req)
}

func (c *Client) StopWorkspace(ctx context.Context, id string) error {
	// Try POST /workspaces/{id}/stop then DELETE
	if err := c.post(ctx, "/workspaces/"+url.PathEscape(id)+"/stop", map[string]any{}, nil); err == nil {
		return nil
	}
	return c.del(ctx, "/workspaces/"+url.PathEscape(id), nil)
}

func (c *Client) DeleteWorkspace(ctx context.Context, id string) error {
	return c.del(ctx, "/workspaces/"+url.PathEscape(id), nil)
}

// --- identity recovery ---

type RecoverRequest struct {
	Email string `json:"email"`
}

type RecoverResponse struct {
	Email     string  `json:"email"`
	LoginURL  *string `json:"login_url,omitempty"`
	Code      *string `json:"code,omitempty"`
	ExpiresAt *string `json:"expires_at,omitempty"`
	Message   string  `json:"message,omitempty"`
}

func (c *Client) RecoverIdentity(ctx context.Context, email string) (*RecoverResponse, error) {
	var out RecoverResponse
	if err := c.post(ctx, "/identity/recover", RecoverRequest{Email: email}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- email ingestion (client view) ---

func (c *Client) ListEmails(ctx context.Context) ([]domain.EmailMessage, error) {
	var env ListEnvelope[domain.EmailMessage]
	if err := c.get(ctx, "/emails", &env); err != nil {
		// fallback to /email/messages
		if err2 := c.get(ctx, "/email/messages", &env); err2 == nil {
			return env.Items, nil
		}
		return nil, err
	}
	return env.Items, nil
}
func (c *Client) GetEmail(ctx context.Context, id string) (*domain.EmailMessage, error) {
	var out domain.EmailMessage
	if err := c.get(ctx, "/emails/"+url.PathEscape(id), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- provider OAuth (subscription) ---

// StartProviderOAuth initiates subscription OAuth for chatgpt (device_code) or xai (loopback).
// Returns verification URL and session metadata; never returns device codes, tokens, or master key.
func (c *Client) StartProviderOAuth(ctx context.Context, provider, flow string) (*api.OAuthSession, error) {
	var out api.OAuthSession
	if err := c.post(ctx, "/provider-oauth/"+url.PathEscape(provider)+"/start", api.StartProviderOAuthRequest{Flow: flow}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PollProviderOAuth polls OAuth session status; returns safe OAuthSession without device codes, tokens, or master key.
func (c *Client) PollProviderOAuth(ctx context.Context, provider, sessionID string) (*api.OAuthSession, error) {
	var out api.OAuthSession
	if err := c.get(ctx, "/provider-oauth/"+url.PathEscape(provider)+"/poll/"+url.PathEscape(sessionID), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ForwardProviderOAuthCallback forwards only the received /callback?<query> path to LiteLLM's fixed loopback at 127.0.0.1:56121.
// Requires device token from an enrolled companion with allow_provider_oauth=true; admin bearer is rejected with 403.
// This is device-only; for CLI non-QML use the same relay is attempted, but fallback is SSH local forward: ssh -L 56121:127.0.0.1:56121 omahab@<server>.
func (c *Client) ForwardProviderOAuthCallback(ctx context.Context, provider, sessionID, callbackPath string) (*api.OAuthSession, error) {
	var out api.OAuthSession
	if err := c.post(ctx, "/provider-oauth/"+url.PathEscape(provider)+"/callback/"+url.PathEscape(sessionID), api.ForwardProviderOAuthCallbackRequest{CallbackPath: callbackPath}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

