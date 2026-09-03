package backups

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apps"
)

type fakeAppLister struct {
	statuses []apps.Status
	bundles  []apps.Bundle
	err      error
}

func (f fakeAppLister) List(_ context.Context) ([]apps.Status, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.statuses, nil
}

func (f fakeAppLister) CatalogBundles() []apps.Bundle { return f.bundles }

func bundlesForTest() []apps.Bundle {
	b1 := apps.Bundle{
		ID:   "forgejo",
		Name: "Forgejo",
		Units: []string{"forgejo.service"},
		Backup: apps.BackupHooks{
			PreBackup:   []string{"/bin/sh", "-c", "pg_dump -Fc forgejo -f /tmp/forgejo.pgdump"},
			PostRestore: []string{"/bin/sh", "-c", "pg_restore --clean --if-exists /tmp/forgejo.pgdump"},
		},
	}
	b2 := apps.Bundle{
		ID:   "immich",
		Name: "Immich",
		Units: []string{"immich.service"},
		Backup: apps.BackupHooks{
			PreBackup:   []string{"/bin/sh", "-c", "pg_dump -Fc immich -f /tmp/immich.pgdump"},
			PostRestore: []string{"/bin/sh", "-c", "pg_restore --clean --if-exists /tmp/immich.pgdump && restart"},
		},
	}
	b3 := apps.Bundle{
		ID:    "syncthing",
		Name:  "Syncthing",
		Units: []string{"syncthing.service"},
		Backup: apps.BackupHooks{},
	}
	// Validate through NewCatalog to ensure fields satisfy rules.
	cat, err := apps.NewCatalog(b1, b2, b3)
	if err != nil {
		panic(fmt.Sprintf("test bundles invalid: %v", err))
	}
	return cat.Bundles()
}

func statusesFor(ids ...string) []apps.Status {
	var out []apps.Status
	for _, id := range ids {
		out = append(out, apps.Status{BundleID: id})
	}
	// Also set Application.Name for completeness (not used by Hooks).
	for i := range out {
		out[i].Name = out[i].BundleID
	}
	return out
}

func TestAppHookSourcePreBackup(t *testing.T) {
	bundles := bundlesForTest()
	src := NewAppHookSource(fakeAppLister{
		statuses: statusesFor("forgejo", "immich", "syncthing"),
		bundles:  bundles,
	})
	hooks, err := src.Hooks(context.Background(), HookPreBackup)
	if err != nil {
		t.Fatalf("Hooks: %v", err)
	}
	if len(hooks) != 2 {
		t.Fatalf("expected 2 pre-backup hooks (syncthing has none), got %d: %+v", len(hooks), hooks)
	}
	byApp := map[string][]string{}
	for _, h := range hooks {
		byApp[h.Application] = h.Command
	}
	if _, ok := byApp["forgejo"]; !ok {
		t.Fatalf("missing forgejo hook: %+v", hooks)
	}
	if _, ok := byApp["immich"]; !ok {
		t.Fatalf("missing immich hook: %+v", hooks)
	}
	if _, ok := byApp["syncthing"]; ok {
		t.Fatalf("syncthing should have no pre-backup hook: %+v", hooks)
	}
	for _, h := range hooks {
		if len(h.Command) == 0 {
			t.Fatalf("empty command for %s", h.Application)
		}
	}
}

func TestAppHookSourcePostRestore(t *testing.T) {
	bundles := bundlesForTest()
	src := NewAppHookSource(fakeAppLister{
		statuses: statusesFor("forgejo", "immich"),
		bundles:  bundles,
	})
	hooks, err := src.Hooks(context.Background(), HookPostRestore)
	if err != nil {
		t.Fatalf("Hooks: %v", err)
	}
	if len(hooks) != 2 {
		t.Fatalf("expected 2 post-restore hooks, got %d: %+v", len(hooks), hooks)
	}
	for _, h := range hooks {
		if h.Application == "" || len(h.Command) == 0 {
			t.Fatalf("invalid hook: %+v", h)
		}
	}
}

func TestAppHookSourceEmptyInstalled(t *testing.T) {
	bundles := bundlesForTest()
	src := NewAppHookSource(fakeAppLister{statuses: nil, bundles: bundles})
	hooks, err := src.Hooks(context.Background(), HookPreBackup)
	if err != nil {
		t.Fatalf("Hooks: %v", err)
	}
	if len(hooks) != 0 {
		t.Fatalf("expected no hooks, got %+v", hooks)
	}
}

func TestAppHookSourceUnknownKind(t *testing.T) {
	bundles := bundlesForTest()
	src := NewAppHookSource(fakeAppLister{
		statuses: statusesFor("forgejo"),
		bundles:  bundles,
	})
	hooks, err := src.Hooks(context.Background(), HookKind("bogus"))
	if err != nil {
		t.Fatalf("Hooks: %v", err)
	}
	if len(hooks) != 0 {
		t.Fatalf("bogus kind should yield no hooks, got %+v", hooks)
	}
}

func TestAppHookSourceUnknownBundleSkipped(t *testing.T) {
	bundles := bundlesForTest()
	src := NewAppHookSource(fakeAppLister{
		statuses: statusesFor("unknown-bundle"),
		bundles:  bundles,
	})
	hooks, err := src.Hooks(context.Background(), HookPreBackup)
	if err != nil {
		t.Fatalf("Hooks: %v", err)
	}
	if len(hooks) != 0 {
		t.Fatalf("unknown bundle should be skipped, got %+v", hooks)
	}
}

func TestAppHookSourceListErrorPropagates(t *testing.T) {
	src := NewAppHookSource(fakeAppLister{err: fmt.Errorf("db down")})
	if _, err := src.Hooks(context.Background(), HookPreBackup); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("expected db down error, got %v", err)
	}
}

func TestAppHookSourceNilSafe(t *testing.T) {
	var src *AppHookSource
	hooks, err := src.Hooks(context.Background(), HookPreBackup)
	if err != nil {
		t.Fatalf("nil source: %v", err)
	}
	if len(hooks) != 0 {
		t.Fatalf("nil source should yield no hooks, got %+v", hooks)
	}
	src2 := NewAppHookSource(nil)
	hooks, err = src2.Hooks(context.Background(), HookPreBackup)
	if err != nil {
		t.Fatalf("nil apps: %v", err)
	}
	if len(hooks) != 0 {
		t.Fatalf("nil apps should yield no hooks, got %+v", hooks)
	}
}

func TestAppHookSourceCommandCopied(t *testing.T) {
	bundles := bundlesForTest()
	src := NewAppHookSource(fakeAppLister{
		statuses: statusesFor("forgejo"),
		bundles:  bundles,
	})
	hooks, _ := src.Hooks(context.Background(), HookPreBackup)
	if len(hooks) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(hooks))
	}
	hooks[0].Command[0] = "MUTATED"
	hooks2, _ := src.Hooks(context.Background(), HookPreBackup)
	if hooks2[0].Command[0] == "MUTATED" {
		t.Fatalf("Hook Command slice not copied; mutation leaked")
	}
}

func TestAppHookSourcePreHookFailureAbortsRestic(t *testing.T) {
	bundles := bundlesForTest()
	lister := fakeAppLister{
		statuses: statusesFor("forgejo", "immich"),
		bundles:  bundles,
	}
	src := NewAppHookSource(lister)
	svc, _, fr := newTestService(t, func(cfg *Config, deps *Deps) {
		deps.Hooks = src
		deps.HookRunner = fakeHookRunner{results: map[string]HookOutcome{
			"forgejo": {ExitCode: intPtr(1), Output: "pg_dump failed: connection refused"},
		}}
	})
	mustConfigure(t, svc)
	run, err := svc.RunBackup(context.Background(), RunRequest{})
	if err == nil {
		t.Fatal("expected RunBackup to fail on hook failure")
	}
	if run.Status != StatusFailed || run.Stage != StageHooks {
		t.Fatalf("run state: %+v", run)
	}
	if !strings.Contains(run.Error, "forgejo") {
		t.Fatalf("error should mention failed app: %q", run.Error)
	}
	if got := hookStatuses(t, svc, run.ID); len(got) != 2 || got[0] != HookFailed || got[1] != HookSkipped {
		// Order is by List order (forgejo, immich). Forgejo fails first, immich skipped.
		// If order by List sorted by name, forgejo < immich, so forgejo first.
		t.Fatalf("hook outcomes: %v", got)
	}
	if fr.backups() != 0 {
		t.Fatalf("restic must not run after pre-hook failure, got %d calls", fr.backups())
	}
}

func TestAppHookSourcePostRestoreHooksExecuteOnRestore(t *testing.T) {
	bundles := bundlesForTest()
	lister := fakeAppLister{
		statuses: statusesFor("forgejo"),
		bundles:  bundles,
	}
	src := NewAppHookSource(lister)
	svc, _, _ := newTestService(t, func(cfg *Config, deps *Deps) {
		deps.Hooks = src
		deps.HookRunner = fakeHookRunner{}
	})
	mustConfigure(t, svc)
	if _, err := svc.RunBackup(context.Background(), RunRequest{}); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	target := t.TempDir()
	run, err := svc.Restore(context.Background(), RestoreRequest{SnapshotID: "fakesnap111111", TargetDir: target})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if run.Status != StatusCompleted {
		t.Fatalf("restore not completed: %+v", run)
	}
	d, err := svc.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if len(d.Hooks) != 1 || d.Hooks[0].Status != HookOK || d.Hooks[0].Hook != HookPostRestore {
		t.Fatalf("expected one successful post-restore hook: %+v", d.Hooks)
	}
}

func TestAppHookSourcePostRestoreHookFailureAbortsRestore(t *testing.T) {
	b := apps.Bundle{
		ID:   "immich",
		Name: "Immich",
		Units: []string{"immich.service"},
		Backup: apps.BackupHooks{
			PostRestore: []string{"/bin/sh", "-c", "pg_restore --clean --if-exists /tmp/immich.pgdump"},
		},
	}
	cat, err := apps.NewCatalog(b)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	bundles := cat.Bundles()
	lister := fakeAppLister{
		statuses: statusesFor("immich"),
		bundles:  bundles,
	}
	src := NewAppHookSource(lister)
	svc, _, _ := newTestService(t, func(cfg *Config, deps *Deps) {
		deps.Hooks = src
		deps.HookRunner = fakeHookRunner{results: map[string]HookOutcome{
			"immich": {ExitCode: intPtr(2), Output: "pg_restore failed"},
		}}
	})
	mustConfigure(t, svc)
	if _, err := svc.RunBackup(context.Background(), RunRequest{}); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	target := t.TempDir()
	run, err := svc.Restore(context.Background(), RestoreRequest{SnapshotID: "fakesnap111111", TargetDir: target})
	if err == nil {
		t.Fatal("expected restore to fail on hook failure")
	}
	if run.Status != StatusFailed || run.Stage != StageHooks {
		t.Fatalf("restore run: %+v", run)
	}
	if !strings.Contains(run.Error, "immich") {
		t.Fatalf("error should mention immich: %q", run.Error)
	}
}
