package api

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/config"
	"github.com/omahab/omahab/internal/controlplane"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

// fakeAppsRunner performs no host side effects: installs never reach
// systemctl or the appenv directory, so route tests run without escalation.
type fakeAppsRunner struct{}

func (fakeAppsRunner) Deploy(context.Context, domain.Application, apps.DeploySpec) error {
	return nil
}
func (fakeAppsRunner) Start(context.Context, domain.Application, apps.DeploySpec) error {
	return nil
}
func (fakeAppsRunner) Stop(context.Context, domain.Application, apps.DeploySpec) error {
	return nil
}
func (fakeAppsRunner) Remove(context.Context, domain.Application, apps.DeploySpec) error {
	return nil
}
func (fakeAppsRunner) Check(context.Context, domain.Application, apps.DeploySpec) (domain.Health, error) {
	return domain.HealthUnknown, nil
}

func testConfig(root string) config.Config {
	return config.Config{
		StateDir:      filepath.Join(root, "state"),
		DataDir:       filepath.Join(root, "data"),
		Listen:        "127.0.0.1:8484",
		DatabasePath:  filepath.Join(root, "state", "control.db"),
		MasterKeyPath: filepath.Join(root, "state", "master.key"),
		APITokenPath:  filepath.Join(root, "state", "apitypes.token"),
	}
}

func newRealBackend(t *testing.T, mutate func(*config.Config), runners ...apps.Runner) *controlplane.Backend {
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
	opts := controlplane.Options{Config: cfg, Version: "test"}
	if len(runners) > 0 {
		opts.AppsRunner = runners[0]
	}
	backend, err := controlplane.New(context.Background(), st, opts)
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	return backend
}

func newRealServer(t *testing.T, backend *controlplane.Backend, opts ...func(*Config)) *Server {
	t.Helper()
	cfg := Config{
		Backend:     backend,
		BearerToken: "test-token",
	}
	for _, o := range opts {
		o(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv
}
