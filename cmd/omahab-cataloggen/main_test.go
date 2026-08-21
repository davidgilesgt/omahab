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
