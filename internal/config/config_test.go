package config

import (
	"path/filepath"
	"testing"
)

// TestLoadProducesCompleteConfig guards against partially-populated configs:
// every derived path must be absolute so downstream daemons never start with
// a missing master key or token path.
func TestLoadProducesCompleteConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OMAHAB_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("OMAHAB_DATA_DIR", filepath.Join(root, "data"))
	t.Setenv("OMAHAB_CATALOG", filepath.Join(root, "catalog.json"))
	t.Setenv("OMAHAB_LISTEN", "127.0.0.1:8484")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DatabasePath != filepath.Join(root, "state", "control.db") {
		t.Errorf("database path = %q", cfg.DatabasePath)
	}
	if cfg.MasterKeyPath != filepath.Join(root, "state", "master.key") {
		t.Errorf("master key path = %q", cfg.MasterKeyPath)
	}
	if cfg.APITokenPath != filepath.Join(root, "state", "api.token") {
		t.Errorf("api token path = %q", cfg.APITokenPath)
	}
	if cfg.CatalogPath != filepath.Join(root, "catalog.json") {
		t.Errorf("catalog path = %q", cfg.CatalogPath)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
}

func TestDefaultListenIsLoopback(t *testing.T) {
	t.Parallel()
	if DefaultListen != "127.0.0.1:8484" {
		t.Fatalf("DefaultListen = %q, want %q (standalone default must remain loopback-only)", DefaultListen, "127.0.0.1:8484")
	}
	// Standalone default must not be wildcard; packaged systemd unit overrides it.
	if DefaultListen == "0.0.0.0:8484" {
		t.Fatalf("DefaultListen must not be 0.0.0.0:8484; that is only for the packaged systemd service")
	}
}

func TestLoadWithoutListenEnvUsesLoopbackDefault(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OMAHAB_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("OMAHAB_DATA_DIR", filepath.Join(root, "data"))
	t.Setenv("OMAHAB_CATALOG", filepath.Join(root, "catalog.json"))
	// Ensure OMAHAB_LISTEN is not set so Load falls back to DefaultListen.
	t.Setenv("OMAHAB_LISTEN", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen != DefaultListen {
		t.Fatalf("Listen = %q, want DefaultListen %q", cfg.Listen, DefaultListen)
	}
	if cfg.Listen != "127.0.0.1:8484" {
		t.Fatalf("standalone Listen = %q, want 127.0.0.1:8484", cfg.Listen)
	}
}

func TestLoadAcceptsPackagedWildcardWhenExplicitlySet(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OMAHAB_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("OMAHAB_DATA_DIR", filepath.Join(root, "data"))
	t.Setenv("OMAHAB_CATALOG", filepath.Join(root, "catalog.json"))
	t.Setenv("OMAHAB_LISTEN", "0.0.0.0:8484")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen != "0.0.0.0:8484" {
		t.Fatalf("Listen = %q, want 0.0.0.0:8484 (packaged override must be accepted)", cfg.Listen)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate wildcard: %v", err)
	}
}
