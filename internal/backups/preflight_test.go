package backups

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// Configure must initialize the scoped repository path actually used by
// backups (location + instance suffix), not the bare configured location.
func TestConfigureInitializesScopedPath(t *testing.T) {
	svc, _, fr := newTestService(t, func(_ *Config, deps *Deps) {
		deps.InstanceID = func(context.Context) string { return "machine-1" }
	})
	repo, err := svc.Configure(context.Background(), ConfigureRequest{
		Label:     "primary",
		Location:  "sftp://u123@u123.your-storagebox.de/./omahab",
		SecretRef: SecretRef{ID: "sec-backup", Version: 1},
	})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.initCalls) != 1 {
		t.Fatalf("init calls = %d, want 1", len(fr.initCalls))
	}
	want := "sftp://u123@u123.your-storagebox.de/./omahab/machine-1"
	if fr.initCalls[0].Location != want {
		t.Fatalf("init location = %q, want %q", fr.initCalls[0].Location, want)
	}
	if repo.Location == want {
		t.Fatalf("stored location must stay unscoped, got %q", repo.Location)
	}
}

// Local paths under /tmp and /var/tmp are invisible to the daemon
// (PrivateTmp) and must be rejected at configure time with an actionable
// error. Supported locations keep working.
func TestConfigureRejectsSandboxedLocalPaths(t *testing.T) {
	svc, _, _ := newTestService(t, nil)
	ctx := context.Background()
	for _, loc := range []string{
		"/var/tmp/omahab-test-backup",
		"/var/tmp/omahab-test-backup/",
		"/tmp/omahab-repo",
		"/tmp",
		"/var/tmp",
	} {
		if _, err := svc.Configure(ctx, ConfigureRequest{
			Label:     "bad",
			Location:  loc,
			SecretRef: SecretRef{ID: "s", Version: 1},
		}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "/srv/omahab/backups/repos") {
			t.Fatalf("location %q: err = %v, want ErrInvalid pointing at /srv/omahab/backups/repos", loc, err)
		}
	}
	for _, loc := range []string{
		"/srv/omahab/backups/repos/primary",
		"sftp://u123@u123.your-storagebox.de/./omahab",
		"s3:s3.amazonaws.com/bucket/omahab",
	} {
		if _, err := svc.Configure(ctx, ConfigureRequest{
			Label:     "ok",
			Location:  loc,
			SecretRef: SecretRef{ID: "s", Version: 1},
		}); err != nil {
			t.Fatalf("location %q: err = %v, want success", loc, err)
		}
	}
}

// A changed location is validated; an update that keeps a grandfathered
// location (e.g. label or secret rotation on a repository configured
// before sandbox validation) still succeeds.
func TestConfigureUpdateGrandfathersUnchangedLocation(t *testing.T) {
	svc, _, fr := newTestService(t, nil)
	ctx := context.Background()
	now := time.Now().UTC()
	legacy := Repository{
		ID:        "legacy-repo",
		Label:     "legacy",
		Location:  "/var/tmp/omahab-legacy",
		SecretRef: SecretRef{ID: "s", Version: 1},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := svc.insertRepository(ctx, legacy); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	updated, err := svc.Configure(ctx, ConfigureRequest{
		ID:        legacy.ID,
		Label:     "renamed",
		Location:  legacy.Location,
		SecretRef: SecretRef{ID: "s", Version: 2},
	})
	if err != nil {
		t.Fatalf("update keeping grandfathered location: %v", err)
	}
	if updated.Label != "renamed" || updated.SecretRef.Version != 2 {
		t.Fatalf("update not applied: %+v", updated)
	}
	if _, err := svc.Configure(ctx, ConfigureRequest{
		ID:        legacy.ID,
		Label:     "moved",
		Location:  "/var/tmp/omahab-elsewhere",
		SecretRef: SecretRef{ID: "s", Version: 2},
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("changed location to sandboxed path: err = %v, want ErrInvalid", err)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.initCalls) != 0 {
		t.Fatalf("init calls = %d, want 0 (no location change, no init)", len(fr.initCalls))
	}
}

// A repository that Configure never initialized (e.g. created before
// scoped init) is initialized on first preflight, and the backup then
// proceeds against the scoped path.
func TestRunBackupPreflightsMissingRepo(t *testing.T) {
	svc, _, fr := newTestService(t, func(_ *Config, deps *Deps) {
		deps.InstanceID = func(context.Context) string { return "machine-9" }
	})
	ctx := context.Background()
	repo := mustConfigure(t, svc)
	initialized := false
	fr.mu.Lock()
	fr.initCalls = nil
	fr.mu.Unlock()
	fr.snapshotsFn = func(repo Repository, _ Credentials, _ int) ([]SnapshotListEntry, error) {
		if !initialized {
			return nil, errors.New("restic snapshots failed: repository does not exist: unable to open config file")
		}
		return []SnapshotListEntry{{ID: "snap1", Time: "2026-09-01T00:00:00Z"}}, nil
	}
	fr.initFn = func(repo Repository, _ Credentials) error {
		initialized = true
		return nil
	}
	run, err := svc.RunBackup(ctx, RunRequest{RepositoryID: repo.ID})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if run.Status != StatusCompleted {
		t.Fatalf("run status = %q, want completed", run.Status)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.initCalls) != 1 {
		t.Fatalf("init calls = %d, want 1", len(fr.initCalls))
	}
	if !strings.HasSuffix(fr.initCalls[0].Location, "/machine-9") {
		t.Fatalf("init location = %q, want scoped path", fr.initCalls[0].Location)
	}
	if len(fr.backupRepos) != 1 || !strings.HasSuffix(fr.backupRepos[0].Location, "/machine-9") {
		t.Fatalf("backup repos = %+v, want scoped path", fr.backupRepos)
	}
}

// A non-missing probe failure (credentials, network, permissions) aborts
// before any run row, hook, or snapshot attempt.
func TestRunBackupPreflightUnreachableAborts(t *testing.T) {
	svc, _, fr := newTestService(t, nil)
	ctx := context.Background()
	repo := mustConfigure(t, svc)
	fr.snapshotsFn = func(Repository, Credentials, int) ([]SnapshotListEntry, error) {
		return nil, errors.New("restic snapshots failed: connection refused")
	}
	if _, err := svc.RunBackup(ctx, RunRequest{RepositoryID: repo.ID}); err == nil ||
		!strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err = %v, want unreachable-repository error", err)
	}
	if n := fr.backups(); n != 0 {
		t.Fatalf("backup calls = %d, want 0", n)
	}
	runs, err := svc.ListRuns(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %d, want no run row for preflight failure", len(runs))
	}
}

// The child environment is minimal, so the configured cache must be
// carried as both RESTIC_CACHE_DIR and the XDG fallback; without a
// configured cache neither may appear.
func TestCommandRunnerEnvCarriesCacheFallback(t *testing.T) {
	dir := t.TempDir()
	cr := &CommandRunner{CacheDir: dir}
	env := cr.env(Repository{Location: "/srv/omahab/backups/repos/x"}, Credentials{Password: "pw"})
	joined := strings.Join(env, "\n")
	for _, want := range []string{"RESTIC_CACHE_DIR=" + dir, "XDG_CACHE_HOME=" + dir} {
		if !strings.Contains(joined, want) {
			t.Fatalf("env missing %q:\n%s", want, joined)
		}
	}
	bare := (&CommandRunner{}).env(Repository{Location: "/srv/omahab/backups/repos/x"}, Credentials{Password: "pw"})
	for _, v := range bare {
		if strings.HasPrefix(v, "RESTIC_CACHE_DIR=") || strings.HasPrefix(v, "XDG_CACHE_HOME=") {
			t.Fatalf("unconfigured cache leaked %q", v)
		}
	}
}

// A disappeared attached source must fail loudly: preflight never
// initializes over it (which would mask historical snapshots with a fresh
// empty repository), runs no backup, records no run, and leaves the path
// on disk untouched.
func TestPreflightAttachedMissingFailsLoudly(t *testing.T) {
	svc, _, fr := newTestService(t, nil)
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Now().UTC()
	attached := Repository{
		ID:        "attached-1",
		Label:     "attached",
		Location:  dir,
		SecretRef: SecretRef{ID: "s", Version: 1},
		Attached:  true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := svc.insertRepository(ctx, attached); err != nil {
		t.Fatalf("seed attached: %v", err)
	}
	fr.snapshotsFn = func(Repository, Credentials, int) ([]SnapshotListEntry, error) {
		return nil, errors.New("restic snapshots failed: repository does not exist: unable to open config file")
	}
	if _, _, err := svc.PreflightRepository(ctx, attached.ID); err == nil ||
		!strings.Contains(err.Error(), "attached") {
		t.Fatalf("preflight err = %v, want loud attached-missing failure", err)
	}
	if _, err := svc.RunBackup(ctx, RunRequest{RepositoryID: attached.ID}); err == nil {
		t.Fatalf("backup succeeded against a disappeared attached source")
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.initCalls) != 0 {
		t.Fatalf("init calls = %d, want 0 (never init over an attached source)", len(fr.initCalls))
	}
	if n := len(fr.backupCalls); n != 0 {
		t.Fatalf("backup calls = %d, want 0", n)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("attached path changed on disk: %v", entries)
	}
	runs, err := svc.ListRuns(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %d, want no run row for refused attached source", len(runs))
	}
}
