package apps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// Timestamps are stored as UTC RFC3339Nano text.
const timeFormat = time.RFC3339Nano

// appRecord is the persisted row for one application, including fields the
// shared domain view does not carry (bundle ID, release pointers, last error).
type appRecord struct {
	ID                domain.ID
	Name              string
	BundleID          string
	Image             string
	Digest            string
	Hostname          string
	Exposure          domain.Exposure
	Health            domain.Health
	DesiredState      string
	ObservedState     string
	CurrentReleaseID  domain.ID
	PreviousReleaseID domain.ID
	InstalledAt       *time.Time
	UpdatedAt         time.Time
	LastError         string
}

func (r appRecord) application() domain.Application {
	return domain.Application{
		ID:            r.ID,
		Name:          r.Name,
		BundleID:      r.BundleID,
		Image:         r.Image,
		Digest:        r.Digest,
		Hostname:      r.Hostname,
		Exposure:      r.Exposure,
		Health:        r.Health,
		DesiredState:  r.DesiredState,
		ObservedState: r.ObservedState,
		InstalledAt:   r.InstalledAt,
		UpdatedAt:     r.UpdatedAt,
	}
}

// releaseRecord retains the exact rendered Compose definition for one digest
// so deploys and rollbacks are byte-deterministic.
type releaseRecord struct {
	ID        domain.ID
	AppID     domain.ID
	Digest    string
	Compose   string
	CreatedAt time.Time
}

type scanner interface{ Scan(dest ...any) error }

const appColumns = `id, name, bundle_id, image, digest, hostname, exposure, health, desired_state, observed_state, current_release_id, previous_release_id, installed_at, updated_at, last_error`

func scanApp(s scanner) (appRecord, error) {
	var rec appRecord
	var installed sql.NullString
	var updated string
	err := s.Scan(&rec.ID, &rec.Name, &rec.BundleID, &rec.Image, &rec.Digest, &rec.Hostname,
		&rec.Exposure, &rec.Health, &rec.DesiredState, &rec.ObservedState,
		&rec.CurrentReleaseID, &rec.PreviousReleaseID, &installed, &updated, &rec.LastError)
	if err != nil {
		return appRecord{}, err
	}
	if installed.Valid && installed.String != "" {
		t, err := time.Parse(timeFormat, installed.String)
		if err != nil {
			return appRecord{}, fmt.Errorf("parse installed_at for app %s: %w", rec.ID, err)
		}
		rec.InstalledAt = &t
	}
	rec.UpdatedAt, err = time.Parse(timeFormat, updated)
	if err != nil {
		return appRecord{}, fmt.Errorf("parse updated_at for app %s: %w", rec.ID, err)
	}
	return rec, nil
}

func getApp(ctx context.Context, db *sql.DB, id domain.ID) (appRecord, error) {
	row := db.QueryRowContext(ctx, `SELECT `+appColumns+` FROM apps WHERE id = ?`, id)
	rec, err := scanApp(row)
	if errors.Is(err, sql.ErrNoRows) {
		return appRecord{}, fmt.Errorf("%w: app %s", ErrNotFound, id)
	}
	return rec, err
}

func getAppByName(ctx context.Context, db *sql.DB, name string) (appRecord, error) {
	row := db.QueryRowContext(ctx, `SELECT `+appColumns+` FROM apps WHERE name = ?`, name)
	rec, err := scanApp(row)
	if errors.Is(err, sql.ErrNoRows) {
		return appRecord{}, fmt.Errorf("%w: app %q", ErrNotFound, name)
	}
	return rec, err
}

func listApps(ctx context.Context, db *sql.DB) ([]appRecord, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+appColumns+` FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []appRecord
	for rows.Next() {
		rec, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func getRelease(ctx context.Context, db *sql.DB, id domain.ID) (releaseRecord, error) {
	row := db.QueryRowContext(ctx,
		`SELECT id, app_id, digest, compose, created_at FROM app_releases WHERE id = ?`, id)
	var rel releaseRecord
	var created string
	err := row.Scan(&rel.ID, &rel.AppID, &rel.Digest, &rel.Compose, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return releaseRecord{}, fmt.Errorf("%w: release %s", ErrNotFound, id)
	}
	if err != nil {
		return releaseRecord{}, err
	}
	rel.CreatedAt, err = time.Parse(timeFormat, created)
	if err != nil {
		return releaseRecord{}, fmt.Errorf("parse created_at for release %s: %w", id, err)
	}
	return rel, nil
}

func insertRelease(ctx context.Context, db *sql.DB, rel releaseRecord) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO app_releases (id, app_id, digest, compose, created_at) VALUES (?, ?, ?, ?, ?)`,
		rel.ID, rel.AppID, rel.Digest, rel.Compose, rel.CreatedAt.Format(timeFormat))
	return err
}

func deleteRelease(ctx context.Context, db *sql.DB, id domain.ID) error {
	_, err := db.ExecContext(ctx, `DELETE FROM app_releases WHERE id = ?`, id)
	return err
}

// createAppWithRelease claims the application name and its initial release
// atomically, with observed state left to the caller (provisioning).
func createAppWithRelease(ctx context.Context, db *sql.DB, rec appRecord, rel releaseRecord) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The app row must exist before the release that references it
	// (foreign keys are enforced by the shared store).
	var installed any
	if rec.InstalledAt != nil {
		installed = rec.InstalledAt.UTC().Format(timeFormat)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO apps (id, name, bundle_id, image, digest, hostname, exposure, health,
                  desired_state, observed_state, current_release_id, previous_release_id,
                  installed_at, updated_at, last_error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, '')`,
		rec.ID, rec.Name, rec.BundleID, rec.Image, rec.Digest, rec.Hostname, rec.Exposure,
		rec.Health, rec.DesiredState, rec.ObservedState, rec.CurrentReleaseID,
		installed, rec.UpdatedAt.UTC().Format(timeFormat)); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: app %q", ErrAlreadyExists, rec.Name)
		}
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO app_releases (id, app_id, digest, compose, created_at) VALUES (?, ?, ?, ?, ?)`,
		rel.ID, rel.AppID, rel.Digest, rel.Compose, rel.CreatedAt.Format(timeFormat)); err != nil {
		return err
	}
	return tx.Commit()
}

func setDesired(ctx context.Context, db *sql.DB, id domain.ID, desired string, ts time.Time) error {
	res, err := db.ExecContext(ctx,
		`UPDATE apps SET desired_state = ?, updated_at = ? WHERE id = ?`,
		desired, ts.UTC().Format(timeFormat), id)
	if err != nil {
		return err
	}
	return requireUpdated(res, id)
}

func setObserved(ctx context.Context, db *sql.DB, id domain.ID, observed string, health domain.Health, lastErr string, ts time.Time) error {
	res, err := db.ExecContext(ctx,
		`UPDATE apps SET observed_state = ?, health = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		observed, health, lastErr, ts.UTC().Format(timeFormat), id)
	if err != nil {
		return err
	}
	return requireUpdated(res, id)
}

func setHealth(ctx context.Context, db *sql.DB, id domain.ID, health domain.Health, ts time.Time) error {
	res, err := db.ExecContext(ctx,
		`UPDATE apps SET health = ?, updated_at = ? WHERE id = ?`,
		health, ts.UTC().Format(timeFormat), id)
	if err != nil {
		return err
	}
	return requireUpdated(res, id)
}

// activateRelease promotes a release to current, demotes the previous
// current release to previous, and prunes every older release: exactly the
// current and previous releases are retained for rollback.
func activateRelease(ctx context.Context, db *sql.DB, appID, currentID, previousID domain.ID, digest, observed string, health domain.Health, lastErr string, ts time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
UPDATE apps
SET current_release_id = ?, previous_release_id = ?, digest = ?,
    observed_state = ?, health = ?, last_error = ?, updated_at = ?
WHERE id = ?`,
		currentID, previousID, digest, observed, health, lastErr, ts.UTC().Format(timeFormat), appID)
	if err != nil {
		return err
	}
	if err := requireUpdated(res, appID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM app_releases WHERE app_id = ? AND id NOT IN (?, ?)`, appID, currentID, previousID); err != nil {
		return err
	}
	return tx.Commit()
}

func deleteApp(ctx context.Context, db *sql.DB, id domain.ID) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM app_releases WHERE app_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM apps WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if err := requireUpdated(res, id); err != nil {
		return err
	}
	return tx.Commit()
}

func requireUpdated(res sql.Result, id domain.ID) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: app %s", ErrNotFound, id)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
