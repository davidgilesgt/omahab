package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/health"
	"github.com/omahab/omahab/internal/store"
)


// forgejoBaseURL returns the Forgejo base URL for API calls.
// It prefers an explicit secret/env override for testability, falling back to https://git.<domain>.
func (b *Backend) forgejoBaseURL(ctx context.Context, domain string) string {
	if b.forgejoBaseURLOverride != "" {
		return strings.TrimSpace(b.forgejoBaseURLOverride)
	}
	if b.secrets != nil {
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "forgejo_base_url"); err == nil && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if v := strings.TrimSpace(os.Getenv("OMAHAB_FORGEJO_URL")); v != "" {
		return strings.TrimSpace(v)
	}
	domain = strings.TrimSpace(domain)
	if domain == "" || domain == "example.com" || domain == "not-configured.invalid" {
		return "http://127.0.0.1:3000"
	}
	return fmt.Sprintf("https://git.%s", domain)
}

// redactForgejoError redacts sensitive data from Forgejo CLI/API errors.
func redactForgejoError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	redacted := health.RedactDetail(msg)
	if redacted == "[REDACTED]" {
		return errors.New("[REDACTED]")
	}
	// Additionally redact any value after --secret, --password, --key in the message if present
	// to ensure we don't leak via command echo.
	// Simple regex replacement for "--secret <value>" or "--password <value>"
	re := regexp.MustCompile(`(--(secret|password|key|client_secret)\s+)\S+`)
	msg2 := re.ReplaceAllString(msg, `${1}[REDACTED]`)
	if msg2 != msg {
		// Run through RedactDetail again to catch any remaining sensitive tokens
		msg2 = health.RedactDetail(msg2)
		if msg2 == "[REDACTED]" {
			return errors.New("[REDACTED]")
		}
		return errors.New(msg2)
	}
	return errors.New(redacted)
}

// runForgejoAdminCommand executes `forgejo admin <args>` inside the Forgejo container.
// It uses b.forgejoExec if set (for tests), otherwise attempts docker exec.
func (b *Backend) runForgejoAdminCommand(ctx context.Context, args ...string) (string, error) {
	if b.forgejoExec != nil {
		return b.forgejoExec(ctx, args...)
	}
	containerID, err := b.findForgejoContainerID(ctx)
	if err != nil {
		return "", err
	}
	fullArgs := append([]string{"exec", containerID, "forgejo", "admin"}, args...)
	cmd := exec.CommandContext(ctx, "docker", fullArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	out := strings.TrimSpace(stdout.String())
	errOut := strings.TrimSpace(stderr.String())
	combined := strings.TrimSpace(out + "\n" + errOut)
	if err != nil {
		if combined == "" {
			combined = err.Error()
		}
		redacted := health.RedactDetail(combined)
		// Also redact args that contain secrets if they appear in the combined output (some CLIs echo args)
		if redacted == "[REDACTED]" {
			return "", fmt.Errorf("forgejo admin %s: %w", redactArgsForLog(args), errors.New("[REDACTED]"))
		}
		return "", fmt.Errorf("forgejo admin %s: %s", redactArgsForLog(args), redacted)
	}
	if out == "" && errOut != "" {
		out = errOut
	}
	return strings.TrimSpace(out), nil
}

func redactArgsForLog(args []string) string {
	out := make([]string, len(args))
	copy(out, args)
	for i, a := range out {
		low := strings.ToLower(a)
		if low == "--secret" || low == "--password" || low == "--key" || low == "--client-secret" || low == "--client_secret" {
			if i+1 < len(out) {
				out[i+1] = "[REDACTED]"
			}
		}
		// Handle --secret=xxx form
		if strings.HasPrefix(low, "--secret=") || strings.HasPrefix(low, "--password=") || strings.HasPrefix(low, "--key=") {
			parts := strings.SplitN(out[i], "=", 2)
			if len(parts) == 2 {
				out[i] = parts[0] + "=[REDACTED]"
			}
		}
	}
	return strings.Join(out, " ")
}

func (b *Backend) findForgejoContainerID(ctx context.Context) (string, error) {
	// Attempt 1: label filter
	cmd := exec.CommandContext(ctx, "docker", "ps", "-q", "--filter", "label=com.docker.compose.project=omahab-forgejo", "--filter", "label=com.docker.compose.service=forgejo")
	out, err := cmd.Output()
	if err == nil {
		id := strings.TrimSpace(string(out))
		if id != "" {
			fields := strings.Fields(id)
			if len(fields) > 0 && strings.TrimSpace(fields[0]) != "" {
				return strings.TrimSpace(fields[0]), nil
			}
		}
	}
	// Attempt 2: name filter
	cmd2 := exec.CommandContext(ctx, "docker", "ps", "-q", "--filter", "name=forgejo")
	out2, err2 := cmd2.Output()
	if err2 == nil {
		id2 := strings.TrimSpace(string(out2))
		if id2 != "" {
			fields := strings.Fields(id2)
			if len(fields) > 0 && strings.TrimSpace(fields[0]) != "" {
				return strings.TrimSpace(fields[0]), nil
			}
		}
	}
	// Attempt 3: fallback to known container name
	cmd3 := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.Id}}", "omahab-forgejo-forgejo-1")
	out3, err3 := cmd3.CombinedOutput()
	if err3 == nil && strings.TrimSpace(string(out3)) != "" {
		return "omahab-forgejo-forgejo-1", nil
	}
	// Attempt 4: try docker ps format
	cmd4 := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.Names}}")
	out4, err4 := cmd4.Output()
	if err4 == nil {
		names := strings.Split(strings.TrimSpace(string(out4)), "\n")
		for _, n := range names {
			n = strings.TrimSpace(n)
			if strings.Contains(strings.ToLower(n), "forgejo") {
				return n, nil
			}
		}
	}
	return "", fmt.Errorf("forgejo container not found")
}

// validateForgejoToken checks that the given token can authenticate to Forgejo API.
func (b *Backend) validateForgejoToken(ctx context.Context, token, domain string) error {
	base := b.forgejoBaseURL(ctx, domain)
	u := strings.TrimRight(base, "/") + "/api/v1/user"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+strings.TrimSpace(token))
	req.Header.Set("Accept", "application/json")
	client := b.forgejoHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("unauthorized: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("forgejo token validation: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (b *Backend) forgejoHTTPClient() *http.Client {
	if b.forgejoHTTPClientOverride != nil {
		return b.forgejoHTTPClientOverride
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// forgejoAPIRequest performs a Forgejo API request with token auth.
func (b *Backend) forgejoAPIRequest(ctx context.Context, baseURL, token, method, path string, reqBody any, respBody any) (int, error) {
	fullURL := strings.TrimRight(strings.TrimSpace(baseURL), "/") + path
	var body io.Reader
	if reqBody != nil {
		bb, err := json.Marshal(reqBody)
		if err != nil {
			return 0, fmt.Errorf("marshal request: %w", err)
		}
		body = bytes.NewReader(bb)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "token "+strings.TrimSpace(token))
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := b.forgejoHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		// Redact if message contains secrets
		msg = health.RedactDetail(msg)
		return resp.StatusCode, fmt.Errorf("forgejo %s %s: %d %s", method, path, resp.StatusCode, msg)
	}
	if respBody != nil && len(data) > 0 {
		if err := json.Unmarshal(data, respBody); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response %s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// ensureOmahabBot ensures the Forgejo automation user omahab-bot exists and has a valid token.
func (b *Backend) ensureOmahabBot(ctx context.Context, domain string) error {
	if b.secrets == nil {
		return fmt.Errorf("secrets not configured")
	}
	botEmail := fmt.Sprintf("omahab-bot@users.noreply.%s", strings.TrimSpace(domain))
	// Check existing token
	existingToken, err := b.secrets.RevealByName(ctx, "platform-app", "forgejo_token")
	hasToken := err == nil && strings.TrimSpace(existingToken) != ""
	// Check if bot exists via admin user list
	exists, listErr := b.forgejoBotExists(ctx)
	if listErr != nil {
		// If list fails, treat as not exists? But we should return error to retry.
		// For docker not available in tests, we may have mock.
		return fmt.Errorf("check omahab-bot existence: %w", redactForgejoError(listErr))
	}
	if !exists {
		pwd := generateRandomBase64URL(32) + "Aa1!"
		// Create with restricted admin
		_, cerr := b.runForgejoAdminCommand(ctx, "user", "create", "--admin", "--restricted", "--username", "omahab-bot", "--email", botEmail, "--password", pwd, "--must-change-password=false")
		if cerr != nil {
			low := strings.ToLower(cerr.Error())
			if !strings.Contains(low, "already exists") && !strings.Contains(low, "duplicate") && !strings.Contains(low, "exists") {
				return fmt.Errorf("create omahab-bot: %w", redactForgejoError(cerr))
			}
		} else {
			// User created, token needs rotation
			hasToken = false
		}
	}
	if hasToken {
		if err := b.validateForgejoToken(ctx, strings.TrimSpace(existingToken), domain); err == nil {
			return nil
		} else {
			low := strings.ToLower(err.Error())
			if !strings.Contains(low, "401") && !strings.Contains(low, "unauthorized") && !strings.Contains(low, "403") && !strings.Contains(low, "invalid") {
				// For transient errors, return but allow retry next reconcile
				// Only rotate on auth failure; otherwise surface error
				if strings.Contains(low, "connection") || strings.Contains(low, "timeout") || strings.Contains(low, "refused") {
					return fmt.Errorf("validate forgejo token: %w", redactForgejoError(err))
				}
				// For other errors, also try to rotate? But spec says rotate only when missing/rejected.
				// Treat as rejected for 401/403/invalid, otherwise error.
				return fmt.Errorf("validate forgejo token: %w", redactForgejoError(err))
			}
			// else rotate
		}
	}
	// Generate new token
	out, err := b.runForgejoAdminCommand(ctx, "user", "generate-access-token", "--username", "omahab-bot", "--token-name", "omahab", "--raw")
	if err != nil {
		return fmt.Errorf("generate forgejo token: %w", redactForgejoError(err))
	}
	newToken := strings.TrimSpace(out)
	if newToken == "" {
		return fmt.Errorf("forgejo generate-access-token returned empty")
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "forgejo_token", newToken); err != nil {
		return fmt.Errorf("store forgejo_token: %w", err)
	}
	return nil
}

func (b *Backend) forgejoBotExists(ctx context.Context) (bool, error) {
	out, err := b.runForgejoAdminCommand(ctx, "user", "list")
	if err != nil {
		return false, err
	}
	// Simple check: output contains omahab-bot as username
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		if strings.Contains(line, "omahab-bot") {
			// Ensure it's a username match, not substring in other field
			fields := strings.Fields(line)
			for _, f := range fields {
				if strings.TrimSpace(f) == "omahab-bot" {
					return true, nil
				}
			}
			// Fallback: if line contains omahab-bot, consider exists
			return true, nil
		}
	}
	// Also try case-insensitive?
	if strings.Contains(strings.ToLower(out), "omahab-bot") {
		return true, nil
	}
	return false, nil
}

// ensureForgejoOrgTeams ensures organization omahab and teams admins/members.
func (b *Backend) ensureForgejoOrgTeams(ctx context.Context, baseURL, token string) error {
	// Ensure org
	orgPath := "/api/v1/orgs/omahab"
	var orgResp map[string]any
	status, err := b.forgejoAPIRequest(ctx, baseURL, token, http.MethodGet, orgPath, nil, &orgResp)
	if err != nil {
		if status == http.StatusNotFound {
			// Create org
			payload := map[string]any{
				"username":    "omahab",
				"visibility":  "private",
				"description": "Omahab organization",
			}
			var created map[string]any
			if _, cerr := b.forgejoAPIRequest(ctx, baseURL, token, http.MethodPost, "/api/v1/orgs", payload, &created); cerr != nil {
				return fmt.Errorf("create org omahab: %w", redactForgejoError(cerr))
			}
		} else {
			return fmt.Errorf("get org omahab: %w", redactForgejoError(err))
		}
	}
	// Ensure teams
	teamsPath := "/api/v1/orgs/omahab/teams"
	var teams []map[string]any
	status, err = b.forgejoAPIRequest(ctx, baseURL, token, http.MethodGet, teamsPath, nil, &teams)
	if err != nil && status != http.StatusNotFound {
		return fmt.Errorf("list teams: %w", redactForgejoError(err))
	}
	// Build map name -> team
	teamByName := map[string]map[string]any{}
	for _, t := range teams {
		if n, ok := t["name"].(string); ok {
			teamByName[strings.ToLower(strings.TrimSpace(n))] = t
		} else if n, ok := t["Name"].(string); ok {
			teamByName[strings.ToLower(strings.TrimSpace(n))] = t
		}
	}
	desired := []struct {
		name       string
		permission string
	}{
		{"admins", "admin"},
		{"members", "write"},
	}
	for _, d := range desired {
		if existing, ok := teamByName[strings.ToLower(d.name)]; ok {
			// Check permission and includes_all_repositories
			perm := ""
			if p, ok := existing["permission"].(string); ok {
				perm = p
			} else if p, ok := existing["Permission"].(string); ok {
				perm = p
			}
			includesAll := false
			if v, ok := existing["includes_all_repositories"].(bool); ok {
				includesAll = v
			} else if v, ok := existing["includesAllRepositories"].(bool); ok {
				includesAll = v
			}
			if strings.EqualFold(perm, d.permission) && includesAll {
				continue
			}
			// Need to update
			id := extractTeamID(existing)
			if id == 0 {
				continue
			}
			payload := map[string]any{
				"name":                        d.name,
				"permission":                  d.permission,
				"includes_all_repositories":   true,
				"can_create_org_repo":         false,
			}
			patchPath := fmt.Sprintf("/api/v1/teams/%d", id)
			if _, perr := b.forgejoAPIRequest(ctx, baseURL, token, http.MethodPatch, patchPath, payload, nil); perr != nil {
				// Try PUT as fallback
				if _, perr2 := b.forgejoAPIRequest(ctx, baseURL, token, http.MethodPut, patchPath, payload, nil); perr2 != nil {
					return fmt.Errorf("update team %s: %w", d.name, redactForgejoError(perr))
				}
			}
			continue
		}
		// Create team
		payload := map[string]any{
			"name":                      d.name,
			"description":               fmt.Sprintf("Omahab %s", d.name),
			"permission":                d.permission,
			"can_create_org_repo":       false,
			"includes_all_repositories": true,
			"units":                     []string{},
		}
		var created map[string]any
		if _, cerr := b.forgejoAPIRequest(ctx, baseURL, token, http.MethodPost, teamsPath, payload, &created); cerr != nil {
			low := strings.ToLower(cerr.Error())
			if strings.Contains(low, "already exists") || strings.Contains(low, "duplicate") {
				continue
			}
			return fmt.Errorf("create team %s: %w", d.name, redactForgejoError(cerr))
		}
	}
	return nil
}

func extractTeamID(m map[string]any) int64 {
	if v, ok := m["id"]; ok {
		switch vv := v.(type) {
		case float64:
			return int64(vv)
		case int64:
			return vv
		case int:
			return int64(vv)
		case string:
			if i, err := strconv.ParseInt(vv, 10, 64); err == nil {
				return i
			}
		}
	}
	if v, ok := m["ID"]; ok {
		switch vv := v.(type) {
		case float64:
			return int64(vv)
		case int64:
			return vv
		case int:
			return int64(vv)
		}
	}
	return 0
}

func parseAuthSourceID(output, name string) string {
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if !strings.Contains(line, name) {
			continue
		}
		// Try split by |
		if strings.Contains(line, "|") {
			parts := strings.Split(line, "|")
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p == name {
					// ID is first part
					if len(parts) > 0 {
						id := strings.TrimSpace(parts[0])
						if _, err := strconv.Atoi(id); err == nil {
							return id
						}
					}
				}
			}
			// fallback: first numeric field before |
			first := strings.TrimSpace(parts[0])
			if _, err := strconv.Atoi(first); err == nil {
				return first
			}
		}
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == name && i > 0 {
				// Look backward for numeric ID
				for j := i - 1; j >= 0; j-- {
					if _, err := strconv.Atoi(fields[j]); err == nil {
						return fields[j]
					}
				}
			}
		}
		// Regex fallback: find leading number
		re := regexp.MustCompile(`^\s*(\d+)\b`)
		if m := re.FindStringSubmatch(line); len(m) == 2 {
			return m[1]
		}
	}
	return ""
}

// ensureForgejoAuthSource reconciles the Forgejo OIDC auth source named PocketID.
func (b *Backend) ensureForgejoAuthSource(ctx context.Context, domain, clientID, clientSecret string) error {
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return fmt.Errorf("clientID and clientSecret required")
	}
	out, err := b.runForgejoAdminCommand(ctx, "auth", "list")
	if err != nil {
		return fmt.Errorf("list auth sources: %w", redactForgejoError(err))
	}
	id := parseAuthSourceID(out, "PocketID")
	discovery := fmt.Sprintf("https://id.%s/.well-known/openid-configuration", strings.TrimSpace(domain))
	scopes := "openid email profile"
	groupClaim := "groups"
	adminGroup := "admins"
	teamMap := `{"admins":{"omahab":["admins"]},"members":{"omahab":["members"]}}`

	baseArgs := []string{
		"--name", "PocketID",
		"--provider", "openIDConnect",
		"--key", strings.TrimSpace(clientID),
		"--secret", strings.TrimSpace(clientSecret),
		"--auto-discover-url", discovery,
		"--scopes", scopes,
		"--group-claim-name", groupClaim,
		"--admin-group", adminGroup,
		"--group-team-map", teamMap,
		"--group-team-map-removal-enabled",
	}
	// Also try alternative flag names if needed: --groups-claim-name, --group-team-map etc.
	// We will attempt with current flags; if error indicates unknown flag, retry with alternatives.
	attempt := func(isUpdate bool, id string) error {
		var args []string
		if isUpdate {
			args = append([]string{"auth", "update-oauth", "--id", id}, baseArgs...)
		} else {
			args = append([]string{"auth", "add-oauth"}, baseArgs...)
		}
		_, aerr := b.runForgejoAdminCommand(ctx, args...)
		if aerr != nil {
			low := strings.ToLower(aerr.Error())
			if strings.Contains(low, "unknown flag") || strings.Contains(low, "unknown shorthand") || strings.Contains(low, "flag provided but not defined") {
				// Try alternative flag set: use --group-claim-name vs --groups-claim-name, etc.
				altArgs := []string{
					"--name", "PocketID",
					"--provider", "openIDConnect",
					"--key", strings.TrimSpace(clientID),
					"--secret", strings.TrimSpace(clientSecret),
					"--auto-discover-url", discovery,
					"--scopes", scopes,
					"--groups-claim-name", groupClaim,
					"--admin-group", adminGroup,
					"--group-team-map", teamMap,
					"--group-team-map-removal-enabled",
				}
				var altFull []string
				if isUpdate {
					altFull = append([]string{"auth", "update-oauth", "--id", id}, altArgs...)
				} else {
					altFull = append([]string{"auth", "add-oauth"}, altArgs...)
				}
				_, aerr2 := b.runForgejoAdminCommand(ctx, altFull...)
				if aerr2 != nil {
					return aerr2
				}
				return nil
			}
			return aerr
		}
		return nil
	}
	if id == "" {
		if err := attempt(false, ""); err != nil {
			return fmt.Errorf("add oauth auth source: %w", redactForgejoError(err))
		}
	} else {
		if err := attempt(true, id); err != nil {
			return fmt.Errorf("update oauth auth source: %w", redactForgejoError(err))
		}
	}
	return nil
}

type oauthAppDTO struct {
	ID              int64    `json:"id"`
	Name            string   `json:"name"`
	ClientID        string   `json:"client_id"`
	ClientIDAlt     string   `json:"clientId"`
	ClientSecret    string   `json:"client_secret"`
	ClientSecretAlt string   `json:"clientSecret"`
	RedirectURIs    []string `json:"redirect_uris"`
	RedirectURIsAlt []string `json:"redirectUris"`
	RedirectURI     string   `json:"redirect_uri"`
}

func (d *oauthAppDTO) effectiveClientID() string {
	if s := strings.TrimSpace(d.ClientID); s != "" {
		return s
	}
	return strings.TrimSpace(d.ClientIDAlt)
}
func (d *oauthAppDTO) effectiveClientSecret() string {
	if s := strings.TrimSpace(d.ClientSecret); s != "" {
		return s
	}
	return strings.TrimSpace(d.ClientSecretAlt)
}
func (d *oauthAppDTO) effectiveRedirectURIs() []string {
	if len(d.RedirectURIs) > 0 {
		out := make([]string, 0, len(d.RedirectURIs))
		for _, u := range d.RedirectURIs {
			if s := strings.TrimSpace(u); s != "" {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if len(d.RedirectURIsAlt) > 0 {
		out := make([]string, 0, len(d.RedirectURIsAlt))
		for _, u := range d.RedirectURIsAlt {
			if s := strings.TrimSpace(u); s != "" {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if s := strings.TrimSpace(d.RedirectURI); s != "" {
		return []string{s}
	}
	return nil
}

// ensureWoodpeckerOAuthApp ensures the Woodpecker OAuth2 application exists in Forgejo.
func (b *Backend) ensureWoodpeckerOAuthApp(ctx context.Context, baseURL, token, domain string) (string, string, error) {
	redirect := fmt.Sprintf("https://ci.%s/authorize", strings.TrimSpace(domain))
	listPath := "/api/v1/user/applications/oauth2"
	var apps []oauthAppDTO
	status, err := b.forgejoAPIRequest(ctx, baseURL, token, http.MethodGet, listPath, nil, &apps)
	if err != nil && status != http.StatusNotFound {
		// Try alternative path without /v1 prefix? Some Forgejo versions use /api/v1/user/applications/oauth2
		// If list fails, treat as empty and try to create
		apps = nil
	}
	var existing *oauthAppDTO
	for i := range apps {
		if strings.EqualFold(strings.TrimSpace(apps[i].Name), "Woodpecker") {
			existing = &apps[i]
			break
		}
	}
	if existing != nil {
		effRedirects := existing.effectiveRedirectURIs()
		hasRedirect := false
		for _, u := range effRedirects {
			if strings.TrimSpace(u) == redirect {
				hasRedirect = true
				break
			}
		}
		cid := existing.effectiveClientID()
		sec := existing.effectiveClientSecret()
		if hasRedirect && cid != "" && sec != "" {
			return cid, sec, nil
		}
		// Need to update redirect URIs (and possibly get secret)
		payload := map[string]any{
			"name":          "Woodpecker",
			"redirect_uris": []string{redirect},
		}
		// Include alternative key for compatibility
		payload["redirectUris"] = []string{redirect}
		var updated oauthAppDTO
		patchPath := fmt.Sprintf("/api/v1/user/applications/oauth2/%d", existing.ID)
		_, perr := b.forgejoAPIRequest(ctx, baseURL, token, http.MethodPatch, patchPath, payload, &updated)
		if perr != nil {
			// Fallback to PUT
			_, perr = b.forgejoAPIRequest(ctx, baseURL, token, http.MethodPut, patchPath, payload, &updated)
			if perr != nil {
				return "", "", fmt.Errorf("patch woodpecker oauth: %w", redactForgejoError(perr))
			}
		}
		cid2 := updated.effectiveClientID()
		sec2 := updated.effectiveClientSecret()
		if strings.TrimSpace(cid2) == "" {
			cid2 = cid
		}
		if strings.TrimSpace(sec2) == "" {
			// Try to fetch via GET
			var fetched oauthAppDTO
			if _, ferr := b.forgejoAPIRequest(ctx, baseURL, token, http.MethodGet, patchPath, nil, &fetched); ferr == nil {
				if v := fetched.effectiveClientSecret(); v != "" {
					sec2 = v
				}
				if v := fetched.effectiveClientID(); v != "" && cid2 == "" {
					cid2 = v
				}
			}
			if strings.TrimSpace(sec2) == "" {
				sec2 = sec
			}
		}
		if strings.TrimSpace(cid2) == "" || strings.TrimSpace(sec2) == "" {
			return "", "", fmt.Errorf("woodpecker oauth patch returned empty credentials")
		}
		return strings.TrimSpace(cid2), strings.TrimSpace(sec2), nil
	}
	// Create new
	payload := map[string]any{
		"name":          "Woodpecker",
		"redirect_uris": []string{redirect},
	}
	payload["redirectUris"] = []string{redirect}
	var created oauthAppDTO
	if _, cerr := b.forgejoAPIRequest(ctx, baseURL, token, http.MethodPost, listPath, payload, &created); cerr != nil {
		return "", "", fmt.Errorf("create woodpecker oauth: %w", redactForgejoError(cerr))
	}
	cid := created.effectiveClientID()
	sec := created.effectiveClientSecret()
	if strings.TrimSpace(cid) == "" || strings.TrimSpace(sec) == "" {
		// Some versions return client_id/secret at top level differently, try to fetch
		// Try alternative: maybe response is wrapped in data?
		// For now, try to list again and find
		var apps2 []oauthAppDTO
		if _, err2 := b.forgejoAPIRequest(ctx, baseURL, token, http.MethodGet, listPath, nil, &apps2); err2 == nil {
			for _, a := range apps2 {
				if strings.EqualFold(strings.TrimSpace(a.Name), "Woodpecker") {
					if c := a.effectiveClientID(); c != "" {
						cid = c
					}
					if s := a.effectiveClientSecret(); s != "" {
						sec = s
					}
					break
				}
			}
		}
	}
	if strings.TrimSpace(cid) == "" || strings.TrimSpace(sec) == "" {
		return "", "", fmt.Errorf("woodpecker oauth create returned empty credentials")
	}
	return strings.TrimSpace(cid), strings.TrimSpace(sec), nil
}

// Ensure Woodpecker OAuth client persistence helpers are not logging secrets.
// The caller must use health.RedactDetail for any error wrapping.

// forgejoHTTPClientOverride and forgejoBaseURLOverride are test injectables.
// They are set via Backend fields for tests.
var _ = store.ErrNotFound
var _ = filepath.Join
var _ = health.RedactDetail

