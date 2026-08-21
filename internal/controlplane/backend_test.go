package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/omahab/omahab/internal/api"
	"github.com/omahab/omahab/internal/config"
	"github.com/omahab/omahab/internal/store"
)

func testConfig(root string) config.Config {
	return config.Config{
		EtcDir:        filepath.Join(root, "etc"),
		StateDir:      filepath.Join(root, "state"),
		DataDir:       filepath.Join(root, "data"),
		Listen:        "127.0.0.1:8484",
		DatabasePath:  filepath.Join(root, "state", "control.db"),
		MasterKeyPath: filepath.Join(root, "state", "master.key"),
		APITokenPath:  filepath.Join(root, "state", "api.token"),
	}
}

func TestCreateBackupDoesNotPersistFakePendingRun(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	if err := cfg.EnsureDirectories(); err != nil {
		t.Fatalf("ensure directories: %v", err)
	}

	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	backend, err := New(context.Background(), st, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	if _, err := backend.CreateBackup(context.Background(), api.CreateBackupRequest{}); err == nil {
		t.Fatal("CreateBackup succeeded without a configured repository")
	}

	var count int
	if err := st.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM backup_runs`).Scan(&count); err != nil {
		t.Fatalf("count backup runs: %v", err)
	}
	if count != 0 {
		t.Fatalf("backup_runs contains %d fake run(s), want 0", count)
	}
}

const catalogFixture = `{"bundles":[{
	"id": "demo", "name": "demo", "image": "docker.io/example/demo",
	"digest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	"architectures": ["amd64", "arm64"],
	"compose": "services:\n  demo:\n    image: {{.Image}}@{{.Digest}}\n",
	"max_exposure": "shared",
	"health_check": {"kind": "none"},
	"resources": {"memory_mb": 128}
}]}`

func newTestBackend(t *testing.T, mutate func(*config.Config)) *Backend {
	t.Helper()
	root := t.TempDir()
	cfg := testConfig(root)
	if mutate != nil {
		mutate(&cfg)
	}
	if err := cfg.EnsureDirectories(); err != nil {
		t.Fatalf("ensure directories: %v", err)
	}
	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	backend, err := New(context.Background(), st, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	return backend
}

func TestCatalogLoadsFromFileAndInstallFailsClosed(t *testing.T) {
	var catalogPath string
	backend := newTestBackend(t, func(c *config.Config) {
		dir := t.TempDir()
		catalogPath = filepath.Join(dir, "apps-catalog.json")
		if err := os.WriteFile(catalogPath, []byte(catalogFixture), 0o644); err != nil {
			t.Fatal(err)
		}
		c.CatalogPath = catalogPath
	})

	bundles, err := backend.ListCatalog(context.Background())
	if err != nil {
		t.Fatalf("list catalog: %v", err)
	}
	if len(bundles) != 1 || bundles[0].ID != "demo" || bundles[0].MaxExposure != "shared" || bundles[0].Installed {
		t.Fatalf("unexpected catalog: %+v", bundles)
	}

	// Unknown bundles must fail closed, never silently provision state.
	if _, err := backend.InstallApplication(context.Background(), api.InstallApplicationRequest{BundleID: "missing"}); err == nil {
		t.Fatal("install of unknown bundle succeeded")
	}
	var count int
	if err := backend.store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM apps`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("apps table has %d row(s) after failed install, want 0", count)
	}
}

func TestMissingCatalogFailsClosedWithWarning(t *testing.T) {
	backend := newTestBackend(t, func(c *config.Config) {
		c.CatalogPath = filepath.Join(t.TempDir(), "absent.json")
	})
	bundles, err := backend.ListCatalog(context.Background())
	if err != nil {
		t.Fatalf("list catalog: %v", err)
	}
	if len(bundles) != 0 {
		t.Fatalf("expected empty catalog, got %+v", bundles)
	}
	var warnings int
	if err := backend.store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM events WHERE type = 'applications.catalog_missing'`).Scan(&warnings); err != nil {
		t.Fatal(err)
	}
	if warnings != 1 {
		t.Fatalf("catalog_missing warning events = %d, want 1", warnings)
	}
}

func TestCorruptCatalogAbortsStartup(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	cfg.CatalogPath = filepath.Join(root, "bad.json")
	if err := os.WriteFile(cfg.CatalogPath, []byte(`{"bundles": [{"id": "x"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureDirectories(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := New(context.Background(), st, Options{Config: cfg, Version: "test"}); err == nil {
		t.Fatal("startup succeeded with an invalid runtime catalog")
	}
}
