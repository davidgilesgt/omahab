package apps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Maintenance reasons. Markers are the boot-time authority; database records
// are the UI view. Temporary backup quiescence never creates a pause.
const (
	MaintenanceImport  = "import"
	MaintenanceRestore = "restore"
	MaintenanceStorage = "storage"
)

// ErrMaintained reports that an app is paused for maintenance. Callers map it
// to a structured conflict with an operation link.
var ErrMaintained = errors.New("app is in maintenance")

// MaintenanceError carries the bundle, reason and operation link for a
// maintenance conflict. It wraps ErrMaintained.
type MaintenanceError struct {
	BundleID    string
	Reason      string
	OperationID string
}

func (e *MaintenanceError) Error() string {
	if e.OperationID != "" {
		return fmt.Sprintf("app %q is in maintenance (%s, operation %s)", e.BundleID, e.Reason, e.OperationID)
	}
	return fmt.Sprintf("app %q is in maintenance (%s)", e.BundleID, e.Reason)
}

func (e *MaintenanceError) Unwrap() error { return ErrMaintained }

// Maintenance is the persistent pause record for one bundle.
type Maintenance struct {
	BundleID    string `json:"bundle_id"`
	Reason      string `json:"reason"`
	OperationID string `json:"operation_id,omitempty"`
	CreatedAt   string `json:"created_at"`
}

// maintenanceView is the JSON surface on application status.
type maintenanceView = Maintenance

func validReason(r string) bool {
	switch r {
	case MaintenanceImport, MaintenanceRestore, MaintenanceStorage:
		return true
	}
	return false
}

// GetMaintenance returns the maintenance record for a bundle, or
// ErrNotFound when the bundle is not paused.
func GetMaintenance(ctx context.Context, db *sql.DB, bundleID string) (Maintenance, error) {
	var m Maintenance
	err := db.QueryRowContext(ctx,
		`SELECT bundle_id, reason, operation_id, created_at FROM app_maintenance WHERE bundle_id = ?`,
		bundleID).Scan(&m.BundleID, &m.Reason, &m.OperationID, &m.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Maintenance{}, ErrNotFound
		}
		return Maintenance{}, err
	}
	return m, nil
}

// ListMaintenance returns every maintenance record ordered by bundle.
func ListMaintenance(ctx context.Context, db *sql.DB) ([]Maintenance, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT bundle_id, reason, operation_id, created_at FROM app_maintenance ORDER BY bundle_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Maintenance
	for rows.Next() {
		var m Maintenance
		if err := rows.Scan(&m.BundleID, &m.Reason, &m.OperationID, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// setMaintenance inserts or replaces the pause record for a bundle.
func setMaintenance(ctx context.Context, db *sql.DB, m Maintenance) error {
	if !validReason(m.Reason) {
		return fmt.Errorf("%w: reason %q must be import|restore|storage", ErrInvalid, m.Reason)
	}
	if m.CreatedAt == "" {
		m.CreatedAt = time.Now().UTC().Format(timeFormat)
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO app_maintenance (bundle_id, reason, operation_id, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (bundle_id) DO UPDATE SET reason = excluded.reason,
			operation_id = excluded.operation_id, created_at = excluded.created_at`,
		m.BundleID, m.Reason, m.OperationID, m.CreatedAt)
	return err
}

// clearMaintenance removes the pause record for a bundle. Records owned by
// restore/storage cannot be cleared by a generic resume; only the owning
// operation clears them.
func clearMaintenance(ctx context.Context, db *sql.DB, bundleID string, generic bool) error {
	if generic {
		var reason string
		err := db.QueryRowContext(ctx,
			`SELECT reason FROM app_maintenance WHERE bundle_id = ?`, bundleID).Scan(&reason)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if reason == MaintenanceRestore || reason == MaintenanceStorage {
			return fmt.Errorf("%w: maintenance for %q is owned by %s and cannot be cleared by generic resume",
				ErrConflict, bundleID, reason)
		}
	}
	_, err := db.ExecContext(ctx, `DELETE FROM app_maintenance WHERE bundle_id = ?`, bundleID)
	return err
}

// PauseForMaintenance records a pause for a bundle. Callers stop writers
// before acknowledging the pause; dependents stop in reverse dependency order
// without overwriting prior desired state (enforced by callers holding the
// maintenance lock).
func (s *Service) PauseForMaintenance(ctx context.Context, bundleID, reason, operationID string) (Maintenance, error) {
	s.catMu.RLock()
	_, ok := s.catalog.Get(bundleID)
	s.catMu.RUnlock()
	if !ok {
		return Maintenance{}, fmt.Errorf("%w: unknown bundle %q", ErrNotFound, bundleID)
	}
	if !validReason(reason) {
		return Maintenance{}, fmt.Errorf("%w: reason %q must be import|restore|storage", ErrInvalid, reason)
	}
	m := Maintenance{BundleID: bundleID, Reason: reason, OperationID: operationID,
		CreatedAt: s.now().UTC().Format(timeFormat)}
	if err := setMaintenance(ctx, s.db, m); err != nil {
		return Maintenance{}, err
	}
	return m, nil
}

// ResumeFromMaintenance clears a generic (import) pause after validation, only
// to prior desired state (enforced by callers restarting units).
func (s *Service) ResumeFromMaintenance(ctx context.Context, bundleID string) error {
	return clearMaintenance(ctx, s.db, bundleID, true)
}

// maintainedError returns a *MaintenanceError when the bundle is paused.
func (s *Service) maintainedError(ctx context.Context, bundleID string) error {
	m, err := GetMaintenance(ctx, s.db, bundleID)
	if err != nil {
		return nil
	}
	return &MaintenanceError{BundleID: m.BundleID, Reason: m.Reason, OperationID: m.OperationID}
}

// requireNotMaintained is the maintenance gate for mutating operations.
func (s *Service) requireNotMaintained(ctx context.Context, bundleID string) error {
	return s.maintainedError(ctx, bundleID)
}
