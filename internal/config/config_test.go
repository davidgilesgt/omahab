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
	t.Setenv("OMAHAB_ETC_DIR", filepath.Join(root, "etc"))
	t.Setenv("OMAHAB_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("OMAHAB_DATA_DIR", filepath.Join(root, "data"))
	t.Setenv("OMAHAB_CATALOG", filepath.Join(root, "apps-catalog.json"))
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
	if cfg.CatalogPath != filepath.Join(root, "apps-catalog.json") {
		t.Errorf("catalog path = %q", cfg.CatalogPath)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
}
