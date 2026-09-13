package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
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

func TestEnsureDefaultSyncFolders(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t, nil)
	if b.syncer == nil {
		t.Fatal("syncer not wired")
	}

	b.ensureDefaultSyncFolders(ctx)
	b.ensureDefaultSyncFolders(ctx)

	ob, err := b.syncer.GetByName(ctx, "obsidian")
	if err != nil {
		t.Fatalf("obsidian folder: %v", err)
	}
	if !ob.ShareWithAI {
		t.Fatal("obsidian must share with AI (notes source)")
	}
	if got := filepath.Join(b.cfg.DataDir, "sync", "obsidian"); ob.ServerPath != got {
		t.Fatalf("obsidian path = %q, want %q", ob.ServerPath, got)
	}

	dr, err := b.syncer.GetByName(ctx, "drops")
	if err != nil {
		t.Fatalf("drops folder: %v", err)
	}
	if dr.ShareWithAI {
		t.Fatal("drops must not share with AI (router output is indexed once by its target app)")
	}
}

func TestCreateCompanionSyncFolderEnrollsExisting(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t, nil)
	if b.syncer == nil {
		t.Fatal("syncer not wired")
	}

	// Pre-provisioned folder (as ensureDefaultSyncFolders leaves it).
	b.ensureDefaultSyncFolders(ctx)
	pre, err := b.syncer.GetByName(ctx, "obsidian")
	if err != nil {
		t.Fatalf("obsidian folder: %v", err)
	}

	// Device enrollment into the existing folder must succeed, not conflict.
	share := true
	f, err := b.CreateCompanionSyncFolder(ctx, apitypes.CreateCompanionSyncFolderRequest{
		Name:        "obsidian",
		ShareWithAI: &share,
		DeviceID:    "DEVICE123456789012345678901234567890123456789012345",
		DeviceName:  "test-device",
	})
	if err != nil {
		t.Fatalf("enroll into existing folder: %v", err)
	}
	if f.ID != pre.ID {
		t.Fatalf("returned folder %q, want pre-provisioned %q", f.ID, pre.ID)
	}
	devs, err := b.syncer.ListDevices(ctx, string(f.ID))
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("enrolled devices = %d, want 1", len(devs))
	}

	// Repeat enrollment is idempotent.
	if _, err := b.CreateCompanionSyncFolder(ctx, apitypes.CreateCompanionSyncFolderRequest{
		Name:        "obsidian",
		ShareWithAI: &share,
		DeviceID:    "DEVICE123456789012345678901234567890123456789012345",
		DeviceName:  "test-device",
	}); err != nil {
		t.Fatalf("repeat enroll: %v", err)
	}
}
