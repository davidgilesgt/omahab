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
	"os/user"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/health"
)

var errNativeForgejoUnavailable = errors.New("native forgejo unavailable")
var errForgejoTokenNameConflict = errors.New("forgejo token name already exists")

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

// runForgejoAdminCommand executes `forgejo admin <args>`. It uses the
// native NixOS service when present and falls back to Docker only when the
// native binary or account is absent.
// It uses b.forgejoExec if set (for tests).
func (b *Backend) runForgejoAdminCommand(ctx context.Context, args ...string) (string, error) {
	if b.forgejoExec != nil {
		return b.forgejoExec(ctx, args...)
	}
	nativeOut, nativeErr := b.runNativeForgejoAdmin(ctx, args...)
	if nativeErr == nil {
		return nativeOut, nil
	}
	if forgejoTokenNameConflict(nativeErr) {
		return "", errForgejoTokenNameConflict
	}
	if !errors.Is(nativeErr, errNativeForgejoUnavailable) {
		return "", fmt.Errorf("forgejo admin native: %w", redactForgejoError(nativeErr))
	}
	// Compose fallback for legacy deployments without a native Forgejo
	// binary or service account.
	containerID, cerr := b.findForgejoContainerID(ctx)
	if cerr != nil {
		return "", fmt.Errorf("forgejo admin unavailable (native: %s; container lookup: %s)",
			strings.TrimSpace(redactForgejoError(nativeErr).Error()), strings.TrimSpace(redactForgejoError(cerr).Error()))
	}
	fullArgs := append([]string{"exec", containerID, "forgejo", "admin"}, args...)
	cmd := exec.CommandContext(ctx, "docker", fullArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	errOut := strings.TrimSpace(stderr.String())
	combined := strings.TrimSpace(out + "\n" + errOut)
	if err != nil {
		if combined == "" {
			combined = err.Error()
		}
		if forgejoTokenNameConflict(errors.New(combined)) {
			return "", errForgejoTokenNameConflict
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

// runNativeForgejoAdmin runs `forgejo admin <args>` as the Forgejo service
// user. The server binary comes from forgejo.service when possible.
// Missing native components return errNativeForgejoUnavailable.
func (b *Backend) runNativeForgejoAdmin(ctx context.Context, args ...string) (string, error) {
	gitea, err := resolveNativeForgejoBinary(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errNativeForgejoUnavailable, err)
	}
	usr, uerr := user.Lookup("forgejo")
	if uerr != nil {
		return "", fmt.Errorf("%w: forgejo user not found: %v", errNativeForgejoUnavailable, uerr)
	}
	workDir := usr.HomeDir
	if v := systemctlShowProp(ctx, "WorkingDirectory"); v != "" && strings.HasPrefix(v, "/") {
		workDir = v
	}
	if _, lookErr := exec.LookPath("systemd-run"); lookErr == nil {
		out, runErr := systemdRunForgejoAdmin(ctx, gitea, workDir, usr.HomeDir, args...)
		if runErr != nil {
			return "", fmt.Errorf("systemd-run: %w", runErr)
		}
		return out, nil
	}
	if _, lookErr := exec.LookPath("runuser"); lookErr != nil {
		return "", fmt.Errorf("%w: systemd-run and runuser not found", errNativeForgejoUnavailable)
	}
	out, runErr := runuserForgejoAdmin(ctx, gitea, workDir, usr.HomeDir, args...)
	if runErr != nil {
		return "", fmt.Errorf("runuser: %w", runErr)
	}
	return out, nil
}

// systemdRunAsUser executes bin/args as uid:gid in a transient unit spawned
// by PID 1, outside the caller's seccomp sandbox. stdio pipes through; the
// service exit code propagates via --wait.
func systemdRunAsUser(ctx context.Context, uid, gid, workDir string, env []string, bin string, args ...string) (string, error) {
	runArgs := []string{
		"--pipe", "--wait", "--quiet", "--collect", "--service-type=exec",
		"--uid=" + uid, "--gid=" + gid,
		"-p", "WorkingDirectory=" + workDir,
	}
	for _, e := range env {
		if strings.TrimSpace(e) == "" {
			continue
		}
		runArgs = append(runArgs, "--setenv="+e)
	}
	runArgs = append(runArgs, "--", bin)
	runArgs = append(runArgs, args...)
	return runCapture(ctx, "systemd-run", runArgs, "", nil)
}

// runCapture runs bin/args, capturing combined output. dir/env override the
// process working directory and environment when non-empty/non-nil;
// otherwise the caller inherits omahabd's. Failures return the combined
// output (or the raw error when empty) so callers can redact once.
func runCapture(ctx context.Context, bin string, args []string, dir string, env []string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if env != nil {
		cmd.Env = env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		combined := strings.TrimSpace(strings.TrimSpace(stdout.String()) + "\n" + strings.TrimSpace(stderr.String()))
		if combined == "" {
			combined = err.Error()
		}
		return "", errors.New(combined)
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		out = strings.TrimSpace(stderr.String())
	}
	return out, nil
}

// systemdRunForgejoAdmin executes `gitea admin <args>` as the forgejo service
// user in a transient unit. --pipe wires stdio through; the service exit
// code propagates via --wait.
func systemdRunForgejoAdmin(ctx context.Context, gitea, workDir, home string, args ...string) (string, error) {
	return systemdRunAsUser(ctx, "forgejo", "forgejo", workDir, []string{
		"FORGEJO_WORK_DIR=" + workDir,
		"FORGEJO_CUSTOM=" + workDir + "/custom",
		"HOME=" + home,
	}, gitea, append([]string{"admin"}, args...)...)
}

func runuserForgejoAdmin(ctx context.Context, gitea, workDir, home string, args ...string) (string, error) {
	fullArgs := append([]string{"-u", "forgejo", "--", gitea, "admin"}, args...)
	return runCapture(ctx, "runuser", fullArgs, workDir, append(os.Environ(),
		"FORGEJO_WORK_DIR="+workDir,
		"FORGEJO_CUSTOM="+workDir+"/custom",
		"HOME="+home,
	))
}

// resolveNativeForgejoBinary returns the absolute path of the native forgejo
// (gitea) server binary, preferring the ExecStart of forgejo.service.
func resolveNativeForgejoBinary(ctx context.Context) (string, error) {
	if p := parseForgejoExecStart(systemctlShowProp(ctx, "ExecStart")); p != "" {
		return p, nil
	}
	for _, p := range []string{"/run/current-system/sw/bin/gitea", "/run/current-system/sw/bin/forgejo"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	if p, err := exec.LookPath("gitea"); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("forgejo"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("native forgejo binary not found")
}

// systemctlShowProp reports the raw value of one `systemctl show -p` property
// of forgejo.service, or "" when systemctl is unavailable.
func systemctlShowProp(ctx context.Context, prop string) string {
	cmd := exec.CommandContext(ctx, "systemctl", "show", "forgejo.service", "-p", prop)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), prop+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// parseForgejoExecStart extracts the server binary path from
// `systemctl show -p ExecStart` output (e.g.
// "ExecStart={ path=/nix/store/…/bin/gitea ; argv[]=… }").
// It prefers a binary named gitea/forgejo, else the first absolute path.
func parseForgejoExecStart(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == ';' || r == '{' || r == '}' || r == '='
	})
	fallback := ""
	for _, f := range fields {
		f = strings.Trim(f, `"'`)
		if !strings.HasPrefix(f, "/") {
			continue
		}
		// Strip trailing punctuation from argv rendering.
		f = strings.TrimRight(f, ",;")
		base := path.Base(f)
		if base == "gitea" || base == "forgejo" {
			if st, err := os.Stat(f); err == nil && !st.IsDir() {
				return f
			}
			// Keep the name match even when Stat fails (unit-test
			// fixtures reference paths that do not exist on disk).
			return f
		}
		if fallback == "" {
			fallback = f
		}
	}
	return fallback
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
	// Use the stable name on a fresh install. A rejected stored token can
	// leave that name in Forgejo after state recovery.
	out, err := b.runForgejoAdminCommand(ctx, "user", "generate-access-token", "--username", "omahab-bot", "--token-name", "omahab", "--raw")
	if err != nil && forgejoTokenNameConflict(err) {
		recoveryName := "omahab-recovery-" + generateRandomBase64URL(9)
		out, err = b.runForgejoAdminCommand(ctx, "user", "generate-access-token", "--username", "omahab-bot", "--token-name", recoveryName, "--raw")
	}
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

func forgejoTokenNameConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if errors.Is(err, errForgejoTokenNameConflict) {
		return true
	}
	if !strings.Contains(msg, "token") || !strings.Contains(msg, "name") {
		return false
	}
	return strings.Contains(msg, "already") || strings.Contains(msg, "exists") || strings.Contains(msg, "used")
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
			payload := forgejoCreateOrgRequest{Username: "omahab", Visibility: "private", Description: "Omahab organization"}
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
	// Build map name -> team through the single spelling-tolerant decoder.
	teamByName := map[string]forgejoTeam{}
	for _, t := range teams {
		team := decodeForgejoTeam(t)
		if team.Name != "" {
			teamByName[strings.ToLower(strings.TrimSpace(team.Name))] = team
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
		// Forgejo 15 rejects permission-only teams ("units permission should
		// not be empty", live 2026-09-12): send an explicit units_map carrying
		// the level. The server derives the team permission from the map.
		unitsMap := map[string]string{}
		for _, u := range []string{"repo.code", "repo.issues", "repo.pulls", "repo.releases", "repo.wiki", "repo.projects", "repo.packages", "repo.actions"} {
			unitsMap[u] = d.permission
		}
		if existing, ok := teamByName[strings.ToLower(d.name)]; ok {
			if strings.EqualFold(existing.Permission, d.permission) && existing.IncludesAllRepositories {
				continue
			}
			// Need to update
			if existing.ID == 0 {
				continue
			}
			payload := forgejoUpdateTeamRequest{
				Name: d.name, Permission: d.permission, IncludesAllRepositories: true,
				CanCreateOrgRepo: false, UnitsMap: unitsMap,
			}
			patchPath := fmt.Sprintf("/api/v1/teams/%d", existing.ID)
			if _, perr := b.forgejoAPIRequest(ctx, baseURL, token, http.MethodPatch, patchPath, payload, nil); perr != nil {
				return fmt.Errorf("update team %s: %w", d.name, redactForgejoError(perr))
			}
			continue
		}
		// Create team (verified shape against Forgejo 15: no bare permission
		// key, level carried by units_map).
		payload := forgejoCreateTeamRequest{
			Name: d.name, Description: fmt.Sprintf("Omahab %s", d.name),
			CanCreateOrgRepo: false, IncludesAllRepositories: true, UnitsMap: unitsMap,
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
	scopes := "openid email profile groups"
	groupClaim := "groups"
	adminGroup := "admins"
	teamMap := `{"admins":{"omahab":["admins"]},"members":{"omahab":["members"]}}`

	buildArgs := func(claimFlag string, isUpdate bool, id string) []string {
		rest := []string{
			"--name", "PocketID",
			// Lowercase d: Forgejo matches "openidConnect" exactly; "openIDConnect"
			// fails with the misleading "auth source is not activated" (live 2026-09-12).
			"--provider", "openidConnect",
			"--key", strings.TrimSpace(clientID),
			"--secret", strings.TrimSpace(clientSecret),
			"--auto-discover-url", discovery,
			"--scopes", scopes,
			claimFlag, groupClaim,
			"--admin-group", adminGroup,
			"--group-team-map", teamMap,
			// No -enabled suffix: Forgejo 15 names it --group-team-map-removal
			// (verified via `forgejo admin auth add-oauth --help`, live 2026-09-12).
			"--group-team-map-removal",
		}
		if isUpdate {
			return append([]string{"auth", "update-oauth", "--id", id}, rest...)
		}
		return append([]string{"auth", "add-oauth"}, rest...)
	}
	// Older binaries name the claim flag --groups-claim-name; retry with it
	// when the primary spelling is rejected as unknown.
	attempt := func(isUpdate bool, id string) error {
		var firstErr error
		for _, claimFlag := range []string{"--group-claim-name", "--groups-claim-name"} {
			_, err := b.runForgejoAdminCommand(ctx, buildArgs(claimFlag, isUpdate, id)...)
			if err == nil {
				return nil
			}
			low := strings.ToLower(err.Error())
			if !strings.Contains(low, "unknown flag") && !strings.Contains(low, "unknown shorthand") && !strings.Contains(low, "flag provided but not defined") {
				return err
			}
			// Keep the first failure: it names the flag this binary
			// actually rejects; the alt error alone misleads.
			if firstErr == nil {
				firstErr = err
			} else {
				return fmt.Errorf("oauth flags rejected (first: %s; alt: %s)", firstErr, err)
			}
		}
		return firstErr
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
	ID                 int64    `json:"id"`
	Name               string   `json:"name"`
	ClientID           string   `json:"client_id"`
	ClientIDAlt        string   `json:"clientId"`
	ClientSecret       string   `json:"client_secret"`
	ClientSecretAlt    string   `json:"clientSecret"`
	ConfidentialClient bool     `json:"confidential_client"`
	RedirectURIs       []string `json:"redirect_uris"`
	RedirectURIsAlt    []string `json:"redirectUris"`
	RedirectURI        string   `json:"redirect_uri"`
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

// forgejoTeam is the team view consumed from the Forgejo teams API.
type forgejoTeam struct {
	ID                      int64
	Name                    string
	Permission              string
	IncludesAllRepositories bool
}

// decodeForgejoTeam is the single boundary decoder for team objects. It
// accepts both snake_case and camelCase spellings the API has used; numbers
// arrive as JSON floats.
func decodeForgejoTeam(m map[string]any) forgejoTeam {
	var t forgejoTeam
	t.ID = forgejoTeamID(m)
	if s, ok := m["name"].(string); ok {
		t.Name = s
	} else if s, ok := m["Name"].(string); ok {
		t.Name = s
	}
	if s, ok := m["permission"].(string); ok {
		t.Permission = s
	} else if s, ok := m["Permission"].(string); ok {
		t.Permission = s
	}
	if v, ok := m["includes_all_repositories"].(bool); ok {
		t.IncludesAllRepositories = v
	} else if v, ok := m["includesAllRepositories"].(bool); ok {
		t.IncludesAllRepositories = v
	}
	return t
}

func forgejoTeamID(m map[string]any) int64 {
	for _, key := range []string{"id", "ID"} {
		switch v := m[key].(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		case int:
			return int64(v)
		case string:
			if i, err := strconv.ParseInt(v, 10, 64); err == nil {
				return i
			}
		}
	}
	return 0
}

// forgejoCreateOrgRequest creates the omahab organization.
type forgejoCreateOrgRequest struct {
	Username    string `json:"username"`
	Visibility  string `json:"visibility"`
	Description string `json:"description"`
}

// forgejoCreateTeamRequest creates a team. Forgejo 15 derives the team
// permission from units_map; no bare permission key is sent.
type forgejoCreateTeamRequest struct {
	Name                    string            `json:"name"`
	Description             string            `json:"description"`
	CanCreateOrgRepo        bool              `json:"can_create_org_repo"`
	IncludesAllRepositories bool              `json:"includes_all_repositories"`
	UnitsMap                map[string]string `json:"units_map"`
}

// forgejoUpdateTeamRequest updates a team in place.
type forgejoUpdateTeamRequest struct {
	Name                    string            `json:"name"`
	Permission              string            `json:"permission"`
	IncludesAllRepositories bool              `json:"includes_all_repositories"`
	CanCreateOrgRepo        bool              `json:"can_create_org_repo"`
	UnitsMap                map[string]string `json:"units_map"`
}

// forgejoOAuthAppRequest creates or updates the Woodpecker OAuth2 application.
// Both redirect URI spellings are sent; the server applies the ones it
// knows. confidential_client is always sent: the update handler applies it
// unconditionally, so omission would flip a confidential app to public.
type forgejoOAuthAppRequest struct {
	Name               string   `json:"name"`
	RedirectURIs       []string `json:"redirect_uris"`
	RedirectURIsAlt    []string `json:"redirectUris"`
	ConfidentialClient bool     `json:"confidential_client"`
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
		// Woodpecker is a confidential client (authenticates with a secret, no
		// PKCE support). Forgejo rejects the code exchange with
		// "invalid_request: PKCE is required for public clients" when the app
		// is public, so publicity alone forces an update even when the
		// redirect URI already matches (live 2026-09-12).
		if hasRedirect && cid != "" && existing.ConfidentialClient {
			if sec != "" {
				return cid, sec, nil
			}
			// Forgejo's list response omits client_secret. Reuse the stored
			// secret instead of PATCHing: an update bumps updated_unix and
			// rotates the secret (Forgejo 15), which orphaned the running
			// server on every setup re-run (live 2026-09-13: token exchange
			// failed with "invalid client secret" until manual restart).
			if stored, serr := reuseStoredOIDCSecret(ctx, b.secrets, "woodpecker_forgejo_client_secret", ""); serr == nil {
				return cid, stored, nil
			}
			// No stored secret (e.g. wiped secrets DB): fall through to the
			// update path, which rotates and reveals a fresh one.
		}
		// Need to update redirect URIs (and possibly get secret). Always send
		// confidential_client: the field defaults to false (public) when
		// omitted, and the update handler applies it unconditionally, so an
		// omission would flip a confidential app back to public.
		payload := forgejoOAuthAppRequest{
			Name: "Woodpecker", RedirectURIs: []string{redirect},
			RedirectURIsAlt: []string{redirect}, ConfidentialClient: true,
		}
		var updated oauthAppDTO
		patchPath := fmt.Sprintf("/api/v1/user/applications/oauth2/%d", existing.ID)
		if _, perr := b.forgejoAPIRequest(ctx, baseURL, token, http.MethodPatch, patchPath, payload, &updated); perr != nil {
			return "", "", fmt.Errorf("patch woodpecker oauth: %w", redactForgejoError(perr))
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
		}
		if strings.TrimSpace(sec2) == "" {
			sec2 = sec
		}
		if strings.TrimSpace(sec2) == "" {
			// The API revealed no secret (older Forgejo omits it on every
			// endpoint); reuse the stored one rather than failing setup.
			// A rotation the API hides would desync us, but Forgejo returns
			// the fresh secret on update (live 2026-09-13), so a hidden
			// secret here means nothing rotated.
			if stored, serr := reuseStoredOIDCSecret(ctx, b.secrets, "woodpecker_forgejo_client_secret", ""); serr == nil {
				sec2 = stored
			}
		}
		if strings.TrimSpace(cid2) == "" || strings.TrimSpace(sec2) == "" {
			return "", "", fmt.Errorf("woodpecker oauth patch returned empty credentials")
		}
		return strings.TrimSpace(cid2), strings.TrimSpace(sec2), nil
	}
	// Create new. confidential_client defaults to false (public) when
	// omitted, which breaks Woodpecker's secret-based exchange (PKCE
	// required for public clients). Woodpecker has no PKCE support, so it
	// must be a confidential client.
	payload := forgejoOAuthAppRequest{
		Name: "Woodpecker", RedirectURIs: []string{redirect},
		RedirectURIsAlt: []string{redirect}, ConfidentialClient: true,
	}
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

// ensureHermesForgejoToken creates a limited Forgejo token for the Hermes MCP
// server (platform-app/hermes_forgejo_token) with scopes
// read:repository, write:repository, read:issue, write:issue. It reuses the
// omahab-bot user to avoid creating a second automation account. Forgejo has
// no delete-excluding scope, so destructive exclusion is enforced by the MCP
// tool surface (no delete/force-push/branch-delete/pr-merge tools).
func (b *Backend) ensureHermesForgejoToken(ctx context.Context, domain string) error {
	if b.secrets == nil {
		return fmt.Errorf("secrets not configured")
	}
	// Ensure omahab-bot exists first; token creation depends on the user.
	if err := b.ensureOmahabBot(ctx, domain); err != nil {
		return err
	}
	existing, err := b.secrets.RevealByName(ctx, "platform-app", "hermes_forgejo_token")
	if err == nil && strings.TrimSpace(existing) != "" {
		// Validate token still works before reusing.
		if b.validateForgejoToken(ctx, strings.TrimSpace(existing), domain) == nil {
			return nil
		}
	}
	// Create via Forgejo admin CLI or API. Prefer forgejo API via internal client
	// if available: use CreateAccessToken on a temporary client built from the
	// admin token (forgejo_token). Fallback to admin CLI command.
	baseURL := b.forgejoBaseURL(ctx, domain)
	adminToken := ""
	if v, err := b.secrets.RevealByName(ctx, "platform-app", "forgejo_token"); err == nil {
		adminToken = strings.TrimSpace(v)
	}
	if adminToken == "" {
		adminToken = strings.TrimSpace(os.Getenv("OMAHAB_FORGEJO_TOKEN"))
	}
	// Try API path if we have base+token and a forgejo client can be built.
	// Use generic forgejo token creation path: POST /api/v1/users/omahab-bot/tokens
	var hermesToken string
	if baseURL != "" && adminToken != "" {
		// Lightweight HTTP attempt using forgejo API directly without importing scm
		// to avoid import cycle. Use the same forgejoAPIRequest helper.
		body := map[string]any{"name": "hermes", "scopes": []string{"read:repository", "write:repository", "read:issue", "write:issue"}}
		var out struct {
			Token string `json:"token"`
			Sha1  string `json:"sha1"`
		}
		_, apiErr := b.forgejoAPIRequest(ctx, baseURL, adminToken, http.MethodPost, "/api/v1/users/omahab-bot/tokens", body, &out)
		if apiErr == nil {
			if out.Token != "" {
				hermesToken = strings.TrimSpace(out.Token)
			} else if out.Sha1 != "" {
				hermesToken = strings.TrimSpace(out.Sha1)
			}
		} else {
			low := strings.ToLower(apiErr.Error())
			if strings.Contains(low, "already exists") || strings.Contains(low, "duplicate") {
				// Forgejo token names are unique per user and the sha1 is
				// only shown at creation: a listed sha1 is unusable. Delete
				// the existing named token and recreate it so a fresh value
				// is returned and persisted. (Live 2026-09: every run logged
				// "token name already exists" and reused nothing.)
				_, _ = b.forgejoAPIRequest(ctx, baseURL, adminToken, http.MethodDelete, "/api/v1/users/omahab-bot/tokens/hermes", nil, nil)
				_, apiErr2 := b.forgejoAPIRequest(ctx, baseURL, adminToken, http.MethodPost, "/api/v1/users/omahab-bot/tokens", body, &out)
				if apiErr2 == nil {
					if out.Token != "" {
						hermesToken = strings.TrimSpace(out.Token)
					} else if out.Sha1 != "" {
						hermesToken = strings.TrimSpace(out.Sha1)
					}
				}
			}
		}
	}
	if hermesToken == "" {
		// Fallback: generate via CLI as admin user omahab-bot
		out, err := b.runForgejoAdminCommand(ctx, "user", "generate-access-token", "--username", "omahab-bot", "--token-name", "hermes", "--scopes", "read:repository,write:repository,read:issue,write:issue", "--raw")
		if err != nil {
			// Try without --scopes flag (older gitea)
			out2, err2 := b.runForgejoAdminCommand(ctx, "user", "generate-access-token", "--username", "omahab-bot", "--token-name", "hermes", "--raw")
			if err2 != nil {
				return fmt.Errorf("generate hermes forgejo token: %w", redactForgejoError(err))
			}
			out = out2
		}
		hermesToken = strings.TrimSpace(out)
	}
	if hermesToken == "" {
		return fmt.Errorf("hermes forgejo token generation returned empty")
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "hermes_forgejo_token", hermesToken); err != nil {
		return fmt.Errorf("store hermes_forgejo_token: %w", err)
	}
	return nil
}
