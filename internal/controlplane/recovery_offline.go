package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/bundles"
	"github.com/omahab/omahab/internal/config"
	"github.com/omahab/omahab/internal/secrets"
	"github.com/omahab/omahab/internal/store"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// The shared offline recovery executor (DESIGN §5.4, §19, §24.6).
//
// One code path serves the CLI (`omahab recovery apply` and
// `--apply-staged`, which the omahab-restore oneshot unit runs as root with
// omahabd stopped) and the daemon-side handoff (TriggerRecoveryRestoreUnit
// only writes the tmpfs phrase handoff and starts the unit; the unit runs
// this same executor). It is deliberately NOT a full online Backend: no
// HTTP server, no reconcilers, no live apps service — only the restored
// control database plus the filesystem.
//
// Execution model: the journal on disk under
// <stateDir>/recovery-restore-operations/<opID>/journal.json is the sole
// authority. Every stage transition is persisted before the stage's live
// effects begin, and every stage is idempotent, so an interruption at any
// point (SIGKILL, power loss, the omahabd restart a bundle-apply switch
// causes) resumes exactly where it stopped through the same entry point.
// A stage whose live effects half-completed rolls its own rollback copy
// back before re-running. Failures are persisted to both the journal and
// restore_setup_state — never swallowed.
//
// Stages (in order): listing → staging → staged → swapping → swapped →
// unit-started (daemon restart) → bundles_written → apps_paused →
// files_promoted → databases_restored → apps_resumed → attaching →
// completed. Scopes beyond server-config restore only when the operator
// asked for them (include_apps or explicit app scopes); excluded scopes
// are never touched.

// Offline recovery journal stages. They are persisted, distinct values so
// an interrupted executor can tell exactly which stage to redo.
const (
	recoveryStageListing         = "listing"
	recoveryStageStaging         = "staging"
	recoveryStageStaged          = "staged"
	recoveryStageSwapping        = "swapping"
	recoveryStageSwapped         = "swapped"
	recoveryStageDaemonRestarted = "unit-started"
	recoveryStageBundlesWritten  = "bundles_written"
	recoveryStageAppsPaused      = "apps_paused"
	recoveryStageFilesPromoted   = "files_promoted"
	recoveryStageDBsRestored     = "databases_restored"
	recoveryStageAppsResumed     = "apps_resumed"
	recoveryStageAttaching       = "attaching"
	recoveryStageCompleted       = "completed"
)

// offlineRestoreOrder is the execution order of persisted stages after the
// swap; an interrupted journal at stage S re-runs S and everything after.
var offlineRestoreOrder = []string{
	recoveryStageSwapped,
	recoveryStageAppsPaused,
	recoveryStageDaemonRestarted,
	recoveryStageBundlesWritten,
	recoveryStageFilesPromoted,
	recoveryStageDBsRestored,
	recoveryStageAppsResumed,
	recoveryStageAttaching,
	recoveryStageCompleted,
}

// offlineRecoveryRuntime carries the resolved environment one executor
// run needs: paths from the environment (OMAHAB_* exactly like the
// daemon's config.Load, but without requiring a full Backend) plus the
// capture-time stateDir the snapshot staged under.
type offlineRecoveryRuntime struct {
	stateDir   string
	dataDir    string
	catalog    string
	layoutPath string
}

// offlineRecoveryRuntimeFromEnv resolves the executor's runtime roots the
// same way config.Load does, without constructing a Backend. Defaults
// match the daemon's.
func offlineRecoveryRuntimeFromEnv() offlineRecoveryRuntime {
	env := func(name, fallback string) string {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
		return fallback
	}
	return offlineRecoveryRuntime{
		stateDir:   env("OMAHAB_STATE_DIR", "/var/lib/omahab"),
		dataDir:    env("OMAHAB_DATA_DIR", "/srv/omahab"),
		catalog:    env("OMAHAB_CATALOG", "/usr/share/omahab/catalog/catalog.json"),
		layoutPath: env("OMAHAB_NATIVE_LAYOUT", ""),
	}
}

// OfflineRecoveryOptions is the exported executor invocation for the CLI
// (`omahab recovery apply`), which cannot reach the unexported options
// type. Field semantics match offlineRecoveryOptions.
type OfflineRecoveryOptions struct {
	StateDir   string
	DataDir    string
	Catalog    string
	LayoutPath string
	Phrase     string
	OpID       string
	// RepositoryPassword is the explicit generic repository password
	// (in memory only; never journaled). When empty, the repository
	// credential is recovered from the restored control database or
	// derived from the phrase.
	RepositoryPassword string
}

// offlineRecoveryOptions is one executor invocation.
type offlineRecoveryOptions struct {
	// StateDir is the live state directory (runtime root).
	StateDir string
	// DataDir is the live data directory.
	DataDir string
	// Catalog is the curated catalog path for bundle resolution.
	Catalog string
	// LayoutPath is OMAHAB_NATIVE_LAYOUT for canonical paths.
	LayoutPath string
	// Phrase is the 24-word recovery phrase; in memory only.
	Phrase string
	// OpID is the journaled operation to run/resume ("" = the CLI's own
	// fresh operation; the staged path always supplies one).
	OpID string
	// RepositoryPassword is the explicit generic repository password
	// (in memory only; never journaled). Empty means: recover from the
	// restored control database, else derive from the phrase seed.
	RepositoryPassword string
}

// RunOfflineRecoveryRestore is the exported single shared offline restore
// executor the CLI calls — the same code path the omahab-restore unit
// runs. See runOfflineRecoveryRestore.
func RunOfflineRecoveryRestore(ctx context.Context, opts OfflineRecoveryOptions) (apitypes.Operation, error) {
	res, err := runOfflineRecoveryRestore(ctx, offlineRecoveryOptions{
		StateDir:           opts.StateDir,
		DataDir:            opts.DataDir,
		Catalog:            opts.Catalog,
		LayoutPath:         opts.LayoutPath,
		Phrase:             opts.Phrase,
		OpID:               opts.OpID,
		RepositoryPassword: opts.RepositoryPassword,
	})
	// The failed operation is returned WITH its error: its stage and
	// resume flag are what the CLI reports and the next run resumes
	// from. Errors before the journal loads have no operation to show.
	if err != nil && res.ID == "" {
		return apitypes.Operation{}, err
	}
	return res, err
}

// StageOfflineRecoverySnapshot is the exported staging half for the CLI:
// extract one snapshot into staging and validate it (manifest,
// fingerprint via the journal's recorded phrase fingerprint, scopes, swap
// plan) without touching live state. The journal is written by the caller
// (the daemon's PrepareRecoveryRestore or the CLI's fresh operation).
//
// An explicit opID loads and revalidates that exact recorded operation
// instead of creating a new one: the CLI uses this to resume an
// interrupted restore with its original snapshot (never a newer one) and
// to restage an operation whose extraction was interrupted. A recorded
// operation that is active (mid-apply) or completed is returned as-is
// rather than re-extracted — the journal stays the sole authority.
func StageOfflineRecoverySnapshot(ctx context.Context, stateDir, dataDir, catalog, layoutPath, snapshotID, staging, phraseFP, location string, scopes []string, includeApps bool, src backups.RecoverySource, opID string) (apitypes.Operation, error) {
	rt := offlineRecoveryRuntime{stateDir: stateDir, dataDir: dataDir, catalog: catalog, layoutPath: layoutPath}
	op := newOperation(recoveryRestoreJournalKind, nil)
	op.Status = OpRunning
	op.Stage = recoveryStageListing
	j := recoveryRestoreJournal{
		Operation: op,
		Source: recoveryJournalSource{
			Location:    location,
			SnapshotID:  strings.TrimSpace(snapshotID),
			PhraseFP:    phraseFP,
			Scopes:      dedupeStrings(scopes),
			IncludeApps: includeApps,
		},
		StagingDir: staging,
	}
	if explicit := strings.TrimSpace(opID); explicit != "" {
		recorded, err := loadRecoveryRestoreJournal(stateDir, explicit)
		if err != nil {
			return apitypes.Operation{}, err
		}
		if recorded.Operation.Stage == recoveryStageCompleted && recorded.Operation.Status == OpCompleted {
			return recorded.Operation, nil
		}
		// An operation that already started applying (swap or beyond,
		// including a mid-swap failure) is never re-extracted: the
		// journal records the exact staged tree and selection, and the
		// live state may already reflect the swap. Only pre-swap stages
		// (listing, staging, staged) re-enter extraction.
		if recorded.Operation.Stage != recoveryStageListing &&
			recorded.Operation.Stage != recoveryStageStaging &&
			recorded.Operation.Stage != recoveryStageStaged {
			return recorded.Operation, nil
		}
		j = recorded
		// The recorded selection wins over the caller's flags; only the
		// caller's snapshot can refine an interrupted extraction whose
		// pick never persisted.
		if strings.TrimSpace(j.Source.SnapshotID) == "" {
			j.Source.SnapshotID = strings.TrimSpace(snapshotID)
		}
		j.Operation.Stage = recoveryStageListing
		j.Operation.Status = OpRunning
		j.Operation.Error = ""
		j.Error = ""
	}
	if err := writeRecoveryRestoreJournal(stateDir, j); err != nil {
		return apitypes.Operation{}, err
	}
	offlineMarkSetupState(stateDir, "in_progress", j.Operation.ID, "")
	runner := &backups.CommandRunner{CacheDir: filepath.Join(stateDir, "restic-cache")}
	_ = os.MkdirAll(runner.CacheDir, 0o700)
	if err := offlineStageAndValidate(ctx, rt, runner, src, j.Source.SnapshotID, j.StagingDir, &j); err != nil {
		_, _ = offlineFailRestore(stateDir, &j, err.Error())
		return apitypes.Operation{}, err
	}
	return j.Operation, nil
}

// FindOfflineRecoveryOperation locates the latest unfinished offline
// recovery restore matching the given source repository, phrase
// fingerprint, and scope selection, so the CLI can resume an interrupted
// restore with its original snapshot instead of choosing a newer one.
// A non-empty snapshotID must also match. Staged operations (status
// completed, stage staged) count as unfinished; a restore that reached
// the completed stage does not. The zero Operation (with nil error and
// empty recorded snapshot) means no matching unfinished operation
// exists; a match returns the operation together with its recorded
// snapshot id so the CLI can pin the resume to that exact snapshot
// before choosing a newest one. Credentials are never consulted or
// persisted here.
func FindOfflineRecoveryOperation(stateDir, location, snapshotID, phraseFP string, scopes []string, includeApps bool) (apitypes.Operation, string, error) {
	dir := operationsDir(stateDir, recoveryRestoreJournalKind)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return apitypes.Operation{}, "", nil
		}
		return apitypes.Operation{}, "", err
	}
	wantLoc := strings.TrimSpace(location)
	wantFP := strings.ToLower(strings.TrimSpace(phraseFP))
	wantSnap := strings.TrimSpace(snapshotID)
	wantScopes := normalizeScopeKey(scopes)
	// Newest first: the journal directories carry the operation id, and
	// the journal records its own creation time.
	var candidates []recoveryRestoreJournal
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		j, err := loadRecoveryRestoreJournal(stateDir, e.Name())
		if err != nil {
			continue
		}
		candidates = append(candidates, j)
	}
	sort.Slice(candidates, func(a, b int) bool {
		return candidates[a].Operation.CreatedAt > candidates[b].Operation.CreatedAt
	})
	for _, j := range candidates {
		if !offlineOperationUnfinished(j.Operation) {
			continue
		}
		if strings.TrimSpace(j.Source.Location) != wantLoc {
			continue
		}
		if wantFP != "" && strings.ToLower(strings.TrimSpace(j.Source.PhraseFP)) != wantFP {
			continue
		}
		if len(j.Source.Scopes) > 0 || len(scopes) > 0 {
			if normalizeScopeKey(j.Source.Scopes) != wantScopes {
				continue
			}
		}
		if j.Source.IncludeApps != includeApps {
			continue
		}
		// A pinned snapshot that differs from the recorded one is a
		// conflict, not a silent no-match: silently ignoring it would
		// stage a second restore against a different snapshot while an
		// unfinished one exists. A journal that has not recorded its
		// pick yet (interrupted during listing) accepts any pin — the
		// recording happens at staging time.
		if wantSnap != "" {
			recordedSnap := strings.TrimSpace(j.Source.SnapshotID)
			if recordedSnap != "" && recordedSnap != wantSnap {
				return apitypes.Operation{}, "", translateError(fmt.Errorf(
					"%w: unfinished recovery restore %q is staged for snapshot %q, not %q; resume it (no --snapshot) or let it fail before staging another",
					apitypes.ErrConflict, j.Operation.ID, recordedSnap, wantSnap))
			}
		}
		return j.Operation, strings.TrimSpace(j.Source.SnapshotID), nil
	}
	return apitypes.Operation{}, "", nil
}

// offlineOperationUnfinished reports whether a recorded recovery restore
// operation still has work to run. A staged operation (status completed,
// stage staged) has finished staging but not applying, so it counts as
// unfinished; completed means the whole restore finished.
func offlineOperationUnfinished(op apitypes.Operation) bool {
	if op.Stage == recoveryStageCompleted && op.Status == OpCompleted {
		return false
	}
	if op.Status == OpCompleted && op.Stage == recoveryStageStaged {
		return true
	}
	// Running / pending / failed-with-resume stages are resumable; a
	// terminal failure without a stage position is not re-selected.
	return op.Status == OpRunning || op.Status == OpPending || (op.Status == OpFailed && op.CanResume)
}

// normalizeScopeKey builds a comparable key from a scope selection.
func normalizeScopeKey(scopes []string) string {
	vals := make([]string, 0, len(scopes))
	for _, s := range scopes {
		if s = strings.TrimSpace(s); s != "" {
			vals = append(vals, s)
		}
	}
	sort.Strings(vals)
	return strings.Join(vals, ",")
}

// runOfflineRecoveryRestore is the single shared offline restore executor.
//
// It re-validates the phrase against the staged snapshot (fingerprint),
// then walks the persisted journal stages: swap server-config + the
// inseparable PocketID identity checkpoint, restart omahabd, write
// snapshot-carried custom bundle definitions and dispatch the bundle
// apply (health checks skipped for restore-paused bundles), pause the
// selected apps' writers, promote their payloads, restore their logical
// databases, resume and health-check, attach the exact source repository
// into the restored control database, and mark completion. Every failure
// is journaled and reflected in restore_setup_state.
//
// The phrase never leaves process memory (the documented tmpfs handoff
// is consumed by the caller before this runs and passed in here).
func runOfflineRecoveryRestore(ctx context.Context, opts offlineRecoveryOptions) (RecoveryRestoreStatus, error) {
	// The restore is a backup/restore-exclusive operation exactly like
	// the live restore: the daemon the executor restarts mid-apply must
	// not run a scheduled capture against partially promoted payloads
	// (the capture planner only skips paused bundles whose data is still
	// empty). Holding the shared maintenance lock for the whole run
	// blocks the daily backup/verify timer units and the panel's create
	// endpoint until the restore finishes or fails.
	var result RecoveryRestoreStatus
	if err := withMaintenanceLock(func() error {
		var err error
		result, err = runOfflineRecoveryRestoreLocked(ctx, opts)
		return err
	}); err != nil {
		// The failed operation is returned WITH its error: its stage
		// and resume flag are what callers report and the next run
		// resumes from. Failures before the journal loads carry no
		// operation.
		return result, err
	}
	return result, nil
}

// runOfflineRecoveryRestoreLocked is the executor body under the shared
// maintenance lock.
func runOfflineRecoveryRestoreLocked(ctx context.Context, opts offlineRecoveryOptions) (RecoveryRestoreStatus, error) {
	rt := offlineRecoveryRuntime{
		stateDir:   opts.StateDir,
		dataDir:    opts.DataDir,
		catalog:    opts.Catalog,
		layoutPath: opts.LayoutPath,
	}
	if rt.stateDir == "" {
		rt = offlineRecoveryRuntimeFromEnv()
	}
	seed, err := phraseSeedFromRequest(opts.Phrase)
	if err != nil {
		return RecoveryRestoreStatus{}, err
	}
	fp := recoverySeedFingerprint(seed)

	j, err := loadRecoveryRestoreJournal(rt.stateDir, strings.TrimSpace(opts.OpID))
	if err != nil {
		return RecoveryRestoreStatus{}, err
	}
	if j.StagingDir == "" || j.Source.SnapshotID == "" {
		return offlineFailRestore(rt.stateDir, &j, "journal has no staged snapshot; re-stage the restore")
	}
	// The phrase must still be the one that staged this snapshot.
	if !strings.EqualFold(fp, strings.TrimSpace(j.Source.PhraseFP)) {
		return RecoveryRestoreStatus{}, translateError(fmt.Errorf("%w: phrase fingerprint changed between staging and apply; re-stage with the original phrase", store.ErrValidation))
	}
	if j.Operation.Status == OpCompleted && j.Operation.Stage == recoveryStageCompleted {
		return j.Operation, nil // idempotent re-run of a finished restore
	}
	// Recovered state must never present a stale failure as final while a
	// resume is in flight.
	j.Operation.Status = OpRunning
	j.Operation.CanResume = true
	j.Operation.Error = ""
	j.Error = ""
	if err := writeRecoveryRestoreJournal(rt.stateDir, j); err != nil {
		return RecoveryRestoreStatus{}, err
	}

	// --- validation of the staged tree (idempotent, no live effects) ---
	manifest, err := backups.ServerConfigManifest(j.StagingDir)
	if err != nil {
		return offlineFailRestore(rt.stateDir, &j, err.Error())
	}
	if err := backups.CheckRecoveryFingerprint(manifest, fp); err != nil {
		return offlineFailRestore(rt.stateDir, &j, err.Error())
	}

	// --- server-config swap (with rollback copy; idempotent re-entry) ---
	if stageIndex(j.Operation.Stage) < stageIndex(recoveryStageSwapped) {
		if j.Operation.Stage == recoveryStageSwapping || j.Operation.Stage == "rolling_back" {
			// Interrupted mid-swap or mid-rollback: undo the partial
			// swap from its rollback copy before redoing it. Both
			// journals sort before swapped, so re-entry always
			// converges here.
			if err := offlineUndoInterruptedSwap(rt.stateDir, &j); err != nil {
				return offlineFailRestore(rt.stateDir, &j, "recover interrupted swap: "+err.Error())
			}
		}
		master, err := unwrapStagedMasterKey(j.StagingDir, rt.stateDir, seed)
		if err != nil {
			return offlineFailRestore(rt.stateDir, &j, err.Error())
		}
		plan, err := backups.PlanServerConfigSwap(j.StagingDir, rt.stateDir, manifest)
		if err != nil {
			zeroBytes(master[:])
			return offlineFailRestore(rt.stateDir, &j, "swap plan: "+err.Error())
		}
		j.Operation.Stage = recoveryStageSwapping
		if err := writeRecoveryRestoreJournal(rt.stateDir, j); err != nil {
			// Stage-first invariant: never mutate live state while the
			// journal still says staged. A resume after this failure
			// re-enters the swap cleanly (no rollback dir EEXIST).
			return offlineFailRestore(rt.stateDir, &j, "persist swapping stage: "+err.Error())
		}
		rollback, _, err := backups.ApplyServerConfigSwap(ctx, plan, master[:])
		zeroBytes(master[:])
		if err != nil {
			j.Operation.Stage = "rolling_back"
			// Best-effort journal write: the durable swapping stage
			// above already lets a resume undo the partial swap, so a
			// failed write here loses nothing. Roll back regardless.
			_ = writeRecoveryRestoreJournal(rt.stateDir, j)
			if rollback != nil {
				if rbErr := rollback(); rbErr != nil {
					return offlineFailRestore(rt.stateDir, &j, fmt.Sprintf("swap failed (%v) and rollback failed (%v); state may be partially swapped — inspect %s", err, rbErr, j.StagingDir))
				}
			}
			return offlineFailRestore(rt.stateDir, &j, "swap failed and rolled back: "+err.Error())
		}
		// Keep the rollback copy until the whole restore completes: a
		// later stage failing can still be recovered by hand.
		j.Operation.Stage = recoveryStageSwapped
		j.Operation.Progress = 40
		if err := writeRecoveryRestoreJournal(rt.stateDir, j); err != nil {
			return offlineFailRestore(rt.stateDir, &j, "persist swapped stage: "+err.Error())
		}
	}

	// --- selected app scopes (nothing below runs for a config-only restore) ---
	appScopes := offlineSelectedAppScopes(j, manifest)
	identity := offlineIdentityScope(manifest)

	restoredDB, err := offlineOpenControlDB(rt.stateDir)
	if err != nil {
		return offlineFailRestore(rt.stateDir, &j, "open restored control db: "+err.Error())
	}
	defer restoredDB.Close()

	appPlan, err := offlinePlanAppRestore(rt, restoredDB, manifest, appScopes, j.StagingDir)
	if err != nil {
		return offlineFailRestore(rt.stateDir, &j, err.Error())
	}
	if identity != "" && !slices.Contains(appPlan.bundles, identity) {
		appPlan.bundles = append([]string{identity}, appPlan.bundles...)
		appPlan.units[identity] = offlineIdentityUnits()
	}
	// Pause the selected apps' writers with durable markers so nothing
	// resumes them mid-restore (marker + app_maintenance row, reason
	// restore, owned by this operation).
	if len(appPlan.bundles) > 0 && stageIndex(j.Operation.Stage) < stageIndex(recoveryStageAppsPaused) {
		if err := offlinePauseAppWriters(ctx, rt, restoredDB, appPlan, j.Operation.ID); err != nil {
			return offlineFailRestore(rt.stateDir, &j, "pause app writers: "+err.Error())
		}
		j.Operation.Stage = recoveryStageAppsPaused
		if err := writeRecoveryRestoreJournal(rt.stateDir, j); err != nil {
			return offlineFailRestore(rt.stateDir, &j, "persist apps_paused stage: "+err.Error())
		}
	}
	if len(appScopes) > 0 || identity != "" {
		// omahabd must be up before the delegated bundle apply's panel
		// health gate runs, and the restored control DB is what serves it.
		if stageIndex(j.Operation.Stage) < stageIndex(recoveryStageDaemonRestarted) {
			if out, err := exec.Command("systemctl", "start", "omahabd.service").CombinedOutput(); err != nil {
				return offlineFailRestore(rt.stateDir, &j, fmt.Sprintf("restart omahabd after swap: %v: %s", err, strings.TrimSpace(string(out))))
			}
			j.Operation.Stage = recoveryStageDaemonRestarted
			if err := writeRecoveryRestoreJournal(rt.stateDir, j); err != nil {
				return offlineFailRestore(rt.stateDir, &j, "persist daemon restart stage: "+err.Error())
			}
		}
	}

	// PocketID identity checkpoint: promote with server-config semantics
	// (it is inseparable from the control plane). One path swap before
	// app payloads; the pocket-id service itself stays stopped until the
	// resume stage.
	if identity != "" && stageIndex(j.Operation.Stage) < stageIndex(recoveryStageBundlesWritten) {
		if err := offlinePromoteIdentity(ctx, manifest, j.StagingDir); err != nil {
			return offlineFailRestore(rt.stateDir, &j, "identity checkpoint: "+err.Error())
		}
	}

	// Custom bundle definitions carried by the snapshot must exist as
	// modules before their payloads promote. The apply itself is
	// dispatched through the control-plane staging path below.
	if (len(appPlan.customBundles) > 0 || identity != "") && stageIndex(j.Operation.Stage) < stageIndex(recoveryStageBundlesWritten) {
		if err := offlineWriteCustomBundles(ctx, rt, restoredDB, appPlan, j); err != nil {
			return offlineFailRestore(rt.stateDir, &j, "custom bundles: "+err.Error())
		}
	}

	pausedBundles := append([]string{}, appPlan.bundles...)
	if identity != "" && !slices.Contains(pausedBundles, identity) {
		pausedBundles = append(pausedBundles, identity)
	}

	// Dispatch the bundle apply through the shared staged path (journal +
	// trusted tmpfs handoff + systemctl --no-block), skipping health
	// probes exactly for the bundles this restore holds paused. The apply
	// ID is journaled BEFORE the unit starts so an interruption converges
	// this exact apply on resume.
	if len(pausedBundles) > 0 && j.BundleApplyID == "" {
		b := &Backend{cfg: configForRuntime(rt), db: restoredDB}
		op, err := b.stageBundleApply(ctx, pausedBundles)
		if err != nil {
			return offlineFailRestore(rt.stateDir, &j, "stage bundle apply: "+err.Error())
		}
		j.BundleApplyID = op.ID
		j.Operation.CanResume = true
		if err := writeRecoveryRestoreJournal(rt.stateDir, j); err != nil {
			return offlineFailRestore(rt.stateDir, &j, "journal bundle apply: "+err.Error())
		}
		if err := b.startStagedBundleApply(op); err != nil {
			return offlineFailRestore(rt.stateDir, &j, "activate restored custom bundles: "+err.Error())
		}
	} else if j.BundleApplyID != "" {
		// A journaled apply whose dispatch never happened (the journal
		// write above survived, the process died before the unit start)
		// must never sit permanently undispatched: reconstruct the
		// trusted skip_health handoff for exactly this apply and start
		// the unit. A terminal failed apply is retried through a fresh
		// staging so the resume does not wait forever on an outcome
		// that already recorded its failure.
		op, err := readBundleApplyJournal(rt.stateDir, j.BundleApplyID)
		if err != nil {
			return offlineFailRestore(rt.stateDir, &j, "load journaled bundle apply: "+err.Error())
		}
		done, status, _ := readBundleApplyOutcome(rt.stateDir, j.BundleApplyID)
		if done && status != OpCompleted {
			b := &Backend{cfg: configForRuntime(rt), db: restoredDB}
			retry, err := b.stageBundleApply(ctx, pausedBundles)
			if err != nil {
				return offlineFailRestore(rt.stateDir, &j, "retry bundle apply: "+err.Error())
			}
			j.BundleApplyID = retry.ID
			j.Operation.CanResume = true
			if err := writeRecoveryRestoreJournal(rt.stateDir, j); err != nil {
				return offlineFailRestore(rt.stateDir, &j, "journal bundle apply: "+err.Error())
			}
			if err := b.startStagedBundleApply(retry); err != nil {
				return offlineFailRestore(rt.stateDir, &j, "activate restored custom bundles: "+err.Error())
			}
		} else if !done {
			// Still running or undispatched: the trusted handoff may
			// have been wiped with the tmpfs, so reconstruct it from
			// the operation's own recorded skip list and (re)start the
			// oneshot unit. An already-queued start is a no-op.
			if err := writeBundleApplyHandoff(op.ID, op.AffectedApp); err != nil {
				return offlineFailRestore(rt.stateDir, &j, "restore bundle apply handoff: "+err.Error())
			}
			b := &Backend{cfg: configForRuntime(rt), db: restoredDB}
			// This process never staged the persisted apply, so claim
			// its unique slot before the dispatch: the start helper
			// refuses an unreserved operation.
			if err := b.reserveBundleApply(op.ID); err != nil {
				return offlineFailRestore(rt.stateDir, &j, "resume bundle apply: "+err.Error())
			}
			if err := b.startStagedBundleApply(op); err != nil {
				return offlineFailRestore(rt.stateDir, &j, "activate restored custom bundles: "+err.Error())
			}
		}
	}
	if stageIndex(j.Operation.Stage) < stageIndex(recoveryStageBundlesWritten) && len(pausedBundles) > 0 {
		if err := offlineWaitBundleApply(ctx, rt.stateDir, j.BundleApplyID); err != nil {
			// Resumable: the journal already holds the apply ID.
			j.Operation.CanResume = true
			return offlineFailRestore(rt.stateDir, &j, "bundle apply: "+err.Error())
		}
		j.Operation.Stage = recoveryStageBundlesWritten
		if err := writeRecoveryRestoreJournal(rt.stateDir, j); err != nil {
			return offlineFailRestore(rt.stateDir, &j, "persist bundles_written stage: "+err.Error())
		}
	}

	// Promote selected app payloads (canonical destinations only; every
	// scope not selected is untouched by construction).
	if len(appPlan.stagedFiles) > 0 && stageIndex(j.Operation.Stage) < stageIndex(recoveryStageFilesPromoted) {
		if err := offlinePromoteFiles(ctx, appPlan); err != nil {
			j.Operation.CanResume = true
			return offlineFailRestore(rt.stateDir, &j, "promote: "+err.Error())
		}
		j.Operation.Stage = recoveryStageFilesPromoted
		if err := writeRecoveryRestoreJournal(rt.stateDir, j); err != nil {
			return offlineFailRestore(rt.stateDir, &j, "persist files_promoted stage: "+err.Error())
		}
	}

	// Logical database restores into the running cluster.
	if len(appPlan.databases) > 0 && stageIndex(j.Operation.Stage) < stageIndex(recoveryStageDBsRestored) {
		if err := offlineRestoreDatabases(ctx, appPlan); err != nil {
			j.Operation.CanResume = true
			return offlineFailRestore(rt.stateDir, &j, "databases: "+err.Error())
		}
		j.Operation.Stage = recoveryStageDBsRestored
		if err := writeRecoveryRestoreJournal(rt.stateDir, j); err != nil {
			return offlineFailRestore(rt.stateDir, &j, "persist databases_restored stage: "+err.Error())
		}
	}

	// Resume the paused writers and wait for them to report up.
	if len(appPlan.bundles) > 0 || identity != "" {
		if stageIndex(j.Operation.Stage) < stageIndex(recoveryStageAppsResumed) {
			if err := offlineResumeAppWriters(ctx, rt, restoredDB, appPlan, identity, j.Operation.ID); err != nil {
				j.Operation.CanResume = true
				return offlineFailRestore(rt.stateDir, &j, "resume apps: "+err.Error())
			}
			j.Operation.Stage = recoveryStageAppsResumed
			if err := writeRecoveryRestoreJournal(rt.stateDir, j); err != nil {
				return offlineFailRestore(rt.stateDir, &j, "persist apps_resumed stage: "+err.Error())
			}
		}
	}

	// Attach the exact source repository into the restored control
	// database so backups continue into the same instance path.
	if stageIndex(j.Operation.Stage) < stageIndex(recoveryStageCompleted) {
		if err := offlineAttachSourceRepository(ctx, rt, restoredDB, j, seed, opts.RepositoryPassword); err != nil {
			j.Operation.CanResume = true
			return offlineFailRestore(rt.stateDir, &j, "attach repository: "+err.Error())
		}
		j.Operation.Stage = recoveryStageCompleted
		j.Operation.Status = OpCompleted
		j.Operation.Progress = 100
		j.Operation.CanResume = false
		if err := writeRecoveryRestoreJournal(rt.stateDir, j); err != nil {
			return offlineFailRestore(rt.stateDir, &j, "persist completed stage: "+err.Error())
		}
		offlineMarkSetupState(rt.stateDir, "completed", j.Operation.ID, "")
	}
	return j.Operation, nil
}

// stageIndex returns the position of a persisted stage in the execution
// order; pre-swap stages sort before everything.
func stageIndex(stage string) int {
	if stage == recoveryStageSwapping || stage == "rolling_back" {
		// A mid-swap journal re-enters the swap stage.
		return stageIndex(recoveryStageSwapped) - 1
	}
	for i, s := range offlineRestoreOrder {
		if s == stage {
			return i
		}
	}
	return -1
}

// offlineSelectedAppScopes resolves which non-config scopes this restore
// must restore: explicit scopes when the operator picked a subset (app:*
// plus the server-owned data scopes server-sync, server-projects,
// machine-backups), every non-config scope in the manifest for
// include_apps, and nothing for a config-only restore. server-config is
// always restored and never appears here; excluded scopes are never
// touched by construction.
func offlineSelectedAppScopes(j recoveryRestoreJournal, manifest backups.SnapshotManifest) []string {
	isRestorable := func(s string) bool {
		if s == backups.ScopeServerConfig {
			return false
		}
		if _, ok := backups.ScopeBundleID(s); ok {
			return true
		}
		switch s {
		case backups.ScopeServerSync, backups.ScopeServerProjects, backups.ScopeMachineBackups:
			return true
		}
		return false
	}
	var out []string
	if len(j.Source.Scopes) > 0 {
		for _, s := range j.Source.Scopes {
			if isRestorable(s) {
				out = append(out, s)
			}
		}
		return out
	}
	if j.Source.IncludeApps {
		for _, s := range manifest.ScopeNames() {
			if isRestorable(s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// offlineIdentityScope returns the PocketID app scope name when the
// snapshot carries it, else "". Server-config always captures it, so a
// config-only restore still promotes the identity checkpoint.
func offlineIdentityScope(manifest backups.SnapshotManifest) string {
	if _, ok := manifest.Scope(backups.AppScope("pocket-id")); ok {
		return "pocket-id"
	}
	return ""
}

// configForRuntime builds the minimal Backend config the executor's
// delegated control-plane calls need (staged bundle apply). It never
// constructs a full online Backend.
func configForRuntime(rt offlineRecoveryRuntime) config.Config {
	return config.Config{
		StateDir:         rt.stateDir,
		DataDir:          rt.dataDir,
		CatalogPath:      rt.catalog,
		NativeLayoutPath: rt.layoutPath,
		CustomBundlesDir: customBundlesDirFor(rt.stateDir),
	}
}

// customBundlesDirFor mirrors config.Load's default: the git repo of
// custom bundles under the state dir unless OMAHAB_CUSTOM_BUNDLES
// relocates it.
func customBundlesDirFor(stateDir string) string {
	if v := strings.TrimSpace(os.Getenv("OMAHAB_CUSTOM_BUNDLES")); v != "" {
		return v
	}
	return filepath.Join(stateDir, "bundles")
}

// offlineOpenControlDB opens the restored control database read-write on
// the executor's own connection. The swap already placed it.
func offlineOpenControlDB(stateDir string) (*sql.DB, error) {
	st, err := store.Open(filepath.Join(stateDir, "control.db"))
	if err != nil {
		return nil, err
	}
	return st.DB(), nil
}

// offlineAppPlan is the resolved app-scope restore set for the offline
// executor — the same fields the live restore plans, derived from the
// staged manifest and the merged catalog rather than the apps service.
type offlineAppPlan struct {
	bundles       []string          // bundle IDs to restore, dependency order
	stagedFiles   map[string]string // canonical dest -> staged source
	databases     map[string]string // db name -> staged dump path
	dbHashes      map[string]string // db name -> manifest sha256
	customBundles []*backups.ManifestBundle
	units         map[string][]string // bundle ID -> systemd units to control
}

// offlinePlanAppRestore resolves the selected app scopes against the
// merged catalog (curated + committed custom definitions) using the
// restored control database's bundle knowledge. Data paths come from the
// catalog; database dumps from the manifest; custom definitions from the
// staged tree next to the manifest.
func offlinePlanAppRestore(rt offlineRecoveryRuntime, db *sql.DB, manifest backups.SnapshotManifest, appScopes []string, staging string) (*offlineAppPlan, error) {
	plan := &offlineAppPlan{
		stagedFiles: map[string]string{},
		databases:   map[string]string{},
		dbHashes:    map[string]string{},
		units:       map[string][]string{},
	}
	if len(appScopes) == 0 {
		return plan, nil
	}
	catalog, err := offlineMergedCatalog(rt)
	if err != nil {
		return nil, fmt.Errorf("load merged catalog: %w", err)
	}
	byBundle := map[string]apps.Bundle{}
	for _, bd := range catalog.Bundles() {
		byBundle[bd.ID] = bd
	}
	manifestDir, err := backups.StagedManifestDirFor(staging, config.DefaultStateDir)
	if err != nil {
		return nil, err
	}
	for _, name := range appScopes {
		scope, ok := manifest.Scope(name)
		if !ok || scope.Bundle == nil || scope.Bundle.Source != string(bundles.SourceCustom) {
			continue
		}
		mb := scope.Bundle
		if !filepath.IsLocal(mb.DefinitionPath) {
			return nil, fmt.Errorf("%w: custom bundle %q definition escapes staging", store.ErrValidation, mb.ID)
		}
		def := filepath.Join(manifestDir, filepath.FromSlash(mb.DefinitionPath))
		raw, err := os.ReadFile(filepath.Join(def, "bundle.json"))
		if err != nil {
			return nil, err
		}
		bd, err := apps.ParseBundleBytes(raw)
		if err != nil {
			return nil, err
		}
		if _, err := bundles.ValidateCustom(bd, mb.ID); err != nil {
			return nil, err
		}
		if _, err := os.Stat(filepath.Join(def, "module.nix")); err != nil {
			return nil, err
		}
		byBundle[mb.ID] = bd
	}
	// Dependency order for writer stop/start: catalog topo order.
	var ordered []string
	for _, bd := range catalog.Bundles() {
		for _, s := range appScopes {
			if id, ok := backups.ScopeBundleID(s); ok && id == bd.ID {
				ordered = append(ordered, bd.ID)
			}
		}
	}
	for _, scope := range appScopes {
		if id, ok := backups.ScopeBundleID(scope); ok && !slices.Contains(ordered, id) {
			ordered = append(ordered, id)
		}
	}
	for _, s := range appScopes {
		// Server-owned data scopes promote whole directories under the
		// data root; they have no bundle, units, or databases.
		if isOfflineDataScope(s) {
			scope, ok := manifest.Scope(s)
			if !ok {
				return nil, fmt.Errorf("%w: snapshot has no scope %q", store.ErrValidation, s)
			}
			for _, p := range scope.Paths {
				dest := filepath.Join(rt.dataDir, p)
				src := filepath.Join(staging, dest)
				if _, err := os.Stat(src); err != nil {
					return nil, fmt.Errorf("%w: staged data missing for %q (%s)", store.ErrValidation, s, p)
				}
				plan.stagedFiles[dest] = src
			}
			continue
		}
		id, ok := backups.ScopeBundleID(s)
		if !ok {
			return nil, fmt.Errorf("%w: unknown restore scope %q", store.ErrValidation, s)
		}
		scope, ok := manifest.Scope(s)
		if !ok {
			return nil, fmt.Errorf("%w: snapshot has no application scope for %q", store.ErrValidation, id)
		}
		bd, ok := byBundle[id]
		if !ok {
			return nil, fmt.Errorf("%w: unknown bundle %q (custom bundle definitions restore with the snapshot; re-run after they land)", store.ErrValidation, id)
		}
		if len(bd.Units) > 0 {
			plan.units[id] = bd.Units
		}
		for _, v := range bd.Data {
			src := filepath.Join(staging, v.Path)
			if _, err := os.Stat(src); err != nil {
				return nil, fmt.Errorf("%w: staged data missing for %q (%s)", store.ErrValidation, id, v.Path)
			}
			plan.stagedFiles[v.Path] = src
		}
		for _, dbrec := range scope.Databases {
			dump := offlineFindStagedDump(manifestDir, staging, dbrec.Name)
			if dump == "" {
				return nil, fmt.Errorf("%w: staged dump missing for database %q", store.ErrValidation, dbrec.Name)
			}
			if filepath.Base(dump) != filepath.Base(filepath.FromSlash(dbrec.File)) {
				return nil, fmt.Errorf("%w: database %q: manifest declares dump %q but staged dump is %q",
					store.ErrValidation, dbrec.Name, dbrec.File, filepath.Base(dump))
			}
			got, err := backups.SHA256File(dump)
			if err != nil {
				return nil, fmt.Errorf("%w: database %q: hash staged dump: %v", store.ErrValidation, dbrec.Name, err)
			}
			if !strings.EqualFold(got, strings.TrimSpace(dbrec.SHA256)) {
				return nil, fmt.Errorf("%w: database %q: staged dump sha256 mismatch (staged %s, manifest %s)",
					store.ErrValidation, dbrec.Name, got, dbrec.SHA256)
			}
			plan.databases[dbrec.Name] = dump
			plan.dbHashes[dbrec.Name] = strings.TrimSpace(dbrec.SHA256)
		}
		if scope.Bundle != nil && scope.Bundle.Source == "custom" {
			plan.customBundles = append(plan.customBundles, scope.Bundle)
		}
	}
	plan.bundles = ordered
	return plan, nil
}

// isOfflineDataScope reports whether a restore scope is a server-owned
// data scope promoted as whole directories under the data root.
func isOfflineDataScope(s string) bool {
	return s == backups.ScopeServerSync || s == backups.ScopeServerProjects || s == backups.ScopeMachineBackups
}

// offlineMergedCatalog loads curated + committed custom definitions from
// the custom-bundles repo. It uses the daemon's loader so the offline
// catalog view cannot drift from the online one.
func offlineMergedCatalog(rt offlineRecoveryRuntime) (*apps.Catalog, error) {
	m, err := bundles.LoadMerged(context.Background(), rt.catalog, customBundlesDirFor(rt.stateDir))
	if err != nil {
		return nil, err
	}
	return mergedToCatalog(m), nil
}

// offlineFindStagedDump locates one database dump: the manifest directory
// first (captures stage dumps beside the manifest), then a bounded walk
// of the extracted tree.
func offlineFindStagedDump(manifestDir, root, dbName string) string {
	primary := filepath.Join(manifestDir, dbName+".pgdump")
	if _, err := os.Stat(primary); err == nil {
		return primary
	}
	var found string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || found != "" {
			return nil
		}
		if info.Name() == dbName+".pgdump" {
			found = path
		}
		return nil
	})
	return found
}

// offlinePromoteIdentity swaps the PocketID data directory in from the
// staged tree. The identity checkpoint is inseparable from server-config:
// passkeys, encryption key, and OIDC clients restore together or the
// restored control plane cannot authenticate anyone.
func offlinePromoteIdentity(ctx context.Context, manifest backups.SnapshotManifest, staging string) error {
	scope, ok := manifest.Scope(backups.AppScope("pocket-id"))
	if !ok {
		return nil
	}
	var src string
	for _, p := range scope.Paths {
		cand := filepath.Join(staging, p)
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			src = cand
			break
		}
	}
	if src == "" {
		return fmt.Errorf("%w: snapshot carries the identity scope but no staged data directory", store.ErrValidation)
	}
	dest := offlineIdentityDataDir()
	if dest == "" {
		return fmt.Errorf("%w: pocket-id data directory not configured (OMAHAB_NATIVE_LAYOUT)", store.ErrValidation)
	}
	return offlineRsyncInto(ctx, src, dest)
}

// offlineIdentityDataDir resolves the PocketID canonical data path from
// the native layout (nix/app-data.nix rendering), defaulting to the
// upstream service default.
func offlineIdentityDataDir() string {
	rt := offlineRecoveryRuntimeFromEnv()
	if rt.layoutPath == "" {
		return "/var/lib/pocket-id"
	}
	raw, err := os.ReadFile(rt.layoutPath)
	if err != nil {
		return "/var/lib/pocket-id"
	}
	var layout map[string]struct {
		Data []struct {
			Path string `json:"path"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &layout); err != nil {
		return "/var/lib/pocket-id"
	}
	if e, ok := layout["pocket-id"]; ok && len(e.Data) > 0 && strings.TrimSpace(e.Data[0].Path) != "" {
		return strings.TrimSpace(e.Data[0].Path)
	}
	return "/var/lib/pocket-id"
}

// offlineWriteCustomBundles writes the snapshot-carried custom bundle
// definitions into the custom-bundles repo and commits them. Re-entry is
// idempotent: WriteBundleDir overwrites identically and Commit is a
// no-op on a clean tree.
func offlineWriteCustomBundles(ctx context.Context, rt offlineRecoveryRuntime, db *sql.DB, plan *offlineAppPlan, j recoveryRestoreJournal) error {
	if len(plan.customBundles) == 0 {
		return nil
	}
	repo := customBundlesDirFor(rt.stateDir)
	svc, err := bundles.NewService(repo, nil, nil)
	if err != nil {
		return err
	}
	if err := svc.EnsureRepo(ctx); err != nil {
		return fmt.Errorf("ensure custom bundles repo: %w", err)
	}
	// Resolve the manifest directory with the production capture state
	// dir: captures stage at /var/lib/omahab/backup-staging and restic
	// reproduces that path below the extraction target.
	stagingManifestDir, err := backups.StagedManifestDirFor(j.StagingDir, config.DefaultStateDir)
	if err != nil {
		return err
	}
	for _, mb := range plan.customBundles {
		src := filepath.Join(stagingManifestDir, mb.ID)
		if mb.DefinitionPath != "" {
			src = filepath.Join(stagingManifestDir, filepath.FromSlash(mb.DefinitionPath))
		}
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("%w: staged snapshot has no definition for custom bundle %q (want %s)",
				store.ErrValidation, mb.ID, src)
		}
		if err := svc.WriteBundleDir(ctx, mb.ID, src); err != nil {
			return fmt.Errorf("write custom bundle %q: %w", mb.ID, err)
		}
	}
	// Mirror push failure is reported, never fatal: the commit hash is
	// the authority (Forgejo may be absent on a fresh machine).
	if commit, err := svc.Commit(ctx, "omahab restore "+j.Operation.ID+": custom bundle definitions"); err != nil && commit == "" {
		return fmt.Errorf("commit restored custom bundles: %w", err)
	}
	return nil
}

// offlineWaitBundleApply polls the CLI outcome marker the
// omahab-bundle-apply unit run writes next to its journal. The switch may
// restart omahabd mid-apply; the marker survives on disk.
func offlineWaitBundleApply(ctx context.Context, stateDir, applyID string) error {
	if applyID == "" {
		return nil
	}
	deadline := time.Now().Add(restoreBundleApplyTimeout)
	for {
		done, status, detail := readBundleApplyOutcome(stateDir, applyID)
		if done {
			if status != OpCompleted {
				return fmt.Errorf("bundle apply %s: %s: %s", applyID, status, detail)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("bundle apply %s did not finish within %s", applyID, restoreBundleApplyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// offlinePauseAppWriters stops the selected bundles' systemd units and
// records durable pause markers (files + app_maintenance rows, reason
// restore, owned by this operation) so neither a daemon restart nor a
// generic resume un-pauses them mid-restore.
func offlinePauseAppWriters(ctx context.Context, rt offlineRecoveryRuntime, db *sql.DB, plan *offlineAppPlan, opID string) error {
	for _, bundle := range plan.bundles {
		if err := writeMarker(filepath.Join(rt.stateDir, "maintenance", "apps", bundle)); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO app_maintenance (bundle_id, reason, operation_id, created_at)
			 VALUES (?, 'restore', ?, ?)
			 ON CONFLICT (bundle_id) DO UPDATE SET reason='restore', operation_id=excluded.operation_id, created_at=excluded.created_at`,
			bundle, opID, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return err
		}
		for _, unit := range plan.units[bundle] {
			state, err := exec.CommandContext(ctx, "systemctl", "show", unit, "--property=LoadState", "--value").Output()
			if err != nil {
				return err
			}
			if strings.TrimSpace(string(state)) == "not-found" {
				continue
			}
			if out, err := exec.Command("systemctl", "stop", unit).CombinedOutput(); err != nil {
				return fmt.Errorf("stop %s: %v: %s", unit, err, strings.TrimSpace(string(out)))
			}
		}
	}
	return nil
}

// offlineResumeAppWriters clears this operation's maintenance gates before
// starting writers, then checks their current services.
func offlineResumeAppWriters(ctx context.Context, rt offlineRecoveryRuntime, db *sql.DB, plan *offlineAppPlan, identity, opID string) error {
	units := []string{}
	if identity != "" && !slices.Contains(plan.bundles, identity) {
		units = append(units, offlineIdentityUnits()...)
	}
	for _, bundle := range plan.bundles {
		units = append(units, plan.units[bundle]...)
	}
	bundlesToClear := append([]string{}, plan.bundles...)
	if identity != "" && !slices.Contains(plan.bundles, identity) {
		bundlesToClear = append(bundlesToClear, identity)
	}
	for _, bundle := range bundlesToClear {
		if err := os.Remove(filepath.Join(rt.stateDir, "maintenance", "apps", bundle)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, err := db.ExecContext(ctx,
			`DELETE FROM app_maintenance WHERE bundle_id = ? AND reason = 'restore' AND operation_id = ?`, bundle, opID); err != nil {
			return err
		}
	}
	for _, unit := range units {
		if out, err := exec.Command("systemctl", "start", unit).CombinedOutput(); err != nil {
			return fmt.Errorf("start %s: %v: %s", unit, err, strings.TrimSpace(string(out)))
		}
	}
	probe := &Backend{cfg: configForRuntime(rt)}
	for _, bundle := range bundlesToClear {
		deadline := time.Now().Add(5 * time.Minute)
		for {
			err := probe.ProbeBundleHealth(ctx, bundle, "")
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
	return nil
}

// offlineIdentityUnits lists the PocketID systemd units to restart.
func offlineIdentityUnits() []string {
	return []string{"pocket-id.service"}
}

// offlinePromoteFiles promotes the staged app payloads to their canonical
// destinations with ownership and attributes intact.
func offlinePromoteFiles(ctx context.Context, plan *offlineAppPlan) error {
	// Deterministic order: destination paths sorted.
	dests := make([]string, 0, len(plan.stagedFiles))
	for dest := range plan.stagedFiles {
		dests = append(dests, dest)
	}
	sort.Strings(dests)
	for _, dest := range dests {
		if err := offlineRsyncInto(ctx, plan.stagedFiles[dest], dest); err != nil {
			return fmt.Errorf("promote %q: %w", dest, err)
		}
	}
	return nil
}

// offlineRsyncInto syncs a staged source directory into a canonical
// destination, creating it when absent. Native payload roots keep their
// existing ownership; rsync -aHAX preserves what the capture recorded.
// --delete makes each selected destination exactly match the captured
// directory (destination-only leftovers are removed); scopes that were
// never selected never reach this function, so they are untouched.
func offlineRsyncInto(ctx context.Context, src, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "rsync", "-aHAX", "--numeric-ids", "--delete", src+"/", dest+"/").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// offlineRestoreDatabases restores the selected logical databases into
// the running cluster, re-verifying each dump against its manifest hash
// right before pg_restore.
func offlineRestoreDatabases(ctx context.Context, plan *offlineAppPlan) error {
	names := make([]string, 0, len(plan.databases))
	for name := range plan.databases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		dump := plan.databases[name]
		if want, ok := plan.dbHashes[name]; ok {
			got, err := backups.SHA256File(dump)
			if err != nil {
				return fmt.Errorf("database %q: hash staged dump: %w", name, err)
			}
			if !strings.EqualFold(got, want) {
				return fmt.Errorf("database %q: staged dump sha256 mismatch (staged %s, manifest %s); refusing pg_restore", name, got, want)
			}
		}
		input, err := os.Open(dump)
		if err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, "setpriv", "--reuid", "postgres", "--regid", "postgres", "--init-groups", "--",
			"pg_restore", "--clean", "--if-exists", "--exit-on-error", "--dbname="+name)
		cmd.Stdin = input
		out, err := cmd.CombinedOutput()
		_ = input.Close()
		if err != nil {
			return fmt.Errorf("pg_restore %q: %v: %s", name, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// offlineAttachSourceRepository attaches the exact source repository into
// the restored control database: a secret record for the repository
// password, an attached (never re-initialized) repository row at the
// exact discovered location, and enabled backup timers. This is what
// makes backups continue into the same instance path after recovery.
//
// An explicit caller-supplied repository password always wins (generic
// repositories; the password itself is never journaled, so it must be
// re-supplied in process memory). Otherwise the credential is recovered
// from a matching restored repository row, and only when neither exists
// is the phrase-derived value used. A restored row already matching the
// source location is reconfigured in place — never duplicated, so
// schedules keep selecting the one unique source row.
func offlineAttachSourceRepository(ctx context.Context, rt offlineRecoveryRuntime, db *sql.DB, j recoveryRestoreJournal, seed [32]byte, explicitPassword string) error {
	loc := strings.TrimSpace(j.Source.Location)
	if loc == "" {
		return fmt.Errorf("%w: source repository location missing from recovery journal", store.ErrValidation)
	}
	runner := &backups.CommandRunner{CacheDir: filepath.Join(rt.stateDir, "restic-cache")}
	_ = os.MkdirAll(runner.CacheDir, 0o700)
	secretSvc, err := offlineSecretsService(db, rt.stateDir)
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(rt.stateDir, "control.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	backupSvc := backups.New(st, backups.Config{
		VerifyRoot: filepath.Join(rt.dataDir, "backups", "verify"),
		CacheDir:   runner.CacheDir,
	}, backups.Deps{Runner: runner, Secrets: backupSecretSource{service: secretSvc}})
	repositories, err := backupSvc.Repositories(ctx)
	if err != nil {
		return err
	}
	instance, err := st.Instance(ctx)
	if err != nil {
		return err
	}
	// Password precedence: explicit > recovered from a matching
	// restored row > phrase-derived. The explicit form reaches the
	// attach without ever being persisted outside the secrets store.
	password := strings.TrimSpace(explicitPassword)
	var existingRepo *backups.Repository
	for i := range repositories {
		existing := repositories[i]
		effective := existing.Location
		if !existing.Attached && !strings.HasSuffix(strings.TrimSuffix(effective, "/"), "/"+string(instance.ID)) {
			effective, err = backups.JoinInstanceLocation(effective, string(instance.ID))
			if err != nil {
				return err
			}
		}
		if effective != loc {
			continue
		}
		existingRepo = &repositories[i]
		if password == "" {
			creds, err := (backupSecretSource{service: secretSvc}).Resolve(ctx, existing.SecretRef)
			if err != nil {
				return err
			}
			password = creds.Password
		}
		break
	}
	if password == "" {
		password = secrets.DeriveResticPassword(seed)
	}
	// Publish the credential under the per-repo record, then reuse the
	// matching restored row (or attach a fresh one exactly once) so the
	// control database ends with exactly one row for this source.
	if existingRepo != nil {
		repoRef, err := offlinePutRepoSecret(ctx, secretSvc, password, existingRepo.ID)
		if err != nil {
			return err
		}
		if _, err := backupSvc.Configure(ctx, backups.ConfigureRequest{
			ID: existingRepo.ID, Label: existingRepo.Label, Location: existingRepo.Location, SecretRef: repoRef,
		}); err != nil {
			return err
		}
	} else {
		// Reuse the wizard's attach flow: store the credential, attach
		// the exact location (restic snapshots, never init), then move
		// the credential to its per-repo record.
		stageRef, err := offlinePutRepoSecret(ctx, secretSvc, password, "restore-staging")
		if err != nil {
			return err
		}
		repo, err := backupSvc.AttachRepository(ctx, backups.ConfigureRequest{
			Label: "restored-source", Location: loc, SecretRef: stageRef,
		})
		if err != nil {
			return err
		}
		repoRef, err := offlinePutRepoSecret(ctx, secretSvc, password, repo.ID)
		if err != nil {
			return err
		}
		if _, err := backupSvc.Configure(ctx, backups.ConfigureRequest{
			ID: repo.ID, Label: repo.Label, Location: repo.Location, SecretRef: repoRef,
		}); err != nil {
			return err
		}
		_ = secretSvc.DeleteByName(ctx, "platform-app", "backup_repo_"+"restore-staging")
	}
	// Backup timers resume on the normal daemon path; enabling them here
	// as root would race the daemon restart. The restored control state
	// carries the schedule.
	return nil
}

// offlinePutRepoSecret stores a repository credential through the
// restored control database's secrets service, matching the wizard's
// naming contract.
func offlinePutRepoSecret(ctx context.Context, svc *secrets.Service, value, key string) (backups.SecretRef, error) {
	encoded, err := json.Marshal(backups.Credentials{Password: value})
	if err != nil {
		return backups.SecretRef{}, fmt.Errorf("encode credentials: %w", err)
	}
	name := "backup_repo_" + sanitizeSecretName(key)
	sec, err := svc.Put(ctx, "platform-app", name, string(encoded))
	if err != nil {
		if _, err2 := svc.RotateByName(ctx, "platform-app", name, string(encoded)); err2 != nil {
			return backups.SecretRef{}, fmt.Errorf("store credentials: %w", err)
		}
		sec, err = svc.GetByName(ctx, "platform-app", name)
		if err != nil {
			return backups.SecretRef{}, fmt.Errorf("load credential ref: %w", err)
		}
	}
	return backups.SecretRef{ID: string(sec.ID), Version: sec.Version}, nil
}

// offlineSecretsService builds the secrets service over the restored
// control database, unwrapping the master key from the live master.key
// the swap placed.
func offlineSecretsService(db *sql.DB, stateDir string) (*secrets.Service, error) {
	raw, err := os.ReadFile(filepath.Join(stateDir, "master.key"))
	if err != nil {
		return nil, fmt.Errorf("read master.key: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("master.key is %d bytes, want 32", len(raw))
	}
	var key [32]byte
	copy(key[:], raw)
	return secrets.New(db, key[:])
}

// offlineUndoInterruptedSwap undoes only this operation's partial swap.
func offlineUndoInterruptedSwap(stateDir string, j *recoveryRestoreJournal) error {
	return backups.RollbackServerConfigSwap(j.StagingDir, stateDir)
}

// offlineFailRestore persists a terminal failure: journal failed +
// restore_setup_state failed with the detail. The message is returned as
// the error so callers report it; nothing is swallowed.
func offlineFailRestore(stateDir string, j *recoveryRestoreJournal, msg string) (RecoveryRestoreStatus, error) {
	j.Operation.Status = OpFailed
	j.Operation.Error = msg
	j.Error = msg
	_ = writeRecoveryRestoreJournal(stateDir, *j)
	offlineMarkSetupState(stateDir, "failed", j.Operation.ID, msg)
	return j.Operation, translateError(fmt.Errorf("%w: %s", store.ErrValidation, msg))
}

// offlineMarkSetupState persists the restore-setup posture without a
// Backend. Write failures are logged, never fatal — the journal remains
// the authority.
func offlineMarkSetupState(stateDir, state, operation, detail string) {
	st := restoreSetupState{State: state, Operation: operation, Detail: detail}
	if err := writeRestoreSetupState(stateDir, st); err != nil {
		log.Printf("persist restore setup state %q: %v", state, err)
	}
}

// offlineStageAndValidate is the staging half shared by the CLI's own
// fresh operation and the daemon's PrepareRecoveryRestore: list, pick,
// extract to a fresh staging directory, and validate (manifest,
// fingerprint, scopes, swap plan). It writes the journal transitions but
// never touches live state.
func offlineStageAndValidate(ctx context.Context, rt offlineRecoveryRuntime, runner backups.Runner, src backups.RecoverySource, snapshotID, staging string, j *recoveryRestoreJournal) error {
	j.Operation.Stage = recoveryStageStaging
	_ = writeRecoveryRestoreJournal(rt.stateDir, *j)
	if err := backups.StageRecoverySnapshot(ctx, runner, src, snapshotID, staging); err != nil {
		return fmt.Errorf("extract snapshot: %w", err)
	}
	if _, err := backups.ValidateStaging(ctx, staging); err != nil {
		return fmt.Errorf("staging validation: %w", err)
	}
	manifest, err := backups.ServerConfigManifest(staging)
	if err != nil {
		return err
	}
	if err := backups.CheckRecoveryFingerprint(manifest, j.Source.PhraseFP); err != nil {
		return err
	}
	scopes := j.Source.Scopes
	if len(scopes) == 0 && !j.Source.IncludeApps {
		scopes = []string{backups.ScopeServerConfig}
	}
	if err := checkRestoreScopes(manifest, scopes); err != nil {
		return err
	}
	if _, err := backups.PlanServerConfigSwap(staging, rt.stateDir, manifest); err != nil {
		return fmt.Errorf("swap plan: %w", err)
	}
	j.Manifest = &manifest
	j.Operation.Stage = recoveryStageStaged
	j.Operation.Status = OpCompleted
	j.Operation.Progress = 30
	if detail := restoreReleaseRefNote(manifest); detail != "" {
		j.Operation.Preview = detail
	}
	return writeRecoveryRestoreJournal(rt.stateDir, *j)
}

// ErrOfflineNoOpID is returned when an executor run has no operation to
// run; the caller must stage first.
var ErrOfflineNoOpID = errors.New("no staged recovery restore operation")
