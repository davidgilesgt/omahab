package bundles

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func writeTree(t *testing.T, base, rel, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(base, rel)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, rel), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

const demoBundleJSON = `{"id":"demo","name":"Demo","port":20200,"health_check":{"kind":"http","path":"/","port":20200},"units":["demo.service"]}`

// newRepoWithBundle initializes a repo and commits one valid bundle.
func newRepoWithBundle(t *testing.T) (*Service, string) {
	t.Helper()
	repo := t.TempDir()
	svc, err := NewService(repo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := svc.EnsureRepo(ctx); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	writeTree(t, src, "bundle.json", demoBundleJSON)
	writeTree(t, src, "module.nix", `{ config, lib, ... }: { }`)
	if _, err := svc.Add(ctx, src); err != nil {
		t.Fatal(err)
	}
	if commit, err := svc.Commit(ctx, "add demo"); err != nil && commit == "" {
		t.Fatal(err)
	}
	return svc, repo
}

// mergeDocsAt builds the apply-time merge function: validate the given
// docs through the full Merge contract.
func mergeDocsAt(ctx context.Context, docs []CustomDoc) (*Merged, error) {
	return Merge(nil, docs)
}

// End-to-end repo + apply smoke: init repo, add a bundle from a source
// dir, commit, run Apply with stub hooks (build fails first, attributed
// to the custom bundle, disable + retry succeeds), then verify state.
func TestApplyEndToEnd(t *testing.T) {
	svc, repo := newRepoWithBundle(t)
	ctx := context.Background()

	buildCalls := 0
	var rolledBack, probed bool
	res, err := svc.Apply(ctx, "path:/tmp/flake#omahab-installed", ApplyHooks{
		Build: func(ctx context.Context, flakeArg, repo, rev string) ([]byte, error) {
			buildCalls++
			if buildCalls == 1 {
				// First build fails inside the custom bundle's module.
				return []byte("error: attribute 'demo-broken' missing at " + repo + "/demo/module.nix:3:5"), errors.New("exit 1")
			}
			return []byte("built"), nil
		},
		Switch: func(ctx context.Context, flakeArg, repo, rev string) ([]byte, error) {
			return []byte("switched"), nil
		},
		HealthGate: func() bool { return true },
		Rollback:   func() error { rolledBack = true; return nil },
		ProbeBundleHealth: func(ctx context.Context, id, revision string) error {
			probed = true
			return nil
		},
	}, mergeDocsAt)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if buildCalls != 2 {
		t.Fatalf("buildCalls = %d, want 2 (initial + retry after disable)", buildCalls)
	}
	if len(res.Disabled) != 1 || res.Disabled[0] != "demo" {
		t.Fatalf("disabled = %v", res.Disabled)
	}
	if rolledBack {
		t.Fatal("successful apply must not roll back")
	}
	if probed {
		t.Fatal("disabled bundle must not be health-probed")
	}
	// Disabled marker committed: docs now exclude it.
	docs, err := loadCustomDocs(ctx, repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 0 {
		t.Fatalf("disabled bundle still merged: %+v", docs)
	}
}

// TestApplyRejectsDirtyInvalidChanges: a dirty worktree holding an
// invalid bundle (out-of-range port) must fail Apply before any commit
// happens, so the invalid definition never reaches HEAD.
func TestApplyRejectsDirtyInvalidChanges(t *testing.T) {
	svc, repo := newRepoWithBundle(t)
	ctx := context.Background()
	headBefore, err := svc.RevParseHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Dirty the worktree with an invalid second bundle (port outside the
	// reserved custom range).
	writeTree(t, repo, "bad/bundle.json", `{"id":"bad","name":"Bad","port":80,"health_check":{"kind":"http","path":"/","port":80},"units":["bad.service"]}`)
	writeTree(t, repo, "bad/module.nix", `{ config, lib, ... }: { }`)

	buildRan := false
	res, err := svc.Apply(ctx, "path:/tmp/flake#omahab-installed", ApplyHooks{
		Build: func(ctx context.Context, flakeArg, repo, rev string) ([]byte, error) {
			buildRan = true
			return []byte("built"), nil
		},
		Switch:     func(ctx context.Context, flakeArg, repo, rev string) ([]byte, error) { return nil, nil },
		HealthGate: func() bool { return true },
	}, mergeDocsAt)
	if err == nil {
		t.Fatalf("apply must reject an invalid dirty worktree, got res=%+v", res)
	}
	if buildRan {
		t.Fatal("no build may run when the intended changes are invalid")
	}
	// HEAD is untouched: the invalid files stay dirty in the worktree.
	headAfter, err := svc.RevParseHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if headAfter != headBefore {
		t.Fatalf("HEAD moved from %s to %s; invalid changes must not be committed", headBefore, headAfter)
	}
}

// TestApplyRejectsDirtyCollision: a dirty bundle colliding with a
// curated bundle id must fail before commit. The merge function here
// stands in for the curated catalog.
func TestApplyRejectsDirtyCollision(t *testing.T) {
	svc, repo := newRepoWithBundle(t)
	ctx := context.Background()
	headBefore, err := svc.RevParseHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Dirty worktree adds a bundle whose unit collides with "demo".
	writeTree(t, repo, "clash/bundle.json", `{"id":"clash","name":"Clash","port":20201,"health_check":{"kind":"http","path":"/","port":20201},"units":["demo.service"]}`)
	writeTree(t, repo, "clash/module.nix", `{ config, lib, ... }: { }`)

	_, err = svc.Apply(ctx, "path:/tmp/flake#omahab-installed", ApplyHooks{
		Build:      func(ctx context.Context, flakeArg, repo, rev string) ([]byte, error) { return []byte("built"), nil },
		Switch:     func(ctx context.Context, flakeArg, repo, rev string) ([]byte, error) { return nil, nil },
		HealthGate: func() bool { return true },
	}, mergeDocsAt)
	if err == nil {
		t.Fatal("apply must reject a colliding dirty bundle before commit")
	}
	headAfter, err := svc.RevParseHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if headAfter != headBefore {
		t.Fatalf("HEAD moved despite a rejected collision: %s -> %s", headBefore, headAfter)
	}
}

func TestApplyRevalidatesCommitAfterConcurrentEdit(t *testing.T) {
	svc, repo := newRepoWithBundle(t)
	ctx := context.Background()
	headBefore, err := svc.RevParseHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, repo, "demo/bundle.json", demoBundleJSON+"\n ")
	edited := false
	built := false
	_, err = svc.Apply(ctx, "path:/tmp/flake#omahab-installed", ApplyHooks{
		Build: func(ctx context.Context, flakeArg, repo, rev string) ([]byte, error) {
			built = true
			return nil, nil
		},
		Switch:     func(ctx context.Context, flakeArg, repo, rev string) ([]byte, error) { return nil, nil },
		HealthGate: func() bool { return true },
	}, func(ctx context.Context, docs []CustomDoc) (*Merged, error) {
		merged, err := Merge(nil, docs)
		if err == nil && !edited {
			edited = true
			// A writer changes the live repo after intended-state validation.
			writeTree(t, repo, "demo/bundle.json", `{"id":"demo","name":"Demo","port":80,"health_check":{"kind":"http","path":"/","port":80},"units":["demo.service"]}`)
		}
		return merged, err
	})
	if err == nil || built {
		t.Fatalf("invalid committed catalog reached a build: err=%v built=%v", err, built)
	}
	headAfter, err := svc.RevParseHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if headAfter == headBefore {
		t.Fatal("the edit did not reach the committed revision")
	}
}

// TestApplySwitchFailureRollsBack: a switch failure must attempt a
// rollback before returning, because nixos-rebuild may have activated a
// generation before failing.
func TestApplySwitchFailureRollsBack(t *testing.T) {
	svc, _ := newRepoWithBundle(t)
	ctx := context.Background()
	rolledBack := false
	res, err := svc.Apply(ctx, "path:/tmp/flake#omahab-installed", ApplyHooks{
		Build: func(ctx context.Context, flakeArg, repo, rev string) ([]byte, error) { return []byte("built"), nil },
		Switch: func(ctx context.Context, flakeArg, repo, rev string) ([]byte, error) {
			return []byte("unit failed"), errors.New("exit 1")
		},
		HealthGate: func() bool { return true },
		Rollback:   func() error { rolledBack = true; return nil },
	}, mergeDocsAt)
	if err == nil {
		t.Fatal("switch failure must fail the apply")
	}
	if !rolledBack {
		t.Fatal("switch failure must attempt rollback (a generation may be active)")
	}
	if res == nil || !res.RolledBack {
		t.Fatalf("result must report RolledBack, got %+v", res)
	}
}

// TestApplySwitchFailureWithoutRollbackHook: no Rollback hook means the
// switch error is reported as-is.
func TestApplySwitchFailureWithoutRollbackHook(t *testing.T) {
	svc, _ := newRepoWithBundle(t)
	ctx := context.Background()
	_, err := svc.Apply(ctx, "path:/tmp/flake#omahab-installed", ApplyHooks{
		Build: func(ctx context.Context, flakeArg, repo, rev string) ([]byte, error) { return []byte("built"), nil },
		Switch: func(ctx context.Context, flakeArg, repo, rev string) ([]byte, error) {
			return nil, errors.New("exit 1")
		},
		HealthGate: func() bool { return true },
	}, mergeDocsAt)
	if err == nil {
		t.Fatalf("want switch error, got %v", err)
	}
}

// TestApplyBundleHealthFailureRollsBack: an unhealthy bundle after
// switch must trigger rollback.
func TestApplyBundleHealthFailureRollsBack(t *testing.T) {
	svc, _ := newRepoWithBundle(t)
	ctx := context.Background()
	rolledBack := false
	res, err := svc.Apply(ctx, "path:/tmp/flake#omahab-installed", ApplyHooks{
		Build:      func(ctx context.Context, flakeArg, repo, rev string) ([]byte, error) { return []byte("built"), nil },
		Switch:     func(ctx context.Context, flakeArg, repo, rev string) ([]byte, error) { return []byte("switched"), nil },
		HealthGate: func() bool { return true },
		Rollback:   func() error { rolledBack = true; return nil },
		ProbeBundleHealth: func(ctx context.Context, id, revision string) error {
			return fmt.Errorf("connection refused")
		},
	}, mergeDocsAt)
	if err == nil {
		t.Fatal("unhealthy bundle must fail the apply")
	}
	if !rolledBack {
		t.Fatal("bundle health failure must roll back")
	}
	if res == nil || !res.RolledBack {
		t.Fatalf("result must report RolledBack, got %+v", res)
	}
}

func TestApplyHealthFailureDoesNotClaimUnsuccessfulRollback(t *testing.T) {
	for _, bundleHealth := range []bool{false, true} {
		for _, missingRollback := range []bool{false, true} {
			t.Run(fmt.Sprintf("bundle=%t/missing-rollback=%t", bundleHealth, missingRollback), func(t *testing.T) {
				svc, _ := newRepoWithBundle(t)
				hooks := ApplyHooks{
					Build:      func(context.Context, string, string, string) ([]byte, error) { return nil, nil },
					Switch:     func(context.Context, string, string, string) ([]byte, error) { return nil, nil },
					HealthGate: func() bool { return bundleHealth },
					ProbeBundleHealth: func(context.Context, string, string) error {
						return errors.New("service unavailable")
					},
				}
				if !missingRollback {
					hooks.Rollback = func() error { return errors.New("rollback command failed") }
				}
				res, err := svc.Apply(context.Background(), "path:/tmp/flake#omahab-installed", hooks, mergeDocsAt)
				if err == nil || res == nil || res.RolledBack {
					t.Fatalf("unsuccessful rollback reported success: result=%+v err=%v", res, err)
				}
			})
		}
	}
}

// TestValidateIntendedRejectsMissingModule: a dirty bundle without
// module.nix fails ValidateIntended (the exported pre-commit gate used
// by `omahab bundle add`).
func TestValidateIntendedRejectsMissingModule(t *testing.T) {
	svc, repo := newRepoWithBundle(t)
	ctx := context.Background()
	writeTree(t, repo, "nomodule/bundle.json", `{"id":"nomodule","name":"NoModule","port":20202,"health_check":{"kind":"http","path":"/","port":20202},"units":["nomodule.service"]}`)
	if err := svc.ValidateIntended(ctx, mergeDocsAt); err == nil {
		t.Fatal("ValidateIntended must reject a bundle without module.nix")
	}
}
