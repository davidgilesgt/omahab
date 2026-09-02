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
			"exposure": {"default": "private", "allowed": ["private", "shared"], "caddyRoute": "demo.{{.Domain}}", "internalPort": 8080}
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
	if b.Port != 8080 || b.Route != "demo" {
		t.Fatalf("unexpected route/port: port=%d route=%q", b.Port, b.Route)
	}
	wantHook := "/srv/omahab/apps/demo/compose.yaml"
	if len(b.Backup.PreBackup) != 3 || !strings.Contains(b.Backup.PreBackup[2], wantHook) {
		t.Fatalf("hook argv wrong: %q", b.Backup.PreBackup)
	}
	if _, err := apps.NewCatalog(b); err != nil {
		t.Fatalf("generated bundle rejected: %v", err)
	}
}

func TestConvertRejectsRoutedBundleWithoutPort(t *testing.T) {
	root := writeTree(t, map[string]string{
		"catalog.json": `{"bundles":[{
			"name": "demo", "displayName": "Demo", "composeFile": "compose/demo.yml",
			"supportedArchitectures": ["amd64"],
			"images": {"demo": "docker.io/example/demo@sha256:${DEMO_DIGEST:?required}"},
			"healthCheck": {"endpoint": "http://demo:8080/up"},
			"exposure": {"default": "private", "allowed": ["private"], "caddyRoute": "demo.{{.Domain}}"}
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
	digests := map[string]string{"demo": "sha256:" + strings.Repeat("a", 64)}
	if _, err := convert(doc.Bundles[0], root, digests); err == nil {
		t.Fatal("convert succeeded without internalPort")
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

func TestHealthCheckPrefersCommandOverEndpoint(t *testing.T) {
	root := writeTree(t, map[string]string{
		"catalog.json": `{"bundles":[{
			"name": "demo", "displayName": "Demo", "composeFile": "compose/demo.yml",
			"supportedArchitectures": ["amd64"],
			"images": {"demo": "docker.io/example/demo@sha256:${DEMO_DIGEST:?required}"},
			"healthCheck": {
				"test": ["CMD-SHELL", "wget --spider http://127.0.0.1:2019/config/ || exit 1"],
				"endpoint": "http://127.0.0.1:2019/config/"
			},
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
	b, err := convert(doc.Bundles[0], root, map[string]string{"demo": "sha256:" + strings.Repeat("a", 64)})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if b.HealthCheck.Kind != apps.CheckCommand {
		t.Fatalf("want command health check, got %+v", b.HealthCheck)
	}
	if b.HealthCheck.Service != "demo" {
		t.Fatalf("service = %q want demo", b.HealthCheck.Service)
	}
	if len(b.HealthCheck.Command) != 3 || b.HealthCheck.Command[0] != "sh" || b.HealthCheck.Command[1] != "-c" {
		t.Fatalf("command = %q", b.HealthCheck.Command)
	}
}

func TestRunFailsOnMissingEnabledByDefaultDigest(t *testing.T) {
	root := writeTree(t, map[string]string{
		"catalog.json": `{"bundles":[{
			"name": "caddy", "displayName": "Caddy", "composeFile": "compose/caddy.yml",
			"enabledByDefault": true,
			"supportedArchitectures": ["amd64"],
			"images": {"caddy": "ghcr.io/caddybuilds/caddy-cloudflare@sha256:${CADDY_DIGEST:?required}"},
			"healthCheck": {"test": ["CMD-SHELL", "caddy version"]},
			"exposure": {"default": "public", "allowed": ["public"]}
		}]}`,
		"compose/caddy.yml": "services:\n  caddy:\n    image: ghcr.io/caddybuilds/caddy-cloudflare@sha256:${CADDY_DIGEST:?required}\n",
		"digests.json":      `{"other":"sha256:` + strings.Repeat("b", 64) + `"}`,
	})
	out := filepath.Join(root, "apps-catalog.json")
	err := run([]string{
		"-catalog", filepath.Join(root, "catalog.json"),
		"-compose-dir", root,
		"-digests", filepath.Join(root, "digests.json"),
		"-out", out,
	})
	if err == nil {
		t.Fatal("run succeeded without required digest")
	}
	if !strings.Contains(err.Error(), "caddy") {
		t.Fatalf("error %q should name the default bundle", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Fatal("must not write catalog when a default digest is missing")
	}
}

func TestConvertRealCatalogDefaultsAndHealth(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "catalog", "catalog.json"))
	if err != nil {
		t.Skipf("real catalog not present: %v", err)
	}
	var doc curatedDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse real catalog: %v", err)
	}
	realComposeDir := filepath.Join("..", "..", "deploy", "catalog")
	digests := map[string]string{}
	for _, cb := range doc.Bundles {
		for key := range cb.Images {
			digests[key] = "sha256:" + strings.Repeat("a", 64)
		}
		if cb.PipelineImage != "" {
			if _, _, ok := splitImageRef(cb.PipelineImage); ok {
				// Extract variable to key mapping similar to convert logic
				_, variable, _ := splitImageRef(cb.PipelineImage)
				lowerVar := strings.ToLower(variable)
				if strings.HasSuffix(lowerVar, "_digest") {
					candidate := strings.TrimSuffix(lowerVar, "_digest")
					candidate = strings.ReplaceAll(candidate, "_", "-")
					digests[candidate] = "sha256:" + strings.Repeat("a", 64)
				}
			}
		}
		if cb.PipelineImageKey != "" {
			digests[cb.PipelineImageKey] = "sha256:" + strings.Repeat("b", 64)
		}
	}
	var defaults []string
	var immich apps.Bundle
	var pocketID apps.Bundle
	var forgejo apps.Bundle
	var woodpecker apps.Bundle
	for _, cb := range doc.Bundles {
		b, err := convert(cb, realComposeDir, digests)
		if err != nil {
			t.Fatalf("convert %s: %v", cb.Name, err)
		}
		if b.Default {
			defaults = append(defaults, b.ID)
		}
	switch b.ID {
	case "immich":
		immich = b
	case "pocket-id":
		pocketID = b
	case "forgejo":
		forgejo = b
	case "woodpecker":
		woodpecker = b
	}
		if strings.TrimSpace(b.Route) != "" && b.Port <= 0 {
			t.Fatalf("routed bundle %s missing positive port", b.ID)
		}
		if (b.ID == "caddy" || b.ID == "immich" || b.ID == "pocket-id") && b.HealthCheck.Kind != apps.CheckCommand {
			t.Fatalf("%s health check kind = %q want command: %+v", b.ID, b.HealthCheck.Kind, b.HealthCheck)
		}
	}
	wantDefaults := "caddy,pocket-id,forgejo,woodpecker,hermes,immich,paperless-ngx,karakeep,syncthing,litellm,embedding-worker,ntfy"
	if got := strings.Join(defaults, ","); got != wantDefaults {
		t.Fatalf("default bundles = %q want %q", got, wantDefaults)
	}
	// Native (systemd-runtime) bundles carry no compose or digest — their
	// versions track the nixpkgs pin — and hermes stays compose-placed.
	nativeWant := map[string]bool{
		"caddy": true, "pocket-id": true, "forgejo": true, "woodpecker": true,
		"immich": true, "paperless-ngx": true, "karakeep": true, "syncthing": true,
		"litellm": true, "embedding-worker": true, "ntfy": true,
	}
	for _, cb := range doc.Bundles {
		b, err := convert(cb, realComposeDir, digests)
		if err != nil {
			t.Fatalf("bundle %s: %v", cb.Name, err)
		}
		if nativeWant[b.ID] {
			if b.Runtime != apps.RuntimeSystemd {
				t.Fatalf("%s runtime = %q want systemd", b.ID, b.Runtime)
			}
			if len(b.Units) == 0 {
				t.Fatalf("%s systemd bundle declares no units", b.ID)
			}
			if b.Compose != "" {
				t.Fatalf("%s systemd bundle should carry no compose", b.ID)
			}
		}
	}
	if pocketID.Port != 1411 {
		t.Fatalf("pocket-id port = %d want 1411", pocketID.Port)
	}
	if forgejo.Port != 3000 {
		t.Fatalf("forgejo port = %d want 3000", forgejo.Port)
	}
	if woodpecker.Port != 8000 {
		t.Fatalf("woodpecker port = %d want 8000", woodpecker.Port)
	}
	if woodpecker.PipelineImage == "" || !strings.Contains(woodpecker.PipelineImage, "quay.io/podman/stable@sha256:") {
		t.Fatalf("woodpecker pipeline image missing or not podman: %q", woodpecker.PipelineImage)
	}
	if immich.Port != 2283 {
		t.Fatalf("immich port = %d want 2283", immich.Port)
	}
}
