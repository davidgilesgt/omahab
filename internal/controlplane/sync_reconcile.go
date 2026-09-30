package controlplane

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/syncer"
)

// ReconcileSyncFolders converges Syncthing configuration toward the database
// records: defaults, new folders and device enrollment/revocation
// immediately, then on daemon startup and polling. Maintenance and
// missing-disk checks run before reconciliation; folders on an absent
// selected mount are never recreated on the root filesystem.
func (b *Backend) ReconcileSyncFolders(ctx context.Context) ([]syncer.ReconcileResult, error) {
	if b.syncer == nil {
		return nil, fmt.Errorf("%w: syncer not configured", ErrNotConfigured)
	}
	paused := false
	if b.apps != nil {
		if _, err := apps.GetMaintenance(ctx, b.db, "syncthing"); err == nil {
			paused = true
		}
	}
	var unavailable []string
	if sf, _, err := loadStorageFile(storageJSONPath(b.cfg.StateDir)); err == nil {
		known, _ := b.knownVolumes(ctx)
		for _, pl := range sf.Placements {
			if pl.FSUUID == nil || strings.TrimSpace(*pl.FSUUID) == "" {
				continue
			}
			if !strings.HasPrefix(pl.Volume, "syncthing:") {
				continue
			}
			if cp := known[pl.Volume]; cp != "" {
				if err := exec.CommandContext(ctx, "mountpoint", "-q", cp).Run(); err != nil {
					unavailable = append(unavailable, cp)
				}
			}
		}
	}
	results, err := b.syncer.Reconcile(ctx, syncer.ReconcileInput{Paused: paused, Unavailable: unavailable})
	if err != nil {
		return nil, translateError(err)
	}
	for _, r := range results {
		if r.Status == "pending" {
			log.Printf("syncthing reconcile: folder %q (%s): %s", r.FolderID, r.Status, r.Error)
		}
	}
	return results, nil
}
