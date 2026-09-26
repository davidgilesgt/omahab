package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/config"
	"github.com/omahab/omahab/internal/domain"
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

// --- scoped backup failure safety ---

type captureFakeRunner struct {
	mu             sync.Mutex
	snapshotsErr   error
	snapshotsCalls int
	initCalls      int
	initErr        error
	backupErr      error
	backupCalls    int
}

func (f *captureFakeRunner) Init(context.Context, backups.Repository, backups.Credentials) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.initCalls++
	return f.initErr
}

func (f *captureFakeRunner) Backup(_ context.Context, _ backups.Repository, _ backups.Credentials, req backups.BackupRequest) (backups.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.backupCalls++
	if f.backupErr != nil {
		return backups.Snapshot{}, f.backupErr
	}
	return backups.Snapshot{ID: "snap-scoped-test", Paths: req.Paths}, nil
}

func (f *captureFakeRunner) Restore(context.Context, backups.Repository, backups.Credentials, string, string) error {
	return nil
}

func (f *captureFakeRunner) Snapshots(context.Context, backups.Repository, backups.Credentials, int) ([]backups.SnapshotListEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshotsCalls++
	if f.snapshotsErr != nil {
		return nil, f.snapshotsErr
	}
	return []backups.SnapshotListEntry{{ID: "s1"}}, nil
}

func (f *captureFakeRunner) Check(context.Context, backups.Repository, backups.Credentials) error {
	return nil
}

type captureFakeSecrets struct {
	creds backups.Credentials
}

func (f captureFakeSecrets) Resolve(context.Context, backups.SecretRef) (backups.Credentials, error) {
	return f.creds, nil
}

func scopedBackupTestBackend(t *testing.T, runner apps.Runner) (*Backend, *captureFakeRunner) {
	t.Helper()
	b, _ := newSetupBackend(t, runner)
	ctx := context.Background()
	if err := b.store.Migrate(ctx, backups.Migrations()...); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stateDir, "dumps"), 0o700); err != nil {
		t.Fatal(err)
	}
	b.cfg = config.Config{StateDir: stateDir, DataDir: t.TempDir()}
	t.Setenv("OMAHAB_MAINTENANCE_LOCK", filepath.Join(t.TempDir(), "maintenance.lock"))
	fr := &captureFakeRunner{}
	b.backups = backups.New(b.store, backups.Config{}, backups.Deps{
		Runner:     fr,
		Secrets:    captureFakeSecrets{creds: backups.Credentials{Password: "test-password"}},
		InstanceID: func(context.Context) string { return "test-instance" },
	})
	return b, fr
}

func mustConfigureScopedRepo(t *testing.T, b *Backend) string {
	t.Helper()
	repo, err := b.backups.Configure(context.Background(), backups.ConfigureRequest{
		Label:     "primary",
		Location:  "/srv/omahab/backups/repos/primary",
		SecretRef: backups.SecretRef{ID: "sec-test", Version: 1},
	})
	if err != nil {
		t.Fatalf("configure repo: %v", err)
	}
	return repo.ID
}

func mustInstallScopeApp(t *testing.T, b *Backend, bundle string) domain.ID {
	t.Helper()
	req := apps.InstallRequest{BundleID: bundle}
	if bundle == "caddy" {
		// caddy defaults to public exposure, which requires a hostname.
		req.Hostname = "caddy.omahab.com"
	}
	st, err := b.apps.Install(context.Background(), req)
	if err != nil {
		t.Fatalf("install %s: %v", bundle, err)
	}
	if st.ObservedState != apps.ObservedRunning {
		t.Fatalf("%s observed = %q, want running before backup", bundle, st.ObservedState)
	}
	return st.ID
}

func scopedAppObserved(t *testing.T, b *Backend, id domain.ID) string {
	t.Helper()
	st, err := b.apps.Status(context.Background(), id)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	return st.ObservedState
}

// An injected restic failure fails the backup but leaves the running app
// restored to running.
func TestScopedBackupFailureRestoresRunningApps(t *testing.T) {
	b, fr := scopedBackupTestBackend(t, storageTestRunner{})
	ctx := context.Background()
	appID := mustInstallScopeApp(t, b, "immich")
	repoID := mustConfigureScopedRepo(t, b)
	fr.mu.Lock()
	fr.backupErr = errors.New("restic backup failed: connection reset by peer")
	fr.mu.Unlock()

	_, err := b.CreateBackupScoped(ctx, repoID, []string{"app:immich"})
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("err = %v, want the injected restic failure", err)
	}
	if got := scopedAppObserved(t, b, appID); got != apps.ObservedRunning {
		t.Fatalf("app observed = %q, want running after failed backup", got)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.backupCalls != 1 {
		t.Fatalf("backup calls = %d, want 1 (quiesce ran before the failure)", fr.backupCalls)
	}
}

// First backup against a supported location succeeds end to end.
func TestScopedBackupSucceedsWithSupportedLocation(t *testing.T) {
	b, fr := scopedBackupTestBackend(t, storageTestRunner{})
	ctx := context.Background()
	appID := mustInstallScopeApp(t, b, "immich")
	repoID := mustConfigureScopedRepo(t, b)

	got, err := b.CreateBackupScoped(ctx, repoID, []string{"app:immich"})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if got.ID == "" {
		t.Fatalf("backup returned empty record")
	}
	if observed := scopedAppObserved(t, b, appID); observed != apps.ObservedRunning {
		t.Fatalf("app observed = %q, want running after successful backup", observed)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.backupCalls != 1 || fr.snapshotsCalls == 0 {
		t.Fatalf("backupCalls = %d snapshotsCalls = %d, want preflight plus capture", fr.backupCalls, fr.snapshotsCalls)
	}
}

// An unreachable repository fails preflight with writers still running:
// no quiesce, no snapshot attempt.
func TestScopedBackupPreflightFailureKeepsAppsRunning(t *testing.T) {
	b, fr := scopedBackupTestBackend(t, storageTestRunner{})
	ctx := context.Background()
	appID := mustInstallScopeApp(t, b, "immich")
	repoID := mustConfigureScopedRepo(t, b)
	fr.mu.Lock()
	fr.snapshotsErr = errors.New("restic snapshots failed: permission denied")
	fr.mu.Unlock()

	_, err := b.CreateBackupScoped(ctx, repoID, []string{"app:immich"})
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err = %v, want unreachable-repository preflight error", err)
	}
	if got := scopedAppObserved(t, b, appID); got != apps.ObservedRunning {
		t.Fatalf("app observed = %q, want still running (never quiesced)", got)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.backupCalls != 0 {
		t.Fatalf("backup calls = %d, want 0", fr.backupCalls)
	}
}

// failSecondStopRunner fails the second Stop call so quiesce always stops
// exactly one app before failing, regardless of bundle iteration order.
type failSecondStopRunner struct {
	storageTestRunner
	mu    sync.Mutex
	stops int
}

func (f *failSecondStopRunner) Stop(_ context.Context, _ domain.Application, _ apps.DeploySpec) error {
	f.mu.Lock()
	f.stops++
	n := f.stops
	f.mu.Unlock()
	if n == 2 {
		return errors.New("stop failed: unit timeout")
	}
	return nil
}

// Partial quiesce resumes the app that was already stopped and reports
// the failed stop.
func TestScopedBackupPartialQuiesceResumesStopped(t *testing.T) {
	b, fr := scopedBackupTestBackend(t, &failSecondStopRunner{})
	ctx := context.Background()
	first := mustInstallScopeApp(t, b, "immich")
	second := mustInstallScopeApp(t, b, "caddy")
	repoID := mustConfigureScopedRepo(t, b)

	_, err := b.CreateBackupScoped(ctx, repoID, []string{"app:immich", "app:caddy"})
	if err == nil || !strings.Contains(err.Error(), "unit timeout") {
		t.Fatalf("err = %v, want the failed quiesce stop", err)
	}
	if got := scopedAppObserved(t, b, first); got != apps.ObservedRunning {
		t.Fatalf("first app observed = %q, want running after partial quiesce", got)
	}
	if got := scopedAppObserved(t, b, second); got != apps.ObservedRunning {
		t.Fatalf("second app observed = %q, want running after partial quiesce", got)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.backupCalls != 0 {
		t.Fatalf("backup calls = %d, want 0 (quiesce failed first)", fr.backupCalls)
	}
}

// failStartRunner refuses every start so the resume pass fails after a
// successful snapshot; the backup must report the resume failure.
type failStartRunner struct {
	storageTestRunner
}

func (failStartRunner) Start(_ context.Context, _ domain.Application, _ apps.DeploySpec) error {
	return errors.New("start failed: unit timeout")
}

func TestScopedBackupResumeFailureReported(t *testing.T) {
	b, _ := scopedBackupTestBackend(t, failStartRunner{})
	ctx := context.Background()
	appID := mustInstallScopeApp(t, b, "immich")
	repoID := mustConfigureScopedRepo(t, b)

	_, err := b.CreateBackupScoped(ctx, repoID, []string{"app:immich"})
	if err == nil || !strings.Contains(err.Error(), "resume") {
		t.Fatalf("err = %v, want reported resume failure", err)
	}
	if got := scopedAppObserved(t, b, appID); got == apps.ObservedRunning {
		t.Fatalf("app observed = running despite failed resume; state must stay honest")
	}
}
