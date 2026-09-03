package backups

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

// --- fakes ---

type fakeRunner struct {
	mu           sync.Mutex
	initFn       func(repo Repository, creds Credentials) error
	backupFn     func(repo Repository, creds Credentials, req BackupRequest) (Snapshot, error)
	restoreFn    func(repo Repository, creds Credentials, snapshotID, target string) error
	snapshotsFn  func(repo Repository, creds Credentials, latest int) ([]SnapshotListEntry, error)
	backupCalls  []BackupRequest
	backupRepos  []Repository
	backupCreds  []Credentials
	restoreCalls []restoreCall
	initCalls    []Repository
}

type restoreCall struct {
	Repo       Repository
	Creds      Credentials
	SnapshotID string
	Target     string
}

func (f *fakeRunner) Init(_ context.Context, repo Repository, creds Credentials) error {
	f.mu.Lock()
	f.initCalls = append(f.initCalls, repo)
	fn := f.initFn
	f.mu.Unlock()
	if fn != nil {
		return fn(repo, creds)
	}
	return nil
}

func (f *fakeRunner) Snapshots(_ context.Context, repo Repository, creds Credentials, latest int) ([]SnapshotListEntry, error) {
	f.mu.Lock()
	fn := f.snapshotsFn
	f.mu.Unlock()
	if fn != nil {
		return fn(repo, creds, latest)
	}
	return []SnapshotListEntry{{ID: "snap1", Time: "2026-09-01T00:00:00Z", Hostname: "omahab-host"}}, nil
}

func (f *fakeRunner) Backup(_ context.Context, repo Repository, creds Credentials, req BackupRequest) (Snapshot, error) {
	f.mu.Lock()
	f.backupCalls = append(f.backupCalls, req)
	f.backupRepos = append(f.backupRepos, repo)
	f.backupCreds = append(f.backupCreds, creds)
	fn := f.backupFn
	f.mu.Unlock()
	if fn != nil {
		return fn(repo, creds, req)
	}
	return Snapshot{ID: "fakesnap111111", Paths: req.Paths, FileCount: 7, SizeBytes: 1234}, nil
}

func (f *fakeRunner) Restore(_ context.Context, repo Repository, creds Credentials, snapshotID, target string) error {
	f.mu.Lock()
	f.restoreCalls = append(f.restoreCalls, restoreCall{Repo: repo, Creds: creds, SnapshotID: snapshotID, Target: target})
	fn := f.restoreFn
	f.mu.Unlock()
	if fn != nil {
		return fn(repo, creds, snapshotID, target)
	}
	writeTestTree(target, map[string]string{"a.txt": "aaa", "sub/b.txt": "bbbb"})
	return nil
}

func (f *fakeRunner) backups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.backupCalls)
}

type fakeSecrets struct {
	creds Credentials
	calls []SecretRef
}

func (f *fakeSecrets) Resolve(_ context.Context, ref SecretRef) (Credentials, error) {
	f.calls = append(f.calls, ref)
	return f.creds, nil
}

type fakeHooks struct {
	byKind map[HookKind][]Hook
}

func (f fakeHooks) Hooks(_ context.Context, kind HookKind) ([]Hook, error) {
	return f.byKind[kind], nil
}

type fakeHookRunner struct {
	results map[string]HookOutcome
}

func (f fakeHookRunner) RunHook(_ context.Context, h Hook) HookOutcome {
	if out, ok := f.results[h.Application]; ok {
		if out.StartedAt.IsZero() {
			out.StartedAt = time.Now()
			out.FinishedAt = out.StartedAt.Add(time.Second)
		}
		return out
	}
	now := time.Now()
	return HookOutcome{ExitCode: intPtr(0), StartedAt: now, FinishedAt: now.Add(time.Millisecond)}
}

type fakePublisher struct {
	mu     sync.Mutex
	events []domain.Event
}

func (f *fakePublisher) Publish(_ context.Context, e domain.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return nil
}

func (f *fakePublisher) byType(t string) []domain.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Event
	for _, e := range f.events {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

// --- helpers ---

const testPassword = "correct-horse-battery"
const testAccessKey = "sak-do-not-leak"

func writeTestTree(dir string, files map[string]string) {
	for name, content := range files {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(content), 0o644)
	}
}

func newTestService(t *testing.T, mutate func(*Config, *Deps)) (*Service, *fakePublisher, *fakeRunner) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "control.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background(), Migrations()...); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cfg := Config{
		Paths:          []string{filepath.Join(root, "data")},
		Host:           "testhost",
		VerifyRoot:     filepath.Join(root, "verify"),
		CacheDir:       filepath.Join(root, "cache"),
		RPO:            24 * time.Hour,
		VerifyInterval: 168 * time.Hour,
	}
	fr := &fakeRunner{}
	pub := &fakePublisher{}
	deps := Deps{
		Runner:     fr,
		Hooks:      fakeHooks{byKind: map[HookKind][]Hook{}},
		HookRunner: fakeHookRunner{},
		Secrets:    &fakeSecrets{creds: Credentials{Password: testPassword, Username: "u123", AccessKey: testAccessKey}},
		Events:     pub,
	}
	if mutate != nil {
		mutate(&cfg, &deps)
	}
	return New(st, cfg, deps), pub, fr
}

func mustConfigure(t *testing.T, svc *Service) Repository {
	t.Helper()
	repo, err := svc.Configure(context.Background(), ConfigureRequest{
		Label:     "hetzner",
		Location:  "sftp://u123@u123.your-storagebox.de/./omahab",
		SecretRef: SecretRef{ID: "sec-backup", Version: 2},
	})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	return repo
}

func mustList(t *testing.T, svc *Service) []Snapshot {
	t.Helper()
	snaps, err := svc.ListSnapshots(context.Background(), "")
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	return snaps
}

func hookStatuses(t *testing.T, svc *Service, runID string) []HookStatus {
	t.Helper()
	d, err := svc.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	out := make([]HookStatus, 0, len(d.Hooks))
	for _, h := range d.Hooks {
		out = append(out, h.Status)
	}
	return out
}

// --- configure ---

func TestConfigureValidation(t *testing.T) {
	svc, _, _ := newTestService(t, nil)
	ctx := context.Background()

	if _, err := svc.Configure(ctx, ConfigureRequest{Location: "", SecretRef: SecretRef{ID: "s", Version: 1}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty location: got %v", err)
	}
	if _, err := svc.Configure(ctx, ConfigureRequest{Location: "sftp://h/repo", SecretRef: SecretRef{ID: "", Version: 1}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty secret id: got %v", err)
	}
	if _, err := svc.Configure(ctx, ConfigureRequest{Location: "sftp://h/repo", SecretRef: SecretRef{ID: "s", Version: 0}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("secret version 0: got %v", err)
	}
}

func TestConfigureLifecycle(t *testing.T) {
	svc, _, _ := newTestService(t, nil)
	ctx := context.Background()

	repo := mustConfigure(t, svc)
	if repo.SecretRef != (SecretRef{ID: "sec-backup", Version: 2}) {
		t.Fatalf("secret ref not persisted verbatim: %+v", repo.SecretRef)
	}

	if _, err := svc.Configure(ctx, ConfigureRequest{Location: repo.Location, SecretRef: SecretRef{ID: "s", Version: 1}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate location: got %v", err)
	}
	updated, err := svc.Configure(ctx, ConfigureRequest{ID: repo.ID, Label: "renamed", Location: "sftp://u123@box2/omahab", SecretRef: SecretRef{ID: "sec-backup", Version: 3}})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Label != "renamed" || updated.SecretRef.Version != 3 {
		t.Fatalf("update not applied: %+v", updated)
	}
	if _, err := svc.Repository(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown repository: got %v", err)
	}

	// A repository with recorded runs is audit history and cannot be deleted.
	if _, err := svc.RunBackup(ctx, RunRequest{}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if err := svc.DeleteRepository(ctx, repo.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete with runs: got %v", err)
	}
	// A fresh repository without runs deletes cleanly.
	fresh, err := svc.Configure(ctx, ConfigureRequest{Location: "sftp://box3/omahab", SecretRef: SecretRef{ID: "s", Version: 1}})
	if err != nil {
		t.Fatalf("configure fresh: %v", err)
	}
	if err := svc.DeleteRepository(ctx, fresh.ID); err != nil {
		t.Fatalf("delete fresh: %v", err)
	}
}

// --- run ---

func TestRunBackupSuccess(t *testing.T) {
	svc, pub, fr := newTestService(t, func(cfg *Config, deps *Deps) {
		deps.Hooks = fakeHooks{byKind: map[HookKind][]Hook{
			HookPreBackup: {
				{Application: "immich", Command: []string{"/usr/local/bin/immich-dump"}},
				{Application: "forgejo", Command: []string{"/usr/local/bin/forgejo-dump"}},
			},
		}}
	})
	ctx := context.Background()
	mustConfigure(t, svc)

	run, err := svc.RunBackup(ctx, RunRequest{Trigger: TriggerScheduled})
	if err != nil {
		t.Fatalf("run backup: %v", err)
	}
	if run.Status != StatusCompleted || run.Stage != StageCompleted {
		t.Fatalf("run not completed: %+v", run)
	}
	if run.SnapshotID != "fakesnap111111" || run.Stats == nil || run.Stats.Files != 7 {
		t.Fatalf("snapshot stats not recorded: %+v", run)
	}
	if got := hookStatuses(t, svc, run.ID); len(got) != 2 || got[0] != HookOK || got[1] != HookOK {
		t.Fatalf("hook outcomes: %v", got)
	}
	snaps := mustList(t, svc)
	if len(snaps) != 1 {
		t.Fatalf("snapshots: %+v", snaps)
	}
	if snaps[0].VerifiedAt != nil {
		t.Fatalf("fresh snapshot must not be verified: %+v", snaps[0])
	}
	if len(fr.backupCalls) != 1 {
		t.Fatalf("restic called %d times", len(fr.backupCalls))
	}
	if len(fr.backupCalls[0].Paths) != 1 || fr.backupCalls[0].Host != "testhost" {
		t.Fatalf("backup request: %+v", fr.backupCalls[0])
	}
	completed := pub.byType(EventBackupCompleted)
	if len(completed) != 1 || completed[0].ResourceID != domain.ID(run.ID) {
		t.Fatalf("backup.completed events: %+v", completed)
	}
	if completed[0].Data["snapshot_id"] != "fakesnap111111" {
		t.Fatalf("completed event data: %+v", completed[0].Data)
	}
}

func TestRunBackupHookPartialFailure(t *testing.T) {
	svc, pub, fr := newTestService(t, func(cfg *Config, deps *Deps) {
		deps.Hooks = fakeHooks{byKind: map[HookKind][]Hook{
			HookPreBackup: {
				{Application: "immich", Command: []string{"immich-dump"}},
				{Application: "forgejo", Command: []string{"forgejo-dump"}},
				{Application: "paperless", Command: []string{"paperless-dump"}},
			},
		}}
		deps.HookRunner = fakeHookRunner{results: map[string]HookOutcome{
			"forgejo": {ExitCode: intPtr(1), Output: "pg_dump: connection refused"},
		}}
	})
	ctx := context.Background()
	mustConfigure(t, svc)

	run, err := svc.RunBackup(ctx, RunRequest{})
	if err == nil {
		t.Fatal("expected failure")
	}
	if run.Status != StatusFailed || run.Stage != StageHooks {
		t.Fatalf("run state: %+v", run)
	}
	if !strings.Contains(run.Error, "forgejo") || !strings.Contains(run.Error, "connection refused") {
		t.Fatalf("hook failure not described: %q", run.Error)
	}
	// All three outcomes persist: ok, failed, skipped.
	if got := hookStatuses(t, svc, run.ID); len(got) != 3 || got[0] != HookOK || got[1] != HookFailed || got[2] != HookSkipped {
		t.Fatalf("partial hook outcomes: %v", got)
	}
	// restic must never run after a consistency failure.
	if fr.backups() != 0 {
		t.Fatalf("restic ran %d times after hook failure", fr.backups())
	}
	failed := pub.byType(EventBackupFailed)
	if len(failed) != 1 || failed[0].Data["stage"] != StageHooks {
		t.Fatalf("backup.failed events: %+v", failed)
	}
	if snaps := mustList(t, svc); len(snaps) != 0 {
		t.Fatalf("no snapshot should persist: %+v", snaps)
	}
}

func TestRunBackupResticFailureRedactsSecrets(t *testing.T) {
	fr := &fakeRunner{}
	svc, pub, _ := newTestService(t, func(cfg *Config, deps *Deps) {
		deps.Hooks = fakeHooks{byKind: map[HookKind][]Hook{
			HookPreBackup: {{Application: "immich", Command: []string{"immich-dump"}}},
		}}
		deps.Runner = fr
	})
	fr.backupFn = func(_ Repository, _ Credentials, _ BackupRequest) (Snapshot, error) {
		return Snapshot{}, fmt.Errorf("restic backup failed: password %q rejected (key %s)", testPassword, testAccessKey)
	}
	ctx := context.Background()
	mustConfigure(t, svc)

	run, err := svc.RunBackup(ctx, RunRequest{})
	if err == nil {
		t.Fatal("expected failure")
	}
	if run.Status != StatusFailed || run.Stage != StageSnapshot {
		t.Fatalf("run state: %+v", run)
	}
	for _, secret := range []string{testPassword, testAccessKey} {
		if strings.Contains(run.Error, secret) {
			t.Fatalf("run error leaks secret: %q", run.Error)
		}
	}
	// Hooks succeeded before restic failed; both facts persist.
	if got := hookStatuses(t, svc, run.ID); len(got) != 1 || got[0] != HookOK {
		t.Fatalf("hook outcomes: %v", got)
	}
	failed := pub.byType(EventBackupFailed)
	if len(failed) != 1 || failed[0].Data["stage"] != StageSnapshot {
		t.Fatalf("backup.failed events: %+v", failed)
	}
	if strings.Contains(fmt.Sprint(failed[0].Data), testPassword) {
		t.Fatalf("event data leaks secret")
	}
}

func TestSingleActiveOperation(t *testing.T) {
	svc, _, _ := newTestService(t, nil)
	ctx := context.Background()
	mustConfigure(t, svc)

	// Seed a snapshot so verification and restore pass request validation.
	if _, err := svc.RunBackup(ctx, RunRequest{}); err != nil {
		t.Fatalf("seed backup: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	svc.runner = &fakeRunner{backupFn: func(_ Repository, _ Credentials, req BackupRequest) (Snapshot, error) {
		closeOnce(entered)
		<-release
		return Snapshot{ID: "fakesnap222222", Paths: req.Paths, FileCount: 1, SizeBytes: 1}, nil
	}}
	go func() {
		_, err := svc.RunBackup(ctx, RunRequest{})
		done <- err
	}()
	<-entered

	if _, err := svc.RunBackup(ctx, RunRequest{}); !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("second concurrent operation: got %v", err)
	}
	if _, _, err := svc.Verify(ctx, VerifyRequest{}); !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("verify during active operation: got %v", err)
	}
	if _, err := svc.Restore(ctx, RestoreRequest{SnapshotID: "fakesnap111111", TargetDir: t.TempDir()}); !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("restore during active operation: got %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first operation failed: %v", err)
	}
}

func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// --- verify ---

func TestVerifySuccess(t *testing.T) {
	svc, pub, fr := newTestService(t, nil)
	ctx := context.Background()
	mustConfigure(t, svc)

	if _, err := svc.RunBackup(ctx, RunRequest{}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	fr.restoreFn = func(_ Repository, _ Credentials, _, target string) error {
		if !strings.HasPrefix(target, svc.Config().VerifyRoot+string(filepath.Separator)) {
			t.Errorf("restore target outside VerifyRoot: %s", target)
		}
		writeTestTree(target, map[string]string{"a.txt": "aaa", "sub/b.txt": "bbbb"})
		return nil
	}

	run, ver, err := svc.Verify(ctx, VerifyRequest{})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if run.Kind != KindVerify || run.Status != StatusCompleted {
		t.Fatalf("verify run: %+v", run)
	}
	if ver.Status != VerificationPassed || ver.FilesRestored != 2 || ver.BytesRestored != 7 {
		t.Fatalf("verification: %+v", ver)
	}
	if ver.CleanedAt == nil || ver.CleanupError != "" {
		t.Fatalf("cleanup not recorded: %+v", ver)
	}
	if _, err := os.Stat(ver.Target); !os.IsNotExist(err) {
		t.Fatalf("verification target not removed: %v", err)
	}
	if len(fr.restoreCalls) != 1 || fr.restoreCalls[0].SnapshotID != "fakesnap111111" {
		t.Fatalf("restore calls: %+v", fr.restoreCalls)
	}
	if fr.restoreCalls[0].Creds.Password != testPassword {
		t.Fatalf("credentials not resolved for restore")
	}
	// The snapshot only becomes verified now.
	snaps := mustList(t, svc)
	if len(snaps) != 1 || snaps[0].VerifiedAt == nil {
		t.Fatalf("snapshot not marked verified: %+v", snaps)
	}
	if got := pub.byType(EventBackupVerified); len(got) != 1 {
		t.Fatalf("backup.verified events: %+v", got)
	}
}

func TestVerifyFailureRedactsAndCleansUp(t *testing.T) {
	svc, pub, fr := newTestService(t, nil)
	ctx := context.Background()
	mustConfigure(t, svc)

	if _, err := svc.RunBackup(ctx, RunRequest{}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	fr.restoreFn = func(_ Repository, creds Credentials, _, target string) error {
		writeTestTree(target, map[string]string{"partial": "x"})
		return fmt.Errorf("restic restore failed: %s", creds.Password)
	}

	run, ver, err := svc.Verify(ctx, VerifyRequest{})
	if err == nil {
		t.Fatal("expected failure")
	}
	if run.Status != StatusFailed || run.Stage != StageRestore {
		t.Fatalf("verify run: %+v", run)
	}
	if ver.Status != VerificationFailed {
		t.Fatalf("verification status: %+v", ver)
	}
	if strings.Contains(ver.Error, testPassword) {
		t.Fatalf("verification error leaks secret: %q", ver.Error)
	}
	if strings.Contains(run.Error, testPassword) {
		t.Fatalf("run error leaks secret: %q", run.Error)
	}
	if _, err := os.Stat(ver.Target); !os.IsNotExist(err) {
		t.Fatalf("failed verification target not cleaned: %v", err)
	}
	// The snapshot stays unverified after a failed restore proof.
	snaps := mustList(t, svc)
	if len(snaps) != 1 || snaps[0].VerifiedAt != nil {
		t.Fatalf("snapshot must remain unverified: %+v", snaps)
	}
	if got := pub.byType(EventBackupVerificationFailed); len(got) != 1 {
		t.Fatalf("backup.verification_failed events: %+v", got)
	}
	if got := pub.byType(EventBackupFailed); len(got) != 1 {
		t.Fatalf("backup.failed events: %+v", got)
	}
}

func TestVerifyEmptyRestoreFails(t *testing.T) {
	svc, _, fr := newTestService(t, nil)
	ctx := context.Background()
	mustConfigure(t, svc)

	if _, err := svc.RunBackup(ctx, RunRequest{}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	fr.restoreFn = func(_ Repository, _ Credentials, _, _ string) error { return nil }

	_, ver, err := svc.Verify(ctx, VerifyRequest{})
	if err == nil {
		t.Fatal("expected failure")
	}
	if ver.Status != VerificationFailed || !strings.Contains(ver.Error, "zero files") {
		t.Fatalf("empty restore must fail verification: %+v", ver)
	}
}

func TestVerifySnapshotSelection(t *testing.T) {
	svc, _, _ := newTestService(t, nil)
	ctx := context.Background()
	mustConfigure(t, svc)

	if _, _, err := svc.Verify(ctx, VerifyRequest{}); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("verify without snapshots: got %v", err)
	}
	if _, err := svc.RunBackup(ctx, RunRequest{}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if _, _, err := svc.Verify(ctx, VerifyRequest{SnapshotID: "unknown"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("verify unknown snapshot: got %v", err)
	}
	if _, _, err := svc.Verify(ctx, VerifyRequest{SnapshotID: "fakesnap111111", RepositoryID: "other"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("verify mismatched repository: got %v", err)
	}
}

// --- restore ---

func TestRestoreFlow(t *testing.T) {
	svc, pub, _ := newTestService(t, func(cfg *Config, deps *Deps) {
		deps.Hooks = fakeHooks{byKind: map[HookKind][]Hook{
			HookPostRestore: {{Application: "immich", Command: []string{"immich-restore"}}},
		}}
	})
	ctx := context.Background()
	mustConfigure(t, svc)

	if _, err := svc.RunBackup(ctx, RunRequest{}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	target := t.TempDir()

	run, err := svc.Restore(ctx, RestoreRequest{SnapshotID: "fakesnap111111", TargetDir: target})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if run.Status != StatusCompleted || run.Stage != StageCompleted {
		t.Fatalf("restore run: %+v", run)
	}
	d, err := svc.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if len(d.Hooks) != 1 || d.Hooks[0].Status != HookOK || d.Hooks[0].Hook != HookPostRestore {
		t.Fatalf("post-restore hooks: %+v", d.Hooks)
	}
	// A real successful restore is itself proof of restorability.
	snaps := mustList(t, svc)
	if len(snaps) != 1 || snaps[0].VerifiedAt == nil {
		t.Fatalf("snapshot not verified after restore: %+v", snaps)
	}
	if got := pub.byType(EventBackupRestored); len(got) != 1 {
		t.Fatalf("backup.restored events: %+v", got)
	}
	if entries, _ := os.ReadDir(target); len(entries) == 0 {
		t.Fatalf("nothing restored into target")
	}
}

func TestRestoreHookFailure(t *testing.T) {
	svc, pub, _ := newTestService(t, func(cfg *Config, deps *Deps) {
		deps.Hooks = fakeHooks{byKind: map[HookKind][]Hook{
			HookPostRestore: {{Application: "immich", Command: []string{"immich-restore"}}},
		}}
		deps.HookRunner = fakeHookRunner{results: map[string]HookOutcome{
			"immich": {ExitCode: intPtr(2), Output: "restore hook exploded"},
		}}
	})
	ctx := context.Background()
	mustConfigure(t, svc)

	if _, err := svc.RunBackup(ctx, RunRequest{}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	target := t.TempDir()

	run, err := svc.Restore(ctx, RestoreRequest{SnapshotID: "fakesnap111111", TargetDir: target})
	if err == nil {
		t.Fatal("expected failure")
	}
	// Data was restored, but the run records the post-restore hook failure.
	if run.Status != StatusFailed || run.Stage != StageHooks {
		t.Fatalf("restore run: %+v", run)
	}
	if !strings.Contains(run.Error, "immich") {
		t.Fatalf("hook failure not described: %q", run.Error)
	}
	if got := pub.byType(EventBackupFailed); len(got) != 1 {
		t.Fatalf("backup.failed events: %+v", got)
	}
}

func TestRestoreValidation(t *testing.T) {
	svc, _, _ := newTestService(t, nil)
	ctx := context.Background()
	mustConfigure(t, svc)
	if _, err := svc.RunBackup(ctx, RunRequest{}); err != nil {
		t.Fatalf("backup: %v", err)
	}

	if _, err := svc.Restore(ctx, RestoreRequest{TargetDir: "/tmp"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing snapshot: got %v", err)
	}
	if _, err := svc.Restore(ctx, RestoreRequest{SnapshotID: "x", TargetDir: "relative/path"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("relative target: got %v", err)
	}
	if _, err := svc.Restore(ctx, RestoreRequest{SnapshotID: "x", TargetDir: "/"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("root target: got %v", err)
	}
	if _, err := svc.Restore(ctx, RestoreRequest{SnapshotID: "unknown", TargetDir: "/tmp"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown snapshot: got %v", err)
	}
	if _, err := svc.Restore(ctx, RestoreRequest{SnapshotID: "fakesnap111111", TargetDir: filepath.Join(t.TempDir(), "missing")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing target dir: got %v", err)
	}
}

// --- health ---

func TestHealthLifecycle(t *testing.T) {
	fakeNow := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	svc, pub, _ := newTestService(t, func(cfg *Config, deps *Deps) {
		deps.Now = func() time.Time { return fakeNow }
	})
	ctx := context.Background()

	rep, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if rep.Health != domain.HealthUnhealthy || !rep.RPOExceeded {
		t.Fatalf("no backup must be unhealthy: %+v", rep)
	}

	mustConfigure(t, svc)
	if _, err := svc.RunBackup(ctx, RunRequest{}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	rep, err = svc.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if rep.Health != domain.HealthDegraded || rep.LastVerifiedAt != nil {
		t.Fatalf("unverified backup must be degraded, never healthy: %+v", rep)
	}

	if _, _, err := svc.Verify(ctx, VerifyRequest{}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	rep, err = svc.EvaluateHealth(ctx)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if rep.Health != domain.HealthHealthy || rep.LastVerifiedAt == nil {
		t.Fatalf("verified backup must be healthy: %+v", rep)
	}
	// Reaching steady state is not an incident.
	if got := pub.byType(EventBackupHealthChanged); len(got) != 0 {
		t.Fatalf("unexpected health events: %+v", got)
	}

	// Breach the 24h recovery point objective.
	fakeNow = fakeNow.Add(25 * time.Hour)
	rep, err = svc.EvaluateHealth(ctx)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if rep.Health != domain.HealthUnhealthy || !rep.RPOExceeded {
		t.Fatalf("RPO breach must be unhealthy: %+v", rep)
	}
	got := pub.byType(EventBackupHealthChanged)
	if len(got) != 1 {
		t.Fatalf("health transition must emit exactly one event: %+v", got)
	}
	if got[0].Severity != severityError || got[0].Data["to"] != string(domain.HealthUnhealthy) {
		t.Fatalf("transition event: %+v", got[0])
	}

	// Repeated evaluation must not spam the event log.
	if _, err := svc.EvaluateHealth(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if got := pub.byType(EventBackupHealthChanged); len(got) != 1 {
		t.Fatalf("duplicate health events: %+v", got)
	}
}

// --- command safety ---

func TestCommandRunnerPlanKeepsSecretsOutOfArgs(t *testing.T) {
	cr := &CommandRunner{CacheDir: "/var/lib/omahab/backups/cache"}
	repo := Repository{Location: "sftp://u123@u123.your-storagebox.de/./omahab"}
	creds := Credentials{Password: testPassword, Username: "u123", AccessKey: testAccessKey}
	req := BackupRequest{Paths: []string{"/etc/omahab", "/srv/omahab/apps"}, Host: "omahab", Tags: backupTags}

	args, env := cr.backupPlan(repo, creds, req)
	joined := strings.Join(args, " ")
	for _, want := range []string{"backup", "--json", "--host", "omahab", "--tag", "omahab", "/etc/omahab", "/srv/omahab/apps"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in args: %v", want, args)
		}
	}
	for _, secret := range []string{testPassword, testAccessKey} {
		if strings.Contains(joined, secret) {
			t.Fatalf("secret leaked into arguments: %v", args)
		}
	}
	envJoined := strings.Join(env, "\n")
	for _, want := range []string{
		"RESTIC_REPOSITORY=" + repo.Location,
		"RESTIC_PASSWORD=" + testPassword,
		"AWS_ACCESS_KEY_ID=u123",
		"AWS_SECRET_ACCESS_KEY=" + testAccessKey,
		"RESTIC_CACHE_DIR=/var/lib/omahab/backups/cache",
	} {
		if !strings.Contains(envJoined, want) {
			t.Fatalf("missing %q in env", want)
		}
	}
}

func TestRedactAndTruncate(t *testing.T) {
	if got := redact("pw correct-horse-battery key sak-do-not-leak", testPassword, testAccessKey); got != "pw [redacted] key [redacted]" {
		t.Fatalf("redact: %q", got)
	}
	if got := redact("nothing to redact", ""); got != "nothing to redact" {
		t.Fatalf("redact with empty secret: %q", got)
	}
	if got := truncate("abcdefghij", 5); got != "abcde...(truncated)" {
		t.Fatalf("truncate: %q", got)
	}
}

// --- reconciliation ---

func TestReconcileInterrupted(t *testing.T) {
	svc, pub, _ := newTestService(t, nil)
	ctx := context.Background()
	repo := mustConfigure(t, svc)

	orphan := &Run{
		ID: newID(), Kind: KindBackup, RepositoryID: repo.ID,
		Status: StatusRunning, Trigger: TriggerScheduled, Stage: StageSnapshot,
		StartedAt: svc.nowUTC(),
	}
	if err := svc.insertRun(ctx, orphan); err != nil {
		t.Fatalf("insert orphan run: %v", err)
	}
	ver := &Verification{
		ID: newID(), RunID: orphan.ID, RepositoryID: repo.ID, SnapshotID: "fakesnap111111",
		Status: VerificationRunning, Target: filepath.Join(svc.Config().VerifyRoot, orphan.ID),
		StartedAt: svc.nowUTC(),
	}
	if err := svc.insertVerification(ctx, ver); err != nil {
		t.Fatalf("insert orphan verification: %v", err)
	}
	if err := os.MkdirAll(ver.Target, 0o700); err != nil {
		t.Fatalf("mkdir stale target: %v", err)
	}

	n, err := svc.ReconcileInterrupted(ctx)
	if err != nil || n != 1 {
		t.Fatalf("reconcile: %d %v", n, err)
	}
	d, err := svc.GetRun(ctx, orphan.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if d.Run.Status != StatusFailed || !strings.Contains(d.Run.Error, "interrupted") {
		t.Fatalf("orphan run not closed: %+v", d.Run)
	}
	if d.Verification == nil || d.Verification.Status != VerificationFailed {
		t.Fatalf("orphan verification not closed: %+v", d.Verification)
	}
	if _, err := os.Stat(ver.Target); !os.IsNotExist(err) {
		t.Fatalf("stale verification target not cleaned: %v", err)
	}
	if got := pub.byType(EventBackupFailed); len(got) != 1 {
		t.Fatalf("backup.failed events: %+v", got)
	}
	// After reconciliation a new operation can start.
	if _, err := svc.RunBackup(ctx, RunRequest{}); err != nil {
		t.Fatalf("backup after reconcile: %v", err)
	}
}

// --- run detail ---

func TestGetRunDetail(t *testing.T) {
	svc, _, _ := newTestService(t, func(cfg *Config, deps *Deps) {
		deps.Hooks = fakeHooks{byKind: map[HookKind][]Hook{
			HookPreBackup: {{Application: "immich", Command: []string{"immich-dump"}}},
		}}
	})
	ctx := context.Background()
	mustConfigure(t, svc)

	brun, err := svc.RunBackup(ctx, RunRequest{})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	vrun, _, err := svc.Verify(ctx, VerifyRequest{})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}

	bd, err := svc.GetRun(ctx, brun.ID)
	if err != nil {
		t.Fatalf("backup detail: %v", err)
	}
	if len(bd.Hooks) != 1 || bd.Snapshot == nil || bd.Verification != nil {
		t.Fatalf("backup detail: %+v", bd)
	}
	vd, err := svc.GetRun(ctx, vrun.ID)
	if err != nil {
		t.Fatalf("verify detail: %v", err)
	}
	if vd.Verification == nil || vd.Verification.Status != VerificationPassed {
		t.Fatalf("verify detail: %+v", vd.Verification)
	}
	if _, err := svc.GetRun(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run: got %v", err)
	}
}

func TestRepositoryLocationCarriesInstanceFolder(t *testing.T) {
	svc, _, fr := newTestService(t, func(cfg *Config, deps *Deps) {
		deps.InstanceID = func(context.Context) string { return "inst123abc" }
	})
	mustConfigure(t, svc)
	ctx := context.Background()
	if _, err := svc.RunBackup(ctx, RunRequest{}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if got := fr.backupRepos[0].Location; got != "sftp://u123@u123.your-storagebox.de/./omahab/inst123abc" {
		t.Fatalf("backup location = %q, want instance-suffixed", got)
	}

	// Verification and disaster restore must target the same folder.
	if _, _, err := svc.Verify(ctx, VerifyRequest{}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got := fr.restoreCalls[0].Repo.Location; got != "sftp://u123@u123.your-storagebox.de/./omahab/inst123abc" {
		t.Fatalf("verify location = %q, want instance-suffixed", got)
	}
	if _, err := svc.Restore(ctx, RestoreRequest{
		SnapshotID: mustList(t, svc)[0].ID,
		TargetDir:  t.TempDir(),
	}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := fr.restoreCalls[1].Repo.Location; got != "sftp://u123@u123.your-storagebox.de/./omahab/inst123abc" {
		t.Fatalf("restore location = %q, want instance-suffixed", got)
	}

	// Persisted configuration stays untouched; only the restic view is scoped.
	repos, err := svc.Repositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if repos[0].Location != "sftp://u123@u123.your-storagebox.de/./omahab" {
		t.Fatalf("stored location mutated: %q", repos[0].Location)
	}
}

func TestInstanceFolderSuffixIsIdempotent(t *testing.T) {
	svc, _, fr := newTestService(t, func(cfg *Config, deps *Deps) {
		deps.InstanceID = func(context.Context) string { return "inst123abc" }
	})
	ctx := context.Background()
	repo, err := svc.Configure(ctx, ConfigureRequest{
		Label:     "pre-suffixed",
		Location:  "sftp://u@host/backups/inst123abc",
		SecretRef: SecretRef{ID: "sec-backup", Version: 1},
	})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if _, err := svc.RunBackup(ctx, RunRequest{RepositoryID: repo.ID}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if got := fr.backupRepos[0].Location; got != "sftp://u@host/backups/inst123abc" {
		t.Fatalf("location = %q, want single suffix", got)
	}
}

func TestNoInstanceSourceLeavesLocationUntouched(t *testing.T) {
	svc, _, fr := newTestService(t, nil)
	mustConfigure(t, svc)
	if _, err := svc.RunBackup(context.Background(), RunRequest{}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if got := fr.backupRepos[0].Location; got != "sftp://u123@u123.your-storagebox.de/./omahab" {
		t.Fatalf("location = %q, want unchanged", got)
	}
}
