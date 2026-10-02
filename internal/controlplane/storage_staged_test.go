package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
)

// stubKnownDisk makes validatePlacementUUID's read-only lsblk scan see one
// placeable ext4 disk for the duration of the test.
func stubKnownDisk(t *testing.T, uuid string) {
	t.Helper()
	lsblkScan = func(ctx context.Context) ([]byte, error) {
		return []byte(`{"blockdevices":[{"name":"vdb","size":"4G","type":"disk","fstype":"ext4","uuid":"` + uuid + `","mountpoint":null}]}`), nil
	}
	t.Cleanup(func() {
		lsblkScan = defaultLSBlockScan
	})
}

func stubStorageUnitStart(t *testing.T, started *string, err error) {
	t.Helper()
	startStorageMigrateUnit = func(opID string) error { *started = opID; return err }
	t.Cleanup(func() { startStorageMigrateUnit = defaultStartStorageMigrateUnit })
}

// A unit that cannot start leaves a failed operation and no staged handoff:
// nothing runs half-staged.
func TestConfigureStorageConfirmedUnitStartFailureFailsOp(t *testing.T) {
	b := storageTestBackend(t)
	runtime := t.TempDir()
	t.Setenv("OMAHAB_RUNTIME_DIR", runtime)
	stubKnownDisk(t, "11111111-1111-1111-1111-111111111111")
	var started string
	stubStorageUnitStart(t, &started, os.ErrProcessDone)

	op, err := b.ConfigureStorage(context.Background(), apitypes.ConfigureStorageRequest{
		Placements: []apitypes.StoragePlacement{{Volume: "aa:state", FSUUID: strptr("11111111-1111-1111-1111-111111111111")}},
		Migrate:    true,
		Confirm:    true,
	})
	if err == nil {
		t.Fatalf("unit start failure must error, op = %+v", op)
	}
	if op.ID == "" {
		t.Fatalf("failed start still returns the operation id")
	}
	j, jerr := loadStorageJournal(b.cfg.StateDir, op.ID)
	if jerr != nil {
		t.Fatalf("journal: %v", jerr)
	}
	if j.Operation.Status != OpFailed {
		t.Fatalf("status = %s, want failed", j.Operation.Status)
	}
	if _, err := os.Stat(filepath.Join(runtime, "storage-migrate", op.ID)); !os.IsNotExist(err) {
		t.Fatalf("handoff must be removed after a failed start")
	}
}

// Unknown disks stay a hard 400 before any journal, handoff, or operation:
// the fail-closed contract survives the staging redesign.
func TestConfigureStorageConfirmedUnknownDiskFailsClosed(t *testing.T) {
	b := storageTestBackend(t)
	runtime := t.TempDir()
	t.Setenv("OMAHAB_RUNTIME_DIR", runtime)
	stubKnownDisk(t, "11111111-1111-1111-1111-111111111111")
	var started string
	stubStorageUnitStart(t, &started, nil)

	_, err := b.ConfigureStorage(context.Background(), apitypes.ConfigureStorageRequest{
		Placements: []apitypes.StoragePlacement{{Volume: "aa:state", FSUUID: strptr("99999999-9999-9999-9999-999999999999")}},
		Migrate:    true,
		Confirm:    true,
	})
	if err == nil || !strings.Contains(err.Error(), "unknown disk uuid") {
		t.Fatalf("err = %v, want unknown disk uuid", err)
	}
	if started != "" {
		t.Fatalf("unit must not start for an unknown disk")
	}
	if _, err := os.Stat(filepath.Join(runtime, "storage-migrate")); !os.IsNotExist(err) {
		t.Fatalf("no handoff may exist for an unknown disk")
	}
}

// A journal stuck in running (unit killed) converges toward the outcome
// marker the staged executor writes, like the bundle-apply convergence.
func TestGetOperationConvergesStorageOutcomeMarker(t *testing.T) {
	b := storageTestBackend(t)
	op := newOperation("storage", nil)
	op.Status = OpRunning
	op.Stage = "copied"
	j := storageJournal{Operation: op, Placements: []StoragePlacement{{Volume: "aa:state", FSUUID: strptr("1")}}}
	if err := writeStorageJournal(b.cfg.StateDir, j); err != nil {
		t.Fatal(err)
	}
	if err := writeStorageOutcome(b.cfg.StateDir, op.ID, OpFailed, "rsync: exit 23"); err != nil {
		t.Fatal(err)
	}
	got, err := b.GetOperation(context.Background(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != OpFailed || !strings.Contains(got.Error, "rsync") {
		t.Fatalf("op = %+v, want failed with rsync error", got)
	}
	again, _ := b.GetOperation(context.Background(), op.ID)
	if again.Status != OpFailed {
		t.Fatalf("second read = %s, want failed", again.Status)
	}
}

// Polling a resumed operation must not replay its previous failure.
func TestStorageResumeDoesNotReplayPreviousOutcome(t *testing.T) {
	b := storageTestBackend(t)
	t.Setenv("OMAHAB_RUNTIME_DIR", t.TempDir())
	var started string
	stubStorageUnitStart(t, &started, nil)
	op := newOperation("storage", nil)
	op.Status = OpFailed
	op.CanResume = true
	if err := writeStorageJournal(b.cfg.StateDir, storageJournal{Operation: op}); err != nil {
		t.Fatal(err)
	}
	if err := writeStorageOutcome(b.cfg.StateDir, op.ID, OpFailed, "previous failure"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.DoOperationAction(context.Background(), op.ID, "resume", true); err != nil {
		t.Fatal(err)
	}
	got, err := b.GetOperation(context.Background(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != OpRunning {
		t.Fatalf("resumed operation = %s, want running", got.Status)
	}
}

// The staged executor refuses to run outside its root unit context.
func TestRunStorageMigrationStagedRefusesNonRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("runs as root in this environment")
	}
	if err := RunStorageMigrationStaged("nonroot-test"); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("err = %v, want root requirement", err)
	}
}
