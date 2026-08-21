package client

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config holds non-secret client configuration. Credentials are never stored here
// and must be kept in a CredentialStore (desktop keyring).
type Config struct {
	ServerURL          string `json:"server_url"`
	PinnedInstanceID   string `json:"pinned_instance_id,omitempty"`
	ExpectedTailnet    string `json:"expected_tailnet,omitempty"`
	ExpectedServerNode string `json:"expected_server_node,omitempty"`
	SocketPath         string `json:"socket_path,omitempty"`
}

// DefaultSocketPath returns the default Unix socket path for the user daemon.
// Prefers XDG_RUNTIME_DIR, then falls back to $HOME/.cache, then /tmp.
func DefaultSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "omahab-clientd.sock")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".cache", "omahab", "clientd.sock")
	}
	uid := os.Getuid()
	return fmt.Sprintf("/tmp/omahab-clientd-%d.sock", uid)
}

// DefaultConfigPath returns the default config file path.
func DefaultConfigPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "omahab", "client.json")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "omahab", "client.json")
	}
	return "./client.json"
}

// Validate checks config invariants.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.ServerURL) == "" {
		return fmt.Errorf("server_url is required")
	}
	if !strings.HasPrefix(c.ServerURL, "https://") && !strings.HasPrefix(c.ServerURL, "http://") {
		return fmt.Errorf("server_url must start with https:// or http://")
	}
	if c.PinnedInstanceID != "" && strings.TrimSpace(c.PinnedInstanceID) != c.PinnedInstanceID {
		return fmt.Errorf("pinned_instance_id must not contain whitespace")
	}
	return nil
}

// EffectiveSocketPath returns SocketPath or the default.
func (c *Config) EffectiveSocketPath() string {
	if c.SocketPath != "" {
		return c.SocketPath
	}
	return DefaultSocketPath()
}

// LoadConfig reads config from path.
func LoadConfig(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigPath()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{SocketPath: DefaultSocketPath()}, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultSocketPath()
	}
	return &cfg, nil
}

// SaveConfig persists config to path. It never writes credentials.
func SaveConfig(path string, cfg *Config) error {
	if path == "" {
		path = DefaultConfigPath()
	}
	// Defensive copy without any credential material.
	safe := *cfg
	// Ensure no accidental credential fields leak via future additions.
	if err := safe.Validate(); err != nil {
		// Still allow saving even if server_url empty during initial setup,
		// but do not persist secrets (there are none to persist).
		if safe.ServerURL == "" {
			// allow empty server_url for bootstrapping; skip strict validate
		} else {
			return err
		}
	}
	data, err := json.MarshalIndent(safe, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir config dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return os.Rename(tmp, path)
}
