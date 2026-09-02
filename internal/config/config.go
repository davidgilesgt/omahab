package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultEtcDir      = "/etc/omahab"
	DefaultStateDir    = "/var/lib/omahab"
	DefaultDataDir     = "/srv/omahab"
	DefaultListen      = "127.0.0.1:8484"
	DefaultCatalogPath = "/usr/share/omahab/catalog/apps-catalog.json"
)

type Config struct {
	EtcDir        string
	StateDir      string
	DataDir       string
	Listen        string
	DatabasePath  string
	MasterKeyPath string
	APITokenPath  string
	ShutdownGrace time.Duration
	CatalogPath   string

	// CaddyConfigPath is where omahabd renders the Caddy JSON config.
	// State-owned (not /etc): NixOS manages /etc, the daemon must not write there.
	CaddyConfigPath string

	// CloudflaredDir holds the cloudflared tunnel token env file.
	CloudflaredDir string
}

func Load() (Config, error) {
	cfg := Config{
		EtcDir:        envOr("OMAHAB_ETC_DIR", DefaultEtcDir),
		StateDir:      envOr("OMAHAB_STATE_DIR", DefaultStateDir),
		DataDir:       envOr("OMAHAB_DATA_DIR", DefaultDataDir),
		Listen:        envOr("OMAHAB_LISTEN", DefaultListen),
		ShutdownGrace: 15 * time.Second,
		CatalogPath:   envOr("OMAHAB_CATALOG", DefaultCatalogPath),
	}
	cfg.DatabasePath = envOr("OMAHAB_DATABASE", filepath.Join(cfg.StateDir, "control.db"))
	cfg.MasterKeyPath = envOr("OMAHAB_MASTER_KEY", filepath.Join(cfg.StateDir, "master.key"))
	cfg.APITokenPath = envOr("OMAHAB_API_TOKEN_FILE", filepath.Join(cfg.StateDir, "api.token"))
	cfg.CaddyConfigPath = envOr("OMAHAB_CADDY_CONFIG", filepath.Join(cfg.StateDir, "caddy", "caddy.json"))
	cfg.CloudflaredDir = envOr("OMAHAB_CLOUDFLARED_DIR", filepath.Join(cfg.StateDir, "cloudflared"))
	if cfg.EtcDir == "" {
		cfg.EtcDir = DefaultEtcDir
	}
	if raw := os.Getenv("OMAHAB_SHUTDOWN_GRACE_SECONDS"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 1 || seconds > 300 {
			return Config{}, fmt.Errorf("OMAHAB_SHUTDOWN_GRACE_SECONDS must be between 1 and 300")
		}
		cfg.ShutdownGrace = time.Duration(seconds) * time.Second
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	for name, value := range map[string]string{
		"etc directory": c.EtcDir, "state directory": c.StateDir, "data directory": c.DataDir,
		"database path": c.DatabasePath, "master key path": c.MasterKeyPath, "API token path": c.APITokenPath,
		"application catalog path": c.CatalogPath, "caddy config path": c.CaddyConfigPath,
		"cloudflared directory": c.CloudflaredDir,
	} {
		if !filepath.IsAbs(value) {
			return fmt.Errorf("%s must be absolute", name)
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("%s contains NUL", name)
		}
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	if host == "" {
		return errors.New("listen address must name an interface explicitly")
	}
	if port == "" {
		return errors.New("listen address must include a port")
	}
	return nil
}

func (c Config) EnsureDirectories() error {
	for _, dir := range []string{c.EtcDir, c.StateDir, c.DataDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
