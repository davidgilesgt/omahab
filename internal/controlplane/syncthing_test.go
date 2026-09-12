package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeSyncthingConfig(t *testing.T, dataDir, apiKey string) {
	t.Helper()
	dir := filepath.Join(dataDir, "sync", ".config", "syncthing")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir syncthing config: %v", err)
	}
	body := `<configuration version="52"><gui enabled="true"><address>127.0.0.1:8384</address><apikey>` + apiKey + `</apikey></gui></configuration>`
	if err := os.WriteFile(filepath.Join(dir, "config.xml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write syncthing config: %v", err)
	}
}

func TestSyncthingConfigAPIKey(t *testing.T) {
	if got := syncthingConfigAPIKey(""); got != "" {
		t.Fatalf("empty dataDir = %q, want empty", got)
	}
	if got := syncthingConfigAPIKey(t.TempDir()); got != "" {
		t.Fatalf("missing file = %q, want empty", got)
	}
	root := t.TempDir()
	writeSyncthingConfig(t, root, "WtpaXRVrPovitLiGAgkgnpfXjsifKZem")
	if got := syncthingConfigAPIKey(root); got != "WtpaXRVrPovitLiGAgkgnpfXjsifKZem" {
		t.Fatalf("parsed key = %q", got)
	}
}

func TestImportSyncthingAPIKey(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t, nil)
	dataDir := b.cfg.DataDir

	// Missing config: no secret, no error.
	b.importSyncthingAPIKey(ctx)

	writeSyncthingConfig(t, dataDir, "FIRSTKEY1234567890123456789012")
	b.importSyncthingAPIKey(ctx)
	got, err := b.secrets.RevealByName(ctx, "platform-app", "syncthing_api_key")
	if err != nil || got != "FIRSTKEY1234567890123456789012" {
		t.Fatalf("stored key = %q err=%v", got, err)
	}

	// Rotated upstream key converges on re-import (config is source of truth).
	writeSyncthingConfig(t, dataDir, "SECONDKEY123456789012345678901")
	b.importSyncthingAPIKey(ctx)
	got, err = b.secrets.RevealByName(ctx, "platform-app", "syncthing_api_key")
	if err != nil || got != "SECONDKEY123456789012345678901" {
		t.Fatalf("rotated key = %q err=%v", got, err)
	}
}
