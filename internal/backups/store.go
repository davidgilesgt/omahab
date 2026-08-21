package backups

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// timeLayout is a fixed-width RFC 3339 variant so lexicographic SQL ordering
// (MAX, ORDER BY) always matches chronological order. All timestamps are
// stored in UTC.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) { return time.Parse(timeLayout, s) }

type rowScanner interface {
	Scan(dest ...any) error
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func nullTime(t *time.Time) sql.NullString {
	if t == nil || t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: fmtTime(*t), Valid: true}
}

func nullStats(st *RunStats) sql.NullString {
	if st == nil {
		return sql.NullString{}
	}
	b, err := json.Marshal(st)
	if err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(b), Valid: true}
}

func mapConflict(err error, msg string) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return fmt.Errorf("%w: %s", ErrConflict, msg)
	}
	return err
}

func isSingleActiveViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") && strings.Contains(msg, "backup_runs")
}

// --- repositories ---

const repositoryColumns = "id, label, location, secret_id, secret_version, created_at, updated_at"

func scanRepository(sc rowScanner) (Repository, error) {
	var (
		r       Repository
		created string
		updated string
	)
	if err := sc.Scan(&r.ID, &r.Label, &r.Location, &r.SecretRef.ID, &r.SecretRef.Version, &created, &updated); err != nil {
		return Repository{}, err
	}
	var err error
	if r.CreatedAt, err = parseTime(created); err != nil {
		return Repository{}, fmt.Errorf("parse repository created_at: %w", err)
	}
	if r.UpdatedAt, err = parseTime(updated); err != nil {
		return Repository{}, fmt.Errorf("parse repository updated_at: %w", err)
	}
	return r, nil
}

func (s *Service) listRepositories(ctx context.Context) ([]Repository, error) {
	rows, err := s.st.DB().QueryContext(ctx, "SELECT "+repositoryColumns+" FROM backup_repositories ORDER BY created_at, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repository
	for rows.Next() {
		r, err := scanRepository(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) getRepository(ctx context.Context, id string) (Repository, error) {
	r, err := scanRepository(s.st.DB().QueryRowContext(ctx, "SELECT "+repositoryColumns+" FROM backup_repositories WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Repository{}, fmt.Errorf("%w: repository %q", ErrNotFound, id)
	}
	return r, err
}

func (s *Service) insertRepository(ctx context.Context, r Repository) error {
	_, err := s.st.DB().ExecContext(ctx,
		"INSERT INTO backup_repositories (id, label, location, secret_id, secret_version, created_at, updated_at) VALUES (?,?,?,?,?,?,?)",
		r.ID, r.Label, r.Location, r.SecretRef.ID, r.SecretRef.Version, fmtTime(r.CreatedAt), fmtTime(r.UpdatedAt))
	return mapConflict(err, "a repository already exists for this location")
}

func (s *Service) updateRepository(ctx context.Context, r Repository) error {
	_, err := s.st.DB().ExecContext(ctx,
		"UPDATE backup_repositories SET label = ?, location = ?, secret_id = ?, secret_version = ?, updated_at = ? WHERE id = ?",
		r.Label, r.Location, r.SecretRef.ID, r.SecretRef.Version, fmtTime(r.UpdatedAt), r.ID)
	return mapConflict(err, "a repository already exists for this location")
}

// --- runs ---

const runColumns = "id, kind, repository_id, status, triggered_by, stage, snapshot_id, error, stats, started_at, finished_at"

func scanRun(sc rowScanner) (Run, error) {
	var (
		r        Run
		stage    sql.NullString
		snapshot sql.NullString
		runErr   sql.NullString
		stats    sql.NullString
		finished sql.NullString
		started  string
	)
	if err := sc.Scan(&r.ID, &r.Kind, &r.RepositoryID, &r.Status, &r.Trigger, &stage, &snapshot, &runErr, &stats, &started, &finished); err != nil {
		return Run{}, err
	}
	r.Stage = stage.String
	r.SnapshotID = snapshot.String
	r.Error = runErr.String
	var err error
	if r.StartedAt, err = parseTime(started); err != nil {
		return Run{}, fmt.Errorf("parse run started_at: %w", err)
	}
	if finished.Valid {
		t, err := parseTime(finished.String)
		if err != nil {
			return Run{}, fmt.Errorf("parse run finished_at: %w", err)
		}
		r.FinishedAt = &t
	}
	if stats.Valid && stats.String != "" {
		var st RunStats
		if err := json.Unmarshal([]byte(stats.String), &st); err != nil {
			return Run{}, fmt.Errorf("parse run stats: %w", err)
		}
		r.Stats = &st
	}
	return r, nil
}

func (s *Service) insertRun(ctx context.Context, r *Run) error {
	_, err := s.st.DB().ExecContext(ctx,
		"INSERT INTO backup_runs (id, kind, repository_id, status, triggered_by, stage, snapshot_id, error, stats, started_at, finished_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		r.ID, r.Kind, r.RepositoryID, r.Status, r.Trigger, r.Stage, r.SnapshotID, nullStr(r.Error), nullStats(r.Stats), fmtTime(r.StartedAt), nullTime(r.FinishedAt))
	if isSingleActiveViolation(err) {
		return ErrOperationInProgress
	}
	return err
}

func (s *Service) updateRun(ctx context.Context, r *Run) error {
	_, err := s.st.DB().ExecContext(ctx,
		"UPDATE backup_runs SET status = ?, triggered_by = ?, stage = ?, snapshot_id = ?, error = ?, stats = ?, started_at = ?, finished_at = ? WHERE id = ?",
		r.Status, r.Trigger, r.Stage, r.SnapshotID, nullStr(r.Error), nullStats(r.Stats), fmtTime(r.StartedAt), nullTime(r.FinishedAt), r.ID)
	return err
}

func (s *Service) getRun(ctx context.Context, id string) (Run, error) {
	r, err := scanRun(s.st.DB().QueryRowContext(ctx, "SELECT "+runColumns+" FROM backup_runs WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, fmt.Errorf("%w: run %q", ErrNotFound, id)
	}
	return r, err
}

func (s *Service) listRuns(ctx context.Context, f ListFilter) ([]Run, error) {
	q := "SELECT " + runColumns + " FROM backup_runs"
	var conds []string
	var args []any
	if f.RepositoryID != "" {
		conds = append(conds, "repository_id = ?")
		args = append(args, f.RepositoryID)
	}
	if f.Kind != "" {
		conds = append(conds, "kind = ?")
		args = append(args, f.Kind)
	}
	if f.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, f.Status)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultListingLimit
	}
	if limit > maxListingLimit {
		limit = maxListingLimit
	}
	q += " ORDER BY started_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) activeRun(ctx context.Context) (*Run, error) {
	runs, err := s.listRuns(ctx, ListFilter{Status: StatusRunning, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, nil
	}
	return &runs[0], nil
}

// --- snapshots ---

const snapshotColumns = "id, repository_id, run_id, created_at, paths, size_bytes, file_count, verified_at"

func scanSnapshot(sc rowScanner) (Snapshot, error) {
	var (
		snap     Snapshot
		paths    string
		created  string
		verified sql.NullString
	)
	if err := sc.Scan(&snap.ID, &snap.RepositoryID, &snap.RunID, &created, &paths, &snap.SizeBytes, &snap.FileCount, &verified); err != nil {
		return Snapshot{}, err
	}
	var err error
	if snap.CreatedAt, err = parseTime(created); err != nil {
		return Snapshot{}, fmt.Errorf("parse snapshot created_at: %w", err)
	}
	if paths != "" && paths != "null" {
		if err := json.Unmarshal([]byte(paths), &snap.Paths); err != nil {
			return Snapshot{}, fmt.Errorf("parse snapshot paths: %w", err)
		}
	}
	if verified.Valid {
		t, err := parseTime(verified.String)
		if err != nil {
			return Snapshot{}, fmt.Errorf("parse snapshot verified_at: %w", err)
		}
		snap.VerifiedAt = &t
	}
	return snap, nil
}

func (s *Service) upsertSnapshot(ctx context.Context, snap Snapshot) error {
	paths, err := json.Marshal(snap.Paths)
	if err != nil {
		return err
	}
	if snap.Paths == nil {
		paths = []byte("[]")
	}
	_, err = s.st.DB().ExecContext(ctx, `INSERT INTO backup_snapshots (id, repository_id, run_id, created_at, paths, size_bytes, file_count, verified_at)
VALUES (?,?,?,?,?,?,?,?)
ON CONFLICT (repository_id, id) DO UPDATE SET
	run_id = excluded.run_id,
	created_at = excluded.created_at,
	paths = excluded.paths,
	size_bytes = excluded.size_bytes,
	file_count = excluded.file_count`,
		snap.ID, snap.RepositoryID, snap.RunID, fmtTime(snap.CreatedAt), string(paths), snap.SizeBytes, snap.FileCount, nullTime(snap.VerifiedAt))
	return err
}

func (s *Service) findSnapshot(ctx context.Context, id string) (Snapshot, error) {
	snap, err := scanSnapshot(s.st.DB().QueryRowContext(ctx, "SELECT "+snapshotColumns+" FROM backup_snapshots WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("%w: snapshot %q", ErrNotFound, id)
	}
	return snap, err
}

func (s *Service) latestSnapshot(ctx context.Context, repositoryID string) (Snapshot, error) {
	snap, err := scanSnapshot(s.st.DB().QueryRowContext(ctx, "SELECT "+snapshotColumns+" FROM backup_snapshots WHERE repository_id = ? ORDER BY created_at DESC LIMIT 1", repositoryID))
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("%w: repository %q has no snapshots", ErrNoSnapshot, repositoryID)
	}
	return snap, err
}

func (s *Service) listSnapshots(ctx context.Context, repositoryID string) ([]Snapshot, error) {
	q := "SELECT " + snapshotColumns + " FROM backup_snapshots"
	var args []any
	if repositoryID != "" {
		q += " WHERE repository_id = ?"
		args = append(args, repositoryID)
	}
	q += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, maxListingLimit)
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		snap, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}

// markSnapshotVerified records that a successful restore demonstrated the
// snapshot is restorable.
func (s *Service) markSnapshotVerified(ctx context.Context, repositoryID, snapshotID string, at time.Time) error {
	res, err := s.st.DB().ExecContext(ctx,
		"UPDATE backup_snapshots SET verified_at = ? WHERE repository_id = ? AND id = ?",
		fmtTime(at), repositoryID, snapshotID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: snapshot %q", ErrNotFound, snapshotID)
	}
	return nil
}

// --- hook results ---

const hookColumns = "id, run_id, application, hook, status, exit_code, error, output, started_at, finished_at"

func scanHookResult(sc rowScanner) (HookResult, error) {
	var (
		h        HookResult
		exit     sql.NullInt64
		herr     sql.NullString
		output   sql.NullString
		started  string
		finished sql.NullString
	)
	if err := sc.Scan(&h.ID, &h.RunID, &h.Application, &h.Hook, &h.Status, &exit, &herr, &output, &started, &finished); err != nil {
		return HookResult{}, err
	}
	if exit.Valid {
		code := int(exit.Int64)
		h.ExitCode = &code
	}
	h.Error = herr.String
	h.Output = output.String
	var err error
	if h.StartedAt, err = parseTime(started); err != nil {
		return HookResult{}, fmt.Errorf("parse hook started_at: %w", err)
	}
	if finished.Valid {
		t, err := parseTime(finished.String)
		if err != nil {
			return HookResult{}, fmt.Errorf("parse hook finished_at: %w", err)
		}
		h.FinishedAt = &t
	}
	return h, nil
}

func (s *Service) insertHookResult(ctx context.Context, h HookResult) error {
	_, err := s.st.DB().ExecContext(ctx,
		"INSERT INTO backup_hook_results (id, run_id, application, hook, status, exit_code, error, output, started_at, finished_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
		h.ID, h.RunID, h.Application, h.Hook, h.Status, h.ExitCode, nullStr(h.Error), nullStr(h.Output), fmtTime(h.StartedAt), nullTime(h.FinishedAt))
	return err
}

func (s *Service) listHookResults(ctx context.Context, runID string) ([]HookResult, error) {
	rows, err := s.st.DB().QueryContext(ctx, "SELECT "+hookColumns+" FROM backup_hook_results WHERE run_id = ? ORDER BY rowid", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HookResult
	for rows.Next() {
		h, err := scanHookResult(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// --- verifications ---

const verificationColumns = "id, run_id, repository_id, snapshot_id, status, target, files_restored, bytes_restored, started_at, finished_at, cleaned_at, error, cleanup_error"

func scanVerification(sc rowScanner) (Verification, error) {
	var (
		v        Verification
		started  string
		finished sql.NullString
		cleaned  sql.NullString
		verr     sql.NullString
		cerr     sql.NullString
	)
	if err := sc.Scan(&v.ID, &v.RunID, &v.RepositoryID, &v.SnapshotID, &v.Status, &v.Target, &v.FilesRestored, &v.BytesRestored, &started, &finished, &cleaned, &verr, &cerr); err != nil {
		return Verification{}, err
	}
	v.Error = verr.String
	v.CleanupError = cerr.String
	var err error
	if v.StartedAt, err = parseTime(started); err != nil {
		return Verification{}, fmt.Errorf("parse verification started_at: %w", err)
	}
	if finished.Valid {
		t, err := parseTime(finished.String)
		if err != nil {
			return Verification{}, fmt.Errorf("parse verification finished_at: %w", err)
		}
		v.FinishedAt = &t
	}
	if cleaned.Valid {
		t, err := parseTime(cleaned.String)
		if err != nil {
			return Verification{}, fmt.Errorf("parse verification cleaned_at: %w", err)
		}
		v.CleanedAt = &t
	}
	return v, nil
}

func (s *Service) insertVerification(ctx context.Context, v *Verification) error {
	_, err := s.st.DB().ExecContext(ctx,
		"INSERT INTO backup_verifications (id, run_id, repository_id, snapshot_id, status, target, files_restored, bytes_restored, started_at, finished_at, cleaned_at, error, cleanup_error) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
		v.ID, v.RunID, v.RepositoryID, v.SnapshotID, v.Status, v.Target, v.FilesRestored, v.BytesRestored, fmtTime(v.StartedAt), nullTime(v.FinishedAt), nullTime(v.CleanedAt), nullStr(v.Error), nullStr(v.CleanupError))
	return err
}

func (s *Service) updateVerification(ctx context.Context, v *Verification) error {
	_, err := s.st.DB().ExecContext(ctx,
		"UPDATE backup_verifications SET status = ?, files_restored = ?, bytes_restored = ?, finished_at = ?, cleaned_at = ?, error = ?, cleanup_error = ? WHERE id = ?",
		v.Status, v.FilesRestored, v.BytesRestored, nullTime(v.FinishedAt), nullTime(v.CleanedAt), nullStr(v.Error), nullStr(v.CleanupError), v.ID)
	return err
}

func (s *Service) getVerificationByRun(ctx context.Context, runID string) (Verification, error) {
	v, err := scanVerification(s.st.DB().QueryRowContext(ctx, "SELECT "+verificationColumns+" FROM backup_verifications WHERE run_id = ? LIMIT 1", runID))
	if errors.Is(err, sql.ErrNoRows) {
		return Verification{}, fmt.Errorf("%w: verification for run %q", ErrNotFound, runID)
	}
	return v, err
}

func (s *Service) listVerificationsByStatus(ctx context.Context, status VerificationStatus) ([]Verification, error) {
	rows, err := s.st.DB().QueryContext(ctx, "SELECT "+verificationColumns+" FROM backup_verifications WHERE status = ? ORDER BY started_at", status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Verification
	for rows.Next() {
		v, err := scanVerification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Service) listVerifications(ctx context.Context, limit int) ([]Verification, error) {
	if limit <= 0 {
		limit = defaultListingLimit
	}
	if limit > maxListingLimit {
		limit = maxListingLimit
	}
	rows, err := s.st.DB().QueryContext(ctx, "SELECT "+verificationColumns+" FROM backup_verifications ORDER BY started_at DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Verification
	for rows.Next() {
		v, err := scanVerification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// --- health state and aggregates ---

func (s *Service) getHealthState(ctx context.Context) (domain.Health, bool, error) {
	var health string
	err := s.st.DB().QueryRowContext(ctx, "SELECT health FROM backup_health_state WHERE id = 1").Scan(&health)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.HealthUnknown, false, nil
	}
	if err != nil {
		return domain.HealthUnknown, false, err
	}
	return domain.Health(health), true, nil
}

func (s *Service) saveHealthState(ctx context.Context, h domain.Health, rep StatusReport) error {
	_, err := s.st.DB().ExecContext(ctx, `INSERT INTO backup_health_state (id, health, last_backup_at, last_verified_at, evaluated_at) VALUES (1,?,?,?,?)
ON CONFLICT (id) DO UPDATE SET
	health = excluded.health,
	last_backup_at = excluded.last_backup_at,
	last_verified_at = excluded.last_verified_at,
	evaluated_at = excluded.evaluated_at`,
		string(h), nullTime(rep.LastBackupAt), nullTime(rep.LastVerifiedAt), fmtTime(s.nowUTC()))
	return err
}

// lastCompletedBackupAt returns the finish time of the most recent
// successful backup run.
func (s *Service) lastCompletedBackupAt(ctx context.Context) (*time.Time, error) {
	var finished sql.NullString
	err := s.st.DB().QueryRowContext(ctx,
		"SELECT finished_at FROM backup_runs WHERE kind = 'backup' AND status = 'completed' AND finished_at IS NOT NULL ORDER BY finished_at DESC LIMIT 1").Scan(&finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !finished.Valid {
		return nil, nil
	}
	t, err := parseTime(finished.String)
	if err != nil {
		return nil, fmt.Errorf("parse last backup time: %w", err)
	}
	return &t, nil
}

// lastVerifiedRestoreAt returns the most recent proof of restorability: the
// finish time of the newest passed verification or completed restore run.
func (s *Service) lastVerifiedRestoreAt(ctx context.Context) (*time.Time, error) {
	var latest sql.NullString
	err := s.st.DB().QueryRowContext(ctx, `SELECT MAX(f) FROM (
	SELECT finished_at AS f FROM backup_verifications WHERE status = 'passed' AND finished_at IS NOT NULL
	UNION ALL
	SELECT finished_at AS f FROM backup_runs WHERE kind = 'restore' AND status = 'completed' AND finished_at IS NOT NULL
)`).Scan(&latest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !latest.Valid {
		return nil, nil
	}
	t, err := parseTime(latest.String)
	if err != nil {
		return nil, fmt.Errorf("parse last verified restore time: %w", err)
	}
	return &t, nil
}

func (s *Service) countRunsForRepository(ctx context.Context, repositoryID string) (int, error) {
	var n int
	err := s.st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM backup_runs WHERE repository_id = ?", repositoryID).Scan(&n)
	return n, err
}
