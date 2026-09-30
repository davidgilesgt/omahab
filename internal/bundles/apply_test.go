package bundles

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// End-to-end repo + apply smoke: init repo, add a bundle from a source dir,
// commit, run Apply with stub hooks (build fails first, attributed to the
// custom bundle, disable + retry succeeds), then verify state.
func TestApplyEndToEnd(t *testing.T) {
	repo := t.TempDir()
	svc, err := NewService(repo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := svc.EnsureRepo(ctx); err != nil {
		t.Fatal(err)
	}

	// Source bundle dir (what `omahab bundle add <dir>` receives).
	src := t.TempDir()
	write := func(rel, content string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(src, rel)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, rel), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("bundle.json", `{"id":"demo","name":"Demo","port":20200,"health_check":{"kind":"http","path":"/","port":20200},"units":["demo.service"]}`)
	write("module.nix", `{ config, lib, ... }: { }`)

	id, err := svc.Add(ctx, src)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if id != "demo" {
		t.Fatalf("id = %q", id)
	}
	// No `mirror` remote in the test repo: the push failure is expected
	// and non-fatal by contract (Forgejo is only a mirror).
	if _, err := svc.Commit(ctx, "add demo"); err != nil && err.Error() == "" {
		t.Fatalf("commit: %v", err)
	}

	// Merged view from the committed repo.
	docs, err := loadCustomDocs(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Bundle.ID != "demo" {
		t.Fatalf("docs = %+v", docs)
	}
	m, err := Merge(nil, docs)
	if err != nil {
		t.Fatalf("merge with empty curated: %v", err)
	}

	buildCalls := 0
	var switched, rolledBack, probed bool
	res, err := svc.Apply(ctx, "path:/tmp/flake#omahab-installed", ApplyHooks{
		Build: func(ctx context.Context, flakeArg, repo string) ([]byte, error) {
			buildCalls++
			if buildCalls == 1 {
				// First build fails inside the custom bundle's module.
				return []byte("error: attribute 'demo-broken' missing at /nix/store/xxx-source/bundles/demo/module.nix:3:5"), errors.New("exit 1")
			}
			return []byte("built"), nil
		},
		Switch: func(ctx context.Context, flakeArg, repo string) ([]byte, error) {
			switched = true
			return []byte("switched"), nil
		},
		HealthGate: func() bool { return true },
		Rollback:   func() error { rolledBack = true; return nil },
		ProbeBundleHealth: func(ctx context.Context, id string) error {
			probed = true
			return nil
		},
	}, m)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if buildCalls != 2 {
		t.Fatalf("buildCalls = %d, want 2 (initial + retry after disable)", buildCalls)
	}
	if len(res.Disabled) != 1 || res.Disabled[0] != "demo" {
		t.Fatalf("disabled = %v", res.Disabled)
	}
	if !switched || rolledBack {
		t.Fatalf("switched=%v rolledBack=%v", switched, rolledBack)
	}
	if probed {
		t.Fatal("disabled bundle must not be health-probed")
	}
	// Disabled marker committed: docs now exclude it.
	docs, err = loadCustomDocs(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 0 {
		t.Fatalf("disabled bundle still merged: %+v", docs)
	}
}
