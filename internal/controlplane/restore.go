package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

// Scoped restore: staging extracts and validates without touching live apps;
// live promotion (4b) requires explicit confirmation plus a pre-restore
// safety snapshot when data exists. Staging finishes after extraction and
// isolated validation; it must not stop, restart, reconfigure or run SQL
// against any live app. The staging tree is kept until explicit dismissal.

// Offline journal stages for restore operations.
const (
	RestorePreflight     = "preflight"
	RestoreStaged        = "staged"
	RestoreSafetySaved   = "safety_saved"
	RestoreWritersStop   = "writers_stopped"
	RestoreFilesPromoted = "files_promoted"
	RestoreDBsRestored   = "databases_restored"
	RestoreValidating    = "validating"
)

// restoreJournal persists a restore operation across restarts. It holds
// stage, scope, source snapshot, paths, previous unit states, safety
// snapshot and error — never plaintext credentials.
type restoreJournal struct {
	Operation Operation `json:"operation"`
	// BackupID is the source backup_runs row.
	BackupID string `json:"backup_id"`
	// RepositoryID and SnapshotID identify the source snapshot.
	RepositoryID string `json:"repository_id"`
	SnapshotID   string `json:"snapshot_id"`
	// Scopes selected for this restore; empty means the snapshot scopes.
	Scopes []string `json:"scopes"`
	// StagingDir is this operation's validated staging tree.
	StagingDir string `json:"staging_dir,omitempty"`
	// AffectedApps lists bundles touched by the selected scopes.
	AffectedApps []string `json:"affected_apps,omitempty"`
	// Error carries the failure reason for failed/paused journals.
	Error string `json:"error,omitempty"`
}

func writeRestoreJournal(stateDir string, j restoreJournal) error {
	j.Operation.UpdatedAt = restoreTimestamp()
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	path := operationJournalPath(stateDir, "restore", j.Operation.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return syncWriteFile(path, append(raw, '\n'), 0o600)
}

func loadRestoreJournal(stateDir, id string) (restoreJournal, error) {
	raw, err := os.ReadFile(operationJournalPath(stateDir, "restore", id))
	if err != nil {
		return restoreJournal{}, fmt.Errorf("%w: restore operation %q not found", apitypes.ErrNotFound, id)
	}
	var j restoreJournal
	if err := json.Unmarshal(raw, &j); err != nil {
		return restoreJournal{}, fmt.Errorf("parse restore journal: %w", err)
	}
	return j, nil
}

func restoreStagingRoot(stateDir string) string {
	return filepath.Join(stateDir, "restore-staging")
}

// RestoreBackup stages (default) or live-restores a snapshot. Staging
// extracts to a fresh operation directory and validates in isolation; live
// mode is gated on confirm plus the safety/identity checkpoints of step 4b.
func (b *Backend) RestoreBackup(ctx context.Context, id domain.ID, req apitypes.RestoreBackupRequest) (domain.Backup, error) {
	if b.backups == nil {
		return domain.Backup{}, translateError(fmt.Errorf("%w: backups not configured", ErrNotConfigured))
	}
	src, err := b.GetBackup(ctx, id)
	if err != nil {
		return domain.Backup{}, err
	}
	if strings.TrimSpace(src.SnapshotID) == "" {
		return domain.Backup{}, translateError(fmt.Errorf("%w: backup %q has no snapshot", store.ErrValidation, id))
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = "staging"
	}
	if mode != "staging" && mode != "live" {
		return domain.Backup{}, translateError(fmt.Errorf("%w: mode must be staging|live", store.ErrValidation))
	}
	scopes := dedupeStrings(req.Scopes)
	op := newOperation("restore", appsForScopes(ctx, b, scopes))
	op.Status = OpRunning
	op.Stage = RestorePreflight
	j := restoreJournal{
		Operation: op, BackupID: string(id),
		RepositoryID: src.Repository, SnapshotID: src.SnapshotID, Scopes: scopes,
		AffectedApps: op.AffectedApp,
	}
	// Preflight: repository credentials resolve and the source snapshot is
	// addressable; target mounts and writable permissions are verified
	// again at promotion time from the journal.
	if _, err := b.backups.Repository(ctx, src.Repository); err != nil {
		return domain.Backup{}, translateError(err)
	}
	if mode == "live" {
		if !req.Confirm {
			return domain.Backup{}, translateError(fmt.Errorf("%w: live restore requires confirm:true", store.ErrValidation))
		}
		if len(scopes) == 0 {
			return domain.Backup{}, translateError(fmt.Errorf("%w: live restore requires explicit scopes; server-config restores use the offline recovery executor", store.ErrValidation))
		}
		for _, sc := range scopes {
			if sc == backups.ScopeServerConfig {
				return domain.Backup{}, translateError(fmt.Errorf("%w: server-config restore runs offline via `sudo omahab recovery apply` (or the setup-UI restore flow); live restore refuses it", store.ErrValidation))
			}
		}
		return b.startLiveRestore(ctx, j)
	}
	if err := writeRestoreJournal(b.cfg.StateDir, j); err != nil {
		return domain.Backup{}, err
	}
	go b.runRestoreStaging(j.Operation.ID)
	src.OperationID = j.Operation.ID
	src.Status = OpRunning
	return src, nil
}

func appsForScopes(ctx context.Context, b *Backend, scopes []string) []string {
	var out []string
	for _, s := range scopes {
		if id, ok := backups.ScopeBundleID(s); ok {
			out = append(out, id)
			continue
		}
		out = append(out, s)
	}
	return dedupeStrings(out)
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// runRestoreStaging extracts the snapshot to a fresh operation staging
// directory and validates it in isolation. Live apps are never touched:
// no stops, restarts, reconfiguration or SQL.
func (b *Backend) runRestoreStaging(opID string) {
	err := withMaintenanceLock(func() error {
		j, err := loadRestoreJournal(b.cfg.StateDir, opID)
		if err != nil {
			return err
		}
		ctx := context.Background()
		staging, err := os.MkdirTemp(restoreStagingRoot(b.cfg.StateDir), "op-")
		if err != nil {
			return failRestoreOp(b.cfg.StateDir, &j, err.Error())
		}
		j.StagingDir = staging
		_ = writeRestoreJournal(b.cfg.StateDir, j)
		repo, err := b.backups.Repository(ctx, j.RepositoryID)
		if err != nil {
			return failRestoreOp(b.cfg.StateDir, &j, err.Error())
		}
		staged, err := b.backups.ExtractSnapshot(ctx, repo, j.SnapshotID, staging)
		if err != nil {
			return failRestoreOp(b.cfg.StateDir, &j, err.Error())
		}
		rep, err := backups.ValidateStaging(ctx, staged)
		if err != nil {
			return failRestoreOp(b.cfg.StateDir, &j, "staging validation: "+err.Error())
		}
		if err := checkRestoreScopes(rep, j.Scopes); err != nil {
			return failRestoreOp(b.cfg.StateDir, &j, err.Error())
		}
		// Staged files/DB contents must equal the snapshot; live service
		// activation timestamps and DB values do not change by construction
		// (nothing live was touched).
		j.Operation.Stage = RestoreStaged
		j.Operation.Status = OpCompleted
		j.Operation.Progress = 100
		_ = writeRestoreJournal(b.cfg.StateDir, j)
		log.Printf("restore %q staged at %s (files=%d sqlite=%d/%d pg=%d/%d legacy=%v)",
			opID, staged, rep.Files, rep.SQLiteOK, rep.SQLiteChecked, rep.PGOK, rep.PGChecked, rep.Legacy)
		return nil
	})
	if err != nil {
		log.Printf("restore staging %q: %v", opID, err)
	}
}

// checkRestoreScopes verifies selected scope presence against the staged
// manifest. Legacy snapshots without a manifest stay staging/manual-only:
// scope selection is refused for them rather than guessed.
func checkRestoreScopes(rep backups.StagingReport, scopes []string) error {
	if len(scopes) == 0 {
		return nil
	}
	if !rep.ManifestFound {
		return fmt.Errorf("%w: scope selection requires a snapshot manifest; this legacy snapshot is staging/manual recovery only",
			store.ErrValidation)
	}
	available := map[string]bool{}
	for _, m := range rep.Manifests {
		for _, s := range m.Scopes {
			available[strings.TrimSpace(s)] = true
		}
	}
	var missing []string
	for _, s := range scopes {
		if !available[strings.TrimSpace(s)] {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: snapshot lacks scopes: %s", store.ErrValidation, strings.Join(missing, ", "))
	}
	return nil
}

func failRestoreOp(stateDir string, j *restoreJournal, msg string) error {
	j.Operation.Status = OpFailed
	j.Operation.Stage = OpFailed
	j.Operation.Error = msg
	j.Operation.CanResume = true
	j.Operation.CanRollback = true
	j.Error = msg
	_ = writeRestoreJournal(stateDir, *j)
	return fmt.Errorf("%s", msg)
}

// ImportRepoSnapshots discovers snapshot metadata for an existing repository
// without the old control database and populates the snapshot listing.
func (b *Backend) ImportRepoSnapshots(ctx context.Context, repositoryID string) (int, error) {
	if b.backups == nil {
		return 0, translateError(fmt.Errorf("%w: backups not configured", ErrNotConfigured))
	}
	n, err := b.backups.ImportSnapshots(ctx, strings.TrimSpace(repositoryID))
	if err != nil {
		return 0, translateError(err)
	}
	return n, nil
}

func restoreTimestamp() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
