package bundles

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apps"
)

// Catalog provenance, port constraints, and missing-module errors.
func TestMergeSmoke(t *testing.T) {
	curated, err := apps.NewCatalog(apps.Bundle{
		ID: "caddy", Name: "Caddy", Port: 80,
		HealthCheck: apps.HealthCheck{Kind: "none"},
		Units:       []string{"caddy.service"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Custom bundle: valid shape, reserved port.
	custom := apps.Bundle{
		ID: "myapp", Name: "My App", Port: 20500,
		DefaultExposure: "private", MaxExposure: "private",
		HealthCheck: apps.HealthCheck{Kind: "http", Path: "/", Port: 20500},
		Units:       []string{"myapp.service"},
	}
	m, err := Merge(curated, []CustomDoc{{Dir: "myapp", Bundle: custom}})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got := m.IDs(); len(got) != 2 || got[0] != "caddy" || got[1] != "myapp" {
		t.Fatalf("ids = %v", got)
	}
	b, ok := m.Get("myapp")
	if !ok || b.Source != SourceCustom {
		t.Fatalf("source missing: %+v", b)
	}
	// Port collision with curated port 80.
	badPort := custom
	badPort.Port = 80
	badPort.HealthCheck.Port = 80
	if _, err := Merge(curated, []CustomDoc{{Dir: "myapp", Bundle: badPort}}); err == nil {
		t.Fatal("expected port collision error")
	}
	// Out of reserved range.
	outRange := custom
	outRange.Port = 3000
	if _, err := ValidateCustom(outRange, "myapp"); err == nil {
		t.Fatal("expected port-range error")
	}
	// Missing module.nix must be rejected by ReadCustomBundleFiles.
	repo := t.TempDir()
	initGitRepo(t, repo)
	writeCommit(t, repo, "nomodule/bundle.json", `{"id":"nomodule","name":"X","port":20100,"health_check":{"kind":"none"},"units":["x.service"]}`)
	if _, _, _, err := ReadCustomBundleFiles(context.Background(), repo, "nomodule"); err == nil {
		t.Fatal("expected missing module.nix error")
	}
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	ctx := context.Background()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@omahab"},
		{"config", "user.name", "test"},
		{"commit", "--allow-empty", "-q", "-m", "init"},
	} {
		if _, err := gitExecOutput(ctx, dir, args); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
}

func writeCommit(t *testing.T, repo, rel, content string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-q", "-m", "add " + rel},
	} {
		if _, err := gitExecOutput(ctx, repo, args); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
}

func TestLoadMergedAtIgnoresLaterCommittedMetadata(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	initGitRepo(t, repo)
	writeCommit(t, repo, "demo/module.nix", "{ config, ... }: { }\n")
	writeCommit(t, repo, "demo/bundle.json", `{"id":"demo","name":"Demo","port":20500,"health_check":{"kind":"http","path":"/old","port":20500},"units":["demo-v1.service"]}`)
	revision, err := gitRun(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	writeCommit(t, repo, "demo/bundle.json", `{"id":"demo","name":"Demo","port":20501,"health_check":{"kind":"http","path":"/new","port":20501},"units":["demo-v2.service"]}`)
	pinned, err := LoadMergedAt(ctx, "", repo, strings.TrimSpace(revision))
	if err != nil {
		t.Fatal(err)
	}
	current, err := LoadMerged(ctx, "", repo)
	if err != nil {
		t.Fatal(err)
	}
	old, ok := pinned.Get("demo")
	if !ok || old.Port != 20500 || old.HealthCheck.Path != "/old" || old.Units[0] != "demo-v1.service" {
		t.Fatalf("pinned service metadata changed: %+v", old)
	}
	now, ok := current.Get("demo")
	if !ok || now.Port != 20501 || now.HealthCheck.Path != "/new" || now.Units[0] != "demo-v2.service" {
		t.Fatalf("current service metadata did not change: %+v", now)
	}
}
