package apiclient

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ClientConfig is stored at ~/.config/omahab/client.json.
// It intentionally contains no secrets. Bearer credentials go through
// CredentialStore and never this file.
type ClientConfig struct {
	Server string `json:"server,omitempty"`
}

// DefaultClientConfigPath returns ~/.config/omahab/client.json (or XDG variant).
func DefaultClientConfigPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config", "omahab")
	} else {
		dir = filepath.Join(dir, "omahab")
	}
	return filepath.Join(dir, "client.json"), nil
}

// LoadClientConfig loads the client config file if present; returns empty
// config when absent.
func LoadClientConfig(path string) (ClientConfig, error) {
	if path == "" {
		p, err := DefaultClientConfigPath()
		if err != nil {
			return ClientConfig{}, err
		}
		path = p
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ClientConfig{}, nil
		}
		return ClientConfig{}, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return ClientConfig{}, nil
	}
	var cfg ClientConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return ClientConfig{}, err
	}
	cfg.Server = strings.TrimSpace(cfg.Server)
	return cfg, nil
}

// ResolveServer resolves the server URL with precedence:
//  1. explicit flag value
//  2. OMAHAB_SERVER env
//  3. client.json server
//  4. default http://127.0.0.1:8484
func ResolveServer(flagValue string, cfg ClientConfig) string {
	if s := strings.TrimSpace(flagValue); s != "" {
		return s
	}
	if s := strings.TrimSpace(os.Getenv("OMAHAB_SERVER")); s != "" {
		return s
	}
	if s := strings.TrimSpace(cfg.Server); s != "" {
		return s
	}
	// Also respect OMAHAB_LISTEN? No, that's server side.
	return "http://127.0.0.1:8484"
}
