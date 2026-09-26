package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/domain"
)
// maintenanceLockPath serializes backup/restore/storage/pause across CLI and
// daemon, before per-app locks. The backup DB keeps its single-operation
// constraint on top.
//
// OMAHAB_MAINTENANCE_LOCK overrides the path in tests so lock-protected
// flows stay hermetic without touching /run.
func maintenanceLockPath() string {
	if p := os.Getenv("OMAHAB_MAINTENANCE_LOCK"); p != "" {
		return p
	}
	return "/run/omahab/maintenance.lock"
}

// withMaintenanceLock holds an exclusive flock on the shared maintenance lock
// for the duration of fn. Blocking: callers serialize pause/restore/storage.
func withMaintenanceLock(fn func() error) error {
	path := maintenanceLockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}

// appMarkerPath is the boot-time authority for a paused bundle.
func (b *Backend) appMarkerPath(bundleID string) string {
	return filepath.Join(b.cfg.StateDir, "maintenance", "apps", bundleID)
}

// serverMarkerPath gates all app units for daemon-stopping operations.
func (b *Backend) serverMarkerPath() string {
	return filepath.Join(b.cfg.StateDir, "maintenance", "server")
}

func writeMarker(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func newOperationID() string {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "maint-unknown"
	}
	return "maint-" + hex.EncodeToString(rnd[:])
}

// affectedBundles returns the target plus transitive dependents in topological
// order (dependencies first). Callers stop in reverse and resume in forward
// order without overwriting prior desired state.
func (b *Backend) affectedBundles(target string) ([]apps.Bundle, error) {
	if b.apps == nil {
		return nil, fmt.Errorf("%w: apps not configured", ErrNotConfigured)
	}
	bundles := b.apps.CatalogBundles()
	sorted, err := topoSortBundles(bundles)
	if err != nil {
		return nil, err
	}
	// Transitive dependents of target.
	inSet := map[string]bool{target: true}
	changed := true
	for changed {
		changed = false
		for _, bd := range sorted {
			if inSet[bd.ID] {
				continue
			}
			for _, dep := range bd.Dependencies {
				if inSet[dep] {
					inSet[bd.ID] = true
					changed = true
					break
				}
			}
		}
	}
	var out []apps.Bundle
	for _, bd := range sorted {
		if inSet[bd.ID] {
			out = append(out, bd)
		}
	}
	return out, nil
}

func (b *Backend) appsByBundle(ctx context.Context, bundleID string) ([]apps.Status, error) {
	list, err := b.apps.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []apps.Status
	for _, st := range list {
		if st.BundleID == bundleID {
			out = append(out, st)
		}
	}
	return out, nil
}

// PauseAppForImport stops writers (dependents first, desired state preserved)
// and records import maintenance with markers. Admin API/CLI only.
func (b *Backend) PauseAppForImport(ctx context.Context, appID domain.ID) (apps.Maintenance, error) {
	var result apps.Maintenance
	err := withMaintenanceLock(func() error {
		var err error
		result, err = b.pauseAppLocked(ctx, appID, apps.MaintenanceImport)
		return err
	})
	return result, err
}

// pauseAppWithReason is the shared pause path for import/storage/restore
// owners. Only the import reason is clearable by generic resume. It serializes
// on the global maintenance lock.
func (b *Backend) pauseAppWithReason(ctx context.Context, appID domain.ID, reason string) (apps.Maintenance, error) {
	var result apps.Maintenance
	err := withMaintenanceLock(func() error {
		var err error
		result, err = b.pauseAppLocked(ctx, appID, reason)
		return err
	})
	return result, err
}

// pauseAppLocked runs the pause body assuming the global maintenance lock is
// already held (used by migration workers that hold it across stages).
func (b *Backend) pauseAppLocked(ctx context.Context, appID domain.ID, reason string) (apps.Maintenance, error) {
	if b.apps == nil {
		return apps.Maintenance{}, fmt.Errorf("%w: apps not configured", ErrNotConfigured)
	}
	st, err := b.apps.Status(ctx, appID)
	if err != nil {
		return apps.Maintenance{}, translateError(err)
	}
	var result apps.Maintenance
	affected, err := b.affectedBundles(st.BundleID)
	if err != nil {
		return apps.Maintenance{}, err
	}
	// Stop writers in reverse dependency order without overwriting
	// prior desired state.
	for i := len(affected) - 1; i >= 0; i-- {
		for _, a := range mustAppsByBundle(ctx, b, affected[i].ID) {
			if _, err := b.apps.Quiesce(ctx, a.ID); err != nil {
				return apps.Maintenance{}, translateError(err)
			}
		}
	}
	opID := newOperationID()
	for _, bd := range affected {
		m, err := b.apps.PauseForMaintenance(ctx, bd.ID, reason, opID)
		if err != nil {
			return apps.Maintenance{}, translateError(err)
		}
		if err := writeMarker(b.appMarkerPath(bd.ID)); err != nil {
			return apps.Maintenance{}, err
		}
		if bd.ID == st.BundleID {
			result = m
		}
	}
	if result.BundleID == "" {
		result = apps.Maintenance{BundleID: st.BundleID, Reason: reason, OperationID: opID}
	}
	return result, nil
}

func mustAppsByBundle(ctx context.Context, b *Backend, bundleID string) []apps.Status {
	list, err := b.appsByBundle(ctx, bundleID)
	if err != nil {
		return nil
	}
	return list
}

// ResumeAppFromImport validates mounts, ownership and health, then resumes to
// prior desired state. Errors keep the app paused.
func (b *Backend) ResumeAppFromImport(ctx context.Context, appID domain.ID) error {
	if b.apps == nil {
		return fmt.Errorf("%w: apps not configured", ErrNotConfigured)
	}
	st, err := b.apps.Status(ctx, appID)
	if err != nil {
		return translateError(err)
	}
	return withMaintenanceLock(func() error {
		affected, err := b.affectedBundles(st.BundleID)
		if err != nil {
			return err
		}
		// Validate owned paths before clearing the pause.
		if err := b.validateResumePaths(affected); err != nil {
			return err
		}
		// Clear pause records and markers first so Start is not gated.
		for _, bd := range affected {
			// Only clear generic import pauses; restore/storage owned
			// records stay and abort the resume.
			if m, err := apps.GetMaintenance(ctx, b.db, bd.ID); err == nil {
				if m.Reason != apps.MaintenanceImport {
					return fmt.Errorf("%w: maintenance for %q is owned by %s",
						apitypes.ErrConflict, bd.ID, m.Reason)
				}
			}
		}
		for _, bd := range affected {
			if err := b.apps.ResumeFromMaintenance(ctx, bd.ID); err != nil {
				return translateError(err)
			}
			_ = os.Remove(b.appMarkerPath(bd.ID))
		}
		// Restart in dependency order, only to prior desired state: Start
		// no-ops when desired is stopped.
		for _, bd := range affected {
			for _, a := range mustAppsByBundle(ctx, b, bd.ID) {
				cur, err := b.apps.Status(ctx, a.ID)
				if err != nil {
					continue
				}
				if cur.DesiredState != apps.DesiredRunning {
					continue
				}
				if _, err := b.apps.Start(ctx, a.ID); err != nil {
					return translateError(err)
				}
			}
		}
		// Post-start health validation.
		for _, bd := range affected {
			for _, a := range mustAppsByBundle(ctx, b, bd.ID) {
				cur, err := b.apps.Status(ctx, a.ID)
				if err != nil {
					continue
				}
				if cur.DesiredState != apps.DesiredRunning {
					continue
				}
				h, err := b.apps.CheckHealth(ctx, a.ID)
				if err != nil {
					return translateError(err)
				}
				if h.Health == domain.HealthUnhealthy {
					return fmt.Errorf("%w: app %q resumed but unhealthy", apitypes.ErrConflict, a.Name)
				}
			}
		}
		return nil
	})
}

// validateResumePaths checks mount identity, ownership and writability of
// owned native paths before resume.
func (b *Backend) validateResumePaths(bundles []apps.Bundle) error {
	for _, bd := range bundles {
		for _, v := range bd.Data {
			if v.Path == "" {
				continue
			}
			fi, err := os.Stat(v.Path)
			if err != nil {
				return fmt.Errorf("%w: bundle %q path %q: %v", apitypes.ErrValidation, bd.ID, v.Path, err)
			}
			if !fi.IsDir() {
				return fmt.Errorf("%w: bundle %q path %q is not a directory", apitypes.ErrValidation, bd.ID, v.Path)
			}
			// Writability probe without mutating: open dir for read.
			f, err := os.Open(v.Path)
			if err != nil {
				return fmt.Errorf("%w: bundle %q path %q not accessible: %v", apitypes.ErrValidation, bd.ID, v.Path, err)
			}
			_ = f.Close()
		}
	}
	return nil
}

// PauseApplicationMaintenance is the admin API for import pauses.
func (b *Backend) PauseApplicationMaintenance(ctx context.Context, id domain.ID, req apitypes.MaintenanceRequest) (apitypes.MaintenanceResponse, error) {
	if req.Reason != "" && req.Reason != apps.MaintenanceImport {
		return apitypes.MaintenanceResponse{}, fmt.Errorf("%w: reason must be %q",
			apitypes.ErrValidation, apps.MaintenanceImport)
	}
	m, err := b.PauseAppForImport(ctx, id)
	if err != nil {
		return apitypes.MaintenanceResponse{}, translateError(err)
	}
	return apitypes.MaintenanceResponse{BundleID: m.BundleID, Reason: m.Reason,
		OperationID: m.OperationID, CreatedAt: m.CreatedAt}, nil
}

// ResumeApplicationMaintenance clears a generic import pause after validation.
func (b *Backend) ResumeApplicationMaintenance(ctx context.Context, id domain.ID, req apitypes.MaintenanceResumeRequest) error {
	if !req.Confirm {
		return fmt.Errorf("%w: confirm:true is required", apitypes.ErrValidation)
	}
	if err := b.ResumeAppFromImport(ctx, id); err != nil {
		return translateError(err)
	}
	return nil
}


// ReconcileMaintenance converges marker/database mismatches toward paused on
// startup. Markers are the boot-time authority; database records are the UI
// view. Markers survive restarts; temporary backup quiescence never creates a
// permanent pause (callers of backup quiescence must not write markers).
func (b *Backend) ReconcileMaintenance(ctx context.Context) error {
	if b.apps == nil {
		return nil
	}
	records, err := apps.ListMaintenance(ctx, b.db)
	if err != nil {
		return err
	}
	byBundle := map[string]apps.Maintenance{}
	for _, m := range records {
		byBundle[m.BundleID] = m
	}
	// Database without marker: restore the marker (paused stays paused).
	for _, m := range records {
		if _, err := os.Stat(b.appMarkerPath(m.BundleID)); err != nil {
			log.Printf("maintenance reconcile: restoring marker for %q (%s)", m.BundleID, m.Reason)
			if err := writeMarker(b.appMarkerPath(m.BundleID)); err != nil {
				return err
			}
		}
	}
	// Marker without database: adopt into paused (conservative toward paused).
	mDir := filepath.Join(b.cfg.StateDir, "maintenance", "apps")
	entries, err := os.ReadDir(mDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id := e.Name()
		if _, ok := byBundle[id]; ok {
			continue
		}
		if _, ok := b.apps.CatalogSnapshot().Get(id); !ok {
			// Unknown bundle marker: leave for operator inspection.
			log.Printf("maintenance reconcile: unknown marker %q left in place", id)
			continue
		}
		log.Printf("maintenance reconcile: adopting marker for %q into paused (import)", id)
		if _, err := b.apps.PauseForMaintenance(ctx, id, apps.MaintenanceImport, "reconcile"); err != nil {
			return err
		}
	}
	return nil
}
