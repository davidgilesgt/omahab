package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apps"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestConvertProducesPinnedTemplate(t *testing.T) {
	root := writeTree(t, map[string]string{
		"catalog.json": `{"bundles":[{
			"name": "demo",
			"displayName": "Demo",
			"composeFile": "compose/demo.yml",
			"supportedArchitectures": ["amd64", "arm64"],
			"images": {
				"demo": "docker.io/example/demo@sha256:${DEMO_DIGEST:?required}",
				"helper": "docker.io/example/helper@sha256:${HELPER_DIGEST:?required}"
			},
			"resources": {"memoryRecommendedMiB": 256},
			"persistence": [
				{"volume": "demo_data", "mountPath": "/data"},
				{"hostPath": "/srv/omahab/apps/demo/files", "mountPath": "/files"}
			],
			"healthCheck": {"endpoint": "http://demo:8080/up"},
			"backup": {"preHooks": ["pg_dump -f /tmp/dump {{.ComposeFile}}"]},
			"oidc": {"supported": true},
			"exposure": {"default": "private", "allowed": ["private", "shared"]}
		}]}`,
		"compose/demo.yml": strings.Join([]string{
			"services:",
			"  demo:",
			"    image: docker.io/example/demo@sha256:${DEMO_DIGEST:?required}",
			"    volumes:",
			"      - demo_data:/data",
			"  helper:",
			"    image: docker.io/example/helper@sha256:${HELPER_DIGEST:?required}",
		}, "\n"),
	})
	digests := map[string]string{
		"demo":   "sha256:" + strings.Repeat("a", 64),
		"helper": "sha256:" + strings.Repeat("b", 64),
	}

	var doc curatedDoc
	raw, err := os.ReadFile(filepath.Join(root, "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	b, err := convert(doc.Bundles[0], root, digests)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if b.ID != "demo" || b.Image != "docker.io/example/demo" || b.Digest != digests["demo"] {
		t.Fatalf("unexpected identity fields: %+v", b)
	}
	if !strings.Contains(b.Compose, "{{.Image}}@{{.Digest}}") {
		t.Fatalf("primary image not templated:\n%s", b.Compose)
	}
	if !strings.Contains(b.Compose, "docker.io/example/helper@"+digests["helper"]) {
		t.Fatalf("helper image not digest-pinned in place:\n%s", b.Compose)
	}
	if len(b.Data) != 1 || b.Data[0].Name != "demo_data" || b.Data[0].Path != "/data" {
		t.Fatalf("host-path bind must not become a named volume: %+v", b.Data)
	}
	if b.HealthCheck.Kind != apps.CheckHTTP || b.HealthCheck.Path != "/up" || b.HealthCheck.Port != 8080 {
		t.Fatalf("unexpected health check: %+v", b.HealthCheck)
	}
	if !b.OIDC.Supported || b.MaxExposure != "shared" || b.Resources.MemoryMB != 256 {
		t.Fatalf("unexpected metadata: %+v", b)
	}
	wantHook := "/srv/omahab/apps/demo/compose.yaml"
	if len(b.Backup.PreBackup) != 3 || !strings.Contains(b.Backup.PreBackup[2], wantHook) {
		t.Fatalf("hook argv wrong: %q", b.Backup.PreBackup)
	}
	if _, err := apps.NewCatalog(b); err != nil {
		t.Fatalf("generated bundle rejected: %v", err)
	}
}

func TestConvertFailsClosedOnMissingDigest(t *testing.T) {
	root := writeTree(t, map[string]string{
		"catalog.json": `{"bundles":[{
			"name": "demo", "displayName": "Demo", "composeFile": "compose/demo.yml",
			"supportedArchitectures": ["amd64"],
			"images": {"demo": "docker.io/example/demo@sha256:${DEMO_DIGEST:?required}"},
			"healthCheck": {"endpoint": "http://demo:8080/up"},
			"exposure": {"default": "private", "allowed": ["private"]}
		}]}`,
		"compose/demo.yml": "services:\n  demo:\n    image: docker.io/example/demo@sha256:${DEMO_DIGEST:?required}\n",
	})
	var doc curatedDoc
	raw, err := os.ReadFile(filepath.Join(root, "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, err := convert(doc.Bundles[0], root, map[string]string{}); err == nil {
		t.Fatal("convert succeeded without a resolved digest")
	}
}
func TestConvertMapsRestoreHooksToPostRestore(t *testing.T) {
	root := writeTree(t, map[string]string{
		"catalog.json": `{"bundles":[{
			"name": "forgejo",
			"displayName": "Forgejo",
			"composeFile": "compose/forgejo.yml",
			"supportedArchitectures": ["amd64", "arm64"],
			"images": {"forgejo": "codeberg.org/forgejo/forgejo@sha256:${FORGEJO_DIGEST:?required}"},
			"resources": {"memoryRecommendedMiB": 1024},
			"persistence": [{"volume": "forgejo_data", "mountPath": "/data"}],
			"healthCheck": {"endpoint": "http://forgejo:3000/-/healthz"},
			"backup": {"preHooks": ["docker compose -f {{.ComposeFile}} exec db pg_dump -Fc -f /tmp/dump"]},
			"restore": {"hooks": ["docker compose -f {{.ComposeFile}} exec db pg_restore --clean --if-exists /tmp/dump", "docker compose -f {{.ComposeFile}} restart app"]},
			"oidc": {"supported": true},
			"exposure": {"default": "private", "allowed": ["private", "shared"]}
		}]}`,
		"compose/forgejo.yml": strings.Join([]string{
			"services:",
			"  forgejo:",
			"    image: codeberg.org/forgejo/forgejo@sha256:${FORGEJO_DIGEST:?required}",
		}, "\n"),
	})
	digests := map[string]string{
		"forgejo": "sha256:" + strings.Repeat("a", 64),
	}
	var doc curatedDoc
	raw, err := os.ReadFile(filepath.Join(root, "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	b, err := convert(doc.Bundles[0], root, digests)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(b.Backup.PreBackup) == 0 {
		t.Fatalf("pre-backup hook missing: %+v", b.Backup)
	}
	if len(b.Backup.PostRestore) == 0 {
		t.Fatalf("restore.hooks should yield non-nil PostRestore, got %+v", b.Backup)
	}
	if len(b.Backup.PostRestore) != 3 || b.Backup.PostRestore[0] != "/bin/sh" || b.Backup.PostRestore[1] != "-c" {
		t.Fatalf("PostRestore argv shape wrong: %q", b.Backup.PostRestore)
	}
	joined := b.Backup.PostRestore[2]
	if !strings.Contains(joined, "pg_restore") || !strings.Contains(joined, "restart app") {
		t.Fatalf("PostRestore joined command missing hooks: %q", joined)
	}
	wantCompose := "/srv/omahab/apps/forgejo/compose.yaml"
	if !strings.Contains(joined, wantCompose) {
		t.Fatalf("PostRestore should embed compose path %q, got %q", wantCompose, joined)
	}
	if !strings.Contains(joined, " && ") {
		t.Fatalf("multiple restore hooks should be joined with &&: %q", joined)
	}
	if _, err := apps.NewCatalog(b); err != nil {
		t.Fatalf("generated bundle rejected: %v", err)
	}
}

func TestConvertRestoreHooksEmptyYieldsNilPostRestore(t *testing.T) {
	root := writeTree(t, map[string]string{
		"catalog.json": `{"bundles":[{
			"name": "demo",
			"displayName": "Demo",
			"composeFile": "compose/demo.yml",
			"supportedArchitectures": ["amd64"],
			"images": {"demo": "docker.io/example/demo@sha256:${DEMO_DIGEST:?required}"},
			"healthCheck": {"endpoint": "http://demo:8080/up"},
			"backup": {"preHooks": []},
			"restore": {"hooks": []},
			"exposure": {"default": "private", "allowed": ["private"]}
		}]}`,
		"compose/demo.yml": "services:\n  demo:\n    image: docker.io/example/demo@sha256:${DEMO_DIGEST:?required}\n",
	})
	digests := map[string]string{"demo": "sha256:" + strings.Repeat("a", 64)}
	var doc curatedDoc
	raw, _ := os.ReadFile(filepath.Join(root, "catalog.json"))
	_ = json.Unmarshal(raw, &doc)
	b, err := convert(doc.Bundles[0], root, digests)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if b.Backup.PostRestore != nil {
		t.Fatalf("empty restore.hooks should yield nil PostRestore, got %q", b.Backup.PostRestore)
	}
	if b.Backup.PreBackup != nil {
		t.Fatalf("empty backup.preHooks should yield nil PreBackup, got %q", b.Backup.PreBackup)
	}
}

func TestConvertBackupPostHooksIgnored(t *testing.T) {
	// backup.postHooks exists in the curated schema (e.g. syncthing resume) but
	// must not be mapped to PostRestore; only restore.hooks drives PostRestore.
	root := writeTree(t, map[string]string{
		"catalog.json": `{"bundles":[{
			"name": "demo",
			"displayName": "Demo",
			"composeFile": "compose/demo.yml",
			"supportedArchitectures": ["amd64"],
			"images": {"demo": "docker.io/example/demo@sha256:${DEMO_DIGEST:?required}"},
			"healthCheck": {"endpoint": "http://demo:8080/up"},
			"backup": {"preHooks": ["echo pre"], "postHooks": ["echo post-should-be-ignored"]},
			"restore": {"hooks": ["echo restore"]},
			"exposure": {"default": "private", "allowed": ["private"]}
		}]}`,
		"compose/demo.yml": "services:\n  demo:\n    image: docker.io/example/demo@sha256:${DEMO_DIGEST:?required}\n",
	})
	digests := map[string]string{"demo": "sha256:" + strings.Repeat("a", 64)}
	var doc curatedDoc
	raw, _ := os.ReadFile(filepath.Join(root, "catalog.json"))
	_ = json.Unmarshal(raw, &doc)
	b, err := convert(doc.Bundles[0], root, digests)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !strings.Contains(strings.Join(b.Backup.PreBackup, " "), "echo pre") {
		t.Fatalf("PreBackup missing: %q", b.Backup.PreBackup)
	}
	if strings.Contains(strings.Join(b.Backup.PostRestore, " "), "post-should-be-ignored") {
		t.Fatalf("backup.postHooks must not leak into PostRestore, got %q", b.Backup.PostRestore)
	}
	if !strings.Contains(strings.Join(b.Backup.PostRestore, " "), "echo restore") {
		t.Fatalf("PostRestore should come from restore.hooks, got %q", b.Backup.PostRestore)
	}
}

func TestConvertRealCatalogRestoreHooksMapped(t *testing.T) {
	// Verify that the checked-in curated catalog's restore.hooks are actually
	// mapped — e.g. forgejo and immich must yield non-nil PostRestore after
	// conversion. Failed mapping would mean the generator still reads
	// backup.postHooks (always empty).
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "catalog", "catalog.json"))
	if err != nil {
		t.Skipf("real catalog not present: %v", err)
	}
	var doc curatedDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse real catalog: %v", err)
	}
	// Build a minimal digests map: every image key gets a dummy sha256.
	for _, cb := range doc.Bundles {
		if len(cb.Restore.Hooks) == 0 {
			continue
		}
		// Need digests for this bundle's images and a compose dir with template.
		// Use the real compose dir so image placeholder checks can pass.
		realComposeDir := filepath.Join("..", "..", "deploy", "catalog")
		digests := map[string]string{}
		for key := range cb.Images {
			digests[key] = "sha256:" + strings.Repeat("a", 64)
		}
		// Only validate one representative bundle to keep test cheap.
		if cb.Name != "forgejo" {
			continue
		}
		b, err := convert(cb, realComposeDir, digests)
		if err != nil {
			t.Fatalf("convert %s: %v", cb.Name, err)
		}
		if len(b.Backup.PostRestore) == 0 {
			t.Fatalf("bundle %s restore.hooks %v should have produced PostRestore, got nil: %+v", cb.Name, cb.Restore.Hooks, b.Backup)
		}
		if len(b.Backup.PreBackup) == 0 {
			t.Fatalf("bundle %s backup.preHooks %v should have produced PreBackup, got nil", cb.Name, cb.Backup.PreHooks)
		}
		t.Logf("bundle %s PreBackup=%q PostRestore=%q", cb.Name, b.Backup.PreBackup, b.Backup.PostRestore)
		return
	}
	t.Fatal("no bundle with restore.hooks found in real catalog")
}

