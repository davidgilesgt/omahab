package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// Migrations returns the core migrations owned by the store package: the
// singleton instance identity row (DESIGN.md §4.1, "installation and
// instance identity"). Controllers' own migrations stay in their packages and
// are passed to Migrate alongside or after these.
func Migrations() []Migration {
	return []Migration{
		{
			Name: "core-001-instance",
			SQL: `CREATE TABLE IF NOT EXISTS instance (
	singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
	id TEXT NOT NULL UNIQUE,
	domain TEXT NOT NULL,
	tailnet TEXT NOT NULL DEFAULT '',
	tailscale_ip TEXT NOT NULL DEFAULT '',
	assistant_name TEXT NOT NULL,
	assistant_slug TEXT NOT NULL,
	created_at TEXT NOT NULL
)`,
		},
	}
}

// SaveInstance creates or updates the single instance identity row and
// returns the instance as persisted. Blank ID and CreatedAt fields are
// filled in; the created timestamp of an existing row is preserved. Saving
// with an ID different from the existing one fails with ErrConflict: the
// instance identity is stable and must not be silently replaced.
//
// This method now uses sqlc-generated queries for its CRUD operations
// (GetInstanceIdentity, CreateInstance, UpdateInstance) while retaining
// explicit transaction boundaries and domain validation. Schema migrations
// remain explicit.
func (s *Store) SaveInstance(ctx context.Context, in domain.Instance) (domain.Instance, error) {
	var zero domain.Instance
	if s == nil || s.db == nil {
		return zero, Validation("store is not opened")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, fmt.Errorf("store: save instance: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	qTx := New(tx)
	ident, err := qTx.GetInstanceIdentity(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if in.ID == "" {
			in.ID = domain.ID(NewID())
		}
		if in.CreatedAt.IsZero() {
			in.CreatedAt = time.Now().UTC()
		}
		if err := in.Validate(); err != nil {
			return zero, Validationf("%s", err)
		}
		if err := qTx.CreateInstance(ctx, CreateInstanceParams{
			ID:            string(in.ID),
			Domain:        in.Domain,
			Tailnet:       in.Tailnet,
			TailscaleIP:   in.TailscaleIP,
			AssistantName: in.AssistantName,
			AssistantSlug: in.AssistantSlug,
			CreatedAt:     FormatTime(in.CreatedAt),
		}); err != nil {
			return zero, fmt.Errorf("store: save instance: %w", Translate(err))
		}
	case err != nil:
		return zero, fmt.Errorf("store: save instance: %w", err)
	default:
		if in.ID == "" {
			in.ID = domain.ID(ident.ID)
		}
		if in.ID != domain.ID(ident.ID) {
			return zero, Conflictf("instance identity %q does not match existing %q", in.ID, ident.ID)
		}
		createdAt, err := ParseTime(ident.CreatedAt)
		if err != nil {
			return zero, fmt.Errorf("store: parse instance created_at: %w", err)
		}
		in.CreatedAt = createdAt
		if err := in.Validate(); err != nil {
			return zero, Validationf("%s", err)
		}
		if err := qTx.UpdateInstance(ctx, UpdateInstanceParams{
			Domain:        in.Domain,
			Tailnet:       in.Tailnet,
			TailscaleIP:   in.TailscaleIP,
			AssistantName: in.AssistantName,
			AssistantSlug: in.AssistantSlug,
		}); err != nil {
			return zero, fmt.Errorf("store: save instance: %w", Translate(err))
		}
	}

	if err := tx.Commit(); err != nil {
		return zero, fmt.Errorf("store: save instance: %w", err)
	}
	return in, nil
}

// Instance returns the instance identity. Before the first SaveInstance it
// fails with ErrNotFound.
//
// Uses the sqlc-generated GetInstance query.
func (s *Store) Instance(ctx context.Context) (domain.Instance, error) {
	var zero domain.Instance
	if s == nil || s.db == nil {
		return zero, Validation("store is not opened")
	}

	q := New(s.db)
	row, err := q.GetInstance(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return zero, NotFound("instance is not initialized")
	}
	if err != nil {
		return zero, fmt.Errorf("store: load instance: %w", err)
	}
	created, err := ParseTime(row.CreatedAt)
	if err != nil {
		return zero, fmt.Errorf("store: parse instance created_at: %w", err)
	}
	return domain.Instance{
		ID:            domain.ID(row.ID),
		Domain:        row.Domain,
		Tailnet:       row.Tailnet,
		TailscaleIP:   row.TailscaleIP,
		AssistantName: row.AssistantName,
		AssistantSlug: row.AssistantSlug,
		CreatedAt:     created,
	}, nil
}
