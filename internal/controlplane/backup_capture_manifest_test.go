package controlplane

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/config"
)

// customBundleRepoForTest builds a committed custom-bundles repo holding
// one bundle (demo) with bundle.json + module.nix, mirroring the on-disk
// layout /var/lib/omahab/bundles/<id>/.
func customBundleRepoForTest(t *testing.T) (repo string, head string) {
	t.Helper()
	repo = t.TempDir()
	ctx := context.Background()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@omahab",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@omahab")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	dir := filepath.Join(repo, "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bundleJSON := `{"id":"demo","name":"Demo","port":20200,"health_check":{"kind":"none"},"units":["demo.service"]}`
	if err := os.WriteFile(filepath.Join(dir, "bundle.json"), []byte(bundleJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "module.nix"), []byte("{ config, lib, ... }: { }"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "demo bundle")
	out, err := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return repo, strings.TrimSpace(string(out))
}

// captureBackendWithCustomBundle wires a backend whose catalog contains
// the custom bundle (the merged view the runtime would serve).
func captureBackendWithCustomBundle(t *testing.T, repo string) *Backend {
	t.Helper()
	b, _ := newSetupBackend(t, nil)
	ctx := context.Background()
	stateDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stateDir, "dumps"), 0o700); err != nil {
		t.Fatal(err)
	}
	b.cfg = config.Config{StateDir: stateDir, DataDir: t.TempDir(), CustomBundlesDir: repo}
	// The catalog must know the bundle: load the merged view the daemon
	// serves and install it into the apps service.
	c, err := b.loadMergedCatalog(ctx)
	if err != nil {
		t.Fatalf("merged catalog: %v", err)
	}
	demo, ok := c.Get("demo")
	if !ok {
		t.Fatal("merged catalog lacks the custom bundle")
	}
	svc, err := apps.NewService(b.db, apps.Options{Catalog: c, Runner: storageTestRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	b.apps = svc
	_ = demo
	return b
}

// TestCaptureStagesCustomBundleDefinitions pins the capture contract for
// custom bundles: a custom bundle in the plan is archived into
// staging/bundles/<id> (committed files at HEAD) and the manifest scope
// records Source=custom plus the definition rev.
func TestCaptureStagesCustomBundleDefinitions(t *testing.T) {
	repo, head := customBundleRepoForTest(t)
	b := captureBackendWithCustomBundle(t, repo)
	ctx := context.Background()
	if _, err := b.apps.Install(ctx, apps.InstallRequest{BundleID: "demo"}); err != nil {
		t.Fatal(err)
	}
	plan, err := b.planBackupCapture(ctx, []string{"app:demo"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	staging := t.TempDir()
	if err := b.stageCustomBundleDefinitions(ctx, plan, staging); err != nil {
		t.Fatalf("stage custom bundle definitions: %v", err)
	}
	// The committed definition files landed in staging/bundles/demo.
	for _, name := range []string{"bundle.json", "module.nix"} {
		if _, err := os.Stat(filepath.Join(staging, "bundles", "demo", name)); err != nil {
			t.Fatalf("staged definition missing %s: %v", name, err)
		}
	}
	// The manifest records the custom source and the definition rev.
	manifest, err := b.buildManifest(ctx, []string{"app:demo"}, plan, staging)
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	scope, ok := manifest.Scope(backups.AppScope("demo"))
	if !ok {
		t.Fatalf("manifest scopes %v lack app:demo", manifest.ScopeNames())
	}
	if scope.Bundle == nil {
		t.Fatal("app scope has no bundle record")
	}
	if scope.Bundle.Source != "custom" {
		t.Fatalf("bundle source = %q, want custom", scope.Bundle.Source)
	}
	if scope.Bundle.DefinitionPath != "bundles/demo" {
		t.Fatalf("definition path = %q", scope.Bundle.DefinitionPath)
	}
	if scope.Bundle.DefinitionRev != head {
		t.Fatalf("definition rev = %q, want repo HEAD %q", scope.Bundle.DefinitionRev, head)
	}
}
