package client

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// parseAssignments is a standalone copy of EnvironmentManager.parseFileAssignments
// (which is a method) so mcp.go does not depend on a manager instance.
func parseAssignments(data []byte) map[string]string {
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

// MCPConfigPath returns the path for the omp MCP client config.
// Precedence: $XDG_CONFIG_HOME, $HOME/.config, fallback /tmp.
// File is ~/.config/omp/mcp.json per roadmap E1 (unverified — confirm before finalizing format).
func MCPConfigPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "omp", "mcp.json")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "omp", "mcp.json")
	}
	return filepath.Join(os.TempDir(), "omp-mcp.json")
}

// mcpConfigFile is the on-disk shape for omp's MCP client.
// It follows the common mcpServers envelope used by Claude/omp clients:
// {"mcpServers":{"omahab":{"url":"https://.../api/v1/companion/mcp","headers":{"Authorization":"Bearer ..."}}}}
// For forward-compat we also write a flat "servers" array, so either envelope is discoverable.
type mcpConfigFile struct {
	MCPServers map[string]mcpServerEntry `json:"mcpServers,omitempty"`
	Servers    []mcpServerEntryFlat      `json:"servers,omitempty"`
}

type mcpServerEntry struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Type    string            `json:"type,omitempty"`
}

type mcpServerEntryFlat struct {
	Name string            `json:"name"`
	URL  string            `json:"url"`
	Type string            `json:"type,omitempty"`
	Auth map[string]string `json:"headers,omitempty"`
}

// EnsureMCPConfig writes the omp MCP client config and ensures OMAHAB_MCP_URL
// is present in the agent-tools environment file. It is idempotent and never logs secrets.
// serverURL is the omahabd base URL (e.g. http://127.0.0.1:8484 or https://omahab.ts.net).
// token is the raw device token (oma_dev_...). If either is empty, it no-ops.
func EnsureMCPConfig(serverURL, token string) error {
	serverURL = strings.TrimSpace(serverURL)
	token = strings.TrimSpace(token)
	if serverURL == "" || token == "" {
		return nil
	}
	if !strings.HasPrefix(token, "oma_dev_") {
		// Only device tokens are used for companion MCP; ignore admin/MCP tokens.
		return nil
	}
	base := strings.TrimRight(serverURL, "/")
	mcpURL := base + "/api/v1/companion/mcp"

	// Write omp MCP client config.
	path := MCPConfigPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir mcp config dir: %w", err)
	}
	cfg := mcpConfigFile{
		MCPServers: map[string]mcpServerEntry{
			"omahab": {
				URL:  mcpURL,
				Type: "http",
				Headers: map[string]string{
					"Authorization": "Bearer " + token,
				},
			},
		},
		Servers: []mcpServerEntryFlat{
			{
				Name: "omahab",
				URL:  mcpURL,
				Type: "http",
				Auth: map[string]string{
					"Authorization": "Bearer " + token,
				},
			},
		},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mcp config: %w", err)
	}
	// Atomic write 0600.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write mcp config tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename mcp config: %w", err)
	}

	// Export OMAHAB_MCP_URL via the agent-tools environment file.
	envPath := EnvironmentFilePath()
	if err := ensureEnvVar(envPath, "OMAHAB_MCP_URL", mcpURL); err != nil {
		// Non-fatal: config file is primary, env export is best-effort.
		return nil
	}
	return nil
}

// ensureEnvVar ensures the managed environment file contains NAME="value" (0600).
// It creates the file/dir if missing, merges with existing content, and preserves other keys.
// It does not invoke D-Bus; the next EnvironmentManager.Sync or a daemon restart will D-Bus-apply.
func ensureEnvVar(filePath, name, value string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("name required")
	}
	if err := validateEnvName(name); err != nil {
		return err
	}
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Read existing file if present.
	var existing map[string]string
	if data, err := os.ReadFile(filePath); err == nil {
		existing = parseAssignments(data)
		if existing == nil {
			existing = make(map[string]string)
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		existing = make(map[string]string)
	}
	if existing[name] == value {
		return nil
	}
	existing[name] = value
	rendered := renderEnvironmentFile(existing)
	return atomicWriteFile0600(filePath, rendered)
}

// ClearMCPConfig removes the omp MCP config file (on revoke/unenroll).
func ClearMCPConfig() error {
	path := MCPConfigPath()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Also remove OMAHAB_MCP_URL from env file if present.
	envPath := EnvironmentFilePath()
	if data, err := os.ReadFile(envPath); err == nil {
		assigns := parseAssignments(data)
		if _, ok := assigns["OMAHAB_MCP_URL"]; ok {
			delete(assigns, "OMAHAB_MCP_URL")
			if len(assigns) == 0 {
				_ = os.Remove(envPath)
			} else {
				_ = atomicWriteFile0600(envPath, renderEnvironmentFile(assigns))
			}
		}
	}
	return nil
}
