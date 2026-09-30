package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/bundles"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

// Scoped snapshot capture: consistent dumps plus a structured manifest,
// taken while selected writers are stopped and resumed afterwards even on
// failure. Bounded downtime is chosen over guessed SQLite filenames and
// hot-copy hooks: SQLite apps are cold-copied while stopped (WAL preserved
// as part of the stopped directory), PostgreSQL is dumped logically as its
// OS user, and any error fails the backup with no raw-copy fallback.

// resolveBackupScopes expands an empty scope list to all configured scopes
// and validates every entry.
func (b *Backend) resolveBackupScopes(ctx context.Context, requested []string) ([]string, error) {
	if len(requested) == 0 {
		return b.allBackupScopes(ctx)
	}
	known := map[string]bool{
		backups.ScopeServerConfig:   true,
		backups.ScopeServerSync:     true,
		backups.ScopeServerProjects: true,
		backups.ScopeMachineBackups: true,
	}
	if b.apps != nil {
		for _, bd := range b.apps.CatalogBundles() {
			known[backups.AppScope(bd.ID)] = true
		}
	}
	var out []string
	for _, s := range requested {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !known[s] {
			return nil, fmt.Errorf("%w: unknown backup scope %q", store.ErrValidation, s)
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return b.allBackupScopes(ctx)
	}
	return out, nil
}

func (b *Backend) allBackupScopes(ctx context.Context) ([]string, error) {
	scopes := []string{backups.ScopeServerConfig}
	if b.apps != nil {
		list, err := b.apps.List(ctx)
		if err == nil {
			installed := map[string]bool{}
			for _, st := range list {
				installed[st.BundleID] = true
			}
			for _, bd := range b.apps.CatalogBundles() {
				if installed[bd.ID] {
					scopes = append(scopes, backups.AppScope(bd.ID))
				}
			}
		}
	}
	for _, extra := range []struct {
		scope string
		path  string
	}{
		{backups.ScopeServerSync, filepath.Join(b.cfg.DataDir, "sync")},
		{backups.ScopeServerProjects, filepath.Join(b.cfg.DataDir, "projects")},
		{backups.ScopeMachineBackups, filepath.Join(b.cfg.DataDir, "machine-backups")},
	} {
		if st, err := os.Stat(extra.path); err == nil && st.IsDir() {
			duplicate := false
			for _, s := range scopes {
				if s == extra.scope {
					duplicate = true
				}
			}
			if !duplicate {
				scopes = append(scopes, extra.scope)
			}
		}
	}
	return scopes, nil
}

// capturePlan is the resolved file set for one scoped backup.
type capturePlan struct {
	scopes    []string
	bundles   []apps.Bundle
	sources   []string
	databases []string
	omitted   []backups.ManifestOmission
	// systemUnits are native units stopped for the capture (meilisearch)
	// with their prior active state.
	systemUnits []stoppedUnit
	// stoppedApps records the applications quiesce actually stopped, in
	// stop order, so resume restores exactly the pre-backup running
	// set even when quiesce fails partway through.
	stoppedApps []domain.ID
	// stoppedSysUnits records the native units quiesce actually stopped.
	stoppedSysUnits []string
}

type stoppedUnit struct {
	unit   string
	active bool
}

func dedupePaths(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		clean := filepath.Clean(strings.TrimSpace(p))
		if clean == "" || clean == "." || seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	return out
}

func (b *Backend) planBackupCapture(ctx context.Context, scopes []string) (*capturePlan, error) {
	plan := &capturePlan{scopes: scopes}
	wantApp := map[string]bool{}
	var wantConfig, wantSync, wantProjects, wantMachine bool
	for _, s := range scopes {
		switch s {
		case backups.ScopeServerConfig:
			wantConfig = true
		case backups.ScopeServerSync:
			wantSync = true
		case backups.ScopeServerProjects:
			wantProjects = true
		case backups.ScopeMachineBackups:
			wantMachine = true
		default:
			if id, ok := backups.ScopeBundleID(s); ok {
				wantApp[id] = true
			}
		}
	}
	bundleByID := map[string]apps.Bundle{}
	if b.apps != nil {
		for _, bd := range b.apps.CatalogBundles() {
			bundleByID[bd.ID] = bd
		}
	}
	installed := map[string][]apps.Status{}
	if b.apps != nil {
		if list, err := b.apps.List(ctx); err == nil {
			for _, st := range list {
				installed[st.BundleID] = append(installed[st.BundleID], st)
			}
		}
	}
	// Server-config always carries the inseparable PocketID identity
	// checkpoint alongside the control dump.
	if wantConfig {
		if _, ok := bundleByID["pocket-id"]; ok {
			wantApp["pocket-id"] = true
		}
	}
	for id := range wantApp {
		bd, ok := bundleByID[id]
		if !ok {
			return nil, fmt.Errorf("%w: unknown bundle %q", store.ErrValidation, id)
		}
		rows := installed[id]
		if len(rows) == 0 {
			plan.omitted = append(plan.omitted, backups.ManifestOmission{Scope: backups.AppScope(id), Reason: "not-installed"})
			continue
		}
		empty := true
		for _, v := range bd.Data {
			if dirNonEmpty(v.Path) {
				empty = false
				break
			}
		}
		if empty {
			if _, err := apps.GetMaintenance(ctx, b.db, id); err == nil {
				plan.omitted = append(plan.omitted, backups.ManifestOmission{Scope: backups.AppScope(id), Reason: "paused-empty"})
				continue
			}
		}
		plan.bundles = append(plan.bundles, bd)
		for _, v := range bd.Data {
			plan.sources = append(plan.sources, v.Path)
		}
		plan.databases = append(plan.databases, bd.Backup.Databases...)
	}
	if wantSync {
		plan.sources = append(plan.sources, filepath.Join(b.cfg.DataDir, "sync"))
	}
	if wantProjects {
		plan.sources = append(plan.sources, filepath.Join(b.cfg.DataDir, "projects"))
	}
	if wantMachine {
		plan.sources = append(plan.sources, filepath.Join(b.cfg.DataDir, "machine-backups"))
	}
	plan.sources = dedupePaths(plan.sources)
	// Missing required native data is an error; omitted scopes were recorded.
	for _, bd := range plan.bundles {
		for _, v := range bd.Data {
			if _, err := os.Stat(v.Path); err != nil {
				return nil, fmt.Errorf("%w: bundle %q required path %q: %v", store.ErrValidation, bd.ID, v.Path, err)
			}
		}
	}
	for _, src := range plan.sources {
		if _, err := os.Stat(src); err != nil {
			return nil, fmt.Errorf("%w: backup source %q: %v", store.ErrValidation, src, err)
		}
	}
	// Meilisearch has no dump interface: quiesce it with Karakeep before the
	// index is captured.
	for _, bd := range plan.bundles {
		if bd.ID == "karakeep" {
			plan.systemUnits = append(plan.systemUnits, stoppedUnit{unit: "meilisearch.service", active: systemctlActive("meilisearch.service")})
		}
	}
	return plan, nil
}

// backupResumeTimeout bounds the full resume pass. A hung service manager
// must surface as a reported resume error, never as apps silently left
// stopped.
const backupResumeTimeout = 10 * time.Minute

// quiesceCapture stops selected writers and records everything it stopped
// on the plan. On partial failure the caller must still resume the plan:
// the returned error describes the failed stop, and plan.stoppedApps /
// plan.stoppedSysUnits hold the earlier stops to restore. Backup
// quiescence writes no maintenance markers and no permanent pause records.
func (b *Backend) quiesceCapture(ctx context.Context, plan *capturePlan) error {
	for _, u := range plan.systemUnits {
		if !u.active {
			continue
		}
		if out, err := exec.CommandContext(ctx, "systemctl", "stop", u.unit).CombinedOutput(); err != nil {
			return fmt.Errorf("stop %s: %v: %s", u.unit, err, strings.TrimSpace(string(out)))
		}
		plan.stoppedSysUnits = append(plan.stoppedSysUnits, u.unit)
	}
	for _, bd := range plan.bundles {
		rows, err := b.appsByBundle(ctx, bd.ID)
		if err != nil {
			return err
		}
		for _, a := range rows {
			cur, err := b.apps.Status(ctx, a.ID)
			if err != nil {
				return translateError(err)
			}
			if cur.ObservedState != apps.ObservedRunning {
				continue
			}
			if _, err := b.apps.Quiesce(ctx, a.ID); err != nil {
				return translateError(err)
			}
			plan.stoppedApps = append(plan.stoppedApps, a.ID)
		}
	}
	return nil
}

// resumeCapture restarts exactly what quiesce stopped, in reverse stop
// order, under a bounded timeout. Every failure is collected and returned
// so a failed resume is reported rather than logged and hidden; callers
// join it with any backup error. Applications whose desired state moved
// away from running meanwhile are left alone.
func (b *Backend) resumeCapture(ctx context.Context, plan *capturePlan) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backupResumeTimeout)
	defer cancel()
	var errs []error
	for i := len(plan.stoppedApps) - 1; i >= 0; i-- {
		id := plan.stoppedApps[i]
		cur, err := b.apps.Status(rctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("resume status %q: %w", id, translateError(err)))
			continue
		}
		if cur.DesiredState != apps.DesiredRunning {
			continue
		}
		if _, err := b.apps.Start(rctx, id); err != nil {
			errs = append(errs, fmt.Errorf("resume %q: %w", cur.Name, translateError(err)))
		}
	}
	for i := len(plan.stoppedSysUnits) - 1; i >= 0; i-- {
		unit := plan.stoppedSysUnits[i]
		if out, err := exec.CommandContext(rctx, "systemctl", "start", unit).CombinedOutput(); err != nil {
			errs = append(errs, fmt.Errorf("resume %s: %v: %s", unit, err, strings.TrimSpace(string(out))))
		}
	}
	return errors.Join(errs...)
}

// dumpControlDB takes the control SQLite DB with VACUUM INTO through the
// existing database connection into a unique file. Queued writes resume after
// the consistent copy via the driver's busy timeout; live control.db/WAL
// files are never copied.
func dumpControlDB(ctx context.Context, db *sql.DB, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	_ = os.Remove(dest)
	quoted := "'" + strings.ReplaceAll(dest, "'", "''") + "'"
	if _, err := db.ExecContext(ctx, "VACUUM INTO "+quoted); err != nil {
		return fmt.Errorf("vacuum control db: %w", err)
	}
	if st, err := os.Stat(dest); err != nil || st.Size() == 0 {
		return fmt.Errorf("vacuum control db: empty dump")
	}
	return nil
}

// dumpPostgresDB dumps one logical database as the postgres OS user with
// pg_dump --format=custom. The staging file is pre-created root-owned, handed
// to the dump writer, then locked back to root-only. Any error fails the
// backup; there is no raw-copy fallback and no empty-dump success.
func dumpPostgresDB(ctx context.Context, database, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_ = f.Close()
	if err := os.Chown(dest, postgresUID(), -1); err != nil {
		return fmt.Errorf("stage pg dump: %w", err)
	}
	defer func() {
		_ = os.Chown(dest, 0, 0)
		_ = os.Chmod(dest, 0o600)
	}()
	runner := "runuser"
	args := []string{"-u", "postgres", "--", "pg_dump", "--format=custom", "--dbname=" + database, "--file=" + dest}
	if _, err := exec.LookPath(runner); err != nil {
		runner = "su"
		args = []string{"postgres", "-s", "/bin/sh", "-c", "pg_dump --format=custom --dbname=" + shellQuoteArg(database) + " --file=" + shellQuoteArg(dest)}
	}
	out, err := exec.CommandContext(ctx, runner, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("pg_dump %s: %v: %s", database, err, strings.TrimSpace(string(out)))
	}
	if st, err := os.Stat(dest); err != nil || st.Size() == 0 {
		return fmt.Errorf("pg_dump %s: empty dump", database)
	}
	return nil
}

func shellQuoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// postgresUID resolves the postgres OS user for dump file staging.
func postgresUID() int {
	if u, err := user.Lookup("postgres"); err == nil {
		if id, err := strconv.Atoi(u.Uid); err == nil {
			return id
		}
	}
	return 70
}

// backupExcludes keeps caches, verification/staging/rollback trees, live
// databases, the raw PostgreSQL cluster, mounts and transient runtime files
// out of snapshots. Local repositories beneath selected data are excluded
// per repository to avoid recursive capture.
func (b *Backend) backupExcludes(ctx context.Context, sources []string) []string {
	excludes := []string{
		"*.sock",
		"*.socket",
		"__pycache__",
		".cache",
	}
	if b.backups == nil {
		return excludes
	}
	repos, err := b.backups.Repositories(ctx)
	if err != nil {
		return excludes
	}
	for _, repo := range repos {
		loc := strings.TrimSpace(repo.Location)
		if !filepath.IsAbs(loc) {
			continue
		}
		for _, src := range sources {
			if loc == src || strings.HasPrefix(loc, src+"/") {
				excludes = append(excludes, loc)
				break
			}
		}
	}
	return excludes
}

// CreateBackupScoped runs a scoped snapshot: preflight the repository,
// resolve scopes, stop selected writers, dump databases, capture with
// restic, and resume writers even on failure. Any dump or capture error
// fails the backup; a failed resume is reported alongside, never hidden,
// so a failed backup always leaves applications in their pre-backup
// running state or reports exactly which resume failed.
func (b *Backend) CreateBackupScoped(ctx context.Context, repository string, scopes []string, scheduled bool) (domain.Backup, error) {
	if b.backups == nil {
		return domain.Backup{}, translateError(fmt.Errorf("%w: backups not configured", ErrNotConfigured))
	}
	resolved, err := b.resolveBackupScopes(ctx, scopes)
	if err != nil {
		return domain.Backup{}, translateError(err)
	}
	plan, err := b.planBackupCapture(ctx, resolved)
	if err != nil {
		return domain.Backup{}, translateError(err)
	}
	var result domain.Backup
	err = withMaintenanceLock(func() error {
		// Preflight before stopping anything: an uninitialized or
		// unreachable repository must fail here, with writers still
		// running, not after quiesce.
		if _, _, err := b.backups.PreflightRepository(ctx, strings.TrimSpace(repository)); err != nil {
			return translateError(err)
		}
		if err := b.quiesceCapture(ctx, plan); err != nil {
			qerr := translateError(err)
			if rerr := b.resumeCapture(ctx, plan); rerr != nil {
				return errors.Join(qerr, fmt.Errorf("backup resume after failed quiesce: %w", translateError(rerr)))
			}
			return qerr
		}
		captured, opErr := b.runCaptureLocked(ctx, plan, resolved, repository, scheduled)
		if rerr := b.resumeCapture(ctx, plan); rerr != nil {
			rerr = fmt.Errorf("backup resume: %w", translateError(rerr))
			if opErr != nil {
				return errors.Join(opErr, rerr)
			}
			return rerr
		}
		if opErr != nil {
			return opErr
		}
		result = captured
		return nil
	})
	if err != nil {
		return domain.Backup{}, err
	}
	return result, nil
}

// backupStagingPath returns the fixed capture staging directory: one
// constant system path, recreated per run under the capture lock, so the
// manifest location inside a snapshot is always
// /var/lib/omahab/backup-staging/manifest.json.
func (b *Backend) backupStagingPath() string {
	return filepath.Join(b.cfg.StateDir, "backup-staging")
}

// runCaptureLocked stages dumps and the manifest and runs the restic
// capture. The caller owns quiesce/resume around it; every error is
// already translated for API callers.
func (b *Backend) runCaptureLocked(ctx context.Context, plan *capturePlan, resolved []string, repository string, scheduled bool) (domain.Backup, error) {
	staging := b.backupStagingPath()
	if err := os.RemoveAll(staging); err != nil && !os.IsNotExist(err) {
		return domain.Backup{}, err
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return domain.Backup{}, err
	}
	success := false
	defer func() {
		if !success {
			log.Printf("backup staging kept for inspection: %s", staging)
		} else {
			_ = os.RemoveAll(staging)
		}
	}()
	// Control checkpoint + identity material.
	hasConfig := false
	for _, s := range resolved {
		if s == backups.ScopeServerConfig {
			hasConfig = true
		}
	}
	if hasConfig {
		if err := dumpControlDB(ctx, b.db, filepath.Join(staging, "control.db")); err != nil {
			return domain.Backup{}, translateError(err)
		}
		if kit, err := os.ReadFile(filepath.Join(b.cfg.StateDir, "recovery.kit")); err == nil {
			_ = os.WriteFile(filepath.Join(staging, "recovery.kit"), kit, 0o600)
		}
		if raw, err := os.ReadFile(storageJSONPath(b.cfg.StateDir)); err == nil {
			_ = os.WriteFile(filepath.Join(staging, "storage.json"), raw, 0o600)
		}
	}
	// Logical database dumps as the postgres OS user.
	for _, dbName := range plan.databases {
		if err := dumpPostgresDB(ctx, dbName, filepath.Join(staging, dbName+".pgdump")); err != nil {
			return domain.Backup{}, translateError(err)
		}
	}
	// Custom bundle definitions travel with their app scopes: the
	// committed files at HEAD are copied into staging bundles/<id>/.
	if err := b.stageCustomBundleDefinitions(ctx, plan, staging); err != nil {
		return domain.Backup{}, translateError(err)
	}
	manifest, err := b.buildManifest(ctx, resolved, plan, staging)
	if err != nil {
		return domain.Backup{}, translateError(err)
	}
	if err := backups.WriteManifest(staging, manifest); err != nil {
		return domain.Backup{}, translateError(err)
	}
	sources := dedupePaths(append([]string{staging}, plan.sources...))
	run, err := b.backups.RunBackup(ctx, backups.RunRequest{
		RepositoryID: strings.TrimSpace(repository),
		Trigger:      triggerFor(scheduled),
		Scopes:       resolved,
		Paths:        sources,
		Excludes:     b.backupExcludes(ctx, sources),
	})
	if err != nil {
		return domain.Backup{}, translateError(err)
	}
	got, err := b.GetBackup(ctx, domain.ID(run.ID))
	if err != nil {
		return domain.Backup{}, err
	}
	success = true
	// Retention is due at most weekly (the prune timer is the other
	// trigger): a fresh install gets its first pass right after its
	// first successful backup, steady state stays weekly.
	if run.Trigger == backups.TriggerScheduled {
		go b.backups.MaybeApplyRetention(context.WithoutCancel(ctx), run.RepositoryID)
	}
	return got, nil
}

// triggerFor maps the scheduled flag to the run trigger vocabulary.
func triggerFor(scheduled bool) string {
	if scheduled {
		return backups.TriggerScheduled
	}
	return backups.TriggerManual
}

// buildManifest assembles the v2 manifest for the resolved scopes with
// per-artifact sha256 hashes over the staged files.
func (b *Backend) buildManifest(ctx context.Context, scopes []string, plan *capturePlan, staging string) (backups.SnapshotManifest, error) {
	inst, err := b.store.Instance(ctx)
	if err != nil {
		return backups.SnapshotManifest{}, err
	}
	bundleByID := map[string]apps.Bundle{}
	if b.apps != nil {
		for _, bd := range b.apps.CatalogBundles() {
			bundleByID[bd.ID] = bd
		}
	}
	m := backups.SnapshotManifest{
		FormatVersion: backups.ManifestFormatVersion,
		Instance: backups.ManifestInstance{
			ID:        string(inst.ID),
			Domain:    strings.TrimSpace(inst.Domain),
			Placement: Placement(),
		},
		System: backups.ManifestSystem{
			ReleaseRef:      b.version,
			NixpkgsRev:      strings.TrimSpace(os.Getenv("OMAHAB_NIXPKGS_REV")),
			FlakeLockSHA256: strings.TrimSpace(os.Getenv("OMAHAB_FLAKE_LOCK_SHA256")),
			BundlesRepoRev:  bundlesRepoRev(ctx),
		},
		Scopes:  plan.scopeRecords(ctx, scopes, bundleByID, staging),
		Omitted: plan.omitted,
	}
	if sf, _, err := loadStorageFile(storageJSONPath(b.cfg.StateDir)); err == nil {
		known, _ := b.knownVolumes(ctx)
		for _, pl := range sf.Placements {
			m.Storage = append(m.Storage, backups.ManifestDisk{Volume: pl.Volume, Path: known[pl.Volume], FSUUID: pl.FSUUID})
		}
	}
	if fp, err := b.secrets.RevealByName(ctx, "platform-app", "recovery_fingerprint"); err == nil {
		m.RecoveryFingerprint = strings.TrimSpace(fp)
	}
	return m, nil
}

// bundlesRepoRev reports the custom-bundles repo HEAD when the repo
// exists; empty otherwise.
func bundlesRepoRev(ctx context.Context) string {
	if !bundles.RepoExists() {
		return ""
	}
	out, err := exec.CommandContext(ctx, "git", "-C", bundles.RepoPath, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
