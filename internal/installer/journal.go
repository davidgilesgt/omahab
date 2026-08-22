package installer

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/omahab/omahab/internal/store"
)

// JournalStatus values.
const (
	JournalPending    = "pending"
	JournalRunning    = "running"
	JournalCompleted  = "completed"
	JournalFailed     = "failed"
	JournalRolledBack = "rolled_back"
)

// JournalEntry is one step record.
type JournalEntry struct {
	ID           string     `json:"id"`
	Step         string     `json:"step"`
	Status       string     `json:"status"`
	Attempt      int        `json:"attempt"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	Error        string     `json:"error,omitempty"`
	RollbackData string     `json:"rollback_data,omitempty"`
}

// Migrations returns the SQLite migrations owned by the installer controller.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "installer_journal",
			SQL: `
CREATE TABLE IF NOT EXISTS installer_journal (
    id            TEXT PRIMARY KEY,
    step          TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending',
    attempt       INTEGER NOT NULL DEFAULT 0,
    started_at    TEXT,
    finished_at   TEXT,
    error         TEXT NOT NULL DEFAULT '',
    rollback_data TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_installer_journal_step ON installer_journal(step);
CREATE INDEX IF NOT EXISTS idx_installer_journal_status ON installer_journal(status);

CREATE TABLE IF NOT EXISTS installer_state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);
`,
		},
	}
}

// JournalStore is the persistence layer for the journal. It wraps *sql.DB.
type JournalStore struct {
	db *sql.DB
}

// NewJournalStore creates a store backed by db. Caller must have run migrations.
func NewJournalStore(db *sql.DB) *JournalStore { return &JournalStore{db: db} }

// EnsureMigrations runs the installer migrations against db directly.
func EnsureMigrations(ctx context.Context, db *sql.DB) error {
	for _, m := range Migrations() {
		if _, err := db.ExecContext(ctx, m.SQL); err != nil {
			return fmt.Errorf("migration %s: %w", m.Name, err)
		}
	}
	return nil
}

// UpsertPending ensures a pending entry exists for each step in order.
func (s *JournalStore) UpsertPending(ctx context.Context, steps []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, step := range steps {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM installer_journal WHERE step = ?`, step).Scan(&id)
		if err == nil {
			continue
		}
		if err != sql.ErrNoRows {
			return err
		}
		newID := newID()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO installer_journal (id, step, status, attempt) VALUES (?, ?, ?, 0)`,
			newID, step, JournalPending); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkRunning transitions a step to running.
func (s *JournalStore) MarkRunning(ctx context.Context, step string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`UPDATE installer_journal SET status = ?, started_at = ?, attempt = attempt + 1 WHERE step = ?`,
		JournalRunning, now, step)
	return err
}

// MarkCompleted transitions a step to completed.
func (s *JournalStore) MarkCompleted(ctx context.Context, step string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`UPDATE installer_journal SET status = ?, finished_at = ?, error = '' WHERE step = ?`,
		JournalCompleted, now, step)
	return err
}

// MarkFailed transitions a step to failed with an error message.
func (s *JournalStore) MarkFailed(ctx context.Context, step, msg string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`UPDATE installer_journal SET status = ?, finished_at = ?, error = ? WHERE step = ?`,
		JournalFailed, now, msg, step)
	return err
}

// MarkRolledBack marks a step as rolled back.
func (s *JournalStore) MarkRolledBack(ctx context.Context, step string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`UPDATE installer_journal SET status = ?, finished_at = ? WHERE step = ?`,
		JournalRolledBack, now, step)
	return err
}

// SetRollbackData stores opaque rollback data for a step.
func (s *JournalStore) SetRollbackData(ctx context.Context, step, data string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE installer_journal SET rollback_data = ? WHERE step = ?`, data, step)
	return err
}

// List returns all journal entries in insertion order.
func (s *JournalStore) List(ctx context.Context) ([]JournalEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, step, status, attempt, started_at, finished_at, error, rollback_data FROM installer_journal ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JournalEntry
	for rows.Next() {
		var e JournalEntry
		var startedAt, finishedAt sql.NullString
		if err := rows.Scan(&e.ID, &e.Step, &e.Status, &e.Attempt, &startedAt, &finishedAt, &e.Error, &e.RollbackData); err != nil {
			return nil, err
		}
		if startedAt.Valid {
			t, _ := time.Parse(time.RFC3339Nano, startedAt.String)
			e.StartedAt = &t
		}
		if finishedAt.Valid {
			t, _ := time.Parse(time.RFC3339Nano, finishedAt.String)
			e.FinishedAt = &t
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Get returns the entry for a step.
func (s *JournalStore) Get(ctx context.Context, step string) (*JournalEntry, error) {
	var e JournalEntry
	var startedAt, finishedAt sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, step, status, attempt, started_at, finished_at, error, rollback_data FROM installer_journal WHERE step = ?`, step).
		Scan(&e.ID, &e.Step, &e.Status, &e.Attempt, &startedAt, &finishedAt, &e.Error, &e.RollbackData)
	if err != nil {
		return nil, err
	}
	if startedAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, startedAt.String)
		e.StartedAt = &t
	}
	if finishedAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, finishedAt.String)
		e.FinishedAt = &t
	}
	return &e, nil
}

func (s *JournalStore) SetState(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO installer_state (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

func (s *JournalStore) GetState(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM installer_state WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// ResetFailedToPending resets a failed step back to pending so it can be retried.
func (s *JournalStore) ResetFailedToPending(ctx context.Context, step string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE installer_journal SET status = ?, error = '' WHERE step = ? AND status = ?`,
		JournalPending, step, JournalFailed)
	return err
}
