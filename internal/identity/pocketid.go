package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

// ErrNotConfigured is returned when PocketID is not configured.
// Callers should use errors.Is to detect it.
var ErrNotConfigured = errors.New("pocket-id not configured")

// Group is the domain view of a Pocket ID user group.
type Group struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	FriendlyName string `json:"friendlyName"`
}

// EnrollmentState describes whether a user has completed passkey enrollment.
type EnrollmentState struct {
	UserID          string `json:"user_id"`
	HasPasskey      bool   `json:"has_passkey"`
	CredentialCount int    `json:"credential_count"`
}

// AppAccess describes one OIDC client the user may access via group membership.
type AppAccess struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// PocketIDConfig holds the configuration for the PocketID admin API client.
// The admin API uses HTTP Basic authentication with an API client id/secret
// on /api/... endpoints (see github.com/stonith404/pocket-id).
type PocketIDConfig struct {
	BaseURL      string
	ClientID     string
	ClientSecret string
	HTTPClient   *http.Client
}

// PocketIDClient is a stdlib net/http adapter for Pocket ID.
// It converts Pocket ID's JSON DTOs to domain types at the boundary and
// never leaks SDK types.
type PocketIDClient struct {
	baseURL      string
	clientID     string
	clientSecret string
	httpClient   *http.Client
}

// NewPocketIDClient creates a new PocketID client.
// It validates BaseURL and returns ErrNotConfigured when the configuration is
// incomplete so callers fail loudly instead of silently returning a noop.
func NewPocketIDClient(cfg PocketIDConfig) (*PocketIDClient, error) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		return nil, fmt.Errorf("%w: BaseURL is required", ErrNotConfigured)
	}
	base = strings.TrimRight(base, "/")
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("invalid PocketID BaseURL %q: %w", base, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid PocketID BaseURL scheme %q: must be http or https", u.Scheme)
	}
	if strings.TrimSpace(cfg.ClientID) == "" || strings.TrimSpace(cfg.ClientSecret) == "" {
		return nil, fmt.Errorf("%w: ClientID and ClientSecret are required", ErrNotConfigured)
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &PocketIDClient{
		baseURL:      base,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		httpClient:   hc,
	}, nil
}

func (c *PocketIDClient) isConfigured() bool {
	return c != nil && c.baseURL != "" && c.clientID != "" && c.clientSecret != ""
}

func (c *PocketIDClient) ensureConfigured() error {
	if !c.isConfigured() {
		return fmt.Errorf("%w: PocketID client not configured", ErrNotConfigured)
	}
	return nil
}

// doJSON performs a JSON request with Basic auth and decodes the response.
func (c *PocketIDClient) doJSON(ctx context.Context, method, path string, reqBody any, respBody any) error {
	if err := c.ensureConfigured(); err != nil {
		return err
	}
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal request %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(b)
	}
	fullURL := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return fmt.Errorf("create request %s %s: %w", method, path, err)
	}
	req.SetBasicAuth(c.clientID, c.clientSecret)
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("pocket-id %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.mapAPIError(method, path, resp.StatusCode, respBytes)
	}
	if respBody == nil || len(respBytes) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBytes, respBody); err != nil {
		return fmt.Errorf("decode response %s %s: %w", method, path, err)
	}
	return nil
}

func (c *PocketIDClient) mapAPIError(method, path string, status int, body []byte) error {
	var errDto struct {
		Error   string         `json:"error"`
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	}
	msg := strings.TrimSpace(string(body))
	if len(body) > 0 {
		_ = json.Unmarshal(body, &errDto)
		if errDto.Error != "" {
			msg = errDto.Error
		} else if errDto.Message != "" {
			msg = errDto.Message
		}
		if errDto.Code != "" && msg != "" {
			msg = fmt.Sprintf("%s (%s)", msg, errDto.Code)
		}
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	switch status {
	case http.StatusNotFound:
		return fmt.Errorf("%w: pocket-id %s %s: %s", store.ErrNotFound, method, path, msg)
	case http.StatusConflict:
		return fmt.Errorf("%w: pocket-id %s %s: %s", store.ErrConflict, method, path, msg)
	case http.StatusBadRequest:
		return fmt.Errorf("%w: pocket-id %s %s: %s", store.ErrValidation, method, path, msg)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("pocket-id %s %s: unauthorized (%d) %s", method, path, status, msg)
	case http.StatusTooManyRequests:
		return fmt.Errorf("pocket-id %s %s: rate limited (%d) %s", method, path, status, msg)
	default:
		return fmt.Errorf("pocket-id %s %s: %d %s", method, path, status, msg)
	}
}

// internal DTOs – never leaked beyond the client boundary.

type pocketUserDto struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	Email       *string `json:"email"`
	FirstName   string `json:"firstName"`
	LastName    *string `json:"lastName"`
	DisplayName string `json:"displayName"`
	IsAdmin     bool   `json:"isAdmin"`
	Disabled    bool   `json:"disabled"`
	UserGroups  []pocketUserGroupMinimalDto `json:"userGroups"`
}

type pocketUserGroupMinimalDto struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	FriendlyName string `json:"friendlyName"`
}

type pocketUserGroupDto struct {
	ID                 string                   `json:"id"`
	Name               string                   `json:"name"`
	FriendlyName       string                   `json:"friendlyName"`
	Users              []pocketUserDto          `json:"users"`
	AllowedOidcClients []pocketOidcClientDto    `json:"allowedOidcClients"`
}

type pocketOidcClientDto struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type paginatedUsersDto struct {
	Data       []pocketUserDto `json:"data"`
	Pagination *struct {
		TotalCount int `json:"totalCount"`
	} `json:"pagination"`
}

type paginatedGroupsDto struct {
	Data       []pocketUserGroupDto `json:"data"`
	Pagination *struct {
		TotalCount int `json:"totalCount"`
	} `json:"pagination"`
}

type paginatedGroupsMinimalDto struct {
	Data       []pocketUserGroupMinimalDto `json:"data"`
	Pagination *struct {
		TotalCount int `json:"totalCount"`
	} `json:"pagination"`
}

type tokenCreateDto struct {
	TTL string `json:"ttl"`
}

type tokenResponseDto struct {
	Token string `json:"token"`
}

type webauthnCredentialDto struct {
	ID        string `json:"id"`
	CreatedAt string `json:"createdAt"`
}

type appConfigVarDto struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Type  string `json:"type"`
}

// CreateRecoveryCode generates a short-lived login code and enrollment URL for email.
// It looks up the user by email, creates a one-time access token via
// POST /api/users/{id}/one-time-access-token, and returns a full enrollment URL.
func (c *PocketIDClient) CreateRecoveryCode(ctx context.Context, email string) (string, string, time.Time, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return "", "", time.Time{}, store.Validation("email is required")
	}
	if !domain.ValidEmail(email) {
		return "", "", time.Time{}, store.Validationf("invalid email %q", email)
	}
	if err := c.ensureConfigured(); err != nil {
		return "", "", time.Time{}, err
	}
	user, err := c.findUserByEmail(ctx, email)
	if err != nil {
		return "", "", time.Time{}, err
	}
	ttl := DefaultRecoveryTTL
	code, err := c.createOneTimeToken(ctx, user.ID, ttl)
	if err != nil {
		return "", "", time.Time{}, err
	}
	expiresAt := time.Now().UTC().Add(ttl)
	u := c.baseURL + "/lc/" + code
	return code, u, expiresAt, nil
}

// ValidateRecovery checks that a recovery code could be valid for email.
// It verifies the user exists and the code has the expected format/length
// without consuming the token. It performs an authenticated request so
// Basic-auth header handling can be verified.
func (c *PocketIDClient) ValidateRecovery(ctx context.Context, email, code string) error {
	email = strings.TrimSpace(strings.ToLower(email))
	code = strings.TrimSpace(code)
	if email == "" || code == "" {
		return store.Validation("email and code are required")
	}
	if err := c.ensureConfigured(); err != nil {
		return err
	}
	if len(code) != 6 && len(code) != 12 {
		return store.Validationf("invalid recovery code length %d", len(code))
	}
	_, err := c.findUserByEmail(ctx, email)
	if err != nil {
		return err
	}
	return nil
}

func (c *PocketIDClient) findUserByEmail(ctx context.Context, email string) (*pocketUserDto, error) {
	// Paginated search; encode email for query
	escaped := url.QueryEscape(email)
	path := "/api/users?search=" + escaped
	var paginated paginatedUsersDto
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &paginated); err != nil {
		return nil, err
	}
	lower := strings.ToLower(email)
	for i := range paginated.Data {
		u := &paginated.Data[i]
		if u.Email != nil && strings.ToLower(strings.TrimSpace(*u.Email)) == lower {
			return u, nil
		}
		// Pocket ID also allows username as email fallback
		if strings.ToLower(u.Username) == lower {
			return u, nil
		}
	}
	// Fallback: try exact get if list empty but we still got data? Already tried.
	return nil, store.NotFoundf("user %q not found", email)
}

func (c *PocketIDClient) createOneTimeToken(ctx context.Context, userID string, ttl time.Duration) (string, error) {
	if strings.TrimSpace(userID) == "" {
		return "", store.Validation("userID is required")
	}
	path := "/api/users/" + url.PathEscape(userID) + "/one-time-access-token"
	req := tokenCreateDto{TTL: ttl.String()}
	var resp tokenResponseDto
	if err := c.doJSON(ctx, http.MethodPost, path, req, &resp); err != nil {
		return "", err
	}
	code := strings.TrimSpace(resp.Token)
	if code == "" {
		return "", fmt.Errorf("pocket-id %s: empty token in response", path)
	}
	return code, nil
}

// CreateUser creates a Pocket ID user and returns an expiring enrollment URL.
// It sets the user as not yet enrolled; the caller must deliver the enrollment
// link to the user. disableEmailOTP is honored by provisioning defaults, not per-call.
func (c *PocketIDClient) CreateUser(ctx context.Context, email, name string, isAdmin bool, groupIDs []string) (string, string, time.Time, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	name = strings.TrimSpace(name)
	if email == "" || name == "" {
		return "", "", time.Time{}, store.Validation("email and name are required")
	}
	if !domain.ValidEmail(email) {
		return "", "", time.Time{}, store.Validationf("invalid email %q", email)
	}
	if err := c.ensureConfigured(); err != nil {
		return "", "", time.Time{}, err
	}
	username := strings.Split(email, "@")[0]
	// Sanitize username to pocket-id constraints (min 3? spec says username required, min 1)
	if len(username) < 3 {
		username = username + "user"
	}
	payload := map[string]any{
		"username":     username,
		"email":        email,
		"firstName":    name,
		"displayName":  name,
		"isAdmin":      isAdmin,
		"userGroupIds": groupIDs,
	}
	var created pocketUserDto
	if err := c.doJSON(ctx, http.MethodPost, "/api/users", payload, &created); err != nil {
		return "", "", time.Time{}, err
	}
	if created.ID == "" {
		return "", "", time.Time{}, fmt.Errorf("pocket-id create user: empty id in response")
	}
	ttl := DefaultRecoveryTTL
	code, err := c.createOneTimeToken(ctx, created.ID, ttl)
	if err != nil {
		return created.ID, "", time.Time{}, err
	}
	expiresAt := time.Now().UTC().Add(ttl)
	enrollmentURL := c.baseURL + "/lc/" + code
	return created.ID, enrollmentURL, expiresAt, nil
}

// GetUser returns a domain.User for the given Pocket ID user ID.
func (c *PocketIDClient) GetUser(ctx context.Context, userID string) (domain.User, error) {
	if strings.TrimSpace(userID) == "" {
		return domain.User{}, store.Validation("userID is required")
	}
	if err := c.ensureConfigured(); err != nil {
		return domain.User{}, err
	}
	var dto pocketUserDto
	if err := c.doJSON(ctx, http.MethodGet, "/api/users/"+url.PathEscape(userID), nil, &dto); err != nil {
		return domain.User{}, err
	}
	return pocketUserToDomain(dto), nil
}

// ListUsers returns all Pocket ID users as domain.Users.
func (c *PocketIDClient) ListUsers(ctx context.Context) ([]domain.User, error) {
	if err := c.ensureConfigured(); err != nil {
		return nil, err
	}
	var paginated paginatedUsersDto
	if err := c.doJSON(ctx, http.MethodGet, "/api/users", nil, &paginated); err != nil {
		return nil, err
	}
	out := make([]domain.User, 0, len(paginated.Data))
	for _, u := range paginated.Data {
		out = append(out, pocketUserToDomain(u))
	}
	return out, nil
}

// DisableUser enables or disables a Pocket ID user.
func (c *PocketIDClient) DisableUser(ctx context.Context, userID string, disabled bool) error {
	if strings.TrimSpace(userID) == "" {
		return store.Validation("userID is required")
	}
	if err := c.ensureConfigured(); err != nil {
		return err
	}
	// Fetch current user to preserve other fields for PUT
	var current pocketUserDto
	if err := c.doJSON(ctx, http.MethodGet, "/api/users/"+url.PathEscape(userID), nil, &current); err != nil {
		return err
	}
	payload := map[string]any{
		"username":    current.Username,
		"email":       current.Email,
		"firstName":   current.FirstName,
		"displayName": current.DisplayName,
		"isAdmin":     current.IsAdmin,
		"disabled":    disabled,
	}
	var updated pocketUserDto
	return c.doJSON(ctx, http.MethodPut, "/api/users/"+url.PathEscape(userID), payload, &updated)
}

// DeleteUser deletes a Pocket ID user.
func (c *PocketIDClient) DeleteUser(ctx context.Context, userID string) error {
	if strings.TrimSpace(userID) == "" {
		return store.Validation("userID is required")
	}
	if err := c.ensureConfigured(); err != nil {
		return err
	}
	return c.doJSON(ctx, http.MethodDelete, "/api/users/"+url.PathEscape(userID), nil, nil)
}

// HealthCheck verifies Pocket ID reachability with an authenticated request.
// It tries the lightweight /api/users endpoint (one element) and falls back to
// /api/application-configuration when list is not permitted.
func (c *PocketIDClient) HealthCheck(ctx context.Context) error {
	if err := c.ensureConfigured(); err != nil {
		return err
	}
	var paginated paginatedUsersDto
	err := c.doJSON(ctx, http.MethodGet, "/api/users?pagination[limit]=1", nil, &paginated)
	if err == nil {
		return nil
	}
	// Fallback: try app config
	var vars []appConfigVarDto
	if err2 := c.doJSON(ctx, http.MethodGet, "/api/application-configuration/all", nil, &vars); err2 == nil {
		return nil
	}
	return err
}

func pocketUserToDomain(u pocketUserDto) domain.User {
	email := ""
	if u.Email != nil {
		email = strings.ToLower(strings.TrimSpace(*u.Email))
	}
	name := u.DisplayName
	if name == "" {
		name = u.FirstName
		if u.LastName != nil && *u.LastName != "" {
			name = strings.TrimSpace(name + " " + *u.LastName)
		}
		if name == "" {
			name = u.Username
		}
	}
	groups := make([]string, 0, len(u.UserGroups))
	for _, g := range u.UserGroups {
		groups = append(groups, g.ID)
	}
	return domain.User{
		ID:       domain.ID(u.ID),
		Email:    email,
		Name:     name,
		Groups:   groups,
		Disabled: u.Disabled,
	}
}

// ListGroups returns all user groups.
func (c *PocketIDClient) ListGroups(ctx context.Context) ([]Group, error) {
	if err := c.ensureConfigured(); err != nil {
		return nil, err
	}
	var pag paginatedGroupsMinimalDto
	if err := c.doJSON(ctx, http.MethodGet, "/api/user-groups", nil, &pag); err != nil {
		return nil, err
	}
	out := make([]Group, 0, len(pag.Data))
	for _, g := range pag.Data {
		out = append(out, Group{ID: g.ID, Name: g.Name, FriendlyName: g.FriendlyName})
	}
	return out, nil
}

// GetGroup returns a single group by ID.
func (c *PocketIDClient) GetGroup(ctx context.Context, groupID string) (Group, error) {
	if strings.TrimSpace(groupID) == "" {
		return Group{}, store.Validation("groupID is required")
	}
	if err := c.ensureConfigured(); err != nil {
		return Group{}, err
	}
	var dto pocketUserGroupDto
	if err := c.doJSON(ctx, http.MethodGet, "/api/user-groups/"+url.PathEscape(groupID), nil, &dto); err != nil {
		return Group{}, err
	}
	return Group{ID: dto.ID, Name: dto.Name, FriendlyName: dto.FriendlyName}, nil
}

// CreateGroup creates a user group.
func (c *PocketIDClient) CreateGroup(ctx context.Context, name, friendlyName string) (Group, error) {
	name = strings.TrimSpace(name)
	friendlyName = strings.TrimSpace(friendlyName)
	if name == "" {
		return Group{}, store.Validation("group name is required")
	}
	if friendlyName == "" {
		friendlyName = name
	}
	if err := c.ensureConfigured(); err != nil {
		return Group{}, err
	}
	payload := map[string]any{"name": name, "friendlyName": friendlyName}
	var created pocketUserGroupDto
	if err := c.doJSON(ctx, http.MethodPost, "/api/user-groups", payload, &created); err != nil {
		return Group{}, err
	}
	if created.ID == "" {
		return Group{}, fmt.Errorf("pocket-id create group: empty id in response")
	}
	return Group{ID: created.ID, Name: created.Name, FriendlyName: created.FriendlyName}, nil
}

// EnsureGroups idempotently ensures that groups with the given names exist.
// It returns the matching groups in the order requested.
func (c *PocketIDClient) EnsureGroups(ctx context.Context, names []string) ([]Group, error) {
	if err := c.ensureConfigured(); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return []Group{}, nil
	}
	existing, err := c.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]Group, len(existing))
	for _, g := range existing {
		byName[strings.ToLower(g.Name)] = g
		byName[strings.ToLower(g.FriendlyName)] = g
	}
	out := make([]Group, 0, len(names))
	for _, n := range names {
		key := strings.ToLower(strings.TrimSpace(n))
		if g, ok := byName[key]; ok {
			out = append(out, g)
			continue
		}
		created, err := c.CreateGroup(ctx, n, n)
		if err != nil {
			// If conflict raced, re-list and try again
			if errors.Is(err, store.ErrConflict) {
				retry, err2 := c.ListGroups(ctx)
				if err2 != nil {
					return nil, err
				}
				for _, g := range retry {
					if strings.EqualFold(g.Name, n) || strings.EqualFold(g.FriendlyName, n) {
						out = append(out, g)
						goto next
					}
				}
			}
			return nil, err
		}
		out = append(out, created)
	next:
	}
	return out, nil
}

// GetUserGroups returns the groups a user belongs to.
func (c *PocketIDClient) GetUserGroups(ctx context.Context, userID string) ([]Group, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, store.Validation("userID is required")
	}
	if err := c.ensureConfigured(); err != nil {
		return nil, err
	}
	var groups []pocketUserGroupMinimalDto
	if err := c.doJSON(ctx, http.MethodGet, "/api/users/"+url.PathEscape(userID)+"/groups", nil, &groups); err != nil {
		// Fallback: fetch user and read embedded groups
		var user pocketUserDto
		if err2 := c.doJSON(ctx, http.MethodGet, "/api/users/"+url.PathEscape(userID), nil, &user); err2 != nil {
			return nil, err
		}
		out := make([]Group, 0, len(user.UserGroups))
		for _, g := range user.UserGroups {
			out = append(out, Group{ID: g.ID, Name: g.Name, FriendlyName: g.FriendlyName})
		}
		return out, nil
	}
	out := make([]Group, 0, len(groups))
	for _, g := range groups {
		out = append(out, Group{ID: g.ID, Name: g.Name, FriendlyName: g.FriendlyName})
	}
	return out, nil
}

// SetUserGroups replaces the groups for a user.
func (c *PocketIDClient) SetUserGroups(ctx context.Context, userID string, groupIDs []string) error {
	if strings.TrimSpace(userID) == "" {
		return store.Validation("userID is required")
	}
	if err := c.ensureConfigured(); err != nil {
		return err
	}
	payload := map[string]any{"userGroupIds": groupIDs}
	var updated pocketUserDto
	// Pocket ID expects PUT /api/users/:id/user-groups
	err := c.doJSON(ctx, http.MethodPut, "/api/users/"+url.PathEscape(userID)+"/user-groups", payload, &updated)
	if err == nil {
		return nil
	}
	// Fallback for case-sensitive path variant
	if strings.Contains(err.Error(), "not found") {
		// Try alternate path
		payload2 := map[string]any{"userIds": groupIDs}
		// This path is for groups->users, not users->groups, so keep original error
		_ = payload2
	}
	return err
}

// AddUserToGroup adds a user to a group idempotently.
func (c *PocketIDClient) AddUserToGroup(ctx context.Context, userID, groupID string) error {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(groupID) == "" {
		return store.Validation("userID and groupID are required")
	}
	if err := c.ensureConfigured(); err != nil {
		return err
	}
	groups, err := c.GetUserGroups(ctx, userID)
	if err != nil {
		return err
	}
	for _, g := range groups {
		if g.ID == groupID {
			return nil
		}
	}
	ids := make([]string, 0, len(groups)+1)
	for _, g := range groups {
		ids = append(ids, g.ID)
	}
	ids = append(ids, groupID)
	return c.SetUserGroups(ctx, userID, ids)
}

// RemoveUserFromGroup removes a user from a group.
func (c *PocketIDClient) RemoveUserFromGroup(ctx context.Context, userID, groupID string) error {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(groupID) == "" {
		return store.Validation("userID and groupID are required")
	}
	if err := c.ensureConfigured(); err != nil {
		return err
	}
	groups, err := c.GetUserGroups(ctx, userID)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(groups))
	found := false
	for _, g := range groups {
		if g.ID == groupID {
			found = true
			continue
		}
		ids = append(ids, g.ID)
	}
	if !found {
		return nil
	}
	return c.SetUserGroups(ctx, userID, ids)
}

// Ensure PocketIDClient satisfies PocketID interface.
var _ PocketID = (*PocketIDClient)(nil)
