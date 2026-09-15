package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/config"
	"github.com/omahab/omahab/internal/store"
)

func captureTestBackend(t *testing.T, dataRoot string) *Backend {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.Migrate(ctx, apps.Migrations()...); err != nil {
		t.Fatal(err)
	}
	immichRoot := filepath.Join(dataRoot, "apps", "immich")
	if err := os.MkdirAll(immichRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(immichRoot, "photo.jpg"), []byte("img"), 0o644); err != nil {
		t.Fatal(err)
	}
	cat, err := apps.NewCatalog(
		apps.Bundle{ID: "immich", Name: "Immich",
			HealthCheck: apps.HealthCheck{Kind: apps.CheckNone},
			Units:       []string{"immich-server.service"},
			Data:        []apps.DataVolume{{Name: "state", Path: immichRoot}},
			Backup:      apps.BackupHooks{Databases: []string{"immich"}, QuiesceUnits: []string{"immich-server.service"}},
		},
		apps.Bundle{ID: "syncthing", Name: "Syncthing",
			HealthCheck: apps.HealthCheck{Kind: apps.CheckNone},
			Units:       []string{"syncthing.service"},
			Data:        []apps.DataVolume{{Name: "state", Path: filepath.Join(dataRoot, "sync")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := apps.NewService(st.DB(), apps.Options{Catalog: cat, Runner: storageTestRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stateDir, "dumps"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &Backend{
		cfg:   config.Config{StateDir: stateDir, DataDir: dataRoot},
		store: st, db: st.DB(), apps: svc,
	}
}

func TestResolveScopesRejectsUnknown(t *testing.T) {
	b := captureTestBackend(t, t.TempDir())
	_, err := b.resolveBackupScopes(context.Background(), []string{"app:nope"})
	if err == nil || !strings.Contains(err.Error(), "unknown backup scope") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanOmitsNotInstalled(t *testing.T) {
	b := captureTestBackend(t, t.TempDir())
	plan, err := b.planBackupCapture(context.Background(), []string{"app:syncthing"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.bundles) != 0 {
		t.Fatalf("bundles = %+v, want none (not installed)", plan.bundles)
	}
	if len(plan.omitted) != 1 || plan.omitted[0].Reason != "not-installed" {
		t.Fatalf("omitted = %+v, want not-installed record", plan.omitted)
	}
}

func TestPlanCapturesInstalled(t *testing.T) {
	root := t.TempDir()
	b := captureTestBackend(t, root)
	ctx := context.Background()
	st, err := b.apps.Install(ctx, apps.InstallRequest{BundleID: "immich"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	_ = st
	plan, err := b.planBackupCapture(ctx, []string{"app:immich"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.bundles) != 1 || plan.bundles[0].ID != "immich" {
		t.Fatalf("bundles = %+v", plan.bundles)
	}
	if len(plan.databases) != 1 || plan.databases[0] != "immich" {
		t.Fatalf("databases = %+v", plan.databases)
	}
	found := false
	for _, src := range plan.sources {
		if strings.HasSuffix(src, "apps/immich") {
			found = true
		}
	}
	if !found {
		t.Fatalf("sources = %v, want immich root", plan.sources)
	}
}

func TestPlanMissingDataIsError(t *testing.T) {
	root := t.TempDir()
	b := captureTestBackend(t, root)
	ctx := context.Background()
	if _, err := b.apps.Install(ctx, apps.InstallRequest{BundleID: "immich"}); err != nil {
		t.Fatal(err)
	}
	// Remove the required data after install: planning must fail, not
	// snapshot an empty app successfully.
	if err := os.RemoveAll(filepath.Join(root, "apps", "immich")); err != nil {
		t.Fatal(err)
	}
	_, err := b.planBackupCapture(ctx, []string{"app:immich"})
	if err == nil || !strings.Contains(err.Error(), "required path") {
		t.Fatalf("err = %v, want required-path error", err)
	}
}

func TestBackupExcludesLocalRepos(t *testing.T) {
	b := captureTestBackend(t, t.TempDir())
	excludes := b.backupExcludes(context.Background(), []string{"/srv/omahab"})
	foundSock := false
	for _, e := range excludes {
		if e == "*.sock" {
			foundSock = true
		}
	}
	if !foundSock {
		t.Fatalf("excludes = %v, want socket/transient coverage", excludes)
	}
}

var _ = backups.ScopeServerConfig
