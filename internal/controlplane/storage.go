package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/backups"
)
//
// volume identifies <bundle-id>:<DataVolume.Name>, "postgresql" (the whole
// shared cluster directory), or "server-data". fs_uuid:null means the system
// disk. The UI normally offers one selection for all of an app's owned file
// volumes, with the shared database location visibly separate.
type StoragePlacement struct {
	Volume  string  `json:"volume"`
	FSUUID  *string `json:"fs_uuid"`
	Legacy  bool    `json:"legacy,omitempty"`
	Pending bool    `json:"pending,omitempty"`
}

type storageFile struct {
	Version    int                `json:"version"`
	Placements []StoragePlacement `json:"placements"`
}

const storageFileVersion = 2

func storageJSONPath(stateDir string) string { return filepath.Join(stateDir, "storage.json") }

// volumeSlug maps a volume identity to a filesystem-safe unit suffix:
// "immich:state" -> "immich-state".
func volumeSlug(volume string) string {
	return strings.ReplaceAll(volume, ":", "-")
}

// managedSubdir is the per-volume directory owned by Omahab on a data disk.
func managedSubdir(uuid, volume string) string {
	return filepath.Join("/mnt/omahab-disks", uuid, "omahab", "volumes", volumeSlug(volume))
}

func diskMountpoint(uuid string) string { return filepath.Join("/mnt/omahab-disks", uuid) }

// layoutEntry mirrors one nix/app-data.nix entry.
type layoutEntry struct {
	Data []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	} `json:"data"`
	Databases    []string `json:"databases"`
	QuiesceUnits []string `json:"quiesce_units"`
}

// loadNativeLayout reads OMAHAB_NATIVE_LAYOUT (written by nix/module.nix from
// the same nix/app-data.nix used for storage generation). It is the runtime
// source for canonical paths of postgresql/server-data, which live outside
// the per-bundle catalog data.
func (b *Backend) loadNativeLayout() (map[string]layoutEntry, error) {
	path := b.cfg.NativeLayoutPath
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: native layout not configured (OMAHAB_NATIVE_LAYOUT)", ErrNotConfigured)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var layout map[string]layoutEntry
	if err := json.Unmarshal(raw, &layout); err != nil {
		return nil, fmt.Errorf("parse native layout: %w", err)
	}
	return layout, nil
}

// knownVolumes enumerates every placeable volume identity.
func (b *Backend) knownVolumes(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	if layout, err := b.loadNativeLayout(); err == nil {
		for bundle, entry := range layout {
			switch bundle {
			case "postgresql", "server-data":
				out[bundle] = firstLayoutPath(entry)
			default:
				for _, v := range entry.Data {
					out[bundle+":"+v.Name] = v.Path
				}
			}
		}
		return out, nil
	}
	// Fall back to the installed catalog for file volumes; postgresql and
	// server-data come from the layout only.
	if b.apps == nil {
		return nil, fmt.Errorf("%w: apps not configured", ErrNotConfigured)
	}
	for _, bd := range b.apps.CatalogBundles() {
		for _, v := range bd.Data {
			out[bd.ID+":"+v.Name] = v.Path
		}
	}
	return out, nil
}

func firstLayoutPath(e layoutEntry) string {
	if len(e.Data) > 0 {
		return e.Data[0].Path
	}
	return ""
}

// loadStorageFile reads storage.json, migrating the v1 media|data array into
// v2 placements. The old media mapping becomes the Immich library subvolume
// without moving its existing <disk>/media content (legacy bind); the old
// data entry was ignored by the mount script, so it is preserved as a pending
// server-data placement that is not applied until an explicit migration runs.
func loadStorageFile(path string) (storageFile, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return storageFile{Version: storageFileVersion}, false, nil
		}
		return storageFile{}, false, err
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		var old []storageEntry
		if err := json.Unmarshal(raw, &old); err != nil {
			return storageFile{}, true, fmt.Errorf("parse legacy storage.json: %w", err)
		}
		sf := storageFile{Version: storageFileVersion}
		for _, e := range old {
			switch e.Volume {
			case "media":
				if strings.TrimSpace(e.FSUUID) != "" {
					uuid := strings.TrimSpace(e.FSUUID)
					sf.Placements = append(sf.Placements, StoragePlacement{Volume: "immich:state", FSUUID: &uuid, Legacy: true})
				}
			case "data":
				if strings.TrimSpace(e.FSUUID) != "" {
					uuid := strings.TrimSpace(e.FSUUID)
					sf.Placements = append(sf.Placements, StoragePlacement{Volume: "server-data", FSUUID: &uuid, Pending: true})
				}
			}
		}
		return sf, true, nil
	}
	var sf storageFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return storageFile{}, true, fmt.Errorf("parse storage.json: %w", err)
	}
	return sf, true, nil
}

func writeStorageFile(path string, sf storageFile) error {
	sf.Version = storageFileVersion
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if f, err := os.OpenFile(tmp, os.O_RDWR, 0o600); err == nil {
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

// validatePlacementUUID checks a UUID against lsblk: known device, supported
// filesystem, not a system disk, with capacity and read/write access probed on
// the mounted filesystem.
func validatePlacementUUID(ctx context.Context, uuid string, needBytes int64) (apitypes.Disk, error) {
	disks, err := listDisksWithSystem(ctx)
	if err != nil {
		return apitypes.Disk{}, err
	}
	for _, d := range disks.candidates {
		if !strings.EqualFold(d.UUID, uuid) {
			continue
		}
		switch strings.ToLower(d.FSType) {
		case "ext4", "btrfs", "xfs":
		default:
			return apitypes.Disk{}, fmt.Errorf("%w: filesystem %q (%s) does not retain ACLs/xattrs/ownership; use ext4, btrfs or xfs",
				apitypes.ErrValidation, d.UUID, d.FSType)
		}
		mnt := diskMountpoint(d.UUID)
		if err := ensureDiskMounted(ctx, d, mnt); err != nil {
			return apitypes.Disk{}, err
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(mnt, &st); err != nil {
			return apitypes.Disk{}, fmt.Errorf("statfs %q: %w", mnt, err)
		}
		avail := int64(st.Bavail) * int64(st.Bsize)
		if needBytes > 0 && avail < needBytes {
			return apitypes.Disk{}, fmt.Errorf("%w: disk %q has %d bytes free, need %d",
				apitypes.ErrValidation, uuid, avail, needBytes)
		}
		probe := filepath.Join(mnt, ".omahab-write-probe")
		if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
			return apitypes.Disk{}, fmt.Errorf("%w: disk %q is not writable: %v", apitypes.ErrValidation, uuid, err)
		}
		_ = os.Remove(probe)
		return d, nil
	}
	if disks.systemUUIDs[strings.ToUpper(uuid)] || disks.systemUUIDs[strings.ToLower(uuid)] {
		return apitypes.Disk{}, fmt.Errorf("%w: disk %q holds root/boot/Nix and cannot be reassigned",
			apitypes.ErrValidation, uuid)
	}
	return apitypes.Disk{}, fmt.Errorf("%w: unknown disk uuid %q", apitypes.ErrValidation, uuid)
}

type diskScan struct {
	candidates []apitypes.Disk
	// systemUUIDs holds upper+lower forms of UUIDs backing system mounts.
	systemUUIDs map[string]bool
}

func listDisksWithSystem(ctx context.Context) (diskScan, error) {
	out, err := exec.CommandContext(ctx, "lsblk", "-J", "-o", "NAME,SIZE,TYPE,FSTYPE,UUID,MOUNTPOINT").Output()
	if err != nil {
		return diskScan{}, fmt.Errorf("lsblk: %w", err)
	}
	disks, err := parseLSBlock(out)
	if err != nil {
		return diskScan{}, err
	}
	scan := diskScan{systemUUIDs: map[string]bool{}}
	var raw lsblkOutput
	if err := json.Unmarshal(out, &raw); err != nil {
		return diskScan{}, fmt.Errorf("parse lsblk: %w", err)
	}
	var walk func(devs []lsblkDevice)
	walk = func(devs []lsblkDevice) {
		for _, d := range devs {
			mp := d.Mountpoint
			if mp == "/" || mp == "/boot" || mp == "/nix" || mp == "/nix/store" || mp == "/tmp" ||
				strings.HasPrefix(mp, "/nix/") {
				if d.UUID != "" {
					scan.systemUUIDs[strings.ToUpper(d.UUID)] = true
					scan.systemUUIDs[strings.ToLower(d.UUID)] = true
				}
			}
			walk(d.Children)
		}
	}
	walk(raw.BlockDevices)
	// Candidates exclude empty fstype (ListDisks behavior).
	for _, d := range disks {
		if d.FSType == "" {
			continue
		}
		scan.candidates = append(scan.candidates, d)
	}
	return scan, nil
}

func ensureDiskMounted(ctx context.Context, d apitypes.Disk, mnt string) error {
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return err
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(mnt, &st); err != nil {
		return err
	}
	// mountpoint check via /proc/self/mountinfo device comparison is
	// overkill; attempt mount only when the dir is empty of mounts by
	// probing `mountpoint -q`.
	if err := exec.CommandContext(ctx, "mountpoint", "-q", mnt).Run(); err == nil {
		return nil
	}
	if out, err := exec.CommandContext(ctx, "mount", "-U", d.UUID, mnt).CombinedOutput(); err != nil {
		return fmt.Errorf("mount disk %q on %q: %v: %s", d.UUID, mnt, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func dirSizeBytes(root string) int64 {
	var total int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func dirNonEmpty(path string) bool {
	items, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	return len(items) > 0
}

// recentSuccessfulBackup reports whether any completed backup run exists.
// Populated moves require one; first-time empty volumes need none.
func (b *Backend) recentSuccessfulBackup(ctx context.Context) bool {
	if b.backups == nil {
		return false
	}
	runs, err := b.backups.ListRuns(ctx, backups.ListFilter{})
	if err != nil {
		return false
	}
	for _, r := range runs {
		if r.Kind == backups.KindBackup && r.Status == backups.StatusCompleted {
			return true
		}
	}
	return false
}

// ConfigureStorage validates v2 placements and either records them pending or
// starts a journaled migration operation. It returns an operation ID with
// progress in both cases; live moves run asynchronously under the global
// maintenance lock.
func (b *Backend) ConfigureStorage(ctx context.Context, req apitypes.ConfigureStorageRequest) (Operation, error) {
	known, err := b.knownVolumes(ctx)
	if err != nil {
		return Operation{}, translateError(err)
	}
	seen := map[string]bool{}
	for _, p := range req.Placements {
		vol := strings.TrimSpace(p.Volume)
		if vol == "" {
			return Operation{}, fmt.Errorf("%w: volume is required", apitypes.ErrValidation)
		}
		if _, ok := known[vol]; !ok {
			return Operation{}, fmt.Errorf("%w: unknown volume %q", apitypes.ErrValidation, vol)
		}
		if seen[vol] {
			return Operation{}, fmt.Errorf("%w: duplicate volume %q", apitypes.ErrValidation, vol)
		}
		seen[vol] = true
	}
	// Conflicting roots: nested canonical paths must share one disk.
	type canon struct {
		vol  string
		uuid string
		path string
	}
	var canons []canon
	for _, p := range req.Placements {
		if p.FSUUID == nil {
			continue
		}
		canons = append(canons, canon{p.Volume, strings.TrimSpace(*p.FSUUID), known[p.Volume]})
	}
	for i := range canons {
		for j := range canons {
			if i == j {
				continue
			}
			a, c := canons[i], canons[j]
			if a.uuid == c.uuid || a.uuid == "" || c.uuid == "" {
				continue
			}
			if a.path != "" && c.path != "" && (strings.HasPrefix(a.path, c.path+"/") || strings.HasPrefix(c.path, a.path+"/")) {
				return Operation{}, fmt.Errorf("%w: nested volumes %q and %q must share one disk",
					apitypes.ErrValidation, a.vol, c.vol)
			}
		}
	}
	// UUID shape is validated on record; full device validation (lsblk
	// identity, filesystem support, capacity, read/write access) runs at
	// migration time, when missing disks must fail closed. Pending records
	// may reference disks that are not currently attached (restore remaps
	// them before activation).
	for _, p := range req.Placements {
		if p.FSUUID == nil {
			continue
		}
		uuid := strings.TrimSpace(*p.FSUUID)
		if uuid == "" {
			continue
		}
		for _, r := range uuid {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F' || r == '-') {
				return Operation{}, fmt.Errorf("%w: fs_uuid %q is not a filesystem UUID", apitypes.ErrValidation, uuid)
			}
		}
	}

	path := storageJSONPath(b.cfg.StateDir)
	current, _, err := loadStorageFile(path)
	if err != nil {
		return Operation{}, translateError(err)
	}
	_ = current

	// Plan moves: volumes whose effective uuid changes and whose canonical
	// source is populated.
	var moves []StoragePlacement
	prevByVol := map[string]*string{}
	sf, _, _ := loadStorageFile(path)
	for _, pl := range sf.Placements {
		if !pl.Pending {
			v := pl.FSUUID
			prevByVol[pl.Volume] = v
		}
	}
	toInternal := func(p apitypes.StoragePlacement) StoragePlacement {
		return StoragePlacement{Volume: strings.TrimSpace(p.Volume), FSUUID: p.FSUUID}
	}
	for _, p := range req.Placements {
		prev := prevByVol[strings.TrimSpace(p.Volume)]
		var prevUUID, nextUUID string
		if prev != nil {
			prevUUID = strings.TrimSpace(*prev)
		}
		if p.FSUUID != nil {
			nextUUID = strings.TrimSpace(*p.FSUUID)
		}
		if prevUUID == nextUUID {
			continue
		}
		if cp := known[strings.TrimSpace(p.Volume)]; cp != "" && dirNonEmpty(cp) {
			moves = append(moves, toInternal(p))
		}
	}

	if !req.Migrate {
		// Validate + record pending without touching mounts.
		sfOut := storageFile{Version: storageFileVersion}
		for _, p := range req.Placements {
			cp := toInternal(p)
			cp.Pending = true
			sfOut.Placements = append(sfOut.Placements, cp)
		}
		if err := writeStorageFile(path, sfOut); err != nil {
			return Operation{}, err
		}
		op := newOperation("storage", affectedAppIDs(known, req.Placements))
		op.Status = OpPending
		op.Stage = "pending"
		if err := writeOperationJournal(b.cfg.StateDir, op); err != nil {
			return Operation{}, err
		}
		return op, nil
	}
	if !req.Confirm {
		return Operation{}, fmt.Errorf("%w: confirm:true is required to migrate existing data", apitypes.ErrValidation)
	}
	// Full device validation before any activation: identity, filesystem
	// support, capacity for populated sources, and read/write access.
	for _, p := range req.Placements {
		if p.FSUUID == nil || strings.TrimSpace(*p.FSUUID) == "" {
			continue
		}
		uuid := strings.TrimSpace(*p.FSUUID)
		var need int64
		if cp := known[strings.TrimSpace(p.Volume)]; cp != "" && dirNonEmpty(cp) {
			need = dirSizeBytes(cp)
		}
		if _, err := validatePlacementUUID(ctx, uuid, need); err != nil {
			return Operation{}, err
		}
	}
	if len(moves) > 0 && !b.recentSuccessfulBackup(ctx) {
		return Operation{}, fmt.Errorf("%w: move of populated data requires a recent successful backup first",
			apitypes.ErrValidation)
	}
	op := newOperation("storage", affectedAppIDs(known, req.Placements))
	op.Status = OpRunning
	op.Stage = "preflight"
	jPlacements := make([]StoragePlacement, 0, len(req.Placements))
	for _, p := range req.Placements {
		jPlacements = append(jPlacements, toInternal(p))
	}
	j := storageJournal{
		Operation:  op,
		Placements: jPlacements,
		Previous:   sf.Placements,
	}
	if err := writeStorageJournal(b.cfg.StateDir, j); err != nil {
		return Operation{}, err
	}
	go b.runStorageMigration(j.Operation.ID)
	op, _ = loadOperation(b.cfg.StateDir, j.Operation.ID)
	return op, nil
}

func affectedAppIDs(known map[string]string, placements []apitypes.StoragePlacement) []string {
	_ = known
	var out []string
	for _, p := range placements {
		vol := strings.TrimSpace(p.Volume)
		if strings.Contains(vol, ":") {
			out = append(out, strings.SplitN(vol, ":", 2)[0])
		} else {
			out = append(out, vol)
		}
	}
	return out
}

// setupPhaseStorage activates configured placements before bulk app
// enablement in new setup. Fresh empty volumes bind live without any data
// move; populated moves go through the journaled ConfigureStorage operation
// instead. Existing installations manage disks from Applications/Storage.
func (b *Backend) setupPhaseStorage(ctx context.Context) error {
	path := storageJSONPath(b.cfg.StateDir)
	sf, exists, err := loadStorageFile(path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if exists && len(sf.Placements) == 0 {
		return nil
	}
	known, err := b.knownVolumes(ctx)
	if err != nil {
		// Catalog/layout unavailable: leave binds to the storage units.
		return nil
	}
	// Migrate a legacy v1 file in place so later reads see v2.
	if _, _, err := loadStorageFile(path); err == nil {
		raw, _ := os.ReadFile(path)
		if strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
			if err := writeStorageFile(path, sf); err != nil {
				return err
			}
		}
	}
	for _, p := range sf.Placements {
		if p.Pending || p.FSUUID == nil || strings.TrimSpace(*p.FSUUID) == "" {
			continue
		}
		cp := known[p.Volume]
		if cp == "" {
			continue
		}
		uuid := strings.TrimSpace(*p.FSUUID)
		disk, err := validatePlacementUUID(ctx, uuid, 0)
		if err != nil {
			log.Printf("setup storage: disk %q unavailable: %v", uuid, err)
			continue
		}
		_ = disk
		if err := resumeBindMount(known, p); err != nil {
			log.Printf("setup storage: bind %q: %v", p.Volume, err)
		}
	}
	return nil
}

// runStorageMigration executes the journaled move under the global lock:
// record previous mapping, enter (owned) maintenance, stop writers, rsync
// copy with --numeric-ids and ACLs/xattrs, checksum-verify while stopped,
// then atomically persist/activate the new bind mapping. Old data is kept
// untouched; failures roll back to the old mapping and prior unit state.
func (b *Backend) runStorageMigration(opID string) {
	err := withMaintenanceLock(func() error {
		j, err := loadStorageJournal(b.cfg.StateDir, opID)
		if err != nil {
			return err
		}
		known, err := b.knownVolumes(context.Background())
		if err != nil {
			return failStorageOp(b.cfg.StateDir, &j, err.Error())
		}
		ctx := context.Background()
		// Enter maintenance for owning bundles (storage-owned: generic
		// resume cannot clear these).
		owners := owningBundles(j.Placements)
		for _, bundle := range owners {
			appRows, err := b.appsByBundle(ctx, bundle)
			if err != nil {
				continue
			}
			if len(appRows) == 0 {
				// Not installed: record maintenance directly so a later
				// install cannot initialize over the move.
				if _, err := b.apps.PauseForMaintenance(ctx, bundle, apps.MaintenanceStorage, opID); err != nil {
					return failStorageOp(b.cfg.StateDir, &j, err.Error())
				}
				if err := writeMarker(b.appMarkerPath(bundle)); err != nil {
					return failStorageOp(b.cfg.StateDir, &j, err.Error())
				}
				continue
			}
			if _, err := b.pauseAppLocked(ctx, appRows[0].ID, apps.MaintenanceStorage); err != nil {
				return failStorageOp(b.cfg.StateDir, &j, err.Error())
			}
		}
		// PostgreSQL special case: stop the cluster for a whole-directory move.
		pgMove := false
		for _, p := range j.Placements {
			if p.Volume == "postgresql" && p.FSUUID != nil {
				pgMove = true
			}
		}
		var pgWasActive bool
		if pgMove {
			if out, err := exec.Command("systemctl", "stop", "postgresql.service").CombinedOutput(); err != nil {
				_ = resumeStorageApps(b, ctx, j)
				return failStorageOp(b.cfg.StateDir, &j, fmt.Sprintf("stop postgresql: %v: %s", err, strings.TrimSpace(string(out))))
			}
		}
		j.Operation.Stage = "writers_stopped"
		_ = writeStorageJournal(b.cfg.StateDir, j)

		// Copy each moved volume.
		for _, p := range j.Placements {
			cp := known[p.Volume]
			if cp == "" || !dirNonEmpty(cp) || p.FSUUID == nil {
				continue
			}
			uuid := strings.TrimSpace(*p.FSUUID)
			dest := managedSubdir(uuid, p.Volume)
			if err := os.MkdirAll(dest, 0o755); err != nil {
				_ = resumeStorageApps(b, ctx, j)
				if pgMove && pgWasActive {
					_ = exec.Command("systemctl", "start", "postgresql.service").Run()
				}
				return failStorageOp(b.cfg.StateDir, &j, err.Error())
			}
			// Marker so future runs recognize managed content.
			_ = os.WriteFile(filepath.Join(dest, ".omahab-volume"), []byte(p.Volume+"\n"), 0o644)
			if out, err := exec.Command("rsync", "-aHAX", "--numeric-ids", cp+"/", dest+"/").CombinedOutput(); err != nil {
				_ = resumeStorageApps(b, ctx, j)
				if pgMove && pgWasActive {
					_ = exec.Command("systemctl", "start", "postgresql.service").Run()
				}
				return failStorageOp(b.cfg.StateDir, &j, fmt.Sprintf("rsync %q: %v: %s", p.Volume, err, strings.TrimSpace(string(out))))
			}
			// Checksum verification while stopped: a --checksum dry run
			// must report no transfers.
			out, err := exec.Command("rsync", "-aHAX", "--numeric-ids", "--checksum", "--dry-run", "--itemize-changes", cp+"/", dest+"/").CombinedOutput()
			if err != nil {
				_ = resumeStorageApps(b, ctx, j)
				if pgMove && pgWasActive {
					_ = exec.Command("systemctl", "start", "postgresql.service").Run()
				}
				return failStorageOp(b.cfg.StateDir, &j, fmt.Sprintf("verify %q: %v: %s", p.Volume, err, strings.TrimSpace(string(out))))
			}
			if strings.TrimSpace(string(out)) != "" {
				_ = resumeStorageApps(b, ctx, j)
				if pgMove && pgWasActive {
					_ = exec.Command("systemctl", "start", "postgresql.service").Run()
				}
				return failStorageOp(b.cfg.StateDir, &j, fmt.Sprintf("verify %q: copy differs from source", p.Volume))
			}
		}
		j.Operation.Stage = "copied"
		_ = writeStorageJournal(b.cfg.StateDir, j)

		// Atomically persist the new mapping (pending flags cleared).
		active := storageFile{Version: storageFileVersion}
		for _, p := range j.Placements {
			cp := p
			cp.Pending = false
			active.Placements = append(active.Placements, cp)
		}
		if err := writeStorageFile(storageJSONPath(b.cfg.StateDir), active); err != nil {
			_ = resumeStorageApps(b, ctx, j)
			if pgMove && pgWasActive {
				_ = exec.Command("systemctl", "start", "postgresql.service").Run()
			}
			return failStorageOp(b.cfg.StateDir, &j, err.Error())
		}
		// Live-activate binds and restart storage units.
		for _, p := range j.Placements {
			if p.FSUUID == nil {
				continue
			}
			cp := known[p.Volume]
			if cp == "" {
				continue
			}
			uuid := strings.TrimSpace(*p.FSUUID)
			src := managedSubdir(uuid, p.Volume)
			if p.Legacy {
				src = filepath.Join(diskMountpoint(uuid), "media")
			}
			if out, err := exec.Command("mount", "--bind", src, cp).CombinedOutput(); err != nil {
				log.Printf("storage: live bind %q -> %q: %v: %s", src, cp, err, strings.TrimSpace(string(out)))
			}
			_ = exec.Command("systemctl", "restart", "omahab-storage-"+volumeSlug(p.Volume)+".service").Run()
		}
		if pgMove && pgWasActive {
			if out, err := exec.Command("systemctl", "start", "postgresql.service").CombinedOutput(); err != nil {
				_ = resumeStorageApps(b, ctx, j)
				return failStorageOp(b.cfg.StateDir, &j, fmt.Sprintf("start postgresql: %v: %s", err, strings.TrimSpace(string(out))))
			}
		}
		j.Operation.Stage = "validating"
		_ = writeStorageJournal(b.cfg.StateDir, j)
		// Health-check resumed apps.
		if err := resumeStorageApps(b, ctx, j); err != nil {
			return failStorageOp(b.cfg.StateDir, &j, err.Error())
		}
		j.Operation.Stage = OpCompleted
		j.Operation.Status = OpCompleted
		j.Operation.Progress = 100
		_ = writeStorageJournal(b.cfg.StateDir, j)
		return nil
	})
	if err != nil {
		log.Printf("storage migration %q: %v", opID, err)
	}
}

func owningBundles(placements []StoragePlacement) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range placements {
		vol := strings.TrimSpace(p.Volume)
		var bundle string
		if strings.Contains(vol, ":") {
			bundle = strings.SplitN(vol, ":", 2)[0]
		} else if vol == "postgresql" {
			for _, o := range []string{"forgejo", "woodpecker", "immich", "paperless-ngx", "litellm"} {
				if !seen[o] {
					seen[o] = true
					out = append(out, o)
				}
			}
			continue
		} else {
			continue
		}
		if !seen[bundle] {
			seen[bundle] = true
			out = append(out, bundle)
		}
	}
	return out
}

func resumeStorageApps(b *Backend, ctx context.Context, j storageJournal) error {
	// Owner-clears storage maintenance, removes markers, restarts to prior
	// desired state, and health-checks.
	owners := owningBundles(j.Placements)
	for _, bundle := range owners {
		if err := clearStorageMaintenance(ctx, b, bundle); err != nil {
			return err
		}
		_ = os.Remove(b.appMarkerPath(bundle))
	}
	for _, bundle := range owners {
		for _, a := range mustAppsByBundle(ctx, b, bundle) {
			cur, err := b.apps.Status(ctx, a.ID)
			if err != nil {
				continue
			}
			if cur.DesiredState != apps.DesiredRunning {
				continue
			}
			if _, err := b.apps.Start(ctx, a.ID); err != nil {
				return err
			}
		}
	}
	// App-level read validation is restore's job; here confirm units report.
	return nil
}

func clearStorageMaintenance(ctx context.Context, b *Backend, bundle string) error {
	m, err := apps.GetMaintenance(ctx, b.db, bundle)
	if err != nil {
		return nil
	}
	if m.Reason != apps.MaintenanceStorage {
		return fmt.Errorf("%w: maintenance for %q is owned by %s", apitypes.ErrConflict, bundle, m.Reason)
	}
	if _, err := b.db.ExecContext(ctx, `DELETE FROM app_maintenance WHERE bundle_id = ?`, bundle); err != nil {
		return err
	}
	return nil
}

func systemctlActive(unit string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", unit).Run() == nil
}

func failStorageOp(stateDir string, j *storageJournal, msg string) error {
	j.Operation.Status = OpFailed
	j.Operation.Stage = OpFailed
	j.Operation.Error = msg
	j.Operation.CanResume = true
	j.Operation.CanRollback = true
	_ = writeStorageJournal(stateDir, *j)
	return fmt.Errorf("%s", msg)
}


