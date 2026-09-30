package controlplane

import (
	"context"
	"errors"
	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/config"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	// Proposals live apart from active bindings: nothing active yet, one
	// pending proposal the boot parser never reads.
	if len(sf.Placements) != 0 {
		t.Fatalf("placements = %+v, want no active bindings", sf.Placements)
	}
	if len(sf.Proposals) != 1 || sf.Proposals[0].Volume != "aa:state" {
		t.Fatalf("proposals = %+v, want one aa:state proposal", sf.Proposals)
	}
}

// Empty or missing placements must fail validation with no file or operation
// created: a bare PUT {} previously persisted placements:null and broke
// boot-time storage reconciliation.
func TestConfigureStorageRejectsEmptyPlacements(t *testing.T) {
	reqs := []apitypes.ConfigureStorageRequest{
		{},
		{Placements: []apitypes.StoragePlacement{}},
		{Placements: nil, Migrate: true, Confirm: true},
	}
	for i, req := range reqs {
		b := storageTestBackend(t)
		if _, err := b.ConfigureStorage(context.Background(), req); err == nil || !errors.Is(err, apitypes.ErrValidation) {
			t.Fatalf("req %d: err = %v, want validation error", i, err)
		}
		if _, serr := os.Stat(storageJSONPath(b.cfg.StateDir)); !os.IsNotExist(serr) {
			t.Fatalf("req %d: storage.json written despite rejection (stat err %v)", i, serr)
		}
		matches, _ := filepath.Glob(filepath.Join(b.cfg.StateDir, "*-operations"))
		if len(matches) != 0 {
			t.Fatalf("req %d: operation journals created despite rejection: %v", i, matches)
		}
	}
}

// A preexisting placements:null file (persisted by the old empty-PUT path)
// must load safely as unplaced rather than breaking reconciliation.
func TestLoadStorageFileNullPlacements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "storage.json")
	if err := os.WriteFile(path, []byte("{\"version\":2,\"placements\":null}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sf, exists, err := loadStorageFile(path)
	if err != nil {
		t.Fatalf("load null placements: %v", err)
	}
	if !exists {
		t.Fatal("exists = false, want true for a present file")
	}
	if len(sf.Placements) != 0 {
		t.Fatalf("placements = %+v, want none", sf.Placements)
	}
}

// A pending storage operation recorded without a migration journal is an
// inert no-op: dismissing it removes the record instead of stranding it.
func TestDismissOrphanPendingStorageOp(t *testing.T) {
	b := storageTestBackend(t)
	ctx := context.Background()
	op, err := b.ConfigureStorage(ctx, apitypes.ConfigureStorageRequest{
		Placements: []apitypes.StoragePlacement{{Volume: "aa:state", FSUUID: strptr("11111111-1111-1111-1111-111111111111")}},
	})
	if err != nil {
		t.Fatalf("pending configure: %v", err)
	}
	got, err := b.DoOperationAction(ctx, op.ID, "dismiss", true)
	if err != nil {
		t.Fatalf("dismiss orphan: %v", err)
	}
	if got.Status != OpCompleted {
		t.Fatalf("status = %q, want completed", got.Status)
	}
	if _, err := b.GetOperation(ctx, op.ID); err == nil {
		t.Fatal("dismissed operation still loads, want not-found")
	}
}

// A pending-status operation that holds a live migration journal still has
// work to resume or roll back, so dismiss stays a conflict.
func TestDismissJournaledStorageOpStillConflicts(t *testing.T) {
	b := storageTestBackend(t)
	op := newOperation("storage", []string{"aa"})
	op.Status = OpPending
	op.Stage = "preflight"
	if err := os.MkdirAll(filepath.Dir(operationJournalPath(b.cfg.StateDir, "storage", op.ID)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeStorageJournal(b.cfg.StateDir, storageJournal{Operation: op}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.DoOperationAction(context.Background(), op.ID, "dismiss", true); err == nil || !errors.Is(err, apitypes.ErrConflict) {
		t.Fatalf("dismiss journaled op err = %v, want conflict", err)
	}
}

// Writers must never persist placements:null: boot-time jq iterates the
// array, and an unplaced file means the system disk.
func TestWriteStorageFileNormalizesNilPlacements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "storage.json")
	if err := writeStorageFile(path, storageFile{Version: storageFileVersion}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "null") {
		t.Fatalf("storage.json contains null: %s", raw)
	}
	sf, _, err := loadStorageFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(sf.Placements) != 0 {
		t.Fatalf("placements = %+v, want none", sf.Placements)
	}
}

// Proposing placements must preserve live bindings: the SecurityAudit
// migrate:false path replaced storage.json with all-pending entries,
// so after a reboot the boot parser saw no active disk and apps started
// against wrong root data.
func TestConfigureStoragePendingPreservesActive(t *testing.T) {
	b := storageTestBackend(t)
	ctx := context.Background()
	uuidA := "11111111-1111-1111-1111-111111111111"
	// Seed a live binding: aa:state on disk A (same disk for bb avoids the
	// nested-roots conflict; /srv/x contains /srv/x/sub).
	if err := writeStorageFile(storageJSONPath(b.cfg.StateDir), storageFile{
		Version:    storageFileVersion,
		Placements: []StoragePlacement{{Volume: "aa:state", FSUUID: strptr(uuidA)}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ConfigureStorage(ctx, apitypes.ConfigureStorageRequest{
		Placements: []apitypes.StoragePlacement{{Volume: "bb:state", FSUUID: strptr(uuidA)}},
	}); err != nil {
		t.Fatalf("propose: %v", err)
	}
	sf, _, err := loadStorageFile(storageJSONPath(b.cfg.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(sf.Placements) != 1 || sf.Placements[0].Volume != "aa:state" ||
		effectiveUUID(sf.Placements[0]) != uuidA {
		t.Fatalf("placements = %+v, want live aa:state binding intact", sf.Placements)
	}
	if len(sf.Proposals) != 1 || sf.Proposals[0].Volume != "bb:state" {
		t.Fatalf("proposals = %+v, want bb:state proposal", sf.Proposals)
	}
	// Reboot view: the boot parser consumes Placements only, so the
	// proposal cannot redirect the live bind.
	var bootUUID string
	for _, p := range sf.Placements {
		if p.Volume == "aa:state" && p.FSUUID != nil {
			bootUUID = strings.TrimSpace(*p.FSUUID)
		}
	}
	if bootUUID != uuidA {
		t.Fatalf("boot-visible aa:state uuid = %q, want %q", bootUUID, uuidA)
	}
	// Restating the active binding is a no-op: no duplicate proposal.
	if _, err := b.ConfigureStorage(ctx, apitypes.ConfigureStorageRequest{
		Placements: []apitypes.StoragePlacement{
			{Volume: "aa:state", FSUUID: strptr(uuidA)},
			{Volume: "bb:state", FSUUID: strptr(uuidA)},
		},
	}); err != nil {
		t.Fatalf("re-propose: %v", err)
	}
	sf, _, err = loadStorageFile(storageJSONPath(b.cfg.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(sf.Proposals) != 1 || sf.Proposals[0].Volume != "bb:state" {
		t.Fatalf("proposals after restate = %+v, want only bb:state", sf.Proposals)
	}
}

// Commit merge: migration targets override the preserved active set while
// unmentioned volumes keep their bindings; null targets unplace.
func TestMergeActivePlacements(t *testing.T) {
	uuidA := "11111111-1111-1111-1111-111111111111"
	uuidB := "22222222-2222-2222-2222-222222222222"
	prev := []StoragePlacement{
		{Volume: "aa:state", FSUUID: strptr(uuidA)},
		{Volume: "bb:state", FSUUID: strptr(uuidA)},
	}
	got := mergeActivePlacements(prev, []StoragePlacement{{Volume: "bb:state", FSUUID: strptr(uuidB)}})
	byVol := map[string]StoragePlacement{}
	for _, p := range got {
		if _, dup := byVol[p.Volume]; dup {
			t.Fatalf("merged = %+v, duplicate volume", got)
		}
		byVol[p.Volume] = p
	}
	if len(got) != 2 {
		t.Fatalf("merged = %+v, want both volumes", got)
	}
	if effectiveUUID(byVol["aa:state"]) != uuidA {
		t.Fatalf("aa:state = %+v, want preserved binding %q", byVol["aa:state"], uuidA)
	}
	if effectiveUUID(byVol["bb:state"]) != uuidB {
		t.Fatalf("bb:state = %+v, want committed %q", byVol["bb:state"], uuidB)
	}
	// Null target drops the volume back to the system disk.
	got = mergeActivePlacements(prev, []StoragePlacement{{Volume: "aa:state"}})
	if len(got) != 1 || got[0].Volume != "bb:state" {
		t.Fatalf("merged after unplace = %+v, want only bb:state", got)
	}
}

// Setup activation fails closed when an expected active disk is missing
// instead of starting apps against root data; pure proposals stay silent.
func TestSetupPhaseStorageFailsClosedOnMissingDisk(t *testing.T) {
	b := storageTestBackend(t)
	ctx := context.Background()
	if err := writeStorageFile(storageJSONPath(b.cfg.StateDir), storageFile{
		Version: storageFileVersion,
		Placements: []StoragePlacement{
			{Volume: "aa:state", FSUUID: strptr("deadbeef-dead-beef-dead-beefdeadbeef")},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.setupPhaseStorage(ctx); err == nil || !strings.Contains(err.Error(), "aa:state") {
		t.Fatalf("setup with missing active disk err = %v, want failure naming aa:state", err)
	}
	// Proposals referencing absent disks never block setup.
	b2 := storageTestBackend(t)
	if err := writeStorageFile(storageJSONPath(b2.cfg.StateDir), storageFile{
		Version:   storageFileVersion,
		Proposals: []StoragePlacement{{Volume: "aa:state", FSUUID: strptr("deadbeef-dead-beef-dead-beefdeadbeef")}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b2.setupPhaseStorage(ctx); err != nil {
		t.Fatalf("proposals-only setup err = %v, want nil", err)
	}
}
