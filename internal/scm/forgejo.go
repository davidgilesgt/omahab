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

func (c *forgejoClient) CreateAccessToken(ctx context.Context, username, tokenName string, scopes []string, repos []RepoRef) (string, error) {
	// Forgejo's token endpoint does not yet support repo-scoped tokens; repos is reserved for Step 5.
	_ = repos
	return c.CreateToken(ctx, username, tokenName, scopes)
}

func (c *forgejoClient) DeleteAccessToken(ctx context.Context, username, tokenName string) error {
	return c.DeleteToken(ctx, username, tokenName)
}

func (c *forgejoClient) ArchiveRepo(ctx context.Context, ref RepoRef, archived bool) error {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s", url.PathEscape(ref.Owner), url.PathEscape(ref.Name))
	body := map[string]any{"archived": archived}
	return c.do(ctx, http.MethodPatch, path, body, nil)
}

func (c *forgejoClient) CreateBranch(ctx context.Context, ref RepoRef, newBranch, fromRef string) error {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	if strings.TrimSpace(newBranch) == "" || strings.TrimSpace(fromRef) == "" {
		return fmt.Errorf("%w: new branch and from ref required", ErrValidation)
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/branches", url.PathEscape(ref.Owner), url.PathEscape(ref.Name))
	body := map[string]any{"new_branch_name": newBranch, "old_ref_name": fromRef}
	return c.do(ctx, http.MethodPost, path, body, nil)
}

func (c *forgejoClient) ListBranches(ctx context.Context, ref RepoRef) ([]*Branch, error) {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return nil, fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/branches", url.PathEscape(ref.Owner), url.PathEscape(ref.Name))
	var out []struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"id"`
			ID  string `json:"sha"`
		} `json:"commit"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	res := make([]*Branch, 0, len(out))
	for _, b := range out {
		sha := b.Commit.SHA
		if sha == "" {
			sha = b.Commit.ID
		}
		res = append(res, &Branch{Name: b.Name, CommitSHA: sha})
	}
	return res, nil
}

func (c *forgejoClient) GetFile(ctx context.Context, ref RepoRef, filePath, refStr string) ([]byte, error) {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return nil, fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	if strings.TrimSpace(filePath) == "" {
		return nil, fmt.Errorf("%w: file path required", ErrValidation)
	}
	parts := strings.Split(strings.TrimPrefix(filePath, "/"), "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	encPath := strings.Join(parts, "/")
	path := fmt.Sprintf("/api/v1/repos/%s/%s/contents/%s", url.PathEscape(ref.Owner), url.PathEscape(ref.Name), encPath)
	if strings.TrimSpace(refStr) != "" {
		path += "?ref=" + url.QueryEscape(refStr)
	}
	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	if out.Encoding == "base64" || strings.Contains(out.Content, "\n") {
		// Normalize: remove newlines, decode.
		clean := strings.ReplaceAll(out.Content, "\n", "")
		clean = strings.TrimSpace(clean)
		if clean == "" {
			return []byte{}, nil
		}
		decoded, err := base64.StdEncoding.DecodeString(clean)
		if err != nil {
			// Try raw StdEncoding without padding variations
			if decoded2, err2 := base64.StdEncoding.DecodeString(strings.TrimSpace(out.Content)); err2 == nil {
				return decoded2, nil
			}
			return nil, fmt.Errorf("decode file content: %w", err)
		}
		return decoded, nil
	}
	// Fallback: raw content (already plain)
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out.Content))
	if err == nil {
		return decoded, nil
	}
	return []byte(out.Content), nil
}

// --- pull requests ---

type forgejoPRJSON struct {
	Number int64  `json:"number"`
	Index  int64  `json:"index"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	HTMLURL string `json:"html_url"`
	User   *struct {
		Login string `json:"login"`
	} `json:"user"`
	Head *struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base *struct {
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
	Merged bool `json:"merged"`
}

func (j *forgejoPRJSON) toPull() *PullRequest {
	pr := &PullRequest{
		Title: j.Title,
		Body:  j.Body,
		State: j.State,
		HTMLURL: j.HTMLURL,
	}
	if j.Number != 0 {
		pr.Index = j.Number
	} else {
		pr.Index = j.Index
	}
	if j.User != nil {
		pr.Author = j.User.Login
	}
	if j.Head != nil {
		pr.HeadSHA = j.Head.SHA
		pr.HeadBranch = j.Head.Ref
		if j.Head.Repo != nil {
			pr.HeadRepoFullName = j.Head.Repo.FullName
		}
	}
	if j.Base != nil {
		pr.BaseBranch = j.Base.Ref
		if j.Base.Repo != nil {
			pr.BaseRepoFullName = j.Base.Repo.FullName
		}
	}
	if pr.State == "" && j.Merged {
		pr.State = "closed"
	}
	return pr
}

func (c *forgejoClient) ListPulls(ctx context.Context, ref RepoRef, state string) ([]*PullRequest, error) {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return nil, fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	q := ""
	if strings.TrimSpace(state) != "" {
		q = "?state=" + url.QueryEscape(state)
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls%s", url.PathEscape(ref.Owner), url.PathEscape(ref.Name), q)
	var raw []forgejoPRJSON
	if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]*PullRequest, 0, len(raw))
	for i := range raw {
		out = append(out, raw[i].toPull())
	}
	return out, nil
}

func (c *forgejoClient) GetPull(ctx context.Context, ref RepoRef, index int64) (*PullRequest, error) {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return nil, fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	if index <= 0 {
		return nil, fmt.Errorf("%w: pull index required", ErrValidation)
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d", url.PathEscape(ref.Owner), url.PathEscape(ref.Name), index)
	var raw forgejoPRJSON
	if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	return raw.toPull(), nil
}

func (c *forgejoClient) GetPullDiff(ctx context.Context, ref RepoRef, index int64) (string, error) {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return "", fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	if index <= 0 {
		return "", fmt.Errorf("%w: pull index required", ErrValidation)
	}
	// Forgejo serves diff at /pulls/{index}.diff ; also supports Accept header.
	// Use the .diff suffix which is explicit and not JSON.
	u := fmt.Sprintf("%s/api/v1/repos/%s/%s/pulls/%d.diff", c.baseURL, url.PathEscape(ref.Owner), url.PathEscape(ref.Name), index)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/plain")
	if c.token != "" {
		req.Header.Set("Authorization", "token "+c.token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", c.mapError(resp.StatusCode, data)
	}
	return string(data), nil
}

func (c *forgejoClient) CreatePull(ctx context.Context, ref RepoRef, in CreatePullInput) (*PullRequest, error) {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return nil, fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.HeadBranch) == "" || strings.TrimSpace(in.BaseBranch) == "" {
		return nil, fmt.Errorf("%w: title, head and base required", ErrValidation)
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls", url.PathEscape(ref.Owner), url.PathEscape(ref.Name))
	body := map[string]any{
		"title": in.Title,
		"body":  in.Body,
		"head":  in.HeadBranch,
		"base":  in.BaseBranch,
	}
	var raw forgejoPRJSON
	if err := c.do(ctx, http.MethodPost, path, body, &raw); err != nil {
		return nil, err
	}
	return raw.toPull(), nil
}

func (c *forgejoClient) CreatePullReview(ctx context.Context, ref RepoRef, index int64, in PullReviewInput) error {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	if index <= 0 {
		return fmt.Errorf("%w: pull index required", ErrValidation)
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/reviews", url.PathEscape(ref.Owner), url.PathEscape(ref.Name), index)
	payload := map[string]any{
		"event": in.Event,
		"body":  in.Body,
	}
	if strings.TrimSpace(in.CommitID) != "" {
		payload["commit_id"] = in.CommitID
	}
	if len(in.Comments) > 0 {
		payload["comments"] = in.Comments
	}
	// Forgejo validates event as COMMENT, APPROVE, REQUEST_CHANGES etc.
	// Normalize to upper; keep as provided.
	return c.do(ctx, http.MethodPost, path, payload, nil)
}

// --- issues ---

type forgejoIssueJSON struct {
	Number  int64  `json:"number"`
	Index   int64  `json:"index"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	User    *struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (j *forgejoIssueJSON) toIssue() *Issue {
	is := &Issue{
		Title:   j.Title,
		Body:    j.Body,
		State:   j.State,
		HTMLURL: j.HTMLURL,
	}
	if j.Number != 0 {
		is.Index = j.Number
	} else {
		is.Index = j.Index
	}
	if j.User != nil {
		is.Author = j.User.Login
	}
	return is
}

func (c *forgejoClient) ListIssues(ctx context.Context, ref RepoRef, state string) ([]*Issue, error) {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return nil, fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	q := ""
	if strings.TrimSpace(state) != "" {
		q = "?state=" + url.QueryEscape(state)
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues%s", url.PathEscape(ref.Owner), url.PathEscape(ref.Name), q)
	var raw []forgejoIssueJSON
	if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]*Issue, 0, len(raw))
	for i := range raw {
		out = append(out, raw[i].toIssue())
	}
	return out, nil
}

func (c *forgejoClient) GetIssue(ctx context.Context, ref RepoRef, index int64) (*Issue, error) {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return nil, fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	if index <= 0 {
		return nil, fmt.Errorf("%w: issue index required", ErrValidation)
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d", url.PathEscape(ref.Owner), url.PathEscape(ref.Name), index)
	var raw forgejoIssueJSON
	if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	return raw.toIssue(), nil
}

func (c *forgejoClient) CreateIssue(ctx context.Context, ref RepoRef, in CreateIssueInput) (*Issue, error) {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return nil, fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	if strings.TrimSpace(in.Title) == "" {
		return nil, fmt.Errorf("%w: title required", ErrValidation)
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues", url.PathEscape(ref.Owner), url.PathEscape(ref.Name))
	body := map[string]any{"title": in.Title, "body": in.Body}
	var raw forgejoIssueJSON
	if err := c.do(ctx, http.MethodPost, path, body, &raw); err != nil {
		return nil, err
	}
	return raw.toIssue(), nil
}

func (c *forgejoClient) CreateIssueComment(ctx context.Context, ref RepoRef, index int64, commentBody string) error {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	if index <= 0 {
		return fmt.Errorf("%w: issue index required", ErrValidation)
	}
	if strings.TrimSpace(commentBody) == "" {
		return fmt.Errorf("%w: comment body required", ErrValidation)
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/comments", url.PathEscape(ref.Owner), url.PathEscape(ref.Name), index)
	body := map[string]any{"body": commentBody}
	return c.do(ctx, http.MethodPost, path, body, nil)
}

// --- webhooks ---

func (c *forgejoClient) EnsureWebhook(ctx context.Context, ref RepoRef, hookURL, secret string, events []string) error {
	if strings.TrimSpace(ref.Owner) == "" || strings.TrimSpace(ref.Name) == "" {
		return fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	if strings.TrimSpace(hookURL) == "" {
		return fmt.Errorf("%w: webhook url required", ErrValidation)
	}
	// List existing hooks.
	listPath := fmt.Sprintf("/api/v1/repos/%s/%s/hooks", url.PathEscape(ref.Owner), url.PathEscape(ref.Name))
	var hooks []struct {
		ID     int64 `json:"id"`
		Type   string `json:"type"`
		Events []string `json:"events"`
		Active bool   `json:"active"`
		Config map[string]string `json:"config"`
	}
	if err := c.do(ctx, http.MethodGet, listPath, nil, &hooks); err != nil && !isNotFoundErr(err) {
		return err
	}
	for _, h := range hooks {
		if u, ok := h.Config["url"]; ok && strings.TrimSpace(u) == strings.TrimSpace(hookURL) {
			// Already exists; ensure events/secret/active match via PATCH if needed.
			// For idempotency, if matching url exists, return nil (caller ensures secret rotation separately).
			return nil
		}
	}
	// Create new hook.
	path := fmt.Sprintf("/api/v1/repos/%s/%s/hooks", url.PathEscape(ref.Owner), url.PathEscape(ref.Name))
	payload := map[string]any{
		"type":   "forgejo",
		"config": map[string]string{"url": hookURL, "content_type": "json", "secret": secret},
		"events": events,
		"active": true,
	}
	err := c.do(ctx, http.MethodPost, path, payload, nil)
	if err != nil && errors.Is(err, ErrConflict) {
		// Race: hook was created concurrently.
		return nil
	}
	return err
}

var _ ForgejoClient = (*forgejoClient)(nil)
