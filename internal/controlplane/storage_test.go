package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/config"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

func strptr(s string) *string { return &s }

type storageTestRunner struct{}

func (storageTestRunner) Deploy(ctx context.Context, app domain.Application, spec apps.DeploySpec) error {
	return nil
}
func (storageTestRunner) Start(ctx context.Context, app domain.Application, spec apps.DeploySpec) error {
	return nil
}
func (storageTestRunner) Stop(ctx context.Context, app domain.Application, spec apps.DeploySpec) error {
	return nil
}
func (storageTestRunner) Remove(ctx context.Context, app domain.Application, spec apps.DeploySpec) error {
	return nil
}
func (storageTestRunner) Check(ctx context.Context, app domain.Application, spec apps.DeploySpec) (domain.Health, error) {
	return domain.HealthHealthy, nil
}

// Legacy v1 media maps to the Immich library subvolume without moving
// content; legacy data is preserved as a pending server-data placement that
// is not applied until an explicit migration runs.
func TestLoadStorageV1Migration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "storage.json")
	raw := `[{"volume":"media","fs_uuid":"UUID-MEDIA"},{"volume":"data","fs_uuid":"UUID-DATA"}]`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	sf, existed, err := loadStorageFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !existed {
		t.Fatal("expected existed=true for legacy file")
	}
	if len(sf.Placements) != 2 {
		t.Fatalf("placements = %+v, want 2", sf.Placements)
	}
	var media, data *StoragePlacement
	for i := range sf.Placements {
		switch sf.Placements[i].Volume {
		case "immich:state":
			media = &sf.Placements[i]
		case "server-data":
			data = &sf.Placements[i]
		}
	}
	if media == nil || media.FSUUID == nil || *media.FSUUID != "UUID-MEDIA" || !media.Legacy {
		t.Fatalf("media migration = %+v, want immich:state legacy UUID-MEDIA", media)
	}
	if media.Pending {
		t.Fatal("legacy media must be active (content stays), not pending")
	}
	if data == nil || data.FSUUID == nil || *data.FSUUID != "UUID-DATA" || !data.Pending {
		t.Fatalf("data migration = %+v, want server-data pending UUID-DATA", data)
	}
}

func TestVolumeSlug(t *testing.T) {
	if got := volumeSlug("immich:state"); got != "immich-state" {
		t.Fatalf("slug = %q", got)
	}
	if got := volumeSlug("postgresql"); got != "postgresql" {
		t.Fatalf("slug = %q", got)
	}
}

func TestOwningBundlesPostgres(t *testing.T) {
	owners := owningBundles([]StoragePlacement{{Volume: "postgresql"}})
	want := map[string]bool{"forgejo": true, "woodpecker": true, "immich": true, "paperless-ngx": true, "litellm": true}
	if len(owners) != len(want) {
		t.Fatalf("owners = %v", owners)
	}
	for _, o := range owners {
		if !want[o] {
			t.Fatalf("unexpected owner %q", o)
		}
	}
	owners = owningBundles([]StoragePlacement{{Volume: "syncthing:state"}})
	if len(owners) != 1 || owners[0] != "syncthing" {
		t.Fatalf("owners = %v", owners)
	}
}

func storageTestBackend(t *testing.T) *Backend {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.Migrate(ctx, apps.Migrations()...); err != nil {
		t.Fatal(err)
	}
	cat, err := apps.NewCatalog(
		apps.Bundle{ID: "aa", Name: "AA", HealthCheck: apps.HealthCheck{Kind: apps.CheckNone},
			Units: []string{"aa.service"}, Data: []apps.DataVolume{{Name: "state", Path: "/srv/x"}}},
		apps.Bundle{ID: "bb", Name: "BB", HealthCheck: apps.HealthCheck{Kind: apps.CheckNone},
			Units: []string{"bb.service"}, Data: []apps.DataVolume{{Name: "state", Path: "/srv/x/sub"}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := apps.NewService(st.DB(), apps.Options{Catalog: cat, Runner: storageTestRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	return &Backend{cfg: config.Config{StateDir: t.TempDir()}, store: st, db: st.DB(), apps: svc}
}

func TestConfigureStorageRejectsUnknownVolume(t *testing.T) {
	b := storageTestBackend(t)
	_, err := b.ConfigureStorage(context.Background(), apitypes.ConfigureStorageRequest{
		Placements: []apitypes.StoragePlacement{{Volume: "nope:state", FSUUID: strptr("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")}},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown volume") {
		t.Fatalf("err = %v, want unknown volume", err)
	}
}

func TestConfigureStorageRejectsDuplicates(t *testing.T) {
	b := storageTestBackend(t)
	_, err := b.ConfigureStorage(context.Background(), apitypes.ConfigureStorageRequest{
		Placements: []apitypes.StoragePlacement{
			{Volume: "aa:state", FSUUID: strptr("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")},
			{Volume: "aa:state", FSUUID: strptr("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v, want duplicate", err)
	}
}

func TestConfigureStorageRejectsNestedDisks(t *testing.T) {
	b := storageTestBackend(t)
	// /srv/x and /srv/x/sub on different disks must share one disk.
	_, err := b.ConfigureStorage(context.Background(), apitypes.ConfigureStorageRequest{
		Placements: []apitypes.StoragePlacement{
			{Volume: "aa:state", FSUUID: strptr("11111111-1111-1111-1111-111111111111")},
			{Volume: "bb:state", FSUUID: strptr("22222222-2222-2222-2222-222222222222")},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "must share one disk") {
		t.Fatalf("err = %v, want nested conflict", err)
	}
}

func TestConfigureStoragePendingRecordsWithoutMounts(t *testing.T) {
	b := storageTestBackend(t)
	op, err := b.ConfigureStorage(context.Background(), apitypes.ConfigureStorageRequest{
		Placements: []apitypes.StoragePlacement{{Volume: "aa:state", FSUUID: strptr("11111111-1111-1111-1111-111111111111")}},
	})
	if err != nil {
		t.Fatalf("pending configure: %v", err)
	}
	if op.Status != OpPending || op.ID == "" {
		t.Fatalf("op = %+v, want pending with id", op)
	}
	sf, _, err := loadStorageFile(storageJSONPath(b.cfg.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(sf.Placements) != 1 || !sf.Placements[0].Pending {
		t.Fatalf("stored = %+v, want one pending placement", sf.Placements)
	}
}
