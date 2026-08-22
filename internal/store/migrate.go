package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Migration is one named, ordered schema change. Controllers own their
// tables and expose their migrations as []Migration.
type Migration struct {
	Name string
	SQL  string
}

// Migrate applies migrations in the order given, recording each in
// schema_migrations. It is idempotent: migrations whose names are already
// recorded are skipped, so callers may pass their full migration list on
// every startup, and different controllers may migrate the same database
// with independent, overlapping lists.
//
// Each migration runs in its own transaction together with the insert that
// records it: if the SQL fails, the transaction rolls back and neither the
// schema change nor its bookkeeping remains. Duplicate or blank names in the
// given list are rejected before anything is applied.
//
// Where a 1:1 mapping exists, this method delegates to sqlc-generated queries
// (CreateSchemaMigrations, CheckSchemaMigration, InsertSchemaMigration).
// Dynamic per-migration DDL (m.SQL) remains an explicit Exec because sqlc
// cannot generate queries for arbitrary runtime SQL strings.
func (s *Store) Migrate(ctx context.Context, migrations ...Migration) error {
	if s == nil || s.db == nil {
		return Validation("store is not opened")
	}
	seen := make(map[string]struct{}, len(migrations))
	for _, m := range migrations {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			return Validation("migration name is required")
		}
		if _, dup := seen[name]; dup {
			return Validationf("duplicate migration name %q", name)
		}
		seen[name] = struct{}{}
		if strings.TrimSpace(m.SQL) == "" {
			return Validationf("migration %q has empty SQL", name)
		}
	}

	if err := New(s.db).CreateSchemaMigrations(ctx); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	for _, m := range migrations {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("store: migrate %q: %w", m.Name, err)
		}
		if err := s.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration applies one migration unless it is already recorded. The
// existence check runs inside the transaction so concurrent Migrate calls
// cannot double-apply a migration: transactions begin IMMEDIATE (see the
// _txlock DSN parameter), which serializes writers before the check.
//
// Generated queries cover the existence check (CheckSchemaMigration) and the
// bookkeeping insert (InsertSchemaMigration). The actual schema change
// (tx.Exec with m.SQL) is intentionally not generated — the SQL string is
// supplied at runtime per migration and cannot be statically analyzed by sqlc.
// Transaction boundaries (BeginTx/Commit/Rollback) are also explicit.
func (s *Store) applyMigration(ctx context.Context, m Migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration %q: %w", m.Name, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful commit

	qTx := New(tx)

	applied, err := qTx.CheckSchemaMigration(ctx, m.Name)
	if err != nil {
		return fmt.Errorf("store: check migration %q: %w", m.Name, err)
	}
	if applied {
		return nil // already recorded; the deferred rollback releases the tx
	}

	// Dynamic DDL — cannot be sqlc-generated. This is the only call site
	// that intentionally bypasses sqlc; every other store operation now
	// goes through generated queries.
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("store: apply migration %q: %w", m.Name, err)
	}
	if err := qTx.InsertSchemaMigration(ctx, InsertSchemaMigrationParams{
		Name:      m.Name,
		AppliedAt: FormatTime(time.Now()),
	}); err != nil {
		return fmt.Errorf("store: record migration %q: %w", m.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %q: %w", m.Name, err)
	}
	return nil
}
