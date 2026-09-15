package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

// ReconcileInput scopes one reconciliation pass.
type ReconcileInput struct {
	// Paused skips everything while the syncthing bundle is maintained.
	Paused bool
	// Unavailable lists canonical paths whose selected disk bind is
	// inactive. Folders beneath them are never recreated on the root
	// filesystem; they stay pending until the disk returns.
	Unavailable []string
}

// ReconcileResult distinguishes configured from pending per folder. No
// silent "done" is ever reported before Syncthing accepts the object.
type ReconcileResult struct {
	FolderID    string
	WireID      string
	Status      string // configured|pending
	Error       string
	RescanReady bool
}

func underUnavailable(path string, unavailable []string) bool {
	clean := filepath.Clean(path)
	for _, u := range unavailable {
		uc := filepath.Clean(strings.TrimSpace(u))
		if uc == "" || uc == "." {
			continue
		}
		if clean == uc || strings.HasPrefix(clean, uc+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "401") || strings.Contains(msg, "403") ||
		strings.Contains(msg, "unauthorized") || strings.Contains(msg, "forbidden")
}

func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	if isAuthError(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{"connection refused", "no such host", "timeout", "connection reset", "network is unreachable", "eof"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	var timeoutErr interface{ Timeout() bool }
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return true
	}
	return false
}

// SetSyncthingID adopts a wire ID into a folder record. Internal database
// IDs and foreign keys never change.
func (s *Service) SetSyncthingID(ctx context.Context, folderID, wireID string) error {
	wireID = strings.TrimSpace(wireID)
	if wireID == "" {
		return fmt.Errorf("%w: wire id is required", ErrValidation)
	}
	if _, err := s.Get(ctx, folderID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE sync_folders SET syncthing_id = ?, updated_at = ? WHERE id = ?`,
		wireID, time.Now().UTC().Format(time.RFC3339Nano), folderID)
	return err
}

// LinkCompanionDevice records which companion device owns an enrollment so
// revocation removes only that device's managed sharing.
func (s *Service) LinkCompanionDevice(ctx context.Context, folderID, deviceID, companionID string) error {
	companionID = strings.TrimSpace(companionID)
	if companionID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE sync_devices SET companion_id = ? WHERE folder_id = ? AND device_id = ?`,
		companionID, folderID, strings.TrimSpace(deviceID))
	return err
}

// FoldersForCompanion lists folders sharing with one companion device.
func (s *Service) FoldersForCompanion(ctx context.Context, companionID string) ([]*domain.SyncFolder, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, server_path, share_with_ai, health, syncthing_id, created_at, updated_at
		 FROM sync_folders WHERE id IN (SELECT folder_id FROM sync_devices WHERE companion_id = ?) ORDER BY name ASC`,
		strings.TrimSpace(companionID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.SyncFolder
	for rows.Next() {
		f, err := scanFolder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// RemoveCompanionDevice unenrolls every Syncthing device owned by one
// companion. Callers re-run Reconcile so managed folders drop only that
// device from sharing; other devices and GUI folders are untouched.
func (s *Service) RemoveCompanionDevice(ctx context.Context, companionID string) ([]string, error) {
	companionID = strings.TrimSpace(companionID)
	if companionID == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT folder_id, device_id FROM sync_devices WHERE companion_id = ?`, companionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type enrollment struct{ folder, device string }
	var enrollments []enrollment
	for rows.Next() {
		var e enrollment
		if err := rows.Scan(&e.folder, &e.device); err != nil {
			return nil, err
		}
		enrollments = append(enrollments, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var affected []string
	for _, e := range enrollments {
		_ = s.RemoveDevice(ctx, e.folder, e.device)
		affected = append(affected, e.folder)
	}
	return dedupeStrings(affected), nil
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func ensureDirOwned(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	if u, err := user.Lookup("syncthing"); err == nil {
		_ = os.Chown(path, atoiOr(u.Uid, -1), atoiOr(u.Gid, -1))
	}
	return nil
}

func atoiOr(s string, fallback int) int {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return fallback
	}
	return n
}

func (s *Service) setFolderHealth(ctx context.Context, folderID string, h domain.Health) {
	_, _ = s.db.ExecContext(ctx,
		`UPDATE sync_folders SET health = ?, updated_at = ? WHERE id = ?`,
		string(h), time.Now().UTC().Format(time.RFC3339Nano), folderID)
}

func (s *Service) emitReconcileEvent(ctx context.Context, folder *domain.SyncFolder, err error, transient bool) {
	severity := "error"
	if transient {
		severity = "warning"
	}
	_ = s.sink.Emit(ctx, domain.Event{
		ID:         domain.ID(store.NewID()),
		Type:       "syncthing.reconcile_failed",
		Severity:   severity,
		ResourceID: folder.ID,
		Message:    fmt.Sprintf("syncthing reconcile for folder %q failed: %v", folder.Name, err),
		Data: map[string]any{
			"folder":      folder.Name,
			"server_path": folder.ServerPath,
			"wire_id":     folder.SyncthingID,
			"transient":   transient,
		},
		CreatedAt: s.now().UTC(),
	})
}

// Reconcile converges Syncthing configuration toward the database records:
// defaults, new folders and device enrollment/revocation immediately, then
// on daemon startup and polling. DB creation survives transient Syncthing
// downtime with pending status and last error; retries converge
// idempotently. Permission/401/403/configuration failures surface on folder
// health and events (Sync and Doctor).
func (s *Service) Reconcile(ctx context.Context, in ReconcileInput) ([]ReconcileResult, error) {
	if in.Paused {
		return nil, nil
	}
	folders, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []ReconcileResult
	for _, f := range folders {
		out = append(out, s.reconcileFolder(ctx, f, in))
	}
	return out, nil
}

func (s *Service) reconcileFolder(ctx context.Context, f *domain.SyncFolder, in ReconcileInput) ReconcileResult {
	res := ReconcileResult{FolderID: string(f.ID), WireID: f.SyncthingID}
	if underUnavailable(f.ServerPath, in.Unavailable) {
		res.Status = "pending"
		res.Error = "selected disk unavailable; folder stays pending until the disk returns"
		s.setFolderHealth(ctx, string(f.ID), domain.HealthUnknown)
		return res
	}
	if err := ensureDirOwned(f.ServerPath); err != nil {
		res.Status = "pending"
		res.Error = err.Error()
		s.setFolderHealth(ctx, string(f.ID), domain.HealthUnknown)
		s.emitReconcileEvent(ctx, f, err, false)
		return res
	}
	wireID := strings.TrimSpace(f.SyncthingID)
	if wireID == "" {
		wireID = string(f.ID)
	}
	// Adopt a unique same-path existing Syncthing ID when the current wire
	// ID is not configured there; otherwise create the server-ID folder.
	// Name-only/different-path or ambiguous matches stay visible conflicts.
	if _, err := s.client.GetFolder(ctx, wireID); err != nil {
		if adopted, ok := s.adoptSamePathID(ctx, f); ok {
			wireID = adopted
			if err := s.SetSyncthingID(ctx, string(f.ID), adopted); err != nil {
				res.Status = "pending"
				res.Error = err.Error()
				return res
			}
			res.RescanReady = true
		}
	}
	res.WireID = wireID
	devices, err := s.ListDevices(ctx, string(f.ID))
	if err != nil {
		res.Status = "pending"
		res.Error = err.Error()
		return res
	}
	var deviceIDs []string
	for _, d := range devices {
		// Sharing uses enrolled device IDs, never all devices.
		deviceIDs = append(deviceIDs, d.DeviceID)
	}
	spec := FolderSpec{
		ID:        wireID,
		Label:     f.Name,
		Path:      f.ServerPath,
		Type:      "sendreceive",
		DeviceIDs: deviceIDs,
	}
	if err := s.client.EnsureFolder(ctx, spec); err != nil {
		res.Status = "pending"
		res.Error = err.Error()
		if isTransientError(err) {
			s.setFolderHealth(ctx, string(f.ID), domain.HealthUnknown)
		} else {
			s.setFolderHealth(ctx, string(f.ID), domain.HealthDegraded)
		}
		s.emitReconcileEvent(ctx, f, err, isTransientError(err))
		return res
	}
	// Ensure enrolled devices exist as device entries (names preserved).
	for _, d := range devices {
		if err := s.client.EnsureDevice(ctx, DeviceSpec{ID: d.DeviceID, Name: d.DeviceName}); err != nil {
			res.Status = "pending"
			res.Error = err.Error()
			s.emitReconcileEvent(ctx, f, err, isTransientError(err))
			return res
		}
	}
	res.Status = "configured"
	s.setFolderHealth(ctx, string(f.ID), domain.HealthUnknown)
	return res
}

// adoptSamePathID returns the unique same-path Syncthing folder ID, if
// exactly one non-managed folder shares the path.
func (s *Service) adoptSamePathID(ctx context.Context, f *domain.SyncFolder) (string, bool) {
	listed, err := s.client.ListFolders(ctx)
	if err != nil {
		return "", false
	}
	var match string
	count := 0
	for _, fi := range listed {
		if filepath.Clean(fi.Path) != filepath.Clean(f.ServerPath) {
			continue
		}
		if strings.TrimSpace(fi.ID) == string(f.ID) || strings.TrimSpace(fi.ID) == strings.TrimSpace(f.SyncthingID) {
			continue
		}
		count++
		match = strings.TrimSpace(fi.ID)
	}
	if count == 1 && match != "" {
		return match, true
	}
	return "", false
}

