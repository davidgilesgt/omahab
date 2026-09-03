package api

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/omahab/omahab/internal/config"
	"github.com/omahab/omahab/internal/controlplane"
	"github.com/omahab/omahab/internal/store"
)

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

func newRealBackend(t *testing.T, mutate func(*config.Config)) *controlplane.Backend {
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
	backend, err := controlplane.New(context.Background(), st, controlplane.Options{Config: cfg, Version: "test"})
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
