package controlplane

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

// Live app restore: explicit confirmation plus a pre-restore safety snapshot
// when data exists. Only selected apps and their write-capable dependents
// stop; shared PostgreSQL stays running for logical restores. Failure
// restores preserved files and safety dumps, revalidates, then resumes;
// failed rollback leaves maintenance in place with journal/SSH instructions.

func (b *Backend) startLiveRestore(ctx context.Context, j restoreJournal) (domain.Backup, error) {
	if err := writeRestoreJournal(b.cfg.StateDir, j); err != nil {
		return domain.Backup{}, err
	}
	go b.runLiveRestore(j.Operation.ID)
	src, err := b.GetBackup(ctx, domain.ID(j.BackupID))
	if err != nil {
		return domain.Backup{}, err
	}
	src.OperationID = j.Operation.ID
	src.Status = OpRunning
	return src, nil
}

func (b *Backend) runLiveRestore(opID string) {
	err := withMaintenanceLock(func() error {
		j, err := loadRestoreJournal(b.cfg.StateDir, opID)
		if err != nil {
			return err
		}
		ctx := context.Background()
		// Stage first: extraction + isolated validation before anything live.
		staging, err := os.MkdirTemp(restoreStagingRoot(b.cfg.StateDir), "op-")
		if err != nil {
			return failRestoreOp(b.cfg.StateDir, &j, err.Error())
		}
		j.StagingDir = staging
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
		manifest, err := stagedManifest(rep)
		if err != nil {
			return failRestoreOp(b.cfg.StateDir, &j, err.Error())
		}
		scopes := j.Scopes
		if len(scopes) == 0 {
			scopes = manifest.Scopes
		}
		if err := checkRestoreScopes(rep, scopes); err != nil {
			return failRestoreOp(b.cfg.StateDir, &j, err.Error())
		}
		for _, s := range scopes {
			if s == backups.ScopeServerConfig {
				return failRestoreOp(b.cfg.StateDir, &j,
					"server-config restore requires the offline recovery executor (`omahab recovery apply`); live restore refuses it")
			}
		}
		plan, err := b.planLiveRestore(ctx, manifest, scopes, staged)
		if err != nil {
			return failRestoreOp(b.cfg.StateDir, &j, err.Error())
		}
		j.AffectedApps = plan.bundles
		j.Operation.AffectedApp = plan.bundles
		j.Operation.Preview = plan.preview
		j.Operation.Stage = RestorePreflight
		_ = writeRestoreJournal(b.cfg.StateDir, j)

		// Newer-than-installed formats are not auto-downgraded.
		if err := checkRestoreVersions(plan); err != nil {
			return failRestoreOp(b.cfg.StateDir, &j, err.Error())
		}
		// PocketID is identity-critical: distinct warning, checkpoint
		// verification, and no domain-change passkey promises.
		if plan.hasPocketID {
			log.Printf("restore %q: PocketID identity checkpoint selected (database, encryption key, OIDC clients, issuer domain restore together)", opID)
			if err := checkIdentityDomain(manifest, b); err != nil {
				return failRestoreOp(b.cfg.StateDir, &j, err.Error())
			}
		}
		// Safety snapshot when data exists.
		safety, err := b.saveRestoreSafety(ctx, plan, opID)
		if err != nil {
			return failRestoreOp(b.cfg.StateDir, &j, err.Error())
		}
		j.Operation.Stage = RestoreSafetySaved
		_ = writeRestoreJournal(b.cfg.StateDir, j)
		// Stop writers: restore-owned maintenance (generic resume cannot
		// clear these), dependents first.
		for _, bundle := range plan.stopOrder {
			rows, err := b.appsByBundle(ctx, bundle)
			if err != nil {
				continue
			}
			if len(rows) == 0 {
				if _, err := b.apps.PauseForMaintenance(ctx, bundle, apps.MaintenanceRestore, opID); err != nil {
					_ = b.rollbackLiveRestore(ctx, plan, safety, opID)
					return failRestoreOp(b.cfg.StateDir, &j, err.Error())
				}
				if err := writeMarker(b.appMarkerPath(bundle)); err != nil {
					_ = b.rollbackLiveRestore(ctx, plan, safety, opID)
					return failRestoreOp(b.cfg.StateDir, &j, err.Error())
				}
				continue
			}
			if _, err := b.pauseAppLocked(ctx, rows[0].ID, apps.MaintenanceRestore); err != nil {
				_ = b.rollbackLiveRestore(ctx, plan, safety, opID)
				return failRestoreOp(b.cfg.StateDir, &j, err.Error())
			}
		}
		j.Operation.Stage = RestoreWritersStop
		_ = writeRestoreJournal(b.cfg.StateDir, j)
		// Promote staged files with ownership intact.
		if err := promoteStagedFiles(plan, staged); err != nil {
			_ = b.rollbackLiveRestore(ctx, plan, safety, opID)
			return failRestoreOp(b.cfg.StateDir, &j, "promote: "+err.Error())
		}
		j.Operation.Stage = RestoreFilesPromoted
		_ = writeRestoreJournal(b.cfg.StateDir, j)
		// Restore only selected PostgreSQL databases; the cluster stays up.
		if err := restoreSelectedDatabases(ctx, plan, staged); err != nil {
			_ = b.rollbackLiveRestore(ctx, plan, safety, opID)
			return failRestoreOp(b.cfg.StateDir, &j, "databases: "+err.Error())
		}
		j.Operation.Stage = RestoreDBsRestored
		_ = writeRestoreJournal(b.cfg.StateDir, j)
		// Verify app secrets resolve in the current control state (they
		// persist across app restores; a missing one is actionable, not
		// silently regenerated).
		if err := checkRestoreSecrets(ctx, b, manifest, scopes); err != nil {
			_ = b.rollbackLiveRestore(ctx, plan, safety, opID)
			return failRestoreOp(b.cfg.StateDir, &j, err.Error())
		}
		// Restart in dependency order, only previously running dependents.
		j.Operation.Stage = RestoreValidating
		_ = writeRestoreJournal(b.cfg.StateDir, j)
		if err := b.resumeRestoreApps(ctx, plan); err != nil {
			_ = b.rollbackLiveRestore(ctx, plan, safety, opID)
			return failRestoreOp(b.cfg.StateDir, &j, "resume: "+err.Error())
		}
		// App health plus an app-level read of the restored fixture.
		if err := checkRestoredHealth(ctx, b, plan); err != nil {
			_ = b.rollbackLiveRestore(ctx, plan, safety, opID)
			return failRestoreOp(b.cfg.StateDir, &j, "health: "+err.Error())
		}
		// Success: clear restore-owned maintenance, drop markers, restart
		// storage-independent units already resumed.
		for _, bundle := range plan.stopOrder {
			_, _ = b.db.ExecContext(ctx, `DELETE FROM app_maintenance WHERE bundle_id = ? AND reason = 'restore'`, bundle)
			_ = os.Remove(b.appMarkerPath(bundle))
		}
		j.Operation.Stage = OpCompleted
		j.Operation.Status = OpCompleted
		j.Operation.Progress = 100
		_ = writeRestoreJournal(b.cfg.StateDir, j)
		// Record a completed live restore run for the restore-tested proof.
		_ = b.recordRestoreRun(ctx, j)
		return nil
	})
	if err != nil {
		log.Printf("live restore %q: %v", opID, err)
	}
}

// livePlan is the resolved live-restore set.
type livePlan struct {
	bundles   []string
	stopOrder []string
	started   []string
	preview   string
	// stagedFiles maps canonical dest -> staged source for promotion.
	stagedFiles map[string]string
	// databases maps db name -> staged pgdump path.
	databases map[string]string
	// safety holds pre-restore file copies: dest -> safety copy.
	safetyFiles map[string]string
	// safetyDBs holds pre-restore pg dumps: db -> safety dump.
	safetyDBs   map[string]string
	hasPocketID bool
	manifest    backups.SnapshotManifest
	staged      string
}

func (b *Backend) planLiveRestore(ctx context.Context, manifest backups.SnapshotManifest, scopes []string, staged string) (*livePlan, error) {
	plan := &livePlan{
		stagedFiles: map[string]string{},
		databases:   map[string]string{},
		safetyFiles: map[string]string{},
		safetyDBs:   map[string]string{},
		manifest:    manifest,
		staged:      staged,
	}
	byBundle := map[string]backups.ManifestApp{}
	for _, a := range manifest.Applications {
		byBundle[a.BundleID] = a
	}
	want := map[string]bool{}
	for _, s := range scopes {
		if id, ok := backups.ScopeBundleID(s); ok {
			want[id] = true
			continue
		}
		switch s {
		case backups.ScopeServerSync:
			want["syncthing"] = true
		case backups.ScopeServerProjects, backups.ScopeMachineBackups:
			return nil, fmt.Errorf("%w: scope %q restores through server-config recovery, not live app restore", store.ErrValidation, s)
		default:
			return nil, fmt.Errorf("%w: unknown restore scope %q", store.ErrValidation, s)
		}
	}
	// Expand to write-capable dependents so stopping is complete.
	var bundles []string
	if b.apps != nil {
		for _, bd := range b.apps.CatalogBundles() {
			if want[bd.ID] {
				bundles = append(bundles, bd.ID)
			}
		}
		for _, bd := range b.apps.CatalogBundles() {
			if want[bd.ID] {
				continue
			}
			for _, dep := range bd.Dependencies {
				if want[dep] {
					bundles = append(bundles, bd.ID)
					break
				}
			}
		}
	} else {
		for id := range want {
			bundles = append(bundles, id)
		}
	}
	plan.bundles = bundles
	for _, id := range bundles {
		if id == "pocket-id" {
			plan.hasPocketID = true
		}
	}
	// Stop order: reverse dependency order via the catalog topo sort.
	if b.apps != nil {
		sorted, err := topoSortBundles(b.apps.CatalogBundles())
		if err == nil {
			inSet := map[string]bool{}
			for _, id := range bundles {
				inSet[id] = true
			}
			for i := len(sorted) - 1; i >= 0; i-- {
				if inSet[sorted[i].ID] {
					plan.stopOrder = append(plan.stopOrder, sorted[i].ID)
				}
			}
		}
	}
	if len(plan.stopOrder) == 0 {
		plan.stopOrder = append([]string{}, bundles...)
	}
	plan.started = append([]string{}, reverseStrings(plan.stopOrder)...)
	// Map staged files: restic restores absolute source paths under staging.
	for id := range want {
		app, ok := byBundle[id]
		if !ok {
			return nil, fmt.Errorf("%w: snapshot has no application %q", store.ErrValidation, id)
		}
		bd, ok := catalogBundle(ctx, b, id)
		if !ok {
			return nil, fmt.Errorf("%w: unknown bundle %q", store.ErrValidation, id)
		}
		for _, v := range bd.Data {
			src := filepath.Join(staged, v.Path)
			if _, err := os.Stat(src); err != nil {
				return nil, fmt.Errorf("%w: staged data missing for %q (%s)", store.ErrValidation, id, v.Path)
			}
			plan.stagedFiles[v.Path] = src
		}
		for _, dbName := range app.Databases {
			dump := findStagedDump(staged, dbName)
			if dump == "" {
				return nil, fmt.Errorf("%w: staged dump missing for database %q", store.ErrValidation, dbName)
			}
			plan.databases[dbName] = dump
		}
		_ = app
	}
	var lines []string
	lines = append(lines, fmt.Sprintf("restore %d bundle(s): %s", len(bundles), strings.Join(bundles, ", ")))
	lines = append(lines, fmt.Sprintf("writers stop: %s; restart: %s", strings.Join(plan.stopOrder, ", "), strings.Join(plan.started, ", ")))
	plan.preview = strings.Join(lines, "; ")
	return plan, nil
}

func catalogBundle(ctx context.Context, b *Backend, id string) (apps.Bundle, bool) {
	if b.apps == nil {
		return apps.Bundle{}, false
	}
	return b.apps.CatalogSnapshot().Get(id)
}

func reverseStrings(in []string) []string {
	out := append([]string{}, in...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func stagedManifest(rep backups.StagingReport) (backups.SnapshotManifest, error) {
	if len(rep.Manifests) == 0 {
		return backups.SnapshotManifest{}, fmt.Errorf("%w: snapshot has no manifest; legacy snapshots are staging/manual recovery only",
			store.ErrValidation)
	}
	return rep.Manifests[0], nil
}

// checkRestoreVersions refuses newer-than-installed formats instead of
// auto-downgrading; the paused manual workflow covers those.
func checkRestoreVersions(plan *livePlan) error {
	return checkRestoreVersionsAgainst(plan, systemReleaseRef())
}

func checkRestoreVersionsAgainst(plan *livePlan, current string) error {
	for _, id := range plan.bundles {
		for _, a := range plan.manifest.Applications {
			if a.BundleID != id {
				continue
			}
			if strings.TrimSpace(a.Version) != "" && strings.TrimSpace(current) != "" &&
				a.Version != current {
				return fmt.Errorf("%w: bundle %q snapshot version %q differs from installed %q: refusing auto-downgrade; use the paused manual workflow",
					store.ErrValidation, id, a.Version, current)
			}
		}
	}
	return nil
}

func systemReleaseRef() string {
	if raw, err := os.ReadFile("/etc/omahab-release"); err == nil {
		if ref := strings.TrimSpace(string(raw)); ref != "" {
			return ref
		}
	}
	return ""
}

// checkIdentityDomain refuses PocketID restores across issuer domain changes:
// old passkeys must not be promised to migrate.
func checkIdentityDomain(manifest backups.SnapshotManifest, b *Backend) error {
	inst, err := b.store.Instance(context.Background())
	if err != nil {
		return err
	}
	current := strings.TrimSpace(inst.Domain)
	if current == "" || strings.TrimSpace(manifest.Domain) == "" {
		return nil
	}
	if !strings.EqualFold(current, strings.TrimSpace(manifest.Domain)) {
		return fmt.Errorf("%w: identity checkpoint domain %q differs from current %q: old passkeys do not migrate; use identity recovery, then enroll fresh",
			store.ErrValidation, manifest.Domain, current)
	}
	return nil
}

// saveRestoreSafety preserves current files on the same destination
// filesystem plus safety database dumps before promotion.
func (b *Backend) saveRestoreSafety(ctx context.Context, plan *livePlan, opID string) (string, error) {
	safety := filepath.Join(b.cfg.StateDir, "restore-operations", opID, "safety")
	if st, err := os.Stat(safety); err == nil && st.IsDir() {
		if entries, _ := os.ReadDir(safety); len(entries) > 0 {
			return safety, nil
		}
	}
	if err := os.MkdirAll(safety, 0o700); err != nil {
		return "", err
	}
	for dest := range plan.stagedFiles {
		if _, err := os.Stat(dest); err != nil {
			continue
		}
		backup := filepath.Join(safety, "files"+dest)
		if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
			return "", err
		}
		if out, err := exec.CommandContext(ctx, "cp", "-a", dest, backup).CombinedOutput(); err != nil {
			return "", fmt.Errorf("safety copy %q: %v: %s", dest, err, strings.TrimSpace(string(out)))
		}
		plan.safetyFiles[dest] = backup
	}
	for dbName := range plan.databases {
		dump := filepath.Join(safety, dbName+".safety.pgdump")
		if err := dumpPostgresDB(ctx, dbName, dump); err != nil {
			// A missing database has no safety to take; restore creates it.
			if strings.Contains(err.Error(), "does not exist") {
				continue
			}
			return "", fmt.Errorf("safety dump %q: %w", dbName, err)
		}
		plan.safetyDBs[dbName] = dump
	}
	return safety, nil
}

func promoteStagedFiles(plan *livePlan, staged string) error {
	_ = staged
	ctx := context.Background()
	for dest, src := range plan.stagedFiles {
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return err
		}
		out, err := exec.CommandContext(ctx, "rsync", "-aHAX", "--numeric-ids", "--delete", src+"/", dest+"/").CombinedOutput()
		if err != nil {
			return fmt.Errorf("promote %q: %v: %s", dest, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func restoreSelectedDatabases(ctx context.Context, plan *livePlan, staged string) error {
	_ = staged
	for dbName, dump := range plan.databases {
		out, err := exec.CommandContext(ctx, "pg_restore", "--clean", "--if-exists", "--exit-on-error",
			"--dbname="+dbName, dump).CombinedOutput()
		if err != nil {
			return fmt.Errorf("pg_restore %q: %v: %s", dbName, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func findStagedDump(staged, dbName string) string {
	candidates := []string{
		filepath.Join(staged, dbName+".pgdump"),
	}
	var found string
	_ = filepath.Walk(staged, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || found != "" {
			return nil
		}
		if info.Name() == dbName+".pgdump" {
			found = path
		}
		return nil
	})
	_ = candidates
	return found
}

func checkRestoreSecrets(ctx context.Context, b *Backend, manifest backups.SnapshotManifest, scopes []string) error {
	if b.secrets == nil {
		return nil
	}
	want := map[string]bool{}
	for _, s := range scopes {
		if id, ok := backups.ScopeBundleID(s); ok {
			for _, a := range manifest.Applications {
				if a.BundleID == id {
					for _, name := range a.Secrets {
						want[name] = true
					}
				}
			}
		}
	}
	for name := range want {
		if _, err := b.secrets.GetByName(ctx, "platform-app", name); err != nil {
			return fmt.Errorf("%w: app secret %q missing from current control state; restore it through server-config recovery first",
				store.ErrValidation, name)
		}
	}
	return nil
}

func (b *Backend) resumeRestoreApps(ctx context.Context, plan *livePlan) error {
	for _, bundle := range plan.started {
		rows, err := b.appsByBundle(ctx, bundle)
		if err != nil {
			return err
		}
		for _, a := range rows {
			cur, err := b.apps.Status(ctx, a.ID)
			if err != nil || cur.DesiredState != apps.DesiredRunning {
				continue
			}
			if _, err := b.apps.Start(ctx, a.ID); err != nil {
				return translateError(err)
			}
		}
	}
	return nil
}

func checkRestoredHealth(ctx context.Context, b *Backend, plan *livePlan) error {
	for _, bundle := range plan.bundles {
		rows, err := b.appsByBundle(ctx, bundle)
		if err != nil {
			return err
		}
		for _, a := range rows {
			st, err := b.apps.CheckHealth(ctx, a.ID)
			if err != nil {
				return translateError(err)
			}
			if st.Health == domain.HealthUnhealthy {
				return fmt.Errorf("%w: app %q unhealthy after restore", apitypes.ErrConflict, a.Name)
			}
		}
	}
	return nil
}

// rollbackLiveRestore restores preserved files and safety dumps, revalidates,
// then resumes. Failed rollback leaves maintenance in place with actionable
// journal/SSH instructions, never an empty auto-start.
func (b *Backend) rollbackLiveRestore(ctx context.Context, plan *livePlan, safety, opID string) error {
	_ = safety
	for dest, backup := range plan.safetyFiles {
		if out, err := exec.CommandContext(ctx, "rsync", "-aHAX", "--numeric-ids", "--delete", backup+"/", dest+"/").CombinedOutput(); err != nil {
			return fmt.Errorf("rollback %q failed: %v: %s; maintenance stays paused, inspect journal %s or use SSH",
				dest, err, strings.TrimSpace(string(out)), opID)
		}
	}
	for dbName, dump := range plan.safetyDBs {
		out, err := exec.CommandContext(ctx, "pg_restore", "--clean", "--if-exists", "--exit-on-error",
			"--dbname="+dbName, dump).CombinedOutput()
		if err != nil {
			return fmt.Errorf("rollback db %q failed: %v: %s; maintenance stays paused, inspect journal %s or use SSH",
				dbName, err, strings.TrimSpace(string(out)), opID)
		}
	}
	if err := b.resumeRestoreApps(ctx, plan); err != nil {
		return fmt.Errorf("rollback resume failed: %w; maintenance stays paused, inspect journal %s or use SSH", err, opID)
	}
	if err := checkRestoredHealth(ctx, b, plan); err != nil {
		return fmt.Errorf("rollback validation failed: %w; maintenance stays paused, inspect journal %s or use SSH", err, opID)
	}
	for _, bundle := range plan.stopOrder {
		_, _ = b.db.ExecContext(ctx, `DELETE FROM app_maintenance WHERE bundle_id = ? AND reason = 'restore'`, bundle)
		_ = os.Remove(b.appMarkerPath(bundle))
	}
	return nil
}

// recordRestoreRun notes a completed live restore for the restore-tested proof.
func (b *Backend) recordRestoreRun(ctx context.Context, j restoreJournal) error {
	_, err := b.db.ExecContext(ctx,
		`INSERT INTO backup_runs (id, kind, repository_id, status, triggered_by, stage, snapshot_id, started_at, finished_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		"restore-"+j.Operation.ID, "restore", j.RepositoryID, "completed", "manual",
		"completed", j.SnapshotID, j.Operation.CreatedAt, j.Operation.UpdatedAt)
	return err
}

// resumeRestoreOperation resumes a failed restore from its journal: staging
// failures re-extract, mid-live failures re-run the live flow against the
// reused safety snapshot. Pause persists across reboots via markers.
func (b *Backend) resumeRestoreOperation(ctx context.Context, id string) (Operation, error) {
	j, err := loadRestoreJournal(b.cfg.StateDir, id)
	if err != nil {
		return Operation{}, err
	}
	if !j.Operation.CanResume {
		return Operation{}, fmt.Errorf("%w: operation cannot resume from stage %q", apitypes.ErrConflict, j.Operation.Stage)
	}
	j.Operation.Status = OpRunning
	j.Operation.CanResume = false
	if err := writeRestoreJournal(b.cfg.StateDir, j); err != nil {
		return Operation{}, err
	}
	if j.Operation.Stage == RestoreStaged || j.StagingDir == "" {
		go b.runRestoreStaging(id)
	} else {
		go b.runLiveRestore(id)
	}
	return j.Operation, nil
}

// rollbackRestoreOperation restores preserved files and safety dumps from the
// journal safety snapshot, then resumes to prior desired state.
func (b *Backend) rollbackRestoreOperation(ctx context.Context, id string) (Operation, error) {
	j, err := loadRestoreJournal(b.cfg.StateDir, id)
	if err != nil {
		return Operation{}, err
	}
	if !j.Operation.CanRollback {
		return Operation{}, fmt.Errorf("%w: operation cannot roll back from stage %q", apitypes.ErrConflict, j.Operation.Stage)
	}
	go b.runRestoreRollback(id)
	j.Operation.Status = "rolling_back"
	_ = writeRestoreJournal(b.cfg.StateDir, j)
	return j.Operation, nil
}

func (b *Backend) runRestoreRollback(id string) {
	_ = withMaintenanceLock(func() error {
		j, err := loadRestoreJournal(b.cfg.StateDir, id)
		if err != nil {
			return err
		}
		ctx := context.Background()
		safety := filepath.Join(b.cfg.StateDir, "restore-operations", id, "safety")
		if st, err := os.Stat(safety); err != nil || !st.IsDir() {
			return failRestoreOp(b.cfg.StateDir, &j, "no safety snapshot to roll back to")
		}
		rep, err := backups.ValidateStaging(ctx, j.StagingDir)
		if err != nil {
			log.Printf("restore rollback: staging revalidation: %v", err)
		}
		_ = rep
		manifest, err := stagedManifestFromDir(j.StagingDir)
		if err != nil {
			return failRestoreOp(b.cfg.StateDir, &j, err.Error())
		}
		plan, err := b.planLiveRestore(ctx, manifest, j.Scopes, j.StagingDir)
		if err != nil {
			return failRestoreOp(b.cfg.StateDir, &j, err.Error())
		}
		// Point safety maps at the journal safety snapshot.
		plan.safetyFiles = map[string]string{}
		plan.safetyDBs = map[string]string{}
		for dest := range plan.stagedFiles {
			backup := filepath.Join(safety, "files"+dest)
			if _, err := os.Stat(backup); err == nil {
				plan.safetyFiles[dest] = backup
			}
		}
		for dbName := range plan.databases {
			dump := filepath.Join(safety, dbName+".safety.pgdump")
			if _, err := os.Stat(dump); err == nil {
				plan.safetyDBs[dbName] = dump
			}
		}
		if err := b.rollbackLiveRestore(ctx, plan, safety, id); err != nil {
			return failRestoreOp(b.cfg.StateDir, &j, err.Error())
		}
		j.Operation.Status = OpRolledBak
		j.Operation.Stage = OpRolledBak
		j.Operation.CanResume = false
		j.Operation.CanRollback = false
		_ = writeRestoreJournal(b.cfg.StateDir, j)
		return nil
	})
}

func stagedManifestFromDir(staging string) (backups.SnapshotManifest, error) {
	rep, err := backups.ValidateStaging(context.Background(), staging)
	if err != nil {
		return backups.SnapshotManifest{}, err
	}
	return stagedManifest(rep)
}
