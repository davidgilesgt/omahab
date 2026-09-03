package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// EnvironmentBundle is the authoritative bundle fetched from the server.
// Variables are the raw values for this device, already composed per-device
// (including reserved OPENAI_*, ANTHROPIC_*, OMAHAB_MODEL_*). The bundle is
// delivered only via device authentication at GET /api/v1/companion/environment.
type EnvironmentBundle struct {
	Revision  int               `json:"revision"`
	Variables map[string]string `json:"variables"`
}

// SystemdDBus abstracts the session D-Bus org.freedesktop.systemd1.Manager
// so tests can inject a fake without a real systemd user manager.
// Production uses godbus/dbus/v5 to call SetEnvironment/UnsetEnvironment.
type SystemdDBus interface {
	SetEnvironment(assignments []string) error
	UnsetEnvironment(names []string) error
}

// EnvironmentManager fetches the authoritative tool environment, atomically
// persists it to environment.d/90-omahab-agent-tools.conf (0600), and live-
// applies it to the systemd user manager via D-Bus SetEnvironment/UnsetEnvironment.
// It never logs or surfaces values, never uses systemctl argv, and keeps the
// last complete file on any failure.
type EnvironmentManager struct {
	remote   *RemoteClient
	creds    CredentialStore
	logger   *slog.Logger
	mu       sync.RWMutex
	etag     string
	revision int
	count    int
	syncedAt *time.Time
	lastErr  string
	filePath string
	dbus     SystemdDBus
}

// EnvironmentManagerOpts configures the manager.
type EnvironmentManagerOpts struct {
	Remote   *RemoteClient
	Creds    CredentialStore
	Logger   *slog.Logger
	FilePath string // override for tests; default is EnvironmentFilePath()
	DBus     SystemdDBus
}

// NewEnvironmentManager creates a manager. Remote may be nil (offline mode) — Sync will then fail gracefully without touching the file.
func NewEnvironmentManager(opts EnvironmentManagerOpts) *EnvironmentManager {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	fp := opts.FilePath
	if fp == "" {
		fp = EnvironmentFilePath()
	}
	creds := opts.Creds
	if creds == nil && opts.Remote != nil {
		// Prefer the remote's credential store if not explicitly given.
		creds = opts.Remote.creds
	}
	return &EnvironmentManager{
		remote:   opts.Remote,
		creds:    creds,
		logger:   logger,
		filePath: fp,
		dbus:     opts.DBus,
	}
}

// EnvironmentFilePath returns the managed file path.
// Linux: $XDG_CONFIG_HOME/environment.d/90-omahab-agent-tools.conf
// Darwin: $XDG_CONFIG_HOME/omahab/agent-tools.env (or ~/.config/omahab/agent-tools.env)
// The darwin path is sourced from shell rc via a one-line include written on enroll.
func EnvironmentFilePath() string {
	if runtimeGOOS() == "darwin" {
		if dir := os.Getenv("XDG_CONFIG_HOME"); strings.TrimSpace(dir) != "" {
			return filepath.Join(strings.TrimSpace(dir), "omahab", "agent-tools.env")
		}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, ".config", "omahab", "agent-tools.env")
		}
		return filepath.Join(os.TempDir(), "omahab-agent-tools.env")
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); strings.TrimSpace(dir) != "" {
		return filepath.Join(strings.TrimSpace(dir), "environment.d", "90-omahab-agent-tools.conf")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "environment.d", "90-omahab-agent-tools.conf")
	}
	return filepath.Join(os.TempDir(), "90-omahab-agent-tools.conf")
}

func runtimeGOOS() string {
	return runtime.GOOS
}

// Status returns redacted sync state for DaemonStatus/QML. Values never included.
func (m *EnvironmentManager) Status() (revision int, count int, syncedAt *time.Time, errStr string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.revision, m.count, m.syncedAt, m.lastErr
}

// ETag returns the last known ETag (for testing).
func (m *EnvironmentManager) ETag() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.etag
}

// Sync fetches the bundle with If-None-Match, handles 304, atomically writes the file, and applies via D-Bus.
// On any fetch or D-Bus failure it keeps the last complete file and records a safe error without values.
func (m *EnvironmentManager) Sync(ctx context.Context) error {
	if m.remote == nil {
		err := fmt.Errorf("no remote configured")
		m.setError(sanitizeEnvError(err))
		return err
	}
	m.mu.RLock()
	etag := m.etag
	m.mu.RUnlock()

	bundle, newETag, notModified, err := m.fetchBundle(ctx, etag)
	if err != nil {
		// Keep last complete file, never partial, never empty overwrite, never fake success.
		safe := sanitizeEnvError(err)
		m.setError(safe)
		m.logger.Warn("environment sync fetch failed", "err", safe)
		return err
	}
	if notModified {
		now := time.Now().UTC()
		m.mu.Lock()
		m.syncedAt = &now
		m.lastErr = ""
		m.mu.Unlock()
		m.logger.Info("environment not modified (304)", "revision", m.revision)
		return nil
	}
	if bundle == nil {
		err = fmt.Errorf("empty bundle")
		safe := sanitizeEnvError(err)
		m.setError(safe)
		return err
	}
	if bundle.Variables == nil {
		bundle.Variables = map[string]string{}
	}

	// Validate names/values early to avoid writing a corrupt file.
	for k, v := range bundle.Variables {
		if err := validateEnvName(k); err != nil {
			safe := fmt.Sprintf("invalid variable name %q: %v", k, err)
			m.setError(safe)
			return fmt.Errorf("%s", safe)
		}
		if strings.Contains(v, "\x00") || strings.Contains(v, "\n") || strings.Contains(v, "\r") {
			safe := fmt.Sprintf("invalid variable value for %q: contains NUL/CR/LF", k)
			m.setError(safe)
			return fmt.Errorf("%s", safe)
		}
	}

	prevKeys := m.readManagedKeys()
	newData := renderEnvironmentFile(bundle.Variables)

	// Capture previous file for rollback on D-Bus failure.
	filePath := m.filePath
	if filePath == "" {
		filePath = EnvironmentFilePath()
		m.filePath = filePath
	}
	oldData, hadOld := m.readFileBytes(filePath)

	// Atomic write 0600.
	if err := atomicWriteFile0600(filePath, newData); err != nil {
		safe := fmt.Sprintf("write file failed: %v", sanitizeEnvError(err))
		m.setError(safe)
		m.logger.Error("environment file write failed", "err", sanitizeEnvError(err))
		return err
	}

	// D-Bus live apply: unset removed, then set current.
	// Determine removed keys.
	var toRemove []string
	for _, k := range prevKeys {
		if _, ok := bundle.Variables[k]; !ok {
			toRemove = append(toRemove, k)
		}
	}
	var toSet []string
	for k, v := range bundle.Variables {
		toSet = append(toSet, k+"="+v)
	}
	sort.Strings(toRemove)
	sort.Strings(toSet)

	dbusClient := m.dbus
	if dbusClient == nil {
		dbusClient = &realDBus{}
	}

	if len(toRemove) > 0 {
		if err := dbusClient.UnsetEnvironment(toRemove); err != nil {
			// Revert file to keep previous active (never partial).
			m.revertFile(filePath, oldData, hadOld)
			safe := fmt.Sprintf("D-Bus UnsetEnvironment failed: %v", sanitizeEnvError(err))
			m.setError(safe)
			m.logger.Error("environment D-Bus Unset failed", "err", sanitizeEnvError(err))
			return err
		}
	}
	if len(toSet) > 0 {
		if err := dbusClient.SetEnvironment(toSet); err != nil {
			// Revert file; we already unset some keys above. Best-effort restore previous live env.
			m.revertFile(filePath, oldData, hadOld)
			if hadOld {
				// Attempt to restore previous live environment for the keys we just unset.
				// Re-parse old values from oldData and set them back.
				restored := m.parseFileAssignments(oldData)
				var toRestore []string
				for _, k := range toRemove {
					if v, ok := restored[k]; ok {
						toRestore = append(toRestore, k+"="+v)
					}
				}
				// Also restore previous values for keys that were to be set (they may have been changed).
				// We don't know prior live values for those, but file revert ensures next login restores.
				if len(toRestore) > 0 {
					_ = dbusClient.SetEnvironment(toRestore)
				}
			}
			safe := fmt.Sprintf("D-Bus SetEnvironment failed: %v", sanitizeEnvError(err))
			m.setError(safe)
			m.logger.Error("environment D-Bus Set failed", "err", sanitizeEnvError(err))
			return err
		}
	}
	now := time.Now().UTC()
	etagToStore := newETag
	if strings.TrimSpace(etagToStore) == "" {
		etagToStore = fmt.Sprintf(`W/"%d"`, bundle.Revision)
	}
	// Darwin: ensure shell rc sources the env file (best-effort).
	if runtimeGOOS() == "darwin" {
		if applier := newDarwinEnvApplierShim(); applier != nil {
			_ = applier.EnsureShellInclude()
		}
	}
	m.mu.Lock()
	m.etag = etagToStore
	m.revision = bundle.Revision
	m.count = len(bundle.Variables)
	m.syncedAt = &now
	m.lastErr = ""
	m.mu.Unlock()
	m.logger.Info("environment synced", "revision", bundle.Revision, "count", len(bundle.Variables))
	return nil
}

// newDarwinEnvApplierShim returns a darwin EnvApplier for EnsureShellInclude without importing platform (avoid cycle).
// On linux it returns nil.
func newDarwinEnvApplierShim() interface{ EnsureShellInclude() error } {
	if runtimeGOOS() != "darwin" {
		return nil
	}
	// Dynamic import via platform would create cycle; implement inline minimal shim that mirrors platform.DarwinEnvApplier.EnsureShellInclude.
	return &darwinShellInclude{}
}

type darwinShellInclude struct{}

func (d *darwinShellInclude) EnsureShellInclude() error {
	envFile := EnvironmentFilePath()
	includeLine := fmt.Sprintf(`[ -f "%s" ] && set -a && . "%s" && set +a # omahab-agent-tools`, envFile, envFile)
	// Legacy fixed path line for compatibility
	legacy := `[ -f "$HOME/.config/omahab/agent-tools.env" ] && set -a && . "$HOME/.config/omahab/agent-tools.env" && set +a # omahab-agent-tools`
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
		if home == "" {
			return nil
		}
	}
	candidates := []string{
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".bash_profile"),
		filepath.Join(home, ".config", "fish", "config.fish"),
	}
	for _, rc := range candidates {
		if _, err := os.Stat(rc); err != nil {
			if os.IsNotExist(err) && strings.Contains(rc, "fish") {
				_ = os.MkdirAll(filepath.Dir(rc), 0755)
			} else {
				continue
			}
		}
		data, err := os.ReadFile(rc)
		if err != nil {
			continue
		}
		content := string(data)
		if strings.Contains(content, "omahab-agent-tools") {
			continue
		}
		f, err := os.OpenFile(rc, os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			continue
		}
		if !strings.HasSuffix(content, "\n") && len(content) > 0 {
			_, _ = f.WriteString("\n")
		}
		if strings.Contains(rc, "fish") {
			fishLine := fmt.Sprintf("test -f %s; and set -a; and source %s; and set +a # omahab-agent-tools\n", envFile, envFile)
			_, _ = f.WriteString(fishLine)
		} else {
			// Prefer legacy line if envFile is the default darwin path
			line := includeLine
			if envFile == filepath.Join(home, ".config", "omahab", "agent-tools.env") {
				line = legacy
			}
			_, _ = f.WriteString(line + "\n")
		}
		_ = f.Close()
	}
	return nil
}

// Clear removes the managed file and unsets only names recorded in that file via D-Bus.
// It does not revoke server grants or delete unrelated entries.
func (m *EnvironmentManager) Clear(ctx context.Context) error {
	filePath := m.filePath
	if filePath == "" {
		filePath = EnvironmentFilePath()
		m.filePath = filePath
	}
	keys := m.readManagedKeys()
	oldData, hadOld := m.readFileBytes(filePath)

	// Remove file first? Spec says clear removes the managed file and unsets via D-Bus UnsetEnvironment.
	// We remove file, then D-Bus unset. If D-Bus fails, we restore file.
	if hadOld {
		if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
			safe := fmt.Sprintf("remove file failed: %v", sanitizeEnvError(err))
			m.setError(safe)
			return err
		}
		// fsync dir after remove
		_ = fsyncDir(filepath.Dir(filePath))
	}

	// If no keys recorded, still consider success (file already absent).
	if len(keys) == 0 {
		m.mu.Lock()
		m.revision = 0
		m.count = 0
		m.etag = ""
		now := time.Now().UTC()
		m.syncedAt = &now
		m.lastErr = ""
		m.mu.Unlock()
		m.logger.Info("environment cleared (no keys)")
		return nil
	}

	dbusClient := m.dbus
	if dbusClient == nil {
		dbusClient = &realDBus{}
	}
	sort.Strings(keys)
	if err := dbusClient.UnsetEnvironment(keys); err != nil {
		// Restore file on D-Bus failure (keep previous active, never partial).
		if hadOld {
			_ = atomicWriteFile0600(filePath, oldData)
		}
		safe := fmt.Sprintf("D-Bus UnsetEnvironment failed: %v", sanitizeEnvError(err))
		m.setError(safe)
		m.logger.Error("environment clear D-Bus Unset failed", "err", sanitizeEnvError(err))
		return err
	}

	m.mu.Lock()
	m.revision = 0
	m.count = 0
	m.etag = ""
	now := time.Now().UTC()
	m.syncedAt = &now
	m.lastErr = ""
	m.mu.Unlock()
	m.logger.Info("environment cleared", "count", len(keys))
	return nil
}

func (m *EnvironmentManager) setError(s string) {
	m.mu.Lock()
	m.lastErr = s
	m.mu.Unlock()
}

func (m *EnvironmentManager) readFileBytes(path string) ([]byte, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false
		}
		return nil, false
	}
	return data, true
}

func (m *EnvironmentManager) revertFile(path string, oldData []byte, hadOld bool) {
	if !hadOld {
		_ = os.Remove(path)
		_ = fsyncDir(filepath.Dir(path))
		return
	}
	_ = atomicWriteFile0600(path, oldData)
}

func (m *EnvironmentManager) readManagedKeys() []string {
	path := m.filePath
	if path == "" {
		path = EnvironmentFilePath()
	}
	return parseEnvFileKeys(path)
}

func (m *EnvironmentManager) parseFileAssignments(data []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:idx])
		v := strings.TrimSpace(line[idx+1:])
		// Remove surrounding quotes and unescape.
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			inner := v[1 : len(v)-1]
			// Reverse escaping: \\ \" \$ \`
			// Need to handle \  escaping correctly.
			inner = strings.ReplaceAll(inner, "\\`", "`")
			inner = strings.ReplaceAll(inner, "\\$", "$")
			inner = strings.ReplaceAll(inner, "\\\"", "\"")
			inner = strings.ReplaceAll(inner, "\\\\", "\\")
			v = inner
		}
		out[k] = v
	}
	return out
}

// parseFileAssignments is a package-level helper for mcp.go (which needs to parse env files without a manager).
// It mirrors EnvironmentManager.parseFileAssignments but is accessible package-wide.
func parseFileAssignments(data []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:idx])
		v := strings.TrimSpace(line[idx+1:])
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			inner := v[1 : len(v)-1]
			inner = strings.ReplaceAll(inner, "\\`", "`")
			inner = strings.ReplaceAll(inner, "\\$", "$")
			inner = strings.ReplaceAll(inner, "\\\"", "\"")
			inner = strings.ReplaceAll(inner, "\\\\", "\\")
			v = inner
		}
		out[k] = v
	}
	return out
}
// fetchBundle performs GET /api/v1/companion/environment with If-None-Match and device-token auth.
// Returns bundle, ETag, notModified, error. Never includes values in errors.
func (m *EnvironmentManager) fetchBundle(ctx context.Context, ifNoneMatch string) (*EnvironmentBundle, string, bool, error) {
	if m.remote == nil {
		return nil, "", false, fmt.Errorf("no remote configured")
	}
	// Use RemoteClient's httpClient and baseURL via exported accessors or via private fields (same package).
	// We have access to private fields because same package.
	baseURL := m.remote.baseURL
	if baseURL == "" {
		return nil, "", false, fmt.Errorf("no remote base URL")
	}
	hc := m.remote.httpClient
	if hc == nil {
		hc = http.DefaultClient
	}
	u := strings.TrimRight(baseURL, "/") + "/api/v1/companion/environment"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", false, err
	}
	// Device-token first, then server-token fallback.
	var token string
	if m.creds != nil {
		if t, _ := m.creds.Get(CredentialService, "device-token"); t != "" {
			token = t
		} else if m.remote.creds != nil {
			if t2, _ := m.remote.creds.Get(CredentialService, "device-token"); t2 != "" {
				token = t2
			}
		}
	}
	if token == "" && m.remote.creds != nil {
		if t, _ := m.remote.creds.Get(CredentialService, CredentialAccount); t != "" {
			token = t
		}
	}
	if token == "" && m.creds != nil {
		if t, _ := m.creds.Get(CredentialService, CredentialAccount); t != "" {
			token = t
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(ifNoneMatch) != "" {
		req.Header.Set("If-None-Match", strings.TrimSpace(ifNoneMatch))
	}

	resp, err := hc.Do(req)
	if err != nil {
		return nil, "", false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil, resp.Header.Get("ETag"), true, nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, "", false, fmt.Errorf("not authenticated (401)")
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, "", false, fmt.Errorf("forbidden (403)")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		// Do not include values; body is safe metadata only.
		return nil, "", false, fmt.Errorf("fetch environment: %d %s", resp.StatusCode, msg)
	}
	etag := resp.Header.Get("ETag")
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, "", false, err
	}
	bundle, err := parseEnvironmentBundle(data)
	if err != nil {
		return nil, "", false, fmt.Errorf("decode environment: %w", err)
	}
	// If server didn't send ETag, synthesize from revision.
	if strings.TrimSpace(etag) == "" && bundle.Revision != 0 {
		etag = fmt.Sprintf(`W/"%d"`, bundle.Revision)
	}
	return bundle, etag, false, nil
}

func parseEnvironmentBundle(data []byte) (*EnvironmentBundle, error) {
	if len(data) == 0 {
		return &EnvironmentBundle{Variables: map[string]string{}}, nil
	}
	// Try structured {"revision":..., "variables":{...}}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	// If top-level is map[string]string without revision/variables wrapper, treat as variables.
	if _, hasRev := raw["revision"]; !hasRev {
		if _, hasVars := raw["variables"]; !hasVars {
			if _, hasValues := raw["values"]; !hasValues {
				if _, hasItems := raw["items"]; !hasItems {
					// Try flat map[string]string
					var flat map[string]string
					if err := json.Unmarshal(data, &flat); err == nil {
						// Heuristic: if all values are strings and keys look like env names, treat as variables.
						// But revision unknown -> 0.
						return &EnvironmentBundle{Revision: 0, Variables: flat}, nil
					}
				}
			}
		}
	}
	var rev int
	if v, ok := raw["revision"]; ok {
		_ = json.Unmarshal(v, &rev)
	} else if v, ok := raw["Revision"]; ok {
		_ = json.Unmarshal(v, &rev)
	}
	vars := map[string]string{}

	// Try "variables"
	if v, ok := raw["variables"]; ok {
		if err := json.Unmarshal(v, &vars); err != nil {
			// Maybe array of {name,value}
			var arr []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			}
			if err2 := json.Unmarshal(v, &arr); err2 == nil {
				for _, it := range arr {
					vars[it.Name] = it.Value
				}
			} else {
				return nil, fmt.Errorf("invalid variables: %w", err)
			}
		}
	} else if v, ok := raw["values"]; ok {
		_ = json.Unmarshal(v, &vars)
	} else if v, ok := raw["items"]; ok {
		var arr []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal(v, &arr); err == nil {
			for _, it := range arr {
				vars[it.Name] = it.Value
			}
		}
	} else if v, ok := raw["data"]; ok {
		// fallback
		_ = json.Unmarshal(v, &vars)
	}

	if vars == nil {
		vars = map[string]string{}
	}
	return &EnvironmentBundle{Revision: rev, Variables: vars}, nil
}

func sanitizeEnvError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// Never include values: we never add them, so just return trimmed message.
	// Ensure no NUL/CR/LF leaks.
	msg = strings.ReplaceAll(msg, "\n", " ")
	msg = strings.ReplaceAll(msg, "\r", " ")
	msg = strings.ReplaceAll(msg, "\x00", "")
	msg = strings.TrimSpace(msg)
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return msg
}

func validateEnvName(name string) error {
	if name == "" {
		return fmt.Errorf("empty")
	}
	if len(name) > 128 {
		return fmt.Errorf("too long")
	}
	// ^[A-Z_][A-Z0-9_]{0,127}$
	if name[0] != '_' && (name[0] < 'A' || name[0] > 'Z') {
		return fmt.Errorf("must start with A-Z or _")
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if c != '_' && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return fmt.Errorf("invalid char %q", c)
		}
	}
	if strings.Contains(name, "\x00") || strings.Contains(name, "\n") || strings.Contains(name, "\r") {
		return fmt.Errorf("contains NUL/CR/LF")
	}
	return nil
}

func escapeEnvValue(v string) string {
	// Escape \ -> \\ , " -> \" , $ -> \$ , ` -> \`
	// Must escape backslash first.
	r := strings.ReplaceAll(v, "\\", "\\\\")
	r = strings.ReplaceAll(r, "\"", "\\\"")
	r = strings.ReplaceAll(r, "$", "\\$")
	r = strings.ReplaceAll(r, "`", "\\`")
	return r
}

func renderEnvironmentFile(vars map[string]string) []byte {
	var keys []string
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString("# Managed by omahab-clientd — do not edit. Tool variables for agent.\n")
	sb.WriteString("# This file is mode 0600 and contains plaintext values readable by same-user processes.\n")
	sb.WriteString("# New apps/terminals via uwsm-app inherit live systemd user manager env; already-running processes keep old env until restarted.\n")
	for _, k := range keys {
		v := vars[k]
		escaped := escapeEnvValue(v)
		// Use quoted KEY="escaped" per environment.d(5)
		sb.WriteString(k)
		sb.WriteString("=\"")
		sb.WriteString(escaped)
		sb.WriteString("\"\n")
	}
	return []byte(sb.String())
}

func atomicWriteFile0600(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Ensure dir is 0700.
	_ = os.Chmod(dir, 0o700)
	// Use temp file with pid and random suffix.
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	// Add extra uniqueness via time.
	tmp = fmt.Sprintf("%s.%d", tmp, time.Now().UnixNano())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	// Explicit chmod 0600.
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// Fsync temp file.
	if f, err := os.Open(tmp); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// Fsync directory.
	_ = fsyncDir(dir)
	return nil
}

func fsyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func parseEnvFileKeys(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var keys []string
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:idx])
		if k == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

