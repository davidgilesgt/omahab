package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/config"
	"github.com/omahab/omahab/internal/store"
)

// Storage mounts and rsync run in a constrained host-namespace unit.
// The daemon keeps its filesystem namespace and syscall sandbox.

// storageMigrateUnitName is the privileged oneshot unit executing staged
// storage migrations in the host mount namespace.
const storageMigrateUnitName = "omahab-storage-migrate@.service"

// defaultStartStorageMigrateUnit starts the privileged migration unit
// without blocking on its work.
var defaultStartStorageMigrateUnit = func(opID string) error {
	unit := "omahab-storage-migrate@" + opID + ".service"
	out, err := execCommand("systemctl", "--no-block", "start", unit)
	if err != nil {
		return fmt.Errorf("start %s: %v: %s", unit, err, strings.TrimSpace(out))
	}
	return nil
}

var startStorageMigrateUnit = defaultStartStorageMigrateUnit

// storageHandoffPath is the tmpfs handoff consumed by the staged executor.
func storageHandoffPath(opID string) string {
	if v := strings.TrimSpace(os.Getenv("OMAHAB_RUNTIME_DIR")); v != "" {
		return filepath.Join(v, "storage-migrate", opID)
	}
	return filepath.Join("/run/omahab/storage-migrate", opID)
}

// startStorageExecutor stages the trusted handoff for opID and starts the
// privileged migration unit. A start failure fails the journal loudly and
// removes the handoff: nothing stays half-staged.
func (b *Backend) startStorageExecutor(opID, action string) error {
	if err := os.Remove(filepath.Join(operationsDir(b.cfg.StateDir, "storage"), opID, "outcome.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := writeStorageHandoff(opID, action); err != nil {
		return err
	}
	if err := startStorageMigrateUnit(opID); err != nil {
		_ = os.Remove(storageHandoffPath(opID))
		j, jerr := loadStorageJournal(b.cfg.StateDir, opID)
		if jerr == nil {
			_ = failStorageOp(b.cfg.StateDir, &j, err.Error())
		}
		return err
	}
	return nil
}

// writeStorageHandoff drops the trusted staging handoff on tmpfs for the
// oneshot unit's CLI run: the operation id and the action (migrate or
// rollback). /run is wiped at boot and the file holds no secret material.
func writeStorageHandoff(opID, action string) error {
	path := storageHandoffPath(opID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create runtime dir: %w", err)
	}
	body, err := json.Marshal(struct {
		OperationID string `json:"operation_id"`
		Action      string `json:"action"`
	}{OperationID: opID, Action: action})
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

// readStorageHandoff consumes (and removes) the tmpfs handoff. A malformed
// handoff is deleted so a one-time trigger never loops.
func readStorageHandoff(expectedID string) (opID, action string, err error) {
	path := storageHandoffPath(expectedID)
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("read storage handoff %s: %w (stage a migration from the panel first)", path, err)
	}
	_ = os.Remove(path)
	var h struct {
		OperationID string `json:"operation_id"`
		Action      string `json:"action"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return "", "", fmt.Errorf("parse storage handoff: %w", err)
	}
	if h.OperationID != expectedID {
		return "", "", fmt.Errorf("storage handoff operation mismatch")
	}
	return strings.TrimSpace(h.OperationID), h.Action, nil
}

// writeStorageOutcome records the completion marker the daemon converges the
// operation journal from.
func writeStorageOutcome(stateDir, opID, status, detail string) error {
	dir := filepath.Join(operationsDir(stateDir, "storage"), opID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(map[string]string{"status": status, "detail": detail}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "outcome.json"), append(raw, '\n'), 0o600)
}

// readStorageOutcome reads the completion marker the staged executor wrote.
func readStorageOutcome(stateDir, opID string) (done bool, status string, detail string) {
	raw, err := os.ReadFile(filepath.Join(operationsDir(stateDir, "storage"), opID, "outcome.json"))
	if err != nil {
		return false, "", ""
	}
	var m struct {
		Status string `json:"status"`
		Detail string `json:"detail,omitempty"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Status == "" {
		return false, "", ""
	}
	return true, m.Status, m.Detail
}

// storageExecutorBackend opens the control database and builds the minimal
// Backend the migration stages need: apps lifecycle, maintenance markers,
// storage file and journals. It runs in the privileged unit only.
func storageExecutorBackend(rt storageRuntime) (*Backend, *store.Store, error) {
	st, err := store.Open(filepath.Join(rt.stateDir, "control.db"))
	if err != nil {
		return nil, nil, err
	}
	b := &Backend{cfg: rt.config()}
	if err := b.attachExecutorServices(st); err != nil {
		_ = st.Close()
		return nil, nil, err
	}
	return b, st, nil
}

// storageRuntime resolves the executor's roots from the environment, exactly
// like the offline recovery runtime.
type storageRuntime struct {
	stateDir   string
	dataDir    string
	catalog    string
	layoutPath string
}

func storageRuntimeFromEnv() storageRuntime {
	env := func(name, fallback string) string {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
		return fallback
	}
	return storageRuntime{
		stateDir:   env("OMAHAB_STATE_DIR", "/var/lib/omahab"),
		dataDir:    env("OMAHAB_DATA_DIR", "/srv/omahab"),
		catalog:    env("OMAHAB_CATALOG", "/usr/share/omahab/catalog/catalog.json"),
		layoutPath: env("OMAHAB_NATIVE_LAYOUT", ""),
	}
}

func (rt storageRuntime) config() config.Config {
	return config.Config{
		StateDir:         rt.stateDir,
		DataDir:          rt.dataDir,
		CatalogPath:      rt.catalog,
		NativeLayoutPath: rt.layoutPath,
		CustomBundlesDir: customBundlesDirFor(rt.stateDir),
	}
}

// RunStorageMigrationStaged executes one trusted handoff in its host-namespace
// unit and records its terminal outcome. Failures also return nonzero to systemd.
func RunStorageMigrationStaged(operationID string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("storage migrate --apply-staged requires root (the %s unit runs it)", storageMigrateUnitName)
	}
	if operationID == "" || filepath.Base(operationID) != operationID || operationID == "." || operationID == ".." {
		return fmt.Errorf("storage migrate requires an operation identifier")
	}
	opID, action, err := readStorageHandoff(operationID)
	if err != nil {
		return err
	}
	rt := storageRuntimeFromEnv()
	b, st, err := storageExecutorBackend(rt)
	if err != nil {
		return err
	}
	defer st.Close()

	status := OpCompleted
	var runErr error
	switch action {
	case "migrate":
		runErr = b.runStorageMigrationSync(opID)
	case "rollback":
		status = OpRolledBak
		runErr = b.runStorageRollbackSync(opID)
	default:
		runErr = fmt.Errorf("unsupported storage action %q", action)
	}
	detail := ""
	if runErr != nil {
		status = OpFailed
		detail = runErr.Error()
	}
	if err := writeStorageOutcome(rt.stateDir, opID, status, detail); err != nil {
		return fmt.Errorf("storage outcome: %w; execution: %v", err, runErr)
	}
	return runErr
}

// runStorageMigrationSync owns the maintenance lock for a journaled move.
func (b *Backend) runStorageMigrationSync(opID string) error {
	err := withMaintenanceLock(func() error {
		j, err := loadStorageJournal(b.cfg.StateDir, opID)
		if err != nil {
			return err
		}
		return b.executeStorageMigration(j)
	})
	if err != nil {
		log.Printf("storage migration %q: %v", opID, err)
		return err
	}
	return nil
}

// runStorageRollbackSync owns the maintenance lock for a journaled rollback.
func (b *Backend) runStorageRollbackSync(opID string) error {
	err := withMaintenanceLock(func() error {
		j, err := loadStorageJournal(b.cfg.StateDir, opID)
		if err != nil {
			return err
		}
		return b.executeStorageRollback(j)
	})
	if err != nil {
		log.Printf("storage rollback %q: %v", opID, err)
		return err
	}
	return nil
}

// executeStorageRollback restores the previous mapping and prior unit state
// (host mount namespace, privileged). Old data was kept untouched, so
// rollback re-persists the old file, re-binds, and resumes apps.
func (b *Backend) executeStorageRollback(j storageJournal) error {
	ctx := context.Background()
	prev := storageFile{Version: storageFileVersion, Placements: j.Previous, Proposals: rollbackProposals(j)}
	if err := writeStorageFile(storageJSONPath(b.cfg.StateDir), prev); err != nil {
		return failStorageOp(b.cfg.StateDir, &j, err.Error())
	}
	known, err := b.knownVolumes(ctx)
	if err != nil {
		return failStorageOp(b.cfg.StateDir, &j, err.Error())
	}
	for _, p := range j.Previous {
		if p.FSUUID == nil {
			continue
		}
		if cp := known[p.Volume]; cp != "" {
			_ = resumeBindMount(known, p)
		}
		_ = restartStorageUnit(p.Volume)
	}
	if err := resumeStorageApps(b, ctx, j); err != nil {
		return failStorageOp(b.cfg.StateDir, &j, "rollback resume: "+err.Error())
	}
	j.Operation.Status = OpRolledBak
	j.Operation.Stage = OpRolledBak
	j.Operation.CanResume = false
	j.Operation.CanRollback = false
	_ = writeStorageJournal(b.cfg.StateDir, j)
	return nil
}

// attachExecutorServices wires the apps service the migration stages need
// onto the executor's lean Backend. Catalog and runner mirror the daemon's
// construction; events default to the log sink.
func (b *Backend) attachExecutorServices(st *store.Store) error {
	catalog, err := b.loadMergedCatalog(context.Background())
	if err != nil {
		return fmt.Errorf("apps catalog: %w", err)
	}
	runner := apps.NewSystemdRunner(nil, "", func(bundleID string) []string {
		bundle, ok := catalog.Get(bundleID)
		if !ok {
			return nil
		}
		return bundle.Units
	})
	appSvc, err := apps.NewService(st.DB(), apps.Options{Catalog: catalog, Runner: runner})
	if err != nil {
		return err
	}
	b.db = st.DB()
	b.apps = appSvc
	return nil
}
