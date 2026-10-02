package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/apitypes"
)

func execCommand(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// current journal stage, never from arbitrary journal paths.
const (
	OpPending   = "pending"
	OpRunning   = "running"
	OpPaused    = "paused"
	OpCompleted = "completed"
	OpFailed    = "failed"
	OpRolledBak = "rolled_back"
)

// Operation is the shared progress record for storage/restore/import work.
// It aliases the API type so handlers return it without conversion.
type Operation = apitypes.Operation

func newOperation(kind string, affected []string) Operation {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return Operation{
		ID:          newOperationID(),
		Kind:        kind,
		Status:      OpPending,
		Stage:       "preflight",
		AffectedApp: affected,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

func operationsDir(stateDir, kind string) string {
	return filepath.Join(stateDir, kind+"-operations")
}

func operationJournalPath(stateDir, kind, id string) string {
	return filepath.Join(operationsDir(stateDir, kind), id, "journal.json")
}

func writeOperationJournal(stateDir string, op Operation) error {
	op.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	path := operationJournalPath(stateDir, op.Kind, op.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(op, "", "  ")
	if err != nil {
		return err
	}
	return syncWriteFile(path, append(raw, '\n'), 0o600)
}

func syncWriteFile(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if f, err := os.OpenFile(tmp, os.O_RDWR, mode); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// loadOperation searches storage and restore journals for an operation ID.
func loadOperation(stateDir, id string) (Operation, error) {
	for _, kind := range []string{"storage", "restore", bundleApplyJournalKind} {
		raw, err := os.ReadFile(operationJournalPath(stateDir, kind, id))
		if err != nil {
			continue
		}
		// Storage journals wrap the operation; restore journals may too.
		var sj storageJournal
		if err := json.Unmarshal(raw, &sj); err == nil && sj.Operation.ID == id {
			return sj.Operation, nil
		}
		var op Operation
		if err := json.Unmarshal(raw, &op); err == nil && op.ID == id {
			return op, nil
		}
	}
	return Operation{}, fmt.Errorf("%w: operation %q not found", apitypes.ErrNotFound, id)
}

// storageJournal persists a storage migration across restarts.
type storageJournal struct {
	Operation         Operation          `json:"operation"`
	Placements        []StoragePlacement `json:"placements"`
	Previous          []StoragePlacement `json:"previous"`
	PreviousProposals []StoragePlacement `json:"previous_proposals,omitempty"`
}

func writeStorageJournal(stateDir string, j storageJournal) error {
	j.Operation.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	path := operationJournalPath(stateDir, "storage", j.Operation.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return syncWriteFile(path, append(raw, '\n'), 0o600)
}

func loadStorageJournal(stateDir, id string) (storageJournal, error) {
	raw, err := os.ReadFile(operationJournalPath(stateDir, "storage", id))
	if err != nil {
		return storageJournal{}, fmt.Errorf("%w: storage operation %q not found", apitypes.ErrNotFound, id)
	}
	var j storageJournal
	if err := json.Unmarshal(raw, &j); err != nil {
		return storageJournal{}, fmt.Errorf("parse storage journal: %w", err)
	}
	return j, nil
}

// GetOperation returns the shared progress record for polling. Bundle
// apply journals converge from the CLI outcome marker when the daemon
// restarted mid-apply (the switch inside the apply restarts omahabd).
func (b *Backend) GetOperation(ctx context.Context, id string) (Operation, error) {
	op, err := loadOperation(b.cfg.StateDir, strings.TrimSpace(id))
	if err != nil {
		return Operation{}, err
	}
	if op.Kind == bundleApplyJournalKind && op.Status == OpRunning {
		if done, status, detail := readBundleApplyOutcome(b.cfg.StateDir, op.ID); done {
			op.Status = status
			op.Error = detail
			_ = writeOperationJournal(b.cfg.StateDir, op)
			if op.Status != OpRunning {
				b.releaseBundleApply(op.ID)
			}
			if op.Status == OpCompleted {
				// The switch installed a new merged view; reload it so
				// new/removed/disabled custom bundles show without a
				// daemon restart.
				_ = b.reloadMergedCatalog(ctx)
			}
		}
	}
	// Storage journals converge the same way: the privileged executor may
	// have died before its final journal write, leaving the outcome marker
	// as the only completion record.
	if op.Kind == "storage" && (op.Status == OpRunning || op.Status == "rolling_back") {
		if done, status, detail := readStorageOutcome(b.cfg.StateDir, op.ID); done {
			op.Status = status
			op.Error = detail
			_ = writeOperationJournal(b.cfg.StateDir, op)
		}
	}
	return op, nil
}

// DoOperationAction runs resume|rollback|dismiss from the journal stage.
func (b *Backend) DoOperationAction(ctx context.Context, id string, action string, confirm bool) (Operation, error) {
	if !confirm {
		return Operation{}, fmt.Errorf("%w: confirm:true is required", apitypes.ErrValidation)
	}
	id = strings.TrimSpace(id)
	switch strings.TrimSpace(action) {
	case "dismiss":
		return b.dismissOperation(id)
	case "rollback":
		if _, err := loadStorageJournal(b.cfg.StateDir, id); err == nil {
			return b.rollbackStorageOperation(ctx, id)
		}
		return b.rollbackRestoreOperation(ctx, id)
	case "resume":
		if _, err := loadStorageJournal(b.cfg.StateDir, id); err == nil {
			return b.resumeStorageOperation(ctx, id)
		}
		return b.resumeRestoreOperation(ctx, id)
	default:
		return Operation{}, fmt.Errorf("%w: action must be resume|rollback|dismiss", apitypes.ErrValidation)
	}
}

func (b *Backend) dismissOperation(id string) (Operation, error) {
	op, err := loadOperation(b.cfg.StateDir, id)
	if err != nil {
		return Operation{}, err
	}
	// Orphan pending no-op records (for example pre-validation storage
	// placements recorded without a migration journal) never started work:
	// nothing blocks on them and no journal holds live state, so removing
	// the inert record is safe. Anything with a live journal stays on the
	// resume/rollback path below.
	if op.Status == OpPending && !hasLiveJournal(b.cfg.StateDir, op) {
		_ = os.RemoveAll(filepath.Join(operationsDir(b.cfg.StateDir, op.Kind), id))
		op.Status = OpCompleted
		return op, nil
	}
	if op.Status == OpRunning || op.Status == OpPending || op.Status == OpPaused {
		return Operation{}, fmt.Errorf("%w: cannot dismiss %s operation", apitypes.ErrConflict, op.Status)
	}
	// Dismiss removes only this operation's validated staging tree and its
	// journal; live state is untouched.
	if op.Kind == "restore" {
		if j, err := loadRestoreJournal(b.cfg.StateDir, id); err == nil {
			if j.StagingDir != "" {
				_ = os.RemoveAll(j.StagingDir)
			}
		}
	}
	_ = os.RemoveAll(filepath.Join(operationsDir(b.cfg.StateDir, op.Kind), id))
	op.Status = OpCompleted
	return op, nil
}

// hasLiveJournal reports whether id holds journaled migration/restore state.
// Storage placements recorded without migrate:true persist only a bare
// operation record (no storage journal); a bare record unmarshals with an
// empty wrapped operation, so the ID comparison distinguishes it from a
// live migration journal.
func hasLiveJournal(stateDir string, op Operation) bool {
	switch op.Kind {
	case "storage":
		j, err := loadStorageJournal(stateDir, op.ID)
		if err != nil {
			return false
		}
		return j.Operation.ID == op.ID
	case "restore":
		j, err := loadRestoreJournal(stateDir, op.ID)
		if err != nil {
			return false
		}
		return j.Operation.ID == op.ID
	default:
		return true
	}
}

func (b *Backend) resumeStorageOperation(ctx context.Context, id string) (Operation, error) {
	j, err := loadStorageJournal(b.cfg.StateDir, id)
	if err != nil {
		return Operation{}, err
	}
	if !j.Operation.CanResume {
		return Operation{}, fmt.Errorf("%w: operation cannot resume from stage %q", apitypes.ErrConflict, j.Operation.Stage)
	}
	j.Operation.Status = OpRunning
	j.Operation.CanResume = false
	if err := writeStorageJournal(b.cfg.StateDir, j); err != nil {
		return Operation{}, err
	}
	if err := b.startStorageExecutor(id, "migrate"); err != nil {
		return Operation{}, translateError(err)
	}
	return j.Operation, nil
}

func (b *Backend) rollbackStorageOperation(ctx context.Context, id string) (Operation, error) {
	j, err := loadStorageJournal(b.cfg.StateDir, id)
	if err != nil {
		return Operation{}, err
	}
	if !j.Operation.CanRollback {
		return Operation{}, fmt.Errorf("%w: operation cannot roll back from stage %q", apitypes.ErrConflict, j.Operation.Stage)
	}
	j.Operation.Status = "rolling_back"
	_ = writeStorageJournal(b.cfg.StateDir, j)
	if err := b.startStorageExecutor(id, "rollback"); err != nil {
		return Operation{}, translateError(err)
	}
	return j.Operation, nil
}

func resumeBindMount(known map[string]string, p StoragePlacement) error {
	if p.FSUUID == nil {
		return nil
	}
	cp := known[p.Volume]
	if cp == "" {
		return nil
	}
	uuid := strings.TrimSpace(*p.FSUUID)
	src := managedSubdir(uuid, p.Volume)
	out, err := execCommand("mount", "--bind", src, cp)
	if err != nil {
		return fmt.Errorf("bind %q -> %q: %v: %s", src, cp, err, strings.TrimSpace(out))
	}
	return nil
}

func restartStorageUnit(volume string) error {
	out, err := execCommand("systemctl", "restart", "omahab-storage-"+volumeSlug(volume)+".service")
	if err != nil {
		return fmt.Errorf("restart storage unit: %v: %s", err, strings.TrimSpace(out))
	}
	return nil
}
