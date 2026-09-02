package scm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ForgejoConfig holds configuration for the Forgejo HTTP client.
type ForgejoConfig struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
	SecretStore SecretStore
}

type forgejoClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
	secrets    SecretStore
}

func NewForgejoClient(cfg ForgejoConfig) ForgejoClient {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	base = strings.TrimSuffix(base, "/api/v1")
	hc := cfg.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	return &forgejoClient{baseURL: base, token: cfg.Token, httpClient: hc, secrets: cfg.SecretStore}
}

func (c *forgejoClient) do(ctx context.Context, method, path string, reqBody any, respBody any) error {
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	u := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "token "+c.token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.mapError(resp.StatusCode, data)
	}
	if respBody != nil && len(data) > 0 {
		if err := json.Unmarshal(data, respBody); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func (c *forgejoClient) mapError(code int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(body) > 0 {
		var m struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &m) == nil && strings.TrimSpace(m.Message) != "" {
			msg = m.Message
		}
	}
	if msg == "" {
		msg = http.StatusText(code)
	}
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: forgejo unauthorized (%d): %s", ErrValidation, code, msg)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, msg)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrConflict, msg)
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: %s", ErrValidation, msg)
	default:
		if code >= 400 && code < 500 {
			return fmt.Errorf("%w: forgejo client error %d: %s", ErrValidation, code, msg)
		}
		return fmt.Errorf("forgejo server error %d: %s", code, msg)
	}
}

func isNotFoundErr(err error) bool { return errors.Is(err, ErrNotFound) }

type forgejoRepoJSON struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Description   string `json:"description"`
	Private       bool   `json:"private"`
	CloneURL      string `json:"clone_url"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
	HasActions    bool   `json:"has_actions"`
	Empty         bool   `json:"empty"`
	Owner         *struct {
		Login string `json:"login"`
	} `json:"owner"`
}

func (j *forgejoRepoJSON) toRepo() *Repo {
	owner := ""
	if j.Owner != nil {
		owner = j.Owner.Login
	}
	if owner == "" && j.FullName != "" {
		if idx := strings.Index(j.FullName, "/"); idx > 0 {
			owner = j.FullName[:idx]
		}
	}
	return &Repo{Owner: owner, Name: j.Name, CloneURL: j.CloneURL, Private: j.Private, DefaultBranch: j.DefaultBranch, ActionsEnabled: j.HasActions, RemoteID: j.ID}
}

func (c *forgejoClient) createRepoAt(ctx context.Context, path string, payload any) (*Repo, error) {
	var out forgejoRepoJSON
	if err := c.do(ctx, http.MethodPost, path, payload, &out); err != nil {
		return nil, err
	}
	r := out.toRepo()
	if r.DefaultBranch == "" {
		r.DefaultBranch = "master"
	}
	return r, nil
}

func (c *forgejoClient) CreateRepo(ctx context.Context, in CreateRepoInput) (*Repo, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("%w: repo name required", ErrValidation)
	}
	payload := map[string]any{"name": in.Name, "description": in.Description, "private": in.Private, "auto_init": false}
	if in.DefaultBranch != "" {
		payload["default_branch"] = in.DefaultBranch
	}
	owner := strings.TrimSpace(in.Owner)
	if owner != "" {
		orgPath := "/api/v1/orgs/" + url.PathEscape(owner) + "/repos"
		repo, err := c.createRepoAt(ctx, orgPath, payload)
		if err == nil {
			return repo, nil
		}
		if !isNotFoundErr(err) {
			return nil, err
		}
	}
	repo, err := c.createRepoAt(ctx, "/api/v1/user/repos", payload)
	if err != nil {
		return nil, err
	}
	if owner != "" && repo.Owner == "" {
		repo.Owner = owner
	}
	return repo, nil
}

func (c *forgejoClient) GetRepo(ctx context.Context, ref RepoRef) (*Repo, error) {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return nil, fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s", url.PathEscape(ref.Owner), url.PathEscape(ref.Name))
	var out forgejoRepoJSON
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	r := out.toRepo()
	if r.DefaultBranch == "" {
		r.DefaultBranch = "master"
	}
	return r, nil
}

func (c *forgejoClient) DeleteRepo(ctx context.Context, ref RepoRef) error {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s", url.PathEscape(ref.Owner), url.PathEscape(ref.Name))
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

func (c *forgejoClient) SetActionsEnabled(ctx context.Context, ref RepoRef, enabled bool) error {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s", url.PathEscape(ref.Owner), url.PathEscape(ref.Name))
	body := map[string]any{"has_actions": enabled}
	return c.do(ctx, http.MethodPatch, path, body, nil)
}

type createPushMirrorOption struct {
	RemoteAddress  string `json:"remote_address"`
	RemoteUsername string `json:"remote_username,omitempty"`
	RemotePassword string `json:"remote_password,omitempty"`
	Interval       string `json:"interval,omitempty"`
	SyncOnCommit   bool   `json:"sync_on_commit"`
}

func (c *forgejoClient) PutPushMirror(ctx context.Context, ref RepoRef, in MirrorInput) error {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	if strings.TrimSpace(in.RemoteURL) == "" {
		return fmt.Errorf("%w: remote url required", ErrValidation)
	}
	raw := in.CredentialSecretRef
	if c.secrets != nil && in.CredentialSecretRef != "" {
		if idx := strings.LastIndex(in.CredentialSecretRef, "/"); idx > 0 {
			scope := in.CredentialSecretRef[:idx]
			name := in.CredentialSecretRef[idx+1:]
			if v, err := c.secrets.Get(ctx, scope, name); err == nil {
				raw = v
			}
		}
	}
	remoteUsername := ""
	remotePassword := raw
	if strings.Contains(raw, ":") {
		parts := strings.SplitN(raw, ":", 2)
		remoteUsername = parts[0]
		remotePassword = parts[1]
	}
	interval := ""
	if in.IntervalSeconds > 0 {
		interval = fmt.Sprintf("%ds", in.IntervalSeconds)
	}
	payload := createPushMirrorOption{RemoteAddress: in.RemoteURL, RemoteUsername: remoteUsername, RemotePassword: remotePassword, Interval: interval, SyncOnCommit: true}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/push_mirrors", url.PathEscape(ref.Owner), url.PathEscape(ref.Name))
	err := c.do(ctx, http.MethodPost, path, payload, nil)
	if err != nil && errors.Is(err, ErrConflict) {
		_ = c.DeletePushMirror(ctx, ref, in.RemoteName)
		err = c.do(ctx, http.MethodPost, path, payload, nil)
	}
	return err
}

func (c *forgejoClient) DeletePushMirror(ctx context.Context, ref RepoRef, remoteName string) error {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	if strings.TrimSpace(remoteName) == "" {
		return fmt.Errorf("%w: remote name required", ErrValidation)
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/push_mirrors/%s", url.PathEscape(ref.Owner), url.PathEscape(ref.Name), url.PathEscape(remoteName))
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

func (c *forgejoClient) PutFile(ctx context.Context, ref RepoRef, filePath string, content []byte, message string) error {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	if strings.TrimSpace(filePath) == "" {
		return fmt.Errorf("%w: file path required", ErrValidation)
	}
	if strings.TrimSpace(message) == "" {
		message = "Add " + filePath
	}
	encoded := base64.StdEncoding.EncodeToString(content)
	body := map[string]any{"content": encoded, "message": message}
	parts := strings.Split(strings.TrimPrefix(filePath, "/"), "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	encPath := strings.Join(parts, "/")
	path := fmt.Sprintf("/api/v1/repos/%s/%s/contents/%s", url.PathEscape(ref.Owner), url.PathEscape(ref.Name), encPath)
	return c.do(ctx, http.MethodPost, path, body, nil)
}

func (c *forgejoClient) SeedWoodpeckerConfig(ctx context.Context, ref RepoRef, content string) error {
	return c.PutFile(ctx, ref, ".woodpecker.yaml", []byte(content), "Add .woodpecker.yaml (managed by Omahab)")
}
func (c *forgejoClient) CreateUser(ctx context.Context, username, email string) error {
	if strings.TrimSpace(username) == "" {
		return fmt.Errorf("%w: username required", ErrValidation)
	}
	if strings.TrimSpace(email) == "" {
		email = username + "@users.noreply.example.com"
	}
	// Use admin endpoint to create restricted user with random password.
	body := map[string]any{
		"source_id":            0,
		"login_name":           username,
		"username":             username,
		"email":                email,
		"password":             fmt.Sprintf("rand-%d", timeNowUnixNano()),
		"must_change_password": false,
		"restricted":           true,
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/admin/users", body, nil)
	if err != nil && isNotFoundErr(err) {
		// fallback if admin endpoint not available, try generic user creation?
		return err
	}
	if err != nil && (errors.Is(err, ErrConflict) || strings.Contains(strings.ToLower(err.Error()), "already exists")) {
		return nil
	}
	return err
}

func timeNowUnixNano() int64 {
	return time.Now().UnixNano()
}


func (c *forgejoClient) AddCollaborator(ctx context.Context, ref RepoRef, username, permission string) error {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	if strings.TrimSpace(username) == "" {
		return fmt.Errorf("%w: username required", ErrValidation)
	}
	if permission == "" {
		permission = "write"
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/collaborators/%s", url.PathEscape(ref.Owner), url.PathEscape(ref.Name), url.PathEscape(username))
	body := map[string]any{"permission": permission}
	return c.do(ctx, http.MethodPut, path, body, nil)
}

func (c *forgejoClient) CreateToken(ctx context.Context, username, tokenName string, scopes []string) (string, error) {
	if strings.TrimSpace(username) == "" || strings.TrimSpace(tokenName) == "" {
		return "", fmt.Errorf("%w: username and token name required", ErrValidation)
	}
	path := fmt.Sprintf("/api/v1/users/%s/tokens", url.PathEscape(username))
	body := map[string]any{"name": tokenName, "scopes": scopes}
	var out struct {
		Sha1  string `json:"sha1"`
		Token string `json:"token"`
		Name  string `json:"name"`
	}
	if err := c.do(ctx, http.MethodPost, path, body, &out); err != nil {
		// if already exists, try to get existing token list and return first matching
		if errors.Is(err, ErrConflict) || strings.Contains(strings.ToLower(err.Error()), "already exists") {
			// list tokens
			var list []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
				Sha1 string `json:"sha1"`
			}
			lpath := fmt.Sprintf("/api/v1/users/%s/tokens", url.PathEscape(username))
			if lerr := c.do(ctx, http.MethodGet, lpath, nil, &list); lerr == nil {
				for _, t := range list {
					if t.Name == tokenName && t.Sha1 != "" {
						return t.Sha1, nil
					}
				}
			}
		}
		return "", err
	}
	if out.Token != "" {
		return out.Token, nil
	}
	if out.Sha1 != "" {
		return out.Sha1, nil
	}
	return "", fmt.Errorf("forgejo token creation returned empty token")
}

func (c *forgejoClient) DeleteUser(ctx context.Context, username string) error {
	if strings.TrimSpace(username) == "" {
		return fmt.Errorf("%w: username required", ErrValidation)
	}
	path := fmt.Sprintf("/api/v1/admin/users/%s", url.PathEscape(username))
	err := c.do(ctx, http.MethodDelete, path, nil, nil)
	if err != nil && isNotFoundErr(err) {
		return nil
	}
	return err
}

func (c *forgejoClient) DeleteToken(ctx context.Context, username, tokenName string) error {
	if strings.TrimSpace(username) == "" || strings.TrimSpace(tokenName) == "" {
		return fmt.Errorf("%w: username and token name required", ErrValidation)
	}
	// List tokens to find ID by name
	var list []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	lpath := fmt.Sprintf("/api/v1/users/%s/tokens", url.PathEscape(username))
	if err := c.do(ctx, http.MethodGet, lpath, nil, &list); err != nil {
		if isNotFoundErr(err) {
			return nil
		}
		return err
	}
	for _, t := range list {
		if t.Name == tokenName {
			dpath := fmt.Sprintf("/api/v1/users/%s/tokens/%d", url.PathEscape(username), t.ID)
			err := c.do(ctx, http.MethodDelete, dpath, nil, nil)
			if err != nil && isNotFoundErr(err) {
				return nil
			}
			return err
		}
	}
	return nil
}

func (c *forgejoClient) GetUser(ctx context.Context, username string) (bool, error) {
	if strings.TrimSpace(username) == "" {
		return false, fmt.Errorf("%w: username required", ErrValidation)
	}
	path := fmt.Sprintf("/api/v1/users/%s", url.PathEscape(username))
	err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		if isNotFoundErr(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (c *forgejoClient) RemoveCollaborator(ctx context.Context, ref RepoRef, username string) error {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	if strings.TrimSpace(username) == "" {
		return fmt.Errorf("%w: username required", ErrValidation)
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/collaborators/%s", url.PathEscape(ref.Owner), url.PathEscape(ref.Name), url.PathEscape(username))
	err := c.do(ctx, http.MethodDelete, path, nil, nil)
	if err != nil && isNotFoundErr(err) {
		return nil
	}
	return err
}

func (c *forgejoClient) CreateAccessToken(ctx context.Context, username, tokenName string, scopes []string) (string, error) {
	return c.CreateToken(ctx, username, tokenName, scopes)
}

func (c *forgejoClient) DeleteAccessToken(ctx context.Context, username, tokenName string) error {
	return c.DeleteToken(ctx, username, tokenName)
}

var _ ForgejoClient = (*forgejoClient)(nil)
